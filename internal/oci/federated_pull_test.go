package oci_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"

	"cuelabs.dev/go/oci/ociregistry"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/oci"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
)

func testDescriptor(data []byte, mediaType string) ociregistry.Descriptor {
	return ociregistry.Descriptor{MediaType: mediaType, Size: int64(len(data))}
}

type mockResolver struct {
	peers []oci.BlobPeer
}

func (m *mockResolver) FindBlobPeers(_ context.Context, _ string) ([]oci.BlobPeer, error) {
	return m.peers, nil
}

type mockFetcher struct {
	data []byte
	err  error
}

func (m *mockFetcher) FetchBlobStream(_ context.Context, _, _, _ string) (*peering.BlobStream, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.data == nil {
		return nil, io.ErrUnexpectedEOF
	}
	return &peering.BlobStream{
		Body: io.NopCloser(bytes.NewReader(m.data)),
	}, nil
}

func (m *mockFetcher) FetchManifest(_ context.Context, _, _, _ string) ([]byte, string, error) {
	if m.data != nil {
		return m.data, "application/vnd.oci.image.manifest.v1+json", m.err
	}
	return nil, "", m.err
}

func TestFederatedBlobPull(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	blobData := []byte("federated blob content")

	reg, err := oci.NewRegistry(db, blobs, "https://local.test/ap/actor", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	reg.SetFederation(
		&mockResolver{peers: []oci.BlobPeer{{PeerEndpoint: "https://peer.test"}}},
		&mockFetcher{data: blobData},
	)

	ctx := context.Background()

	// Create repo so the blob push path works
	_, err = db.GetOrCreateRepository(ctx, "local.test/test/fedrepo", "https://local.test/ap/actor")
	require.NoError(t, err)

	// First, push a real blob locally to get its digest
	desc, err := reg.PushBlob(ctx, "local.test/test/fedrepo", testDescriptor(blobData, "application/octet-stream"), bytes.NewReader(blobData))
	require.NoError(t, err)

	// Push a manifest referencing the blob so it's linked to the repo.
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + string(desc.Digest) + `","size":` + fmt.Sprintf("%d", desc.Size) + `,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`)
	_, err = reg.PushManifest(ctx, "local.test/test/fedrepo", "v1", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	// Delete the local blob file to force federation path
	require.NoError(t, blobs.Delete(ctx, string(desc.Digest)))

	// GetBlob should now go through the federation path
	reader, err := reg.GetBlob(ctx, "local.test/test/fedrepo", desc.Digest)
	require.NoError(t, err, "federated GetBlob failed")
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, string(blobData), string(got))
}

func TestBlobPullLocalFirst(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := oci.NewRegistry(db, blobs, "https://local.test/ap/actor", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)

	// No federation configured -- should work for local blobs
	ctx := context.Background()
	blobData := []byte("local blob content")

	desc, err := reg.PushBlob(ctx, "local.test/test/local", testDescriptor(blobData, "application/octet-stream"), bytes.NewReader(blobData))
	require.NoError(t, err)

	// Push a manifest referencing the blob so it's linked to the repo.
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + string(desc.Digest) + `","size":` + fmt.Sprintf("%d", desc.Size) + `,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`)
	_, err = reg.PushManifest(ctx, "local.test/test/local", "v1", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	reader, err := reg.GetBlob(ctx, "local.test/test/local", desc.Digest)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	got, _ := io.ReadAll(reader)
	require.Equal(t, string(blobData), string(got))
}

func TestBlobPullNotFound(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	reg, err := oci.NewRegistry(db, blobs, "https://local.test/ap/actor", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	require.NoError(t, err)
	reg.SetFederation(
		&mockResolver{peers: nil},
		&mockFetcher{},
	)

	ctx := context.Background()
	_, err = reg.GetBlob(ctx, "test/missing", "sha256:nonexistent")
	require.Error(t, err, "expected error for missing blob")
}
