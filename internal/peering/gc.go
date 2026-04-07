package peering

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
)

const (
	gcInterval       = 6 * time.Hour
	stalePeerBlobAge = 30 * 24 * time.Hour // 30 days
	orphanBatchSize  = 500
)

// GarbageCollector periodically cleans up stale data.
type GarbageCollector struct {
	db     *database.DB
	blobs  *blobstore.Store
	logger *slog.Logger
	wg     sync.WaitGroup
	stop   chan struct{}
	once   sync.Once
}

func NewGarbageCollector(db *database.DB, blobs *blobstore.Store, logger *slog.Logger) *GarbageCollector {
	return &GarbageCollector{
		db:     db,
		blobs:  blobs,
		logger: logger,
		stop:   make(chan struct{}),
	}
}

func (gc *GarbageCollector) Start(ctx context.Context) {
	gc.wg.Add(1)
	go gc.run(ctx)
}

func (gc *GarbageCollector) Stop() {
	gc.once.Do(func() { close(gc.stop) })
	gc.wg.Wait()
}

func (gc *GarbageCollector) run(ctx context.Context) {
	defer gc.wg.Done()

	// Run once shortly after startup.
	timer := time.NewTimer(1 * time.Minute)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-gc.stop:
			return
		case <-timer.C:
			gc.collect(ctx)
			timer.Reset(gcInterval)
		}
	}
}

func (gc *GarbageCollector) collect(ctx context.Context) {
	gc.logger.Info("starting garbage collection cycle")

	gc.cleanupStalePeerBlobs(ctx)
	gc.cleanupOrphanedBlobMetadata(ctx)
	gc.cleanupOrphanedBlobFiles(ctx)

	metrics.GCCyclesCompleted.Add(1)
	gc.logger.Info("garbage collection cycle complete")
}

// cleanupStalePeerBlobs removes peer blob references not verified in 30 days.
func (gc *GarbageCollector) cleanupStalePeerBlobs(ctx context.Context) {
	n, err := gc.db.CleanupStalePeerBlobs(ctx, stalePeerBlobAge)
	if err != nil {
		gc.logger.Error("gc: failed to cleanup stale peer blobs", "error", err)
		return
	}
	if n > 0 {
		metrics.GCStalePeerBlobs.Add(n)
		gc.logger.Info("gc: removed stale peer blob references", "count", n)
	}
}

// cleanupOrphanedBlobMetadata removes blob DB records that are not stored locally
// and have no peer references or manifest layer references.
func (gc *GarbageCollector) cleanupOrphanedBlobMetadata(ctx context.Context) {
	digests, err := gc.db.OrphanedBlobs(ctx, orphanBatchSize)
	if err != nil {
		gc.logger.Error("gc: failed to find orphaned blobs", "error", err)
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
		metrics.GCOrphanedMetadata.Add(int64(removed))
		gc.logger.Info("gc: removed orphaned blob metadata", "count", removed)
	}
}

// cleanupOrphanedBlobFiles removes blob files on disk that have no corresponding DB record.
func (gc *GarbageCollector) cleanupOrphanedBlobFiles(ctx context.Context) {
	// Snapshot disk digests before DB digests to avoid deleting a blob that was
	// written to disk after the disk snapshot but before the DB snapshot.
	diskDigests, err := gc.blobs.ListDigests()
	if err != nil {
		gc.logger.Error("gc: failed to list blob files on disk", "error", err)
		return
	}

	knownDigests, err := gc.db.AllBlobDigests(ctx, 1000)
	if err != nil {
		gc.logger.Error("gc: failed to list known blob digests", "error", err)
		return
	}

	removed := 0
	for _, digest := range diskDigests {
		if knownDigests[digest] {
			continue
		}
		if err := gc.blobs.Delete(digest); err != nil {
			gc.logger.Warn("gc: failed to delete orphaned blob file", "digest", digest, "error", err)
			continue
		}
		removed++
	}

	if removed > 0 {
		metrics.GCOrphanedFiles.Add(int64(removed))
		gc.logger.Info("gc: removed orphaned blob files from disk", "count", removed)
	}
}
