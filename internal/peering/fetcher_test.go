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

// A peer running the bearer-challenge build answers an anonymous GET /v2/ with
// 401 + WWW-Authenticate: Bearer. CheckHealth must treat that as healthy — the
// registry is alive and speaking the OCI token protocol (identical to how
// Docker Hub/ghcr.io respond to an anonymous /v2/ ping).
func TestCheckHealthAcceptsBearerChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="https://x/v2/auth",service="registry"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	fetcher := NewFetcher(10*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	require.NoError(t, fetcher.CheckHealth(context.Background(), srv.URL))
}

// A bare 401 with no bearer challenge is not a healthy OCI registry — it stays
// a failure, so we never accept every 401 indiscriminately.
func TestCheckHealthRejectsUnauthenticatedWithoutChallenge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	fetcher := NewFetcher(10*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	require.Error(t, fetcher.CheckHealth(context.Background(), srv.URL), "plain 401 without a Bearer challenge must be unhealthy")
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
