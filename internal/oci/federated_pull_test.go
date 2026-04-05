package oci_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"cuelabs.dev/go/oci/ociregistry"
	"github.com/stretchr/testify/require"

	"github.com/apoci/apoci/internal/blobstore"
	"github.com/apoci/apoci/internal/config"
	"github.com/apoci/apoci/internal/database"
	"github.com/apoci/apoci/internal/oci"
	"github.com/apoci/apoci/internal/peering"
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
	result *peering.FetchResult
	err    error
}

func (m *mockFetcher) FetchBlob(_ context.Context, _, _, _ string) (*peering.FetchResult, error) {
	return m.result, m.err
}

func (m *mockFetcher) FetchBlobStream(_ context.Context, _, _, _ string) (*peering.BlobStream, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.result == nil {
		return nil, io.ErrUnexpectedEOF
	}
	return &peering.BlobStream{
		Body: io.NopCloser(bytes.NewReader(m.result.Data)),
	}, nil
}

func (m *mockFetcher) FetchManifest(_ context.Context, _, _, _ string) ([]byte, string, error) {
	if m.result != nil {
		return m.result.Data, "application/vnd.oci.image.manifest.v1+json", m.err
	}
	return nil, "", m.err
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestFederatedBlobPull(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, discardLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	blobs, err := blobstore.New(dir, discardLog())
	require.NoError(t, err)

	blobData := []byte("federated blob content")

	reg := oci.NewRegistry(db, blobs, "https://local.test/ap/actor", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, discardLog())
	reg.SetFederation(
		&mockResolver{peers: []oci.BlobPeer{{PeerEndpoint: "https://peer.test"}}},
		&mockFetcher{result: &peering.FetchResult{
			Data:   blobData,
			Digest: "sha256:placeholder",
			Size:   int64(len(blobData)),
		}},
	)

	ctx := context.Background()

	// Create repo so the blob push path works
	_, err = db.GetOrCreateRepository(ctx, "test/fedrepo", "https://local.test/ap/actor")
	require.NoError(t, err)

	// First, push a real blob locally to get its digest
	desc, err := reg.PushBlob(ctx, "test/fedrepo", testDescriptor(blobData, "application/octet-stream"), bytes.NewReader(blobData))
	require.NoError(t, err)

	// Delete the local blob file to force federation path
	require.NoError(t, blobs.Delete(string(desc.Digest)))

	// GetBlob should now go through the federation path
	reader, err := reg.GetBlob(ctx, "test/fedrepo", desc.Digest)
	require.NoError(t, err, "federated GetBlob failed")
	defer func() { _ = reader.Close() }()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Equal(t, string(blobData), string(got))
}

func TestBlobPullLocalFirst(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, discardLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	blobs, err := blobstore.New(dir, discardLog())
	require.NoError(t, err)

	reg := oci.NewRegistry(db, blobs, "https://local.test/ap/actor", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, discardLog())

	// No federation configured -- should work for local blobs
	ctx := context.Background()
	blobData := []byte("local blob content")

	desc, err := reg.PushBlob(ctx, "test/local", testDescriptor(blobData, "application/octet-stream"), bytes.NewReader(blobData))
	require.NoError(t, err)

	reader, err := reg.GetBlob(ctx, "test/local", desc.Digest)
	require.NoError(t, err)
	defer func() { _ = reader.Close() }()

	got, _ := io.ReadAll(reader)
	require.Equal(t, string(blobData), string(got))
}

func TestBlobPullNotFound(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, discardLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	blobs, err := blobstore.New(dir, discardLog())
	require.NoError(t, err)

	reg := oci.NewRegistry(db, blobs, "https://local.test/ap/actor", "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, discardLog())
	reg.SetFederation(
		&mockResolver{peers: nil},
		&mockFetcher{err: nil, result: nil},
	)

	ctx := context.Background()
	_, err = reg.GetBlob(ctx, "test/missing", "sha256:nonexistent")
	require.Error(t, err, "expected error for missing blob")
}
