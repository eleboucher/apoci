package peering

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
)

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
