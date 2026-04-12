package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func testServerWithUI(t *testing.T, uiEnabled bool) *Server {
	t.Helper()
	dir := t.TempDir()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	db, err := database.OpenSQLite(dir, 0, 0, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, logger)
	require.NoError(t, err)

	identity, err := activitypub.LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", logger)
	require.NoError(t, err)

	gcEnabled := true
	cfg := &config.Config{
		Name:          "test-node",
		Endpoint:      "https://test.example.com",
		Domain:        "test.example.com",
		AccountDomain: "test.example.com",
		Listen:        ":0",
		RegistryToken: "test-token",
		ImmutableTags: `^v[0-9]`,
		Peering: config.Peering{
			HealthCheckInterval: 30 * time.Second,
			FetchTimeout:        10 * time.Second,
		},
		Limits: config.Limits{
			MaxManifestSize: config.DefaultMaxManifestSize,
			MaxBlobSize:     config.DefaultMaxBlobSize,
		},
		RateLimits: config.RateLimits{
			InboxRate:         1000,
			InboxBurst:        1000,
			RegistryPushRate:  1000,
			RegistryPushBurst: 1000,
		},
		GC: config.GC{
			Enabled:          &gcEnabled,
			Interval:         6 * time.Hour,
			StalePeerBlobAge: 30 * 24 * time.Hour,
			OrphanBatchSize:  500,
		},
		UI: config.UI{
			Enabled: uiEnabled,
		},
	}

	s, err := New(cfg, db, blobs, identity, "test", logger)
	require.NoError(t, err)
	return s
}

func TestUIDisabled(t *testing.T) {
	s := testServerWithUI(t, false)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), `"status":"ok"`)
}

func TestUIIndex(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Create a local repo so we can test the "My Images" section
	_, err := s.db.GetOrCreateRepository(t.Context(), "test.example.com/localapp", s.identity.ActorURL)
	require.NoError(t, err)

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "test-node")
	assert.Contains(t, string(body), "My Images")
	assert.Contains(t, string(body), "test.example.com/localapp")
}

func TestUISearch(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Create a test repo
	_, err := s.db.GetOrCreateRepository(t.Context(), "test.example.com/myapp", s.identity.ActorURL)
	require.NoError(t, err)

	resp, err := http.Get(srv.URL + "/ui/search?q=myapp")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Contains(t, string(body), "test.example.com/myapp")
}

func TestUISearchShortQuery(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/ui/search?q=a")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	// Short query returns empty 200
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Empty(t, strings.TrimSpace(string(body)))
}

func TestUIStaticAssets(t *testing.T) {
	s := testServerWithUI(t, true)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	tests := []struct {
		path string
	}{
		{"/ui/static/pico.min.css"},
		{"/ui/static/htmx.min.js"},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + tc.path)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			assert.Equal(t, http.StatusOK, resp.StatusCode)

			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.NotEmpty(t, body)
		})
	}
}

func TestHumanizeBytes(t *testing.T) {
	tests := []struct {
		bytes    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			assert.Equal(t, tc.expected, humanizeBytes(tc.bytes))
		})
	}
}
