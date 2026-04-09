package database

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const testMediaType = "application/octet-stream"

func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestRepositoryCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Create
	repo, err := db.GetOrCreateRepository(ctx, "myapp/frontend", testAliceActor)
	require.NoError(t, err)
	require.Equal(t, "myapp/frontend", repo.Name)
	require.Equal(t, testAliceActor, repo.OwnerID)

	// Get existing
	repo2, err := db.GetOrCreateRepository(ctx, "myapp/frontend", testAliceActor)
	require.NoError(t, err)
	require.Equal(t, repo.ID, repo2.ID)

	// Reject different owner
	_, err = db.GetOrCreateRepository(ctx, "myapp/frontend", "https://bob.example.com/ap/actor")
	require.Error(t, err, "expected error for different owner")
}

func TestSetRepositoryPrivate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, err := db.GetOrCreateRepository(ctx, "ghcr.io/org/myapp", testAliceActor)
	require.NoError(t, err)
	require.False(t, repo.Private, "new repos default to public")

	// Mark private
	require.NoError(t, db.SetRepositoryPrivate(ctx, repo.ID, true))
	got, err := db.GetRepository(ctx, "ghcr.io/org/myapp")
	require.NoError(t, err)
	require.True(t, got.Private)

	// Toggle back to public
	require.NoError(t, db.SetRepositoryPrivate(ctx, repo.ID, false))
	got, err = db.GetRepository(ctx, "ghcr.io/org/myapp")
	require.NoError(t, err)
	require.False(t, got.Private)
}

func TestRepositoryOwnership(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/repo", testAliceActor)

	isOwner, err := db.IsRepositoryOwner(ctx, repo.ID, testAliceActor)
	require.NoError(t, err)
	require.True(t, isOwner, "expected alice to be owner")

	isOwner, err = db.IsRepositoryOwner(ctx, repo.ID, "https://bob.example.com/ap/actor")
	require.NoError(t, err)
	require.False(t, isOwner, "expected bob to NOT be owner")
}

func TestManifestCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/manifests", testAliceActor)

	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:abc123",
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    256,
		Content:      []byte(`{"schemaVersion":2}`),
	}

	require.NoError(t, db.PutManifest(ctx, m))

	got, err := db.GetManifestByDigest(ctx, repo.ID, "sha256:abc123")
	require.NoError(t, err)
	require.NotNil(t, got, "expected manifest, got nil")
	require.Equal(t, "sha256:abc123", got.Digest)
	require.Equal(t, m.MediaType, got.MediaType)

	// Not found
	notFound, err := db.GetManifestByDigest(ctx, repo.ID, "sha256:nonexistent")
	require.NoError(t, err)
	require.Nil(t, notFound, "expected nil for nonexistent manifest")

	// Delete
	require.NoError(t, db.DeleteManifest(ctx, repo.ID, "sha256:abc123"))
	deleted, err := db.GetManifestByDigest(ctx, repo.ID, "sha256:abc123")
	require.NoError(t, err)
	require.Nil(t, deleted, "expected manifest to not exist after delete")
}

func TestTagCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/tags", testAliceActor)

	// Put manifest first
	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:manifest1",
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    100,
		Content:      []byte(`{}`),
	}
	require.NoError(t, db.PutManifest(ctx, m))

	// Put tag
	require.NoError(t, db.PutTag(ctx, repo.ID, "latest", "sha256:manifest1"))

	// Get tag
	tag, err := db.GetTag(ctx, repo.ID, "latest")
	require.NoError(t, err)
	require.NotNil(t, tag, "expected tag, got nil")
	require.Equal(t, "sha256:manifest1", tag.ManifestDigest)

	// Get manifest by tag
	got, err := db.GetManifestByTag(ctx, repo.ID, "latest")
	require.NoError(t, err)
	require.NotNil(t, got, "expected manifest by tag, got nil")
	require.Equal(t, "sha256:manifest1", got.Digest)

	// Update tag
	require.NoError(t, db.PutTag(ctx, repo.ID, "latest", "sha256:manifest2"))
	tag2, _ := db.GetTag(ctx, repo.ID, "latest")
	require.Equal(t, "sha256:manifest2", tag2.ManifestDigest)

	// List tags
	tags, err := db.ListTagsAfter(ctx, repo.ID, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{"latest"}, tags)

	// Delete tag
	require.NoError(t, db.DeleteTag(ctx, repo.ID, "latest"))
	tag3, _ := db.GetTag(ctx, repo.ID, "latest")
	require.Nil(t, tag3, "expected nil after delete")
}

func TestBlobCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	mt := testMediaType
	require.NoError(t, db.PutBlob(ctx, "sha256:blob1", 1024, &mt, true))

	blob, err := db.GetBlob(ctx, "sha256:blob1")
	require.NoError(t, err)
	require.NotNil(t, blob, "expected blob, got nil")
	require.True(t, blob.StoredLocally, "expected stored_locally=true")
	require.Equal(t, int64(1024), blob.SizeBytes)

	// Remote blob
	require.NoError(t, db.PutBlob(ctx, "sha256:remote1", 2048, nil, false))
	remote, err := db.GetBlob(ctx, "sha256:remote1")
	require.NoError(t, err)
	require.False(t, remote.StoredLocally, "expected remote blob to not be local")

	// Delete
	require.NoError(t, db.DeleteBlob(ctx, "sha256:blob1"))
	blob2, _ := db.GetBlob(ctx, "sha256:blob1")
	require.Nil(t, blob2, "expected nil after delete")
}

func TestUploadSessionCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/uploads", testAliceActor)

	session, err := db.CreateUploadSession(ctx, "uuid-123", repo.ID, 1*time.Hour)
	require.NoError(t, err)
	require.Equal(t, "uuid-123", session.UUID)

	got, err := db.GetUploadSession(ctx, "uuid-123")
	require.NoError(t, err)
	require.NotNil(t, got, "expected session, got nil")

	// Delete
	require.NoError(t, db.DeleteUploadSession(ctx, "uuid-123"))
	got3, _ := db.GetUploadSession(ctx, "uuid-123")
	require.Nil(t, got3, "expected nil after delete")
}

func TestPeerCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	now := time.Now()
	name := "bob-node"
	peer := &Peer{
		ActorURL:          "https://bob.example.com/ap/actor",
		Name:              &name,
		Endpoint:          "https://registry.bob.example.com",
		ReplicationPolicy: "lazy",
		LastSeenAt:        &now,
		IsHealthy:         true,
	}

	require.NoError(t, db.UpsertPeer(ctx, peer))

	got, err := db.GetPeer(ctx, "https://bob.example.com/ap/actor")
	require.NoError(t, err)
	require.NotNil(t, got, "expected peer, got nil")
	require.Equal(t, "https://registry.bob.example.com", got.Endpoint)

	// List peers
	peers, err := db.ListAllPeers(ctx)
	require.NoError(t, err)
	require.Len(t, peers, 1)
	require.True(t, peers[0].IsHealthy)

	// Set unhealthy
	require.NoError(t, db.SetPeerHealth(ctx, "https://bob.example.com/ap/actor", false))
	peers2, _ := db.ListAllPeers(ctx)
	require.Len(t, peers2, 1)
	require.False(t, peers2[0].IsHealthy)
}

func TestPeerBlobLookup(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	now := time.Now()
	name := "alice"
	require.NoError(t, db.UpsertPeer(ctx, &Peer{
		ActorURL:          testAliceActor,
		Name:              &name,
		Endpoint:          "https://alice.example.com",
		ReplicationPolicy: "lazy",
		LastSeenAt:        &now,
		IsHealthy:         true,
	}))

	require.NoError(t, db.PutPeerBlob(ctx, testAliceActor, "sha256:layer1", "https://alice.example.com"))

	pbs, err := db.FindPeersWithBlob(ctx, "sha256:layer1")
	require.NoError(t, err)
	require.Len(t, pbs, 1)
	require.Equal(t, testAliceActor, pbs[0].PeerActor)

	// Unhealthy peer should be excluded
	require.NoError(t, db.SetPeerHealth(ctx, testAliceActor, false))
	pbs2, _ := db.FindPeersWithBlob(ctx, "sha256:layer1")
	require.Len(t, pbs2, 0)
}

func TestManifestLayers(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/layers", testAliceActor)
	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:manifest-with-layers",
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    200,
		Content:      []byte(`{}`),
	}
	require.NoError(t, db.PutManifest(ctx, m))

	got, _ := db.GetManifestByDigest(ctx, repo.ID, "sha256:manifest-with-layers")
	require.NoError(t, db.PutManifestLayers(ctx, got.ID, []string{"sha256:layer1", "sha256:layer2"}))

	// Verify via direct query
	rows, err := db.QueryContext(ctx,
		"SELECT blob_digest FROM manifest_layers WHERE manifest_id = ? ORDER BY blob_digest", got.ID)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var digests []string
	for rows.Next() {
		var d string
		require.NoError(t, rows.Scan(&d))
		digests = append(digests, d)
	}
	require.Len(t, digests, 2)
}

func TestFollowsCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// AddFollow + GetFollow
	require.NoError(t, db.AddFollow(ctx, testAliceActor, "pubkey-alice", "https://alice:5000", nil))
	f, err := db.GetFollow(ctx, testAliceActor)
	require.NoError(t, err)
	require.NotNil(t, f, "expected follow")
	require.Equal(t, testAliceActor, f.ActorURL)

	// ListFollows
	require.NoError(t, db.AddFollow(ctx, "https://bob.example.com/ap/actor", "pubkey-bob", "https://bob:5000", nil))
	follows, _ := db.ListFollows(ctx)
	require.Len(t, follows, 2)

	// RemoveFollow
	require.NoError(t, db.RemoveFollow(ctx, testAliceActor))
	follows, _ = db.ListFollows(ctx)
	require.Len(t, follows, 1)

	// RemoveFollow nonexistent
	err = db.RemoveFollow(ctx, "https://nobody.example.com/ap/actor")
	require.Error(t, err, "expected error")
}

func TestFollowRequests(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Add request
	require.NoError(t, db.AddFollowRequest(ctx, "https://carol.example.com/ap/actor", "pubkey-carol", "https://carol:5000", nil))
	fr, _ := db.GetFollowRequest(ctx, "https://carol.example.com/ap/actor")
	require.NotNil(t, fr, "expected follow request")

	// List requests
	requests, _ := db.ListFollowRequests(ctx)
	require.Len(t, requests, 1)

	// Accept -> promotes to follow, deletes request
	require.NoError(t, db.AcceptFollowRequest(ctx, "https://carol.example.com/ap/actor"))
	fr, _ = db.GetFollowRequest(ctx, "https://carol.example.com/ap/actor")
	require.Nil(t, fr, "expected request to be deleted")
	f, _ := db.GetFollow(ctx, "https://carol.example.com/ap/actor")
	require.NotNil(t, f, "expected follow after accept")

	// Reject
	require.NoError(t, db.AddFollowRequest(ctx, "https://dave.example.com/ap/actor", "pubkey-dave", "https://dave:5000", nil))
	require.NoError(t, db.RejectFollowRequest(ctx, "https://dave.example.com/ap/actor"))
	fr, _ = db.GetFollowRequest(ctx, "https://dave.example.com/ap/actor")
	require.Nil(t, fr, "expected request deleted after reject")

	// Reject nonexistent
	err := db.RejectFollowRequest(ctx, "https://nobody.example.com/ap/actor")
	require.Error(t, err, "expected error")
}

func TestRefreshFollow(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.AddFollow(ctx, testAliceActor, "old-pubkey", "https://old.example.com", nil))

	alias := "Alice"
	require.NoError(t, db.RefreshFollow(ctx, testAliceActor, "new-pubkey", "https://new.example.com", &alias))

	f, err := db.GetFollow(ctx, testAliceActor)
	require.NoError(t, err)
	require.Equal(t, "new-pubkey", f.PublicKeyPEM)
	require.Equal(t, "https://new.example.com", f.Endpoint)
	require.NotNil(t, f.Alias)
	require.Equal(t, "Alice", *f.Alias)
}

func TestRefreshFollowRequest(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, "https://carol.example.com/ap/actor", "old-pubkey", "https://old.example.com", nil))

	alias := "Carol"
	require.NoError(t, db.RefreshFollowRequest(ctx, "https://carol.example.com/ap/actor", "new-pubkey", "https://new.example.com", &alias))

	fr, err := db.GetFollowRequest(ctx, "https://carol.example.com/ap/actor")
	require.NoError(t, err)
	require.Equal(t, "new-pubkey", fr.PublicKeyPEM)
	require.Equal(t, "https://new.example.com", fr.Endpoint)
	require.NotNil(t, fr.Alias)
	require.Equal(t, "Carol", *fr.Alias)
}

func TestRefreshFollowNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	err := db.RefreshFollow(ctx, "https://nobody.example.com/ap/actor", "key", "https://nobody.example.com", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no follow found")
}

func TestRefreshFollowRequestNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	err := db.RefreshFollowRequest(ctx, "https://nobody.example.com/ap/actor", "key", "https://nobody.example.com", nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no follow request found")
}

func TestBlobPutDoesNotOverwriteSizeFromPeerAnnouncement(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	mt := testMediaType
	require.NoError(t, db.PutBlob(ctx, "sha256:sizetest", 1024, &mt, true))

	// Peer announces the same digest with a wrong size — should not overwrite.
	require.NoError(t, db.PutBlob(ctx, "sha256:sizetest", 9999, nil, false))

	blob, err := db.GetBlob(ctx, "sha256:sizetest")
	require.NoError(t, err)
	require.Equal(t, int64(1024), blob.SizeBytes, "size must not be overwritten by peer announcement")
}

func TestBlobExistsInRepo(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/scoped", testAliceActor)

	// Put a blob and a manifest that references it.
	mt := testMediaType
	require.NoError(t, db.PutBlob(ctx, "sha256:scoped1", 100, &mt, true))

	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:manifest-scoped",
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    50,
		Content:      []byte(`{}`),
	}
	require.NoError(t, db.PutManifest(ctx, m))
	got, _ := db.GetManifestByDigest(ctx, repo.ID, "sha256:manifest-scoped")
	require.NoError(t, db.PutManifestLayers(ctx, got.ID, []string{"sha256:scoped1"}))

	// Blob exists in the repo that references it.
	exists, err := db.BlobExistsInRepo(ctx, "test/scoped", "sha256:scoped1")
	require.NoError(t, err)
	require.True(t, exists)

	// Blob does not exist in a different repo.
	_, err = db.GetOrCreateRepository(ctx, "test/other", testAliceActor)
	require.NoError(t, err)
	exists, err = db.BlobExistsInRepo(ctx, "test/other", "sha256:scoped1")
	require.NoError(t, err)
	require.False(t, exists)

	// Non-existent repo returns false.
	exists, err = db.BlobExistsInRepo(ctx, "test/nonexistent", "sha256:scoped1")
	require.NoError(t, err)
	require.False(t, exists)
}

func TestEnqueueDeliveryIdempotent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.EnqueueDelivery(ctx, "activity-1", "https://inbox.example.com", []byte(`{}`)))
	require.NoError(t, db.EnqueueDelivery(ctx, "activity-1", "https://inbox.example.com", []byte(`{}`)))

	pending, err := db.PendingDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Len(t, pending, 1, "duplicate enqueue must be deduplicated")
}

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCleanupDeliveriesPassesTimeDirect(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.EnqueueDelivery(ctx, "act-cleanup-1", "https://inbox.example.com", []byte(`{}`)))

	// Mark as delivered so it is eligible for cleanup.
	pending, err := db.PendingDeliveries(ctx, 1)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, db.MarkDelivered(ctx, pending[0].ID))

	// Cleanup with a negative age — everything delivered counts as older-than.
	n, err := db.CleanupDeliveries(ctx, -1*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

func TestCleanupStalePeerBlobsPassesTimeDirect(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	now := time.Now()
	name := "peer-for-cleanup"
	require.NoError(t, db.UpsertPeer(ctx, &Peer{
		ActorURL:          "https://peer.example.com/ap/actor",
		Name:              &name,
		Endpoint:          "https://peer.example.com",
		ReplicationPolicy: "lazy",
		LastSeenAt:        &now,
		IsHealthy:         true,
	}))
	require.NoError(t, db.PutPeerBlob(ctx, "https://peer.example.com/ap/actor", "sha256:staleclean", "https://peer.example.com"))

	// Cleanup with a negative age — the just-inserted row counts as stale.
	n, err := db.CleanupStalePeerBlobs(ctx, -1*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}
