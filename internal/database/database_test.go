package database

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const (
	testMediaType         = "application/octet-stream"
	testPeerName          = "peer"
	testManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	testLayerMediaType    = "application/vnd.oci.image.layer.v1.tar+gzip"
	replPolicyLazy        = "lazy"
	testBaseDigest        = "sha256:base"
	testLayerDigest       = "sha256:layer1"
	testLayerDigest2      = "sha256:layer2"
	testDigestABC         = "sha256:abc"
	testVersion100        = "1.0.0"
	testTagLatest         = "latest"
	testPeerActorURL      = "https://peer.example.com/ap/actor"
)

func testDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrateV9DropsImmutableColumn(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Simulate a pre-v9 database that still carries the immutable column.
	_, err := db.bun.ExecContext(ctx, "ALTER TABLE package_tags ADD COLUMN immutable BOOLEAN NOT NULL DEFAULT FALSE")
	require.NoError(t, err)
	exists, err := db.columnExists(ctx, "package_tags", "immutable")
	require.NoError(t, err)
	require.True(t, exists)

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/keep", testAliceActor)
	require.NoError(t, err)
	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "v1.0", "sha256:keep"))

	require.NoError(t, db.migrateV9(ctx))

	exists, err = db.columnExists(ctx, "package_tags", "immutable")
	require.NoError(t, err)
	require.False(t, exists, "immutable column should be dropped")

	// The tag row survives the column drop.
	got, err := db.GetPackageTag(ctx, pkg.ID, "v1.0")
	require.NoError(t, err)
	require.Equal(t, "sha256:keep", got.Version)

	// Idempotent: running again on a table without the column is a no-op.
	require.NoError(t, db.migrateV9(ctx))
}

func TestMigrateV6FromV5Data(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Re-bootstrap the pre-v6 schema on top of an already-migrated DB and
	// re-run v6. Idempotent because v6 uses IF NOT EXISTS / WHERE NOT EXISTS / IF EXISTS.
	require.NoError(t, db.migrateV1(ctx))
	require.NoError(t, db.migrateV3(ctx))
	require.NoError(t, db.migrateV4(ctx))

	_, err := db.bun.ExecContext(ctx,
		`INSERT INTO repositories (name, owner_id, private) VALUES ('foo.com/legacy', 'https://alice.example.com/ap/actor', false)`)
	require.NoError(t, err)
	var repoID int64
	require.NoError(t, db.bun.NewRaw("SELECT id FROM repositories WHERE name = ?", "foo.com/legacy").Scan(ctx, &repoID))

	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO manifests (repository_id, digest, media_type, size_bytes, content)
		 VALUES (?, 'sha256:legacy', 'application/vnd.oci.image.manifest.v1+json', 200, ?)`,
		repoID, []byte(`{"schemaVersion":2}`))
	require.NoError(t, err)
	var manifestID int64
	require.NoError(t, db.bun.NewRaw("SELECT id FROM manifests WHERE digest = ?", "sha256:legacy").Scan(ctx, &manifestID))

	require.NoError(t, db.PutBlob(ctx, "sha256:legacylayer", 4096, nil, true))
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO manifest_layers (manifest_id, blob_digest) VALUES (?, ?)`,
		manifestID, "sha256:legacylayer")
	require.NoError(t, err)

	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO tags (repository_id, name, manifest_digest, immutable) VALUES (?, ?, ?, ?)`,
		repoID, "v1.0", "sha256:legacy", true)
	require.NoError(t, err)

	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO deleted_manifests (digest, repo_name, source_actor) VALUES (?, ?, ?)`,
		"sha256:tombstoned", "foo.com/legacy", "https://alice.example.com/ap/actor")
	require.NoError(t, err)

	require.NoError(t, db.migrateV6(ctx))

	for _, table := range []string{"repositories", "manifests", "tags", "manifest_layers", "deleted_manifests", "repository_owners"} {
		exists, err := db.tableExists(ctx, table)
		require.NoError(t, err)
		require.False(t, exists, "table %q should have been dropped", table)
	}

	got, err := db.GetRepository(ctx, "foo.com/legacy")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, repoID, got.ID)

	manifest, err := db.GetManifestByDigest(ctx, repoID, "sha256:legacy")
	require.NoError(t, err)
	require.NotNil(t, manifest)
	require.Equal(t, manifestID, manifest.ID)
	require.Equal(t, testManifestMediaType, manifest.MediaType)
	require.Equal(t, []byte(`{"schemaVersion":2}`), manifest.Content)

	files, err := db.ListPackageFiles(ctx, manifestID)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "sha256:legacylayer", files[0].BlobDigest)
	require.Equal(t, int64(4096), files[0].SizeBytes)

	tag, err := db.GetTag(ctx, repoID, "v1.0")
	require.NoError(t, err)
	require.NotNil(t, tag)
	require.Equal(t, "sha256:legacy", tag.ManifestDigest)

	deleted, err := db.IsManifestDeleted(ctx, "sha256:tombstoned")
	require.NoError(t, err)
	require.True(t, deleted)
}

func TestMigrateV6PartialReRun(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.migrateV1(ctx))
	require.NoError(t, db.migrateV3(ctx))
	require.NoError(t, db.migrateV4(ctx))

	_, err := db.bun.ExecContext(ctx,
		`INSERT INTO repositories (id, name, owner_id, private) VALUES (42, 'foo.com/already', 'https://alice.example.com/ap/actor', false)`)
	require.NoError(t, err)
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO manifests (id, repository_id, digest, media_type, size_bytes, content)
		 VALUES (7, 42, 'sha256:already', 'application/vnd.oci.image.manifest.v1+json', 1, ?)`,
		[]byte(`{}`))
	require.NoError(t, err)
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO tags (id, repository_id, name, manifest_digest, immutable) VALUES (3, 42, 'latest', 'sha256:already', false)`)
	require.NoError(t, err)

	// Pre-seed new tables with a different id but matching natural key.
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO packages (id, type, name, owner_id, private) VALUES (999, 'oci', 'foo.com/already', 'https://alice.example.com/ap/actor', false)`)
	require.NoError(t, err)
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO package_versions (id, package_id, version, metadata, media_type, size_bytes) VALUES (998, 999, 'sha256:already', ?, 'application/vnd.oci.image.manifest.v1+json', 1)`,
		[]byte(`{}`))
	require.NoError(t, err)
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO package_tags (id, package_id, name, version) VALUES (997, 999, 'latest', 'sha256:already')`)
	require.NoError(t, err)

	require.NoError(t, db.migrateV6(ctx))

	pkg, err := db.GetPackage(ctx, "oci", "foo.com/already")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	require.Equal(t, int64(999), pkg.ID)
}

func TestMigrateV6OrphanLayer(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.migrateV1(ctx))
	require.NoError(t, db.migrateV3(ctx))
	require.NoError(t, db.migrateV4(ctx))

	_, err := db.bun.ExecContext(ctx,
		`INSERT INTO repositories (id, name, owner_id, private) VALUES (1, 'foo.com/orphan', 'https://alice.example.com/ap/actor', false)`)
	require.NoError(t, err)
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO manifests (id, repository_id, digest, media_type, size_bytes, content) VALUES (1, 1, 'sha256:m', 'application/vnd.oci.image.manifest.v1+json', 1, ?)`,
		[]byte(`{}`))
	require.NoError(t, err)
	_, err = db.bun.ExecContext(ctx,
		`INSERT INTO manifest_layers (manifest_id, blob_digest) VALUES (1, 'sha256:vanished')`)
	require.NoError(t, err)

	require.NoError(t, db.migrateV6(ctx))

	files, err := db.ListPackageFiles(ctx, 1)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "sha256:vanished", files[0].BlobDigest)
	require.Equal(t, int64(0), files[0].SizeBytes)
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

func TestPutManifestWithLayersAtomic(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo, _ := db.GetOrCreateRepository(ctx, "test/atomic", testAliceActor)

	layer := "sha256:" + strings.Repeat("a", 64)
	require.NoError(t, db.PutBlob(ctx, layer, 100, nil, true))

	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:" + strings.Repeat("b", 64),
		MediaType:    testManifestMediaType,
		SizeBytes:    10,
		Content:      []byte("{}"),
	}
	refs := []BlobRef{{Digest: layer, Size: 100}}
	require.NoError(t, db.PutManifestWithLayers(ctx, m, refs))
	require.NotZero(t, m.ID, "manifest ID should be populated")

	got, err := db.GetManifestByDigest(ctx, repo.ID, m.Digest)
	require.NoError(t, err)
	require.NotNil(t, got)

	// The layer is referenced via package_files written in the same tx, so the
	// GC sweep must not consider it orphaned.
	orphans, err := db.OrphanedBlobs(ctx, 100, time.Time{})
	require.NoError(t, err)
	require.NotContains(t, orphans, layer, "linked layer must not be orphaned after atomic write")

	// A failing transaction (cancelled context) must leave no manifest row —
	// the manifest and its refs are all-or-nothing.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	m2 := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:" + strings.Repeat("c", 64),
		MediaType:    testManifestMediaType,
		SizeBytes:    10,
		Content:      []byte("{}"),
	}
	require.Error(t, db.PutManifestWithLayers(cancelled, m2, refs))
	leftover, err := db.GetManifestByDigest(ctx, repo.ID, m2.Digest)
	require.NoError(t, err)
	require.Nil(t, leftover, "manifest must be rolled back when the transaction fails")
}

func TestManifestCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/manifests", testAliceActor)

	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:abc123",
		MediaType:    testManifestMediaType,
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
		MediaType:    testManifestMediaType,
		SizeBytes:    100,
		Content:      []byte(`{}`),
	}
	require.NoError(t, db.PutManifest(ctx, m))

	// Put tag
	require.NoError(t, db.PutTag(ctx, repo.ID, testTagLatest, "sha256:manifest1"))

	// Get tag
	tag, err := db.GetTag(ctx, repo.ID, testTagLatest)
	require.NoError(t, err)
	require.NotNil(t, tag, "expected tag, got nil")
	require.Equal(t, "sha256:manifest1", tag.ManifestDigest)

	// Get manifest by tag
	got, err := db.GetManifestByTag(ctx, repo.ID, testTagLatest)
	require.NoError(t, err)
	require.NotNil(t, got, "expected manifest by tag, got nil")
	require.Equal(t, "sha256:manifest1", got.Digest)

	// Update tag
	require.NoError(t, db.PutTag(ctx, repo.ID, testTagLatest, "sha256:manifest2"))
	tag2, _ := db.GetTag(ctx, repo.ID, testTagLatest)
	require.Equal(t, "sha256:manifest2", tag2.ManifestDigest)

	// List tags
	tags, err := db.ListTagsAfter(ctx, repo.ID, "", 100)
	require.NoError(t, err)
	require.Equal(t, []string{testTagLatest}, tags)

	// Delete tag
	require.NoError(t, db.DeleteTag(ctx, repo.ID, testTagLatest))
	tag3, _ := db.GetTag(ctx, repo.ID, testTagLatest)
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

	// List helpers used by the recovery path see the session regardless of expiry.
	byRepo, err := db.UploadSessionUUIDsByRepo(ctx, repo.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"uuid-123"}, byRepo)

	all, err := db.AllUploadSessionUUIDs(ctx)
	require.NoError(t, err)
	require.Contains(t, all, "uuid-123")

	listed, err := db.ListUploadSessions(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, "uuid-123", listed[0].UUID)

	// Delete
	require.NoError(t, db.DeleteUploadSession(ctx, "uuid-123"))
	got3, _ := db.GetUploadSession(ctx, "uuid-123")
	require.Nil(t, got3, "expected nil after delete")
}

func TestActorCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	now := time.Now()
	name := "bob-node"
	actor := &Actor{
		ActorURL:          "https://bob.example.com/ap/actor",
		Name:              &name,
		Endpoint:          "https://registry.bob.example.com",
		ReplicationPolicy: replPolicyLazy,
		LastSeenAt:        &now,
		IsHealthy:         true,
	}

	require.NoError(t, db.UpsertActor(ctx, actor))

	got, err := db.GetActor(ctx, "https://bob.example.com/ap/actor")
	require.NoError(t, err)
	require.NotNil(t, got, "expected actor, got nil")
	require.Equal(t, "https://registry.bob.example.com", got.Endpoint)

	// List actors
	actors, err := db.ListAllPeers(ctx)
	require.NoError(t, err)
	require.Len(t, actors, 1)
	require.True(t, actors[0].IsHealthy)

	// Set unhealthy
	require.NoError(t, db.SetPeerHealth(ctx, "https://bob.example.com/ap/actor", false))
	actors2, _ := db.ListAllPeers(ctx)
	require.Len(t, actors2, 1)
	require.False(t, actors2[0].IsHealthy)
}

func TestPeerBlobLookup(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	now := time.Now()
	name := "alice"
	require.NoError(t, db.UpsertActor(ctx, &Actor{
		ActorURL:          testAliceActor,
		Name:              &name,
		Endpoint:          "https://alice.example.com",
		ReplicationPolicy: replPolicyLazy,
		LastSeenAt:        &now,
		IsHealthy:         true,
	}))

	require.NoError(t, db.PutPeerBlob(ctx, testAliceActor, testLayerDigest, "https://alice.example.com"))

	pbs, err := db.FindPeersWithBlob(ctx, testLayerDigest)
	require.NoError(t, err)
	require.Len(t, pbs, 1)
	require.Equal(t, testAliceActor, pbs[0].PeerActor)

	// Unhealthy peer should be excluded
	require.NoError(t, db.SetPeerHealth(ctx, testAliceActor, false))
	pbs2, _ := db.FindPeersWithBlob(ctx, testLayerDigest)
	require.Len(t, pbs2, 0)
}

func TestManifestLayers(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, _ := db.GetOrCreateRepository(ctx, "test/layers", testAliceActor)
	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:manifest-with-layers",
		MediaType:    testManifestMediaType,
		SizeBytes:    200,
		Content:      []byte(`{}`),
	}
	require.NoError(t, db.PutManifest(ctx, m))
	got, _ := db.GetManifestByDigest(ctx, repo.ID, "sha256:manifest-with-layers")

	require.NoError(t, db.PutBlob(ctx, testLayerDigest, 100, nil, true))
	require.NoError(t, db.PutBlob(ctx, testLayerDigest2, 200, nil, true))
	require.NoError(t, db.PutManifestLayers(ctx, got.ID, []BlobRef{
		{Digest: testLayerDigest, Size: 100},
		{Digest: testLayerDigest2, Size: 200},
	}))

	files, err := db.ListPackageFiles(ctx, got.ID)
	require.NoError(t, err)
	require.Len(t, files, 2)
	digests := []string{files[0].BlobDigest, files[1].BlobDigest}
	require.Contains(t, digests, testLayerDigest)
	require.Contains(t, digests, testLayerDigest2)
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
	require.NotNil(t, f.PublicKeyPEM)
	require.Equal(t, "new-pubkey", *f.PublicKeyPEM)
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
		MediaType:    testManifestMediaType,
		SizeBytes:    50,
		Content:      []byte(`{}`),
	}
	require.NoError(t, db.PutManifest(ctx, m))
	got, _ := db.GetManifestByDigest(ctx, repo.ID, "sha256:manifest-scoped")
	require.NoError(t, db.PutManifestLayers(ctx, got.ID, []BlobRef{{Digest: "sha256:scoped1", Size: 1}}))

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
	require.NoError(t, db.UpsertActor(ctx, &Actor{
		ActorURL:          testPeerActorURL,
		Name:              &name,
		Endpoint:          "https://peer.example.com",
		ReplicationPolicy: replPolicyLazy,
		LastSeenAt:        &now,
		IsHealthy:         true,
	}))
	require.NoError(t, db.PutPeerBlob(ctx, testPeerActorURL, "sha256:staleclean", "https://peer.example.com"))

	// Cleanup with a negative age — the just-inserted row counts as stale.
	n, err := db.CleanupStalePeerBlobs(ctx, -1*time.Second)
	require.NoError(t, err)
	require.Equal(t, int64(1), n)
}

func TestCountPeers(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	count, err := db.CountPeers(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	now := time.Now()
	require.NoError(t, db.UpsertActor(ctx, &Actor{
		ActorURL:          "https://peer1.example.com/ap/actor",
		Endpoint:          "https://peer1.example.com",
		IsHealthy:         true,
		ReplicationPolicy: replPolicyLazy,
		LastSeenAt:        &now,
	}))
	require.NoError(t, db.UpsertActor(ctx, &Actor{
		ActorURL:          "https://peer2.example.com/ap/actor",
		Endpoint:          "https://peer2.example.com",
		IsHealthy:         true,
		ReplicationPolicy: replPolicyLazy,
		LastSeenAt:        &now,
	}))

	count, err = db.CountPeers(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestCountFollows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	count, err := db.CountFollows(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, count)

	require.NoError(t, db.AddFollow(ctx, "https://follower1.example.com/ap/actor", "key1", "https://follower1.example.com", nil))
	require.NoError(t, db.AddFollow(ctx, "https://follower2.example.com/ap/actor", "key2", "https://follower2.example.com", nil))

	count, err = db.CountFollows(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestListFollowsPage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Add 3 followers
	require.NoError(t, db.AddFollow(ctx, "https://a.example.com/ap/actor", "key-a", "https://a.example.com", nil))
	require.NoError(t, db.AddFollow(ctx, "https://b.example.com/ap/actor", "key-b", "https://b.example.com", nil))
	require.NoError(t, db.AddFollow(ctx, "https://c.example.com/ap/actor", "key-c", "https://c.example.com", nil))

	// Page 1
	page1, err := db.ListFollowsPage(ctx, 0, 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	// Page 2
	page2, err := db.ListFollowsPage(ctx, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
}

func TestListFollowsBatch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.AddFollow(ctx, "https://batch1.example.com/ap/actor", "key1", "https://batch1.example.com", nil))
	require.NoError(t, db.AddFollow(ctx, "https://batch2.example.com/ap/actor", "key2", "https://batch2.example.com", nil))
	require.NoError(t, db.AddFollow(ctx, "https://batch3.example.com/ap/actor", "key3", "https://batch3.example.com", nil))

	// First batch from ID 0
	batch1, err := db.ListFollowsBatch(ctx, 0, 2)
	require.NoError(t, err)
	require.Len(t, batch1, 2)

	// Next batch using cursor
	batch2, err := db.ListFollowsBatch(ctx, batch1[1].ID, 2)
	require.NoError(t, err)
	require.Len(t, batch2, 1)
}

func TestListFollowing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// No following yet
	following, err := db.ListFollowing(ctx)
	require.NoError(t, err)
	require.Len(t, following, 0)

	// Add outgoing follow (pending)
	require.NoError(t, db.AddOutgoingFollow(ctx, "https://target1.example.com/ap/actor"))

	// Still empty - pending doesn't count
	following, err = db.ListFollowing(ctx)
	require.NoError(t, err)
	require.Len(t, following, 0)

	// Accept it
	require.NoError(t, db.AcceptOutgoingFollow(ctx, "https://target1.example.com/ap/actor"))

	// Now we have one
	following, err = db.ListFollowing(ctx)
	require.NoError(t, err)
	require.Len(t, following, 1)
	require.Equal(t, "https://target1.example.com/ap/actor", following[0].ActorURL)
}

func TestCountOutgoingFollows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	count, err := db.CountOutgoingFollows(ctx, "pending")
	require.NoError(t, err)
	require.Equal(t, 0, count)

	require.NoError(t, db.AddOutgoingFollow(ctx, "https://out1.example.com/ap/actor"))
	require.NoError(t, db.AddOutgoingFollow(ctx, "https://out2.example.com/ap/actor"))

	count, err = db.CountOutgoingFollows(ctx, "pending")
	require.NoError(t, err)
	require.Equal(t, 2, count)

	// Accept one
	require.NoError(t, db.AcceptOutgoingFollow(ctx, "https://out1.example.com/ap/actor"))

	count, err = db.CountOutgoingFollows(ctx, "pending")
	require.NoError(t, err)
	require.Equal(t, 1, count)

	count, err = db.CountOutgoingFollows(ctx, "accepted")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestListOutgoingFollowsPage(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.AddOutgoingFollow(ctx, "https://page1.example.com/ap/actor"))
	require.NoError(t, db.AddOutgoingFollow(ctx, "https://page2.example.com/ap/actor"))
	require.NoError(t, db.AddOutgoingFollow(ctx, "https://page3.example.com/ap/actor"))

	page1, err := db.ListOutgoingFollowsPage(ctx, "pending", 2, 0)
	require.NoError(t, err)
	require.Len(t, page1, 2)

	page2, err := db.ListOutgoingFollowsPage(ctx, "pending", 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
}

func TestUpsertPeer(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := "test-peer"
	require.NoError(t, db.UpsertPeer(ctx, "https://peer.example.com/ap/actor", "https://peer.example.com", &name, "eager", true))

	actor, err := db.GetActor(ctx, "https://peer.example.com/ap/actor")
	require.NoError(t, err)
	require.NotNil(t, actor)
	require.Equal(t, "eager", actor.ReplicationPolicy)
	require.True(t, actor.IsHealthy)
	require.NotNil(t, actor.Name)
	require.Equal(t, "test-peer", *actor.Name)

	// Update
	newName := "updated-peer"
	require.NoError(t, db.UpsertPeer(ctx, "https://peer.example.com/ap/actor", "https://peer.example.com", &newName, "lazy", false))

	actor, err = db.GetActor(ctx, "https://peer.example.com/ap/actor")
	require.NoError(t, err)
	require.Equal(t, "lazy", actor.ReplicationPolicy)
	require.False(t, actor.IsHealthy)
	require.Equal(t, "updated-peer", *actor.Name)
}

func TestGetPeer(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Not found
	peer, err := db.GetPeer(ctx, "https://nonexistent.example.com/ap/actor")
	require.NoError(t, err)
	require.Nil(t, peer)

	// Create and get
	name := testPeerName
	require.NoError(t, db.UpsertPeer(ctx, "https://peer.example.com/ap/actor", "https://peer.example.com", &name, "lazy", true))

	peer, err = db.GetPeer(ctx, "https://peer.example.com/ap/actor")
	require.NoError(t, err)
	require.NotNil(t, peer)
	require.Equal(t, "https://peer.example.com/ap/actor", peer.ActorURL)
}

func TestSetPeerHealthByDomain(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := testPeerName
	// Test various endpoint formats
	require.NoError(t, db.UpsertPeer(ctx, "https://healthy.example.com/ap/actor", "https://healthy.example.com/", &name, "lazy", true))
	require.NoError(t, db.UpsertPeer(ctx, "https://healthy.example.com:8080/ap/actor2", "https://healthy.example.com:8080/", &name, "lazy", true))
	require.NoError(t, db.UpsertPeer(ctx, "https://healthy.example.com:9000/ap/actor3", "https://healthy.example.com:9000", &name, "lazy", true)) // no trailing slash
	require.NoError(t, db.UpsertPeer(ctx, "https://other.example.com/ap/actor", "https://other.example.com/", &name, "lazy", true))

	// Mark healthy.example.com domain as unhealthy
	require.NoError(t, db.SetPeerHealthByDomain(ctx, "healthy.example.com", false))

	// Check the affected peers
	actor1, _ := db.GetActor(ctx, "https://healthy.example.com/ap/actor")
	require.False(t, actor1.IsHealthy)

	actor2, _ := db.GetActor(ctx, "https://healthy.example.com:8080/ap/actor2")
	require.False(t, actor2.IsHealthy)

	actor3, _ := db.GetActor(ctx, "https://healthy.example.com:9000/ap/actor3")
	require.False(t, actor3.IsHealthy)

	// Other domain unaffected
	actor4, _ := db.GetActor(ctx, "https://other.example.com/ap/actor")
	require.True(t, actor4.IsHealthy)
}

func TestUnhealthyPeerDomains(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := testPeerName
	require.NoError(t, db.UpsertPeer(ctx, "https://unhealthy1.example.com/ap/actor", "https://unhealthy1.example.com", &name, "lazy", false))
	require.NoError(t, db.UpsertPeer(ctx, "https://unhealthy2.example.com/ap/actor", "https://unhealthy2.example.com", &name, "lazy", false))
	require.NoError(t, db.UpsertPeer(ctx, "https://healthy.example.com/ap/actor", "https://healthy.example.com", &name, "lazy", true))

	domains, err := db.UnhealthyPeerDomains(ctx)
	require.NoError(t, err)
	require.Len(t, domains, 2)
	require.Contains(t, domains, "unhealthy1.example.com")
	require.Contains(t, domains, "unhealthy2.example.com")
}

func TestListHealthyActors(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	name := testPeerName
	require.NoError(t, db.UpsertPeer(ctx, "https://healthy1.example.com/ap/actor", "https://healthy1.example.com", &name, "lazy", true))
	require.NoError(t, db.UpsertPeer(ctx, "https://healthy2.example.com/ap/actor", "https://healthy2.example.com", &name, "lazy", true))
	require.NoError(t, db.UpsertPeer(ctx, "https://unhealthy.example.com/ap/actor", "https://unhealthy.example.com", &name, "lazy", false))

	healthy, err := db.ListHealthyActors(ctx)
	require.NoError(t, err)
	require.Len(t, healthy, 2)

	for _, a := range healthy {
		require.True(t, a.IsHealthy)
	}
}
