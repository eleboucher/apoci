package peering

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

const maxConcurrentReplications = 10

type ReplicatorRepository interface {
	FindRepoForBlob(ctx context.Context, digest string) (string, error)
	PutBlob(ctx context.Context, digest string, sizeBytes int64, mediaType *string, storedLocally bool) error
}

type BlobStreamFetcher interface {
	FetchBlobStream(ctx context.Context, peerEndpoint, repo, digest string) (*BlobStream, error)
}

type BlobReplicator struct {
	db       ReplicatorRepository
	blobs    blobstore.BlobStore
	fetcher  BlobStreamFetcher
	notifier Notifier
	logger   *slog.Logger
	sem      chan struct{}
	wg       sync.WaitGroup
}

func NewBlobReplicator(db ReplicatorRepository, blobs blobstore.BlobStore, fetcher BlobStreamFetcher, notifier Notifier, logger *slog.Logger) *BlobReplicator {
	return &BlobReplicator{
		db:       db,
		blobs:    blobs,
		fetcher:  fetcher,
		notifier: notifier,
		logger:   logger,
		sem:      make(chan struct{}, maxConcurrentReplications),
	}
}

// ReplicateBlob fetches a blob from a peer and stores it locally in the background.
// The repo is derived from manifest layer references in the database; if none exist
// yet, replication is skipped and the blob will be fetched on-demand via pull-through.
func (r *BlobReplicator) ReplicateBlob(ctx context.Context, peerEndpoint, digest string, size int64) {
	if exists, err := r.blobs.Exists(ctx, digest); err != nil {
		r.logger.Warn("failed to check blob existence before replication", "digest", digest, "error", err)
		return
	} else if exists {
		return
	}

	metrics.BlobReplicationsStarted.Inc()
	metrics.BlobReplicationsInFlight.Inc()
	r.wg.Go(func() {
		defer metrics.BlobReplicationsInFlight.Dec()

		r.sem <- struct{}{}
		defer func() { <-r.sem }()

		// Use a fresh context with a timeout since the HTTP request context will be cancelled.
		bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
		defer cancel()
		r.replicateBlob(bgCtx, peerEndpoint, digest)
	})
}

// Wait blocks until all in-flight replications complete.
func (r *BlobReplicator) Wait() {
	r.wg.Wait()
}

func (r *BlobReplicator) replicateBlob(ctx context.Context, peerEndpoint, digest string) {
	// Find a repo that references this blob so we can construct the OCI pull URL.
	// Blobs are served under /v2/{repo}/blobs/{digest}, so we need a valid repo name.
	repo, _ := r.db.FindRepoForBlob(ctx, digest)
	if repo == "" {
		// No repo references this blob yet (blob announce arrived before the manifest).
		// Skip eager replication; the blob will be fetched on-demand via pull-through
		// when a client actually requests it.
		r.logger.Debug("skipping eager replication: no repo references blob yet", "digest", digest)
		return
	}

	fetchStart := time.Now()
	stream, err := r.fetcher.FetchBlobStream(ctx, peerEndpoint, repo, digest)
	if err != nil {
		metrics.BlobReplicationsFailed.Add(1)
		r.logger.Warn("eager replication failed",
			"digest", digest,
			"peer", peerEndpoint,
			"error", err,
		)
		r.notifier.Send(notify.EventReplicationFailure, fmt.Sprintf("Failed to replicate blob %s from %s: %v", digest, peerEndpoint, err))
		return
	}
	defer func() {
		if err := stream.Body.Close(); err != nil {
			r.logger.Warn("failed to close blob stream", "digest", digest, "error", err)
		}
	}()

	storedDigest, size, err := r.blobs.Put(ctx, stream.Body, digest)
	if err != nil {
		metrics.BlobReplicationsFailed.Add(1)
		r.logger.Warn("failed to store replicated blob",
			"digest", digest,
			"error", err,
		)
		r.notifier.Send(notify.EventReplicationFailure, fmt.Sprintf("Failed to store replicated blob %s: %v", digest, err))
		return
	}

	mt := "application/octet-stream"
	if err := r.db.PutBlob(ctx, storedDigest, size, &mt, true); err != nil {
		r.logger.Warn("failed to update blob metadata after replication",
			"digest", digest,
			"error", err,
		)
		return
	}

	metrics.BlobFetchDuration.Observe(time.Since(fetchStart).Seconds())
	metrics.BlobReplicationsSucceeded.Add(1)
	r.logger.Info("eagerly replicated blob",
		"digest", digest,
		"peer", peerEndpoint,
		"size", size,
	)
}
