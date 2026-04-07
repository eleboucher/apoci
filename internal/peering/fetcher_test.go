package peering

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
)

func TestFetchBlobSuccess(t *testing.T) {
	blobData := []byte("test blob content for fetching")
	h := sha256.Sum256(blobData)
	digest := "sha256:" + hex.EncodeToString(h[:])

	// Mock peer serving the blob
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/test/repo/blobs/"+digest {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(blobData)
		} else {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fetcher := NewFetcher(10*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	result, err := fetcher.FetchBlob(context.Background(), srv.URL, "test/repo", digest)
	require.NoError(t, err)

	require.Equal(t, digest, result.Digest)
	require.Equal(t, string(blobData), string(result.Data))
	require.Equal(t, int64(len(blobData)), result.Size)
}

func TestFetchBlobDigestMismatch(t *testing.T) {
	// Server returns wrong content for the requested digest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("wrong content"))
	}))
	defer srv.Close()

	fetcher := NewFetcher(10*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	_, err := fetcher.FetchBlob(context.Background(), srv.URL, "test/repo", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err, "expected digest mismatch error")
}

func TestFetchBlobPeerDown(t *testing.T) {
	fetcher := NewFetcher(2*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	_, err := fetcher.FetchBlob(context.Background(), "http://127.0.0.1:1", "test/repo", "sha256:abc")
	require.Error(t, err, "expected error for unreachable peer")
}

func TestFetchBlobPeer404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fetcher := NewFetcher(10*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	_, err := fetcher.FetchBlob(context.Background(), srv.URL, "test/repo", "sha256:notfound")
	require.Error(t, err, "expected error for 404 response")
}

func TestCheckHealthSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	fetcher := NewFetcher(10*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	require.NoError(t, fetcher.CheckHealth(context.Background(), srv.URL))
}

func TestCheckHealthFailure(t *testing.T) {
	fetcher := NewFetcher(2*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	require.Error(t, fetcher.CheckHealth(context.Background(), "http://127.0.0.1:1"), "expected health check failure")
}

func TestFetchBlobRejectsOversized(t *testing.T) {
	bigData := make([]byte, 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bigData)
	}))
	defer srv.Close()

	// Limit to 100 bytes.
	fetcher := NewFetcher(10*time.Second, 100, config.DefaultMaxManifestSize, nopLog())
	_, err := fetcher.FetchBlob(context.Background(), srv.URL, "test/repo", "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err, "expected error for oversized blob")
}

func TestFetchManifestRejectsOversized(t *testing.T) {
	bigManifest := make([]byte, 200)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write(bigManifest)
	}))
	defer srv.Close()

	// Limit to 100 bytes.
	fetcher := NewFetcher(10*time.Second, config.DefaultMaxBlobSize, 100, nopLog())
	_, _, err := fetcher.FetchManifest(context.Background(), srv.URL, "test/repo", "latest")
	require.Error(t, err, "expected error for oversized manifest")
}

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
