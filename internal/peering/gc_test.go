package peering

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func testGCDeps(t *testing.T) (*database.DB, *blobstore.Store) {
	t.Helper()

	dbDir := t.TempDir()
	db, err := database.OpenSQLite(dbDir, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobDir := t.TempDir()
	blobs, err := blobstore.New(blobDir, nopLog())
	require.NoError(t, err)

	return db, blobs
}

func insertTestPeer(t *testing.T, ctx context.Context, db *database.DB, actorURL, endpoint string) {
	t.Helper()
	name := "test-peer"
	require.NoError(t, db.UpsertPeer(ctx, &database.Peer{
		ActorURL:          actorURL,
		Name:              &name,
		Endpoint:          endpoint,
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}))
}

func TestGCCleansStalePeerBlobs(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	insertTestPeer(t, ctx, db, "https://stale.example.com/ap/actor", "https://stale.example.com")

	digest := "sha256:aabbccddee000000000000000000000000000000000000000000000000000000"
	require.NoError(t, db.PutPeerBlob(ctx, "https://stale.example.com/ap/actor", digest, "https://stale.example.com"))

	// CleanupStalePeerBlobs computes cutoff = now - olderThan. With a negative duration,
	// cutoff lands slightly in the future, guaranteeing the row is older than the cutoff.
	time.Sleep(10 * time.Millisecond)
	n, err := db.CleanupStalePeerBlobs(ctx, -1*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "expected 1 stale peer blob removed")

	pbs, err := db.FindPeersWithBlob(ctx, digest)
	require.NoError(t, err)
	require.Len(t, pbs, 0)
}

func TestGCCleansOrphanedBlobMetadata(t *testing.T) {
	db, _ := testGCDeps(t)
	ctx := context.Background()

	// Insert a blob that is NOT stored locally and has no peer refs or manifest layers.
	orphanDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	require.NoError(t, db.PutBlob(ctx, orphanDigest, 100, nil, false))

	digests, err := db.OrphanedBlobs(ctx, 100)
	require.NoError(t, err)
	require.Len(t, digests, 1)
	require.Equal(t, orphanDigest, digests[0])

	require.NoError(t, db.DeleteBlob(ctx, orphanDigest))

	blob, err := db.GetBlob(ctx, orphanDigest)
	require.NoError(t, err)
	require.Nil(t, blob, "expected orphaned blob metadata to be removed")
}

func TestGCCleansOrphanedBlobFiles(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	// Write a blob to disk but do NOT register it in the database.
	digest, _, err := blobs.Put(strings.NewReader("orphaned blob data on disk"), "")
	require.NoError(t, err)

	require.True(t, blobs.Exists(digest), "expected blob file to exist before cleanup")

	// Check that AllBlobDigests returns nothing (blob not in DB).
	knownDigests, err := db.AllBlobDigests(ctx, 1000)
	require.NoError(t, err)
	require.False(t, knownDigests[digest], "expected digest to NOT be in DB")

	// Manually delete the orphaned blob file (simulating what GC should do).
	require.NoError(t, blobs.Delete(digest))

	require.False(t, blobs.Exists(digest), "expected orphaned blob file to be removed")
}

func TestGCPreservesValidData(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	// 1. Recent peer blob (should be preserved by a 30-day cleanup).
	insertTestPeer(t, ctx, db, "https://recent.example.com/ap/actor", "https://recent.example.com")

	recentDigest := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	require.NoError(t, db.PutPeerBlob(ctx, "https://recent.example.com/ap/actor", recentDigest, "https://recent.example.com"))

	// Cleanup with 30-day threshold should NOT remove the recent peer blob.
	n, err := db.CleanupStalePeerBlobs(ctx, stalePeerBlobAge)
	require.NoError(t, err)
	require.Equal(t, int64(0), n)

	pbs, err := db.FindPeersWithBlob(ctx, recentDigest)
	require.NoError(t, err)
	require.Len(t, pbs, 1)

	// 2. Locally stored blob should NOT be returned as orphaned.
	localDigest := "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	require.NoError(t, db.PutBlob(ctx, localDigest, 200, nil, true))

	orphans, err := db.OrphanedBlobs(ctx, 100)
	require.NoError(t, err)
	for _, d := range orphans {
		require.NotEqual(t, localDigest, d, "expected local blob to NOT be orphaned")
	}

	// 3. Blob file on disk with a matching DB record should be preserved.
	diskDigest, _, err := blobs.Put(strings.NewReader("valid blob on disk"), "")
	require.NoError(t, err)
	require.NoError(t, db.PutBlob(ctx, diskDigest, 18, nil, true))

	knownDigests, err := db.AllBlobDigests(ctx, 1000)
	require.NoError(t, err)
	require.True(t, knownDigests[diskDigest], "expected disk blob digest to be in known digests")

	require.True(t, blobs.Exists(diskDigest), "expected valid blob file to remain on disk")
}

func TestGCStartStop(t *testing.T) {
	db, blobs := testGCDeps(t)
	ctx := context.Background()

	gc := NewGarbageCollector(db, blobs, nopLog())
	gc.Start(ctx)

	// Stop should return promptly without panic.
	gc.Stop()
}
