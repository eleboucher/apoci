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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cuelabs.dev/go/oci/ociregistry"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
)

const (
	testManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	testHostDomain        = "mortebrume.eu"
)

func testRegistry(t *testing.T) (*Registry, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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
	alice, err := NewRegistry(db, blobs, "https://alice.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	_, err = alice.PushManifest(ctx, "alice.example.com/shared/repo", "v1", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	// Bob tries to push to Alice's repo — rejected because alice.example.com
	// is domain-scoped and doesn't match Bob's namespace.
	bob, err := NewRegistry(db, blobs, "https://bob.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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

func TestTagOverwrite(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	repo := "test.example.com/test/overwrite"
	m1 := []byte(`{"schemaVersion":2}`)
	m2 := []byte(`{"schemaVersion":2,"new":true}`)

	d1, err := reg.PushManifest(ctx, repo, "v1.0", m1, testManifestMediaType)
	require.NoError(t, err)

	// Any tag can be re-pushed to a new digest.
	d2, err := reg.PushManifest(ctx, repo, "v1.0", m2, testManifestMediaType)
	require.NoError(t, err)
	require.NotEqual(t, d1.Digest, d2.Digest)

	rdr, err := reg.GetTag(ctx, repo, "v1.0")
	require.NoError(t, err)
	got, _ := io.ReadAll(rdr)
	_ = rdr.Close()
	require.Equal(t, string(m2), string(got))
}

func TestNamespaceNormalization(t *testing.T) {
	dir := t.TempDir()
	db, _ := database.OpenSQLite(dir, 0, 0, nopLog())
	defer func() { _ = db.Close() }()
	blobs, _ := blobstore.New(dir, nopLog())

	reg, err := NewRegistry(db, blobs, "https://alice.example.com", "alice", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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

func TestLooksLikeNamespaceTypo(t *testing.T) {
	cases := []struct {
		name      string
		repo      string
		namespace string
		want      bool
	}{
		{"empty namespace", "foo/bar", "", false},
		{"empty repo", "", testHostDomain, false},
		{"single-label namespace never matches", "alice/x", "alice", false},
		{"first segment is dns prefix label", "mortebrume/homelab", testHostDomain, true},
		{"first segment is bare label only", "mortebrume", testHostDomain, true},
		{"non-matching first segment", "myteam/x", testHostDomain, false},
		{"first segment already domain-scoped", "mortebrume.eu/x", testHostDomain, false},
		{"foreign domain-scoped path", "other.dev/x", testHostDomain, false},
		{"middle label does not match first label", "eu/x", testHostDomain, false},
		{"first label of multi-level namespace", "registry/x", "registry.example.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, looksLikeNamespaceTypo(tc.repo, tc.namespace))
		})
	}
}

func TestNamespaceTypoRejected(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://registry.mortebrume.eu", testHostDomain, config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	mediaType := testManifestMediaType
	blobData := []byte("typo-test-blob")

	_, err = reg.PushManifest(ctx, "mortebrume/homelab", "v1", manifest, mediaType)
	require.ErrorIs(t, err, ociregistry.ErrDenied)
	require.Contains(t, err.Error(), "mortebrume.eu/homelab", "error should suggest canonical path")

	_, err = reg.PushBlob(ctx, "mortebrume/homelab", descriptorFor(blobData), strings.NewReader(string(blobData)))
	require.ErrorIs(t, err, ociregistry.ErrDenied)

	_, err = reg.PushBlobChunked(ctx, "mortebrume/homelab", 0)
	require.ErrorIs(t, err, ociregistry.ErrDenied)

	_, err = reg.MountBlob(ctx, "anywhere", "mortebrume/homelab", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.ErrorIs(t, err, ociregistry.ErrDenied)

	require.ErrorIs(t, reg.DeleteManifest(ctx, "mortebrume/homelab", "sha256:0000000000000000000000000000000000000000000000000000000000000000"), ociregistry.ErrDenied)
	require.ErrorIs(t, reg.DeleteTag(ctx, "mortebrume/homelab", "v1"), ociregistry.ErrDenied)

	_, err = reg.PushManifest(ctx, "mortebrume.eu/homelab", "v1", manifest, mediaType)
	require.NoError(t, err)

	_, err = reg.PushManifest(ctx, "homelab", "v1", manifest, mediaType)
	require.NoError(t, err)

	_, err = reg.PushManifest(ctx, "myteam/homelab", "v1", manifest, mediaType)
	require.NoError(t, err)

	_, err = reg.PushManifest(ctx, "mortebrume", "v1", manifest, mediaType)
	require.ErrorIs(t, err, ociregistry.ErrDenied)
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
	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, 100, nopLog())
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
	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, limit, nopLog())
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
	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", 200, config.DefaultMaxBlobSize, nopLog())
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

func TestDeleteManifestWithTag(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	desc, err := reg.PushManifest(ctx, "test.example.com/test/tagdel", "v1.0", manifest, testManifestMediaType)
	require.NoError(t, err)

	// A manifest can be deleted even while a tag still points at it.
	require.NoError(t, reg.DeleteManifest(ctx, "test.example.com/test/tagdel", desc.Digest))

	_, err = reg.GetManifest(ctx, "test.example.com/test/tagdel", desc.Digest)
	require.Error(t, err, "manifest should be gone after delete")
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

func TestSweepStaleStagingFiles(t *testing.T) {
	reg, _ := testRegistry(t)
	uploadDir := t.TempDir()
	require.NoError(t, reg.SetUploadDir(uploadDir))
	ctx := context.Background()

	mkfile := func(name string, age time.Duration) string {
		p := filepath.Join(uploadDir, name)
		require.NoError(t, os.WriteFile(p, []byte("x"), 0o600))
		mt := time.Now().Add(-age)
		require.NoError(t, os.Chtimes(p, mt, mt))
		return p
	}

	stale := mkfile("upload-stale", 2*time.Hour)
	fresh := mkfile("upload-fresh", time.Minute)
	other := mkfile("not-an-upload", 2*time.Hour)

	w, err := reg.PushBlobChunked(ctx, "test.example.com/test/active", 0)
	require.NoError(t, err)
	active := w.(*diskBlobWriter).Path()
	require.NotEmpty(t, active)
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(active, old, old))

	reg.sweepStaleStagingFiles(time.Hour)

	require.NoFileExists(t, stale, "stale orphan should be swept")
	require.FileExists(t, fresh, "recently-touched staging file should be kept")
	require.FileExists(t, other, "non upload-* files must be left alone")
	require.FileExists(t, active, "in-flight upload's file must be protected")
}

func TestPurgeUploadSessions(t *testing.T) {
	reg, _ := testRegistry(t)
	require.NoError(t, reg.SetUploadDir(t.TempDir()))
	ctx := context.Background()

	const repoA = "test.example.com/test/wedged"
	const repoB = "test.example.com/test/healthy"

	start := func(repo string) (id, path string) {
		w, err := reg.PushBlobChunked(ctx, repo, 0)
		require.NoError(t, err)
		dw := w.(*diskBlobWriter)
		return dw.ID(), dw.Path()
	}

	idA1, pathA1 := start(repoA)
	idA2, pathA2 := start(repoA)
	idB, pathB := start(repoB)

	// A repo-scoped purge clears only the wedged repo's sessions and files.
	n, err := reg.PurgeUploadSessions(ctx, repoA)
	require.NoError(t, err)
	require.Equal(t, 2, n)
	for _, id := range []string{idA1, idA2} {
		s, err := reg.Repo().GetUploadSession(ctx, id)
		require.NoError(t, err)
		require.Nil(t, s, "purged session should be gone")
	}
	require.NoFileExists(t, pathA1)
	require.NoFileExists(t, pathA2)

	sB, err := reg.Repo().GetUploadSession(ctx, idB)
	require.NoError(t, err)
	require.NotNil(t, sB, "unrelated repo's session must survive a scoped purge")
	require.FileExists(t, pathB)

	// Purge-all clears whatever remains.
	n, err = reg.PurgeUploadSessions(ctx, "")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.NoFileExists(t, pathB)
}

func TestMountBlobExistingBlob(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	blobData := []byte("shared blob content")
	srcRepo := "test.example.com/test/src"
	dstRepo := "test.example.com/test/dst"

	srcDesc := pushBlobWithManifest(t, reg, srcRepo, blobData)

	desc, err := reg.MountBlob(ctx, srcRepo, dstRepo, srcDesc.Digest)
	require.NoError(t, err)
	require.Equal(t, srcDesc.Digest, desc.Digest)
	require.Equal(t, srcDesc.Size, desc.Size)

	manifest := fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`,
		testManifestMediaType, desc.Digest, desc.Size,
	)
	_, err = reg.PushManifest(ctx, dstRepo, "latest", []byte(manifest), testManifestMediaType)
	require.NoError(t, err)

	reader, err := reg.GetBlob(ctx, dstRepo, desc.Digest)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()
	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, blobData, got)
}

func TestMountBlobUnknownReturnsBlobUnknown(t *testing.T) {
	reg, _ := testRegistry(t)
	ctx := context.Background()

	unknown := ociregistry.Digest("sha256:" + strings.Repeat("0", 64))
	_, err := reg.MountBlob(ctx, "test.example.com/test/src", "test.example.com/test/dst", unknown)
	require.ErrorIs(t, err, ociregistry.ErrBlobUnknown)
}

func TestMountBlobHTTPFlow(t *testing.T) {
	reg, srv := testRegistry(t)

	blobData := []byte("http-mounted blob")
	srcRepo := "test.example.com/test/httpsrc"
	dstRepo := "test.example.com/test/httpdst"

	srcDesc := pushBlobWithManifest(t, reg, srcRepo, blobData)

	mountURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/?mount=%s&from=%s",
		srv.URL, dstRepo, srcDesc.Digest, srcRepo)
	req, _ := http.NewRequest("POST", mountURL, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Location"), "/blobs/"+string(srcDesc.Digest))

	unknown := "sha256:" + strings.Repeat("0", 64)
	mountURL = fmt.Sprintf("%s/v2/%s/blobs/uploads/?mount=%s&from=%s",
		srv.URL, dstRepo, unknown, srcRepo)
	req, _ = http.NewRequest("POST", mountURL, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestPushIndexProtectsChildrenFromGC(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)
	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	ctx := context.Background()

	repo := "test.example.com/multiarch"

	pushPlatform := func(platform string) ociregistry.Descriptor {
		cfg := []byte(`{"arch":"` + platform + `"}`)
		cfgDesc, err := reg.PushBlob(ctx, repo, descriptorFor(cfg), strings.NewReader(string(cfg)))
		require.NoError(t, err)
		body := fmt.Sprintf(
			`{"schemaVersion":2,"mediaType":"%s","config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`,
			testManifestMediaType, cfgDesc.Digest, cfgDesc.Size,
		)
		desc, err := reg.PushManifest(ctx, repo, "", []byte(body), testManifestMediaType)
		require.NoError(t, err)
		return desc
	}

	amd := pushPlatform("amd64")
	arm := pushPlatform("arm64")

	indexBody := fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
			`{"mediaType":"%s","digest":"%s","size":%d,"platform":{"os":"linux","architecture":"amd64"}},`+
			`{"mediaType":"%s","digest":"%s","size":%d,"platform":{"os":"linux","architecture":"arm64"}}]}`,
		testManifestMediaType, amd.Digest, amd.Size,
		testManifestMediaType, arm.Digest, arm.Size,
	)
	indexDesc, err := reg.PushManifest(ctx, repo, "v1", []byte(indexBody), "application/vnd.oci.image.index.v1+json")
	require.NoError(t, err)

	bogus := fmt.Sprintf(
		`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[`+
			`{"mediaType":"%s","digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000","size":1}]}`,
		testManifestMediaType,
	)
	_, err = reg.PushManifest(ctx, repo, "broken", []byte(bogus), "application/vnd.oci.image.index.v1+json")
	require.Error(t, err, "index with dangling child must be rejected")

	_, err = db.ExecContext(ctx,
		"UPDATE package_versions SET created_at = ?",
		time.Now().Add(-2*time.Hour))
	require.NoError(t, err)

	rows, err := db.PruneUntaggedManifests(ctx, time.Hour, 100)
	require.NoError(t, err)
	require.Empty(t, rows, "tagged index + referenced children must survive prune")

	for _, d := range []string{string(amd.Digest), string(arm.Digest), string(indexDesc.Digest)} {
		reader, err := reg.GetManifest(ctx, repo, ociregistry.Digest(d))
		require.NoError(t, err, "%s should still be pullable", d)
		_ = reader.Close()
	}
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

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
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

func TestUpstreamManifestHEADPullThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	upstream := newMockUpstreamFetcher()
	upstream.registries["ghcr.io"] = true

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc","size":0,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`)
	manifestDigest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(manifest))

	upstream.manifests["ghcr.io/owner/repo/"+manifestDigest] = mockManifest{
		data:      manifest,
		mediaType: testManifestMediaType,
	}
	reg.SetUpstreamFetcher(upstream)

	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	// HEAD request for an uncached upstream manifest should succeed
	req, _ := http.NewRequest("HEAD", srv.URL+"/v2/ghcr.io/owner/repo/manifests/"+manifestDigest, nil)
	req.Header.Set("Accept", testManifestMediaType)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int64(len(manifest)), resp.ContentLength)

	// Verify the manifest was cached locally
	repoObj, err := db.GetRepository(ctx, "ghcr.io/owner/repo")
	require.NoError(t, err)
	require.NotNil(t, repoObj)

	cached, err := db.GetManifestByDigest(ctx, repoObj.ID, manifestDigest)
	require.NoError(t, err)
	require.NotNil(t, cached, "manifest should be cached after HEAD pull-through")
}

func TestUpstreamTagHEADPullThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	upstream := newMockUpstreamFetcher()
	upstream.registries["quay.io"] = true

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:def","size":0,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`)
	manifestDigest := "sha256:" + fmt.Sprintf("%x", sha256.Sum256(manifest))

	upstream.manifests["quay.io/org/image/v2.0"] = mockManifest{
		data:      manifest,
		mediaType: testManifestMediaType,
	}
	reg.SetUpstreamFetcher(upstream)

	srv := httptest.NewServer(reg.Handler())
	t.Cleanup(srv.Close)

	// HEAD request for an uncached upstream tag should succeed
	req, _ := http.NewRequest("HEAD", srv.URL+"/v2/quay.io/org/image/manifests/v2.0", nil)
	req.Header.Set("Accept", testManifestMediaType)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int64(len(manifest)), resp.ContentLength)

	// Verify the tag was cached locally
	repoObj, err := db.GetRepository(ctx, "quay.io/org/image")
	require.NoError(t, err)
	require.NotNil(t, repoObj)

	tag, err := db.GetTag(ctx, repoObj.ID, "v2.0")
	require.NoError(t, err)
	require.NotNil(t, tag, "tag should be cached after HEAD pull-through")
	require.Equal(t, manifestDigest, tag.ManifestDigest)
}

// TestFederatedBlobPullDoesNotLoopOnUnknownRepo guards against the cluster
// loop that triggers when a peer claims the digest but the requested repo
// has no local reference: the original code fell through to peer fetch
// even when BlobExistsInRepo returned false, causing alice and bob to
// bounce a non-existent-repo request back and forth until something timed
// out.
func TestFederatedBlobPullDoesNotLoopOnUnknownRepo(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := NewRegistry(db, blobs, "https://test.example.com", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	resolver := &loopGuardResolver{}
	fetcher := &loopGuardFetcher{t: t}
	reg.SetFederation(resolver, fetcher)

	ctx := context.Background()

	realRepo := "test.example.com/test/real"
	blobData := []byte("scoped blob")
	desc, err := reg.PushBlob(ctx, realRepo, descriptorFor(blobData), strings.NewReader(string(blobData)))
	require.NoError(t, err)
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + string(desc.Digest) + `","size":` + fmt.Sprintf("%d", desc.Size) + `,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`)
	_, err = reg.PushManifest(ctx, realRepo, "v1", manifest, testManifestMediaType)
	require.NoError(t, err)

	_, err = reg.GetBlob(ctx, "test.example.com/test/nonexistent", desc.Digest)
	require.Error(t, err)
	require.Equal(t, 0, resolver.calls)
	require.Equal(t, 0, fetcher.calls)

	_, err = reg.GetBlob(ctx, "library/unknown", desc.Digest)
	require.Error(t, err)
	require.Equal(t, 0, resolver.calls)
	require.Equal(t, 0, fetcher.calls)

	otherRepo := "test.example.com/test/other"
	_, err = db.GetOrCreateRepository(ctx, otherRepo, "https://test.example.com")
	require.NoError(t, err)
	_, err = reg.GetBlob(ctx, otherRepo, desc.Digest)
	require.Error(t, err)
	require.Equal(t, 0, resolver.calls)
	require.Equal(t, 0, fetcher.calls)
}

type loopGuardResolver struct {
	calls int
}

func (m *loopGuardResolver) FindBlobPeers(_ context.Context, _ string) ([]BlobPeer, error) {
	m.calls++
	return []BlobPeer{{PeerEndpoint: "https://peer.example.test"}}, nil
}

type loopGuardFetcher struct {
	t     *testing.T
	calls int
}

func (m *loopGuardFetcher) FetchBlobStream(_ context.Context, _, _, _ string) (*peering.BlobStream, error) {
	m.calls++
	m.t.Fatalf("FetchBlobStream must not be invoked when blob is not in repo; call #%d", m.calls)
	return nil, nil
}

func (m *loopGuardFetcher) FetchManifest(_ context.Context, _, _, _ string) ([]byte, string, error) {
	m.calls++
	m.t.Fatalf("FetchManifest must not be invoked from getBlob; call #%d", m.calls)
	return nil, "", nil
}
