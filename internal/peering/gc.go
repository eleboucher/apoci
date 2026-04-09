package peering

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"codeberg.org/gruf/go-runners"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/util"
)

type GCRepository interface {
	CleanupStalePeerBlobs(ctx context.Context, olderThan time.Duration) (int64, error)
	OrphanedBlobs(ctx context.Context, limit int) ([]string, error)
	DeleteBlob(ctx context.Context, digest string) error
	AllBlobDigests(ctx context.Context, pageSize int) (map[string]bool, error)
	PruneDeletedManifests(ctx context.Context, olderThan time.Duration) (int64, error)
}

type Notifier interface {
	Send(event, message string)
}

type GCConfig struct {
	Interval         time.Duration
	StalePeerBlobAge time.Duration
	OrphanBatchSize  int
}

type GarbageCollector struct {
	cfg      GCConfig
	db       GCRepository
	blobs    blobstore.BlobStore
	notifier Notifier
	logger   *slog.Logger
	service  runners.Service
}

func NewGarbageCollector(cfg GCConfig, db GCRepository, blobs blobstore.BlobStore, notifier Notifier, logger *slog.Logger) *GarbageCollector {
	return &GarbageCollector{
		cfg:      cfg,
		db:       db,
		blobs:    blobs,
		notifier: notifier,
		logger:   logger,
	}
}

func (gc *GarbageCollector) Start(ctx context.Context) {
	gc.service.GoRun(func(svcCtx context.Context) {
		util.Must(gc.logger, func() {
			gc.run(ctx, svcCtx)
		})
	})
}

func (gc *GarbageCollector) Stop() {
	gc.service.Stop()
}

func (gc *GarbageCollector) run(parentCtx, svcCtx context.Context) {
	// Run once shortly after startup.
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-svcCtx.Done():
			return
		case <-parentCtx.Done():
			return
		case <-timer.C:
			gc.collect(parentCtx)
			timer.Reset(gc.cfg.Interval)
		}
	}
}

const deletedManifestRetention = 30 * 24 * time.Hour

func (gc *GarbageCollector) collect(ctx context.Context) {
	gc.logger.Info("starting garbage collection cycle")

	gc.cleanupStalePeerBlobs(ctx)
	gc.cleanupOrphanedBlobMetadata(ctx)
	gc.cleanupOrphanedBlobFiles(ctx)
	gc.pruneDeletedManifests(ctx)

	metrics.GCCyclesCompleted.Add(1)
	gc.logger.Info("garbage collection cycle complete")
}

func (gc *GarbageCollector) pruneDeletedManifests(ctx context.Context) {
	n, err := gc.db.PruneDeletedManifests(ctx, deletedManifestRetention)
	if err != nil {
		gc.logger.Error("gc: failed to prune deleted manifest tombstones", "error", err)
		return
	}
	if n > 0 {
		gc.logger.Info("gc: pruned old manifest tombstones", "count", n)
	}
}

// cleanupStalePeerBlobs removes peer blob references not verified in 30 days.
func (gc *GarbageCollector) cleanupStalePeerBlobs(ctx context.Context) {
	n, err := gc.db.CleanupStalePeerBlobs(ctx, gc.cfg.StalePeerBlobAge)
	if err != nil {
		gc.logger.Error("gc: failed to cleanup stale peer blobs", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to cleanup stale peer blobs: %v", err))
		return
	}
	if n > 0 {
		metrics.GCStalePeerBlobs.Add(float64(n))
		gc.logger.Info("gc: removed stale peer blob references", "count", n)
	}
}

// cleanupOrphanedBlobMetadata removes blob DB records that are not stored locally
// and have no peer references or manifest layer references.
func (gc *GarbageCollector) cleanupOrphanedBlobMetadata(ctx context.Context) {
	digests, err := gc.db.OrphanedBlobs(ctx, gc.cfg.OrphanBatchSize)
	if err != nil {
		gc.logger.Error("gc: failed to find orphaned blobs", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to find orphaned blobs: %v", err))
		return
	}

	removed := 0
	for _, digest := range digests {
		if err := gc.db.DeleteBlob(ctx, digest); err != nil {
			gc.logger.Warn("gc: failed to delete orphaned blob metadata", "digest", digest, "error", err)
			continue
		}
		removed++
	}

	if removed > 0 {
		metrics.GCOrphanedMetadata.Add(float64(removed))
		gc.logger.Info("gc: removed orphaned blob metadata", "count", removed)
	}
}

// cleanupOrphanedBlobFiles removes blob files on disk that have no corresponding DB record.
func (gc *GarbageCollector) cleanupOrphanedBlobFiles(ctx context.Context) {
	// Snapshot disk digests before DB digests to avoid deleting a blob that was
	// written to disk after the disk snapshot but before the DB snapshot.
	diskDigests, err := gc.blobs.ListDigests(ctx)
	if err != nil {
		gc.logger.Error("gc: failed to list blob files on disk", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to list blob files on disk: %v", err))
		return
	}

	knownDigests, err := gc.db.AllBlobDigests(ctx, 1000)
	if err != nil {
		gc.logger.Error("gc: failed to list known blob digests", "error", err)
		gc.notifier.Send(notify.EventGCError, fmt.Sprintf("GC: failed to list known blob digests: %v", err))
		return
	}

	removed := 0
	for _, digest := range diskDigests {
		if knownDigests[digest] {
			continue
		}
		if err := gc.blobs.Delete(ctx, digest); err != nil {
			gc.logger.Warn("gc: failed to delete orphaned blob file", "digest", digest, "error", err)
			continue
		}
		removed++
	}

	if removed > 0 {
		metrics.GCOrphanedFiles.Add(float64(removed))
		gc.logger.Info("gc: removed orphaned blob files from disk", "count", removed)
	}
}
