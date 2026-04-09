package oci

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cuelabs.dev/go/oci/ociregistry"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
)

const testManifestMediaType = "application/vnd.oci.image.manifest.v1+json"

func testRegistry(t *testing.T) (*Registry, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	return reg, srv
}

func TestV2Endpoint(t *testing.T) {
	_, srv := testRegistry(t)

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestPushAndPullBlob(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	blobData := []byte("hello blob content")
	repo := "test.example.com/test/repo"

	// Push blob
	desc, err := reg.PushBlob(ctx, repo, descriptorFor(blobData), strings.NewReader(string(blobData)))
	require.NoError(t, err)
	require.Equal(t, int64(len(blobData)), desc.Size)

	// Push a manifest referencing the blob so it's linked to the repo.
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`, desc.Digest, desc.Size)
	_, err = reg.PushManifest(ctx, repo, "latest", []byte(manifest), testManifestMediaType)
	require.NoError(t, err)

	// Pull blob
	reader, err := reg.GetBlob(ctx, repo, desc.Digest)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, string(blobData), string(got))
}

func TestPushAndPullManifest(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc"},"layers":[]}`)
	mediaType := testManifestMediaType

	// Push manifest with tag
	desc, err := reg.PushManifest(ctx, "test.example.com/test/myapp", "v1.0", manifest, mediaType)
	require.NoError(t, err)
	require.Equal(t, int64(len(manifest)), desc.Size)

	// Pull by digest
	reader, err := reg.GetManifest(ctx, "test.example.com/test/myapp", desc.Digest)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	got, _ := io.ReadAll(reader)
	require.Equal(t, string(manifest), string(got))

	// Pull by tag
	reader2, err := reg.GetTag(ctx, "test.example.com/test/myapp", "v1.0")
	require.NoError(t, err)
	defer func() { _ = reader2.Close() }()

	got2, _ := io.ReadAll(reader2)
	require.Equal(t, string(manifest), string(got2))
}

func TestListRepositoriesAndTags(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	mediaType := testManifestMediaType

	_, err := reg.PushManifest(ctx, "test.example.com/alpha/app", "v1", manifest, mediaType)
	require.NoError(t, err)
	_, err = reg.PushManifest(ctx, "test.example.com/beta/app", "v2", manifest, mediaType)
	require.NoError(t, err)

	// List repos
	var repos []string
	for name, err := range reg.Repositories(ctx, "") {
		require.NoError(t, err)
		repos = append(repos, name)
	}
	require.Len(t, repos, 2)

	// List tags
	var tags []string
	for tag, err := range reg.Tags(ctx, "test.example.com/alpha/app", "") {
		require.NoError(t, err)
		tags = append(tags, tag)
	}
	require.Equal(t, []string{"v1"}, tags)
}

func TestOwnershipEnforcement(t *testing.T) {
	dir := t.TempDir()

	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	// Alice creates the repo
	alice, err := NewRegistry(db, blobs, "https://alice.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	_, err = alice.PushManifest(ctx, "alice.example.com/shared/repo", "v1", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	// Bob tries to push to Alice's repo — rejected because alice.example.com
	// is domain-scoped and doesn't match Bob's namespace.
	bob, err := NewRegistry(db, blobs, "https://bob.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	_, err = bob.PushManifest(ctx, "alice.example.com/shared/repo", "v2", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.Error(t, err, "push to foreign domain namespace must be rejected")
}

func TestDeleteManifestAndTag(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	desc, _ := reg.PushManifest(ctx, "test.example.com/test/delete", "v1", manifest, "application/vnd.oci.image.manifest.v1+json")

	// Delete tag
	require.NoError(t, reg.DeleteTag(ctx, "test.example.com/test/delete", "v1"))
	_, err := reg.GetTag(ctx, "test.example.com/test/delete", "v1")
	require.Error(t, err, "expected error after tag delete")

	// Manifest still exists by digest
	reader, err := reg.GetManifest(ctx, "test.example.com/test/delete", desc.Digest)
	require.NoError(t, err, "manifest should still exist by digest after tag delete")
	_ = reader.Close()

	// Delete manifest
	require.NoError(t, reg.DeleteManifest(ctx, "test.example.com/test/delete", desc.Digest))
	_, err = reg.GetManifest(ctx, "test.example.com/test/delete", desc.Digest)
	require.Error(t, err, "expected error after manifest delete")
}

func TestHTTPPushPullFlow(t *testing.T) {
	_, srv := testRegistry(t)

	// 1. Check /v2/
	resp, _ := http.Get(srv.URL + "/v2/")
	require.Equal(t, 200, resp.StatusCode)
	_ = resp.Body.Close()

	// 2. Start a chunked upload session
	req, _ := http.NewRequest("POST", srv.URL+"/v2/test.example.com/test/httpflow/blobs/uploads/", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	location := resp.Header.Get("Location")
	require.NotEmpty(t, location, "expected Location header")

	// 3. Push manifest
	manifest := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        map[string]any{"digest": "sha256:abc", "size": 0, "mediaType": "application/vnd.oci.image.config.v1+json"},
		"layers":        []any{},
	}
	manifestBytes, _ := json.Marshal(manifest)

	req, _ = http.NewRequest("PUT", srv.URL+"/v2/test.example.com/test/httpflow/manifests/latest", strings.NewReader(string(manifestBytes)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)

	// 4. Pull manifest by tag
	req, _ = http.NewRequest("GET", srv.URL+"/v2/test.example.com/test/httpflow/manifests/latest", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var gotManifest map[string]any
	require.NoError(t, json.Unmarshal(body, &gotManifest))
	require.Equal(t, float64(2), gotManifest["schemaVersion"])
}

func TestNonexistentRepo(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	_, err := reg.GetTag(ctx, "nonexistent/repo", "latest")
	require.Error(t, err, "expected error for nonexistent repo")

	_, err = reg.GetManifest(ctx, "nonexistent/repo", "sha256:abc")
	require.Error(t, err, "expected error for nonexistent repo")
}

func TestReferrersAPI(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	// Push an image manifest
	imageManifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc","size":0},"layers":[]}`)
	imageDesc, err := reg.PushManifest(ctx, "test.example.com/test/cosign", "v1", imageManifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	// Push a signature manifest referencing the image via subject
	sigManifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.dev.cosign.simplesigning.v1+json","config":{"digest":"sha256:def","size":0,"mediaType":"application/vnd.oci.empty.v1+json"},"layers":[],"subject":{"digest":"` + string(imageDesc.Digest) + `","mediaType":"application/vnd.oci.image.manifest.v1+json","size":` + fmt.Sprintf("%d", imageDesc.Size) + `}}`)
	sigDesc, err := reg.PushManifest(ctx, "test.example.com/test/cosign", "", sigManifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	// Query referrers for the image
	var referrers []ociregistry.Descriptor
	for desc, err := range reg.Referrers(ctx, "test.example.com/test/cosign", imageDesc.Digest, "") {
		require.NoError(t, err)
		referrers = append(referrers, desc)
	}

	require.Len(t, referrers, 1)
	require.Equal(t, sigDesc.Digest, referrers[0].Digest)
	require.Equal(t, "application/vnd.dev.cosign.simplesigning.v1+json", referrers[0].ArtifactType)
}

func TestReferrersFilterByArtifactType(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	imageManifest := []byte(`{"schemaVersion":2,"config":{"digest":"sha256:abc","size":0},"layers":[]}`)
	imageDesc, _ := reg.PushManifest(ctx, "test.example.com/test/filter", "v1", imageManifest, "application/vnd.oci.image.manifest.v1+json")

	// Push two referrers with different artifact types
	sig1 := []byte(`{"schemaVersion":2,"artifactType":"type-a","config":{"digest":"sha256:s1","size":0,"mediaType":"application/vnd.oci.empty.v1+json"},"layers":[],"subject":{"digest":"` + string(imageDesc.Digest) + `","mediaType":"x","size":1}}`)
	_, err := reg.PushManifest(ctx, "test.example.com/test/filter", "", sig1, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	sig2 := []byte(`{"schemaVersion":2,"artifactType":"type-b","config":{"digest":"sha256:s2","size":0,"mediaType":"application/vnd.oci.empty.v1+json"},"layers":[],"subject":{"digest":"` + string(imageDesc.Digest) + `","mediaType":"x","size":1}}`)
	_, err = reg.PushManifest(ctx, "test.example.com/test/filter", "", sig2, testManifestMediaType)
	require.NoError(t, err)

	// Filter by type-a
	var count int
	for _, err := range reg.Referrers(ctx, "test.example.com/test/filter", imageDesc.Digest, "type-a") {
		require.NoError(t, err)
		count++
	}
	require.Equal(t, 1, count)
}

func TestTagImmutability(t *testing.T) {
	dir := t.TempDir()
	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", `^v[0-9]`, config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)

	// First push to v1.0 succeeds
	_, err = reg.PushManifest(ctx, "test.example.com/test/immutable", "v1.0", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err, "first push to v1.0 should succeed")

	// Second push to v1.0 is rejected
	_, err = reg.PushManifest(ctx, "test.example.com/test/immutable", "v1.0", []byte(`{"schemaVersion":2,"new":true}`), "application/vnd.oci.image.manifest.v1+json")
	require.Error(t, err, "second push to v1.0 should be rejected (immutable)")

	// Push to 'latest' succeeds twice (not matching ^v[0-9])
	_, err = reg.PushManifest(ctx, "test.example.com/test/immutable", "latest", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err, "first push to latest should succeed")
	_, err = reg.PushManifest(ctx, "test.example.com/test/immutable", "latest", []byte(`{"schemaVersion":2,"new":true}`), "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err, "second push to latest should succeed")
}

func TestNamespaceNormalization(t *testing.T) {
	dir := t.TempDir()
	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	reg, err := NewRegistry(db, blobs, "https://alice.example.com", "alice", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	mediaType := testManifestMediaType

	// Push with explicit namespace prefix succeeds
	_, err = reg.PushManifest(ctx, "alice/myapp", "v1", manifest, mediaType)
	require.NoError(t, err, "push to alice/myapp should succeed")

	// Push without namespace prefix succeeds (auto-prefixed)
	_, err = reg.PushManifest(ctx, "myapp", "v1", manifest, mediaType)
	require.NoError(t, err, "push to myapp (no prefix) should succeed via auto-prefix")

	// Verify the auto-prefixed repo is stored with the canonical name
	repoObj, err := db.GetRepository(ctx, "alice/myapp")
	require.NoError(t, err)
	require.NotNil(t, repoObj, "alice/myapp should exist in DB")

	// Pull without prefix works (normalized to canonical name)
	reader, err := reg.GetTag(ctx, "myapp", "v1")
	require.NoError(t, err)
	_ = reader.Close()

	// Pull with explicit prefix also works
	reader2, err := reg.GetTag(ctx, "alice/myapp", "v1")
	require.NoError(t, err)
	_ = reader2.Close()

	// Push to a foreign domain-scoped name is rejected
	_, err = reg.PushManifest(ctx, "foreign.example.com/evil", "v1", manifest, mediaType)
	require.Error(t, err, "push to foreign domain must be rejected")
}

func TestBlobRangeRequest(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	blobData := []byte("0123456789abcdef")
	desc := pushBlobWithManifest(t, reg, "test.example.com/test/range", blobData)

	// Read a range: bytes 4..10 (offset0=4, offset1=10)
	reader, err := reg.GetBlobRange(ctx, "test.example.com/test/range", desc.Digest, 4, 10)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, "456789", string(got))

	// Verify descriptor size matches range
	rangeDesc := reader.Descriptor()
	require.Equal(t, int64(6), rangeDesc.Size)
}

func TestBlobRangeOutOfBounds(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	blobData := []byte("short")
	desc := pushBlobWithManifest(t, reg, "test.example.com/test/rangebounds", blobData)

	// offset0 >= totalSize should return ErrRangeInvalid
	_, err := reg.GetBlobRange(ctx, "test.example.com/test/rangebounds", desc.Digest, int64(len(blobData)), -1)
	require.Error(t, err, "expected error for out-of-bounds range")
}

func TestResolveBlobAndManifest(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	// Push a blob + manifest so the blob is linked to the repo.
	blobData := []byte("resolve me")
	blobDesc := pushBlobWithManifest(t, reg, "test.example.com/test/resolve", blobData)

	// Resolve the blob
	resolved, err := reg.ResolveBlob(ctx, "test.example.com/test/resolve", blobDesc.Digest)
	require.NoError(t, err)
	require.Equal(t, blobDesc.Digest, resolved.Digest)
	require.Equal(t, int64(len(blobData)), resolved.Size)

	// Resolve nonexistent blob
	_, err = reg.ResolveBlob(ctx, "test.example.com/test/resolve", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err, "expected error for nonexistent blob")

	// Push a manifest with a tag
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc"},"layers":[]}`)
	mediaType := testManifestMediaType
	mDesc, err := reg.PushManifest(ctx, "test.example.com/test/resolve", "v1", manifest, mediaType)
	require.NoError(t, err)

	// Resolve the manifest
	resolvedM, err := reg.ResolveManifest(ctx, "test.example.com/test/resolve", mDesc.Digest)
	require.NoError(t, err)
	require.Equal(t, mDesc.Digest, resolvedM.Digest)
	require.Equal(t, int64(len(manifest)), resolvedM.Size)
	require.Equal(t, mediaType, resolvedM.MediaType)

	// Resolve nonexistent manifest
	_, err = reg.ResolveManifest(ctx, "test.example.com/test/resolve", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err, "expected error for nonexistent manifest")
}

func TestBlobReadRequiresExistingRepo(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	repoA := "test.example.com/test/repoA"
	repoB := "test.example.com/test/repoB"

	// Push a blob+manifest to repo A.
	blobData := []byte("scoped blob data")
	desc := pushBlobWithManifest(t, reg, repoA, blobData)

	// GetBlob from the same repo works.
	reader, err := reg.GetBlob(ctx, repoA, desc.Digest)
	require.NoError(t, err)
	got, _ := io.ReadAll(reader)
	_ = reader.Close()
	require.Equal(t, string(blobData), string(got))

	// GetBlob from a non-existent repo is rejected.
	_, err = reg.GetBlob(ctx, "test.example.com/test/nonexistent", desc.Digest)
	require.Error(t, err, "blob read from non-existent repo must fail")

	// GetBlobRange from a non-existent repo is rejected.
	_, err = reg.GetBlobRange(ctx, "test.example.com/test/nonexistent", desc.Digest, 0, 5)
	require.Error(t, err, "blob range read from non-existent repo must fail")

	// ResolveBlob from a non-existent repo is rejected.
	_, err = reg.ResolveBlob(ctx, "test.example.com/test/nonexistent", desc.Digest)
	require.Error(t, err, "resolve blob from non-existent repo must fail")

	// ResolveBlob from the same repo works.
	resolved, err := reg.ResolveBlob(ctx, repoA, desc.Digest)
	require.NoError(t, err)
	require.Equal(t, desc.Digest, resolved.Digest)

	// Cross-repo isolation: push a different blob+manifest to repo B.
	blobB := []byte("repo B data")
	descB := pushBlobWithManifest(t, reg, repoB, blobB)

	// ResolveBlob across repos is rejected.
	_, err = reg.ResolveBlob(ctx, repoB, desc.Digest)
	require.Error(t, err, "resolve blob from repoA must not work from repoB")

	_, err = reg.ResolveBlob(ctx, repoA, descB.Digest)
	require.Error(t, err, "resolve blob from repoB must not work from repoA")

	// Blob from repo A is NOT readable from repo B.
	_, err = reg.GetBlob(ctx, repoB, desc.Digest)
	require.Error(t, err, "blob from repoA must not be readable from repoB")

	// Blob from repo B is NOT readable from repo A.
	_, err = reg.GetBlob(ctx, repoA, descB.Digest)
	require.Error(t, err, "blob from repoB must not be readable from repoA")

	// Each blob is readable from its own repo.
	reader, err = reg.GetBlob(ctx, repoB, descB.Digest)
	require.NoError(t, err)
	got, _ = io.ReadAll(reader)
	_ = reader.Close()
	require.Equal(t, string(blobB), string(got))
}

func TestBlobSizeLimitEnforced(t *testing.T) {
	dir := t.TempDir()
	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	// Create registry with a tiny 100-byte blob limit.
	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, 100, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	// Blob under limit succeeds.
	small := make([]byte, 50)
	_, err = reg.PushBlob(ctx, "test.example.com/test/sizelimit", descriptorFor(small), strings.NewReader(string(small)))
	require.NoError(t, err, "small blob should succeed")

	// Blob over limit is rejected.
	big := make([]byte, 200)
	_, err = reg.PushBlob(ctx, "test.example.com/test/sizelimit", descriptorFor(big), strings.NewReader(string(big)))
	require.Error(t, err, "oversized blob should be rejected")
}

func TestBlobSizeLimitBoundary(t *testing.T) {
	dir := t.TempDir()
	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	limit := int64(100)
	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, limit, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	// Exactly at limit succeeds.
	atLimit := make([]byte, limit)
	_, err = reg.PushBlob(ctx, "test.example.com/test/boundary", descriptorFor(atLimit), strings.NewReader(string(atLimit)))
	require.NoError(t, err, "blob at exact limit should succeed")

	// One byte over is rejected.
	overLimit := make([]byte, limit+1)
	_, err = reg.PushBlob(ctx, "test.example.com/test/boundary", descriptorFor(overLimit), strings.NewReader(string(overLimit)))
	require.Error(t, err, "blob 1 byte over limit should be rejected")
}

func TestManifestSizeLimitEnforced(t *testing.T) {
	dir := t.TempDir()
	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	// Create registry with a tiny 200-byte manifest limit.
	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", 200, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	small := []byte(`{"schemaVersion":2}`)
	_, err = reg.PushManifest(ctx, "test.example.com/test/manlimit", "v1", small, testManifestMediaType)
	require.NoError(t, err, "small manifest should succeed")

	// Create a manifest over the limit.
	big := append([]byte(`{"schemaVersion":2,"data":"`), make([]byte, 300)...)
	big = append(big, '"', '}')
	_, err = reg.PushManifest(ctx, "test.example.com/test/manlimit", "v2", big, testManifestMediaType)
	require.Error(t, err, "oversized manifest should be rejected")
}

func TestBlobExistenceCheckOnManifestPush(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	// Push a blob so we have a valid digest.
	blobData := []byte("real blob content for existence check")
	blobDesc, err := reg.PushBlob(ctx, "test.example.com/test/blobcheck", descriptorFor(blobData), strings.NewReader(string(blobData)))
	require.NoError(t, err)

	// Manifest referencing the real blob should succeed.
	manifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`, blobDesc.Digest, blobDesc.Size)
	_, err = reg.PushManifest(ctx, "test.example.com/test/blobcheck", "v1", []byte(manifest), testManifestMediaType)
	require.NoError(t, err, "manifest with existing blob should succeed")

	// Manifest referencing a non-existent well-formed digest should fail.
	fakeDigest := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	badManifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"%s","size":0,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`, fakeDigest)
	_, err = reg.PushManifest(ctx, "test.example.com/test/blobcheck", "v2", []byte(badManifest), testManifestMediaType)
	require.Error(t, err, "manifest referencing non-existent blob should fail")
	require.Contains(t, err.Error(), "blob unknown")
}

func TestDeleteManifestCascadesTags(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	desc, err := reg.PushManifest(ctx, "test.example.com/test/cascade", "latest", manifest, testManifestMediaType)
	require.NoError(t, err)

	// Tag should exist.
	_, err = reg.GetTag(ctx, "test.example.com/test/cascade", "latest")
	require.NoError(t, err)

	// Delete manifest by digest — should also remove the tag.
	require.NoError(t, reg.DeleteManifest(ctx, "test.example.com/test/cascade", desc.Digest))

	// Both manifest and tag should be gone.
	_, err = reg.GetManifest(ctx, "test.example.com/test/cascade", desc.Digest)
	require.Error(t, err, "manifest should be gone")

	_, err = reg.GetTag(ctx, "test.example.com/test/cascade", "latest")
	require.Error(t, err, "tag should be gone after manifest delete")
}

func TestDeleteManifestBlockedByImmutableTag(t *testing.T) {
	dir := t.TempDir()
	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", `^v[0-9]`, config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	desc, err := reg.PushManifest(ctx, "test.example.com/test/immudel", "v1.0", manifest, testManifestMediaType)
	require.NoError(t, err)

	// Deleting a manifest that has an immutable tag should fail.
	err = reg.DeleteManifest(ctx, "test.example.com/test/immudel", desc.Digest)
	require.Error(t, err, "should not delete manifest with immutable tags")
	require.Contains(t, err.Error(), "immutable")

	// Manifest should still be accessible.
	reader, err := reg.GetManifest(ctx, "test.example.com/test/immudel", desc.Digest)
	require.NoError(t, err)
	_ = reader.Close()
}

func TestUploadSessionDBWiring(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	// Start a chunked upload.
	writer, err := reg.PushBlobChunked(ctx, "test.example.com/test/upload", 0)
	require.NoError(t, err)

	uploadID := writer.ID()

	// Verify session was created in DB.
	session, err := reg.Repo().GetUploadSession(ctx, uploadID)
	require.NoError(t, err)
	require.NotNil(t, session, "upload session should be in DB")
	require.Equal(t, uploadID, session.UUID)

	// Write some data and commit.
	_, err = writer.Write([]byte("test data"))
	require.NoError(t, err)

	_, err = writer.Commit(ociregistry.Digest(""))
	// Commit without a proper digest may error, but the session should be cleaned up.
	// We just check the DB is cleaned up after a successful commit.
	if err == nil {
		session, _ = reg.Repo().GetUploadSession(ctx, uploadID)
		require.Nil(t, session, "upload session should be deleted after commit")
	}

	_ = writer.Close()
}

func descriptorFor(data []byte) ociregistry.Descriptor {
	return ociregistry.Descriptor{
		MediaType: "application/octet-stream",
		Size:      int64(len(data)),
	}
}

// pushBlobWithManifest pushes a blob and a manifest that references it as config,
// so the blob is linked to the repo via manifest_layers.
func pushBlobWithManifest(t *testing.T, reg *Registry, repo string, blobData []byte) ociregistry.Descriptor {
	t.Helper()
	ctx := context.Background()

	desc, err := reg.PushBlob(ctx, repo, descriptorFor(blobData), strings.NewReader(string(blobData)))
	require.NoError(t, err)

	manifest := fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`,
		testManifestMediaType, desc.Digest, desc.Size,
	)
	_, err = reg.PushManifest(ctx, repo, "", []byte(manifest), testManifestMediaType)
	require.NoError(t, err)

	return desc
}

func TestGetManifestReturns410ForTombstoned(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	repo := "test.example.com/test/tombstone"

	// Push a manifest.
	blobData := []byte("tombstone test blob")
	blobDesc, err := reg.PushBlob(ctx, repo, descriptorFor(blobData), strings.NewReader(string(blobData)))
	require.NoError(t, err)

	manifest := fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`,
		testManifestMediaType, blobDesc.Digest, blobDesc.Size,
	)
	manifestDesc, err := reg.PushManifest(ctx, repo, "", []byte(manifest), testManifestMediaType)
	require.NoError(t, err)

	// Delete the manifest (simulating what the AP inbox does after a Delete activity).
	require.NoError(t, reg.DeleteManifest(ctx, repo, manifestDesc.Digest))
	require.NoError(t, db.RecordDeletedManifest(ctx, string(manifestDesc.Digest), repo, "https://peer.example.com/ap/actor"))

	// After tombstoning, GET manifest should return 410 Gone instead of 404.
	resp, err := http.Get(srv.URL + "/v2/test/tombstone/manifests/" + string(manifestDesc.Digest))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusGone, resp.StatusCode)
}

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// mockUpstreamFetcher implements UpstreamFetcher for testing.
type mockUpstreamFetcher struct {
	registries map[string]bool
	blobs      map[string][]byte       // digest -> data
	manifests  map[string]mockManifest // "registry/repo/ref" -> manifest
}

type mockManifest struct {
	data      []byte
	mediaType string
}

func newMockUpstreamFetcher() *mockUpstreamFetcher {
	return &mockUpstreamFetcher{
		registries: make(map[string]bool),
		blobs:      make(map[string][]byte),
		manifests:  make(map[string]mockManifest),
	}
}

func (m *mockUpstreamFetcher) HasRegistry(name string) bool {
	return m.registries[name]
}

func (m *mockUpstreamFetcher) FetchBlobStream(_ context.Context, registry, repo, digest string) (*peering.BlobStream, error) {
	data, ok := m.blobs[digest]
	if !ok {
		return nil, fmt.Errorf("blob not found on upstream")
	}
	return &peering.BlobStream{
		Body: io.NopCloser(strings.NewReader(string(data))),
	}, nil
}

func (m *mockUpstreamFetcher) FetchManifest(_ context.Context, registry, repo, reference string) ([]byte, string, error) {
	key := fmt.Sprintf("%s/%s/%s", registry, repo, reference)
	man, ok := m.manifests[key]
	if !ok {
		return nil, "", fmt.Errorf("manifest not found on upstream: key=%s", key)
	}
	return man.data, man.mediaType, nil
}

func (m *mockUpstreamFetcher) IsRepoPrivate(registry, repo string) bool {
	return false
}

func TestUpstreamBlobPullThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	// Setup mock upstream
	upstream := newMockUpstreamFetcher()
	upstream.registries["docker.io"] = true
	blobData := []byte("upstream blob content")
	blobDigest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(blobData))
	upstream.blobs[blobDigest] = blobData
	reg.SetUpstreamFetcher(upstream)

	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	// Pull blob through upstream (docker.io/library/nginx is upstream-prefixed)
	resp, err := http.Get(srv.URL + "/v2/docker.io/library/nginx/blobs/" + blobDigest)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, blobData, body)

	// Verify blob is stored in blobstore
	f, size, err := blobs.Open(ctx, blobDigest)
	require.NoError(t, err)
	require.Equal(t, int64(len(blobData)), size)
	_ = f.Close()

	// Verify repo was created (domain-scoped repos don't get namespace prefix)
	repoObj, err := db.GetRepository(ctx, "docker.io/library/nginx")
	require.NoError(t, err)
	require.NotNil(t, repoObj, "repo should be created after upstream pull")
}

func TestUpstreamManifestPullThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	// Setup mock upstream
	upstream := newMockUpstreamFetcher()
	upstream.registries["ghcr.io"] = true

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc","size":0,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`)
	manifestDigest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(manifest))

	// Register manifest by digest
	upstream.manifests["ghcr.io/owner/repo/"+manifestDigest] = mockManifest{
		data:      manifest,
		mediaType: testManifestMediaType,
	}
	reg.SetUpstreamFetcher(upstream)

	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	// Pull manifest through upstream
	req, _ := http.NewRequest("GET", srv.URL+"/v2/ghcr.io/owner/repo/manifests/"+manifestDigest, nil)
	req.Header.Set("Accept", testManifestMediaType)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, manifest, body)

	// Verify manifest is cached (domain-scoped repos don't get namespace prefix)
	repoObj, err := db.GetRepository(ctx, "ghcr.io/owner/repo")
	require.NoError(t, err)
	require.NotNil(t, repoObj)

	cached, err := db.GetManifestByDigest(ctx, repoObj.ID, manifestDigest)
	require.NoError(t, err)
	require.NotNil(t, cached, "manifest should be cached after upstream pull")
}

func TestUpstreamTagPullThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	// Setup mock upstream
	upstream := newMockUpstreamFetcher()
	upstream.registries["quay.io"] = true

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:def","size":0,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`)
	manifestDigest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(manifest))

	// Register manifest by tag
	upstream.manifests["quay.io/org/image/latest"] = mockManifest{
		data:      manifest,
		mediaType: testManifestMediaType,
	}
	reg.SetUpstreamFetcher(upstream)

	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	// Pull by tag through upstream
	req, _ := http.NewRequest("GET", srv.URL+"/v2/quay.io/org/image/manifests/latest", nil)
	req.Header.Set("Accept", testManifestMediaType)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	// Verify tag is cached (domain-scoped repos don't get namespace prefix)
	repoObj, err := db.GetRepository(ctx, "quay.io/org/image")
	require.NoError(t, err)
	require.NotNil(t, repoObj)

	tag, err := db.GetTag(ctx, repoObj.ID, "latest")
	require.NoError(t, err)
	require.NotNil(t, tag, "tag should be cached after upstream pull")
	require.Equal(t, manifestDigest, tag.ManifestDigest)
}

func TestNonUpstreamRepoDoesNotTriggerUpstream(t *testing.T) {
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	// Setup mock upstream that would fail if called
	upstream := newMockUpstreamFetcher()
	upstream.registries["docker.io"] = true
	// No blobs or manifests registered - will error if called
	reg.SetUpstreamFetcher(upstream)

	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	// Request a local repo (no dot in first segment) - should NOT trigger upstream
	// Use a valid digest format
	fakeDigest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256([]byte("nonexistent")))
	resp, err := http.Get(srv.URL + "/v2/myrepo/myimage/blobs/" + fakeDigest)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Should get 404, not an upstream error
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
