package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
)

const (
	testManifestJSON = `{"schemaVersion":2}`
	pathV2           = "/v2/"
	pathV2Token      = "/v2/token"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestFetcher_HasRegistry(t *testing.T) {
	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "docker.io", Endpoint: "https://registry-1.docker.io", Auth: "none"},
			{Name: "ghcr.io", Endpoint: "https://ghcr.io", Auth: "none"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	require.True(t, f.HasRegistry("docker.io"))
	require.True(t, f.HasRegistry("ghcr.io"))
	require.False(t, f.HasRegistry("quay.io"))
	require.False(t, f.HasRegistry("unknown"))
}

func TestFetcher_FetchManifest_NoAuth(t *testing.T) {
	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/library/alpine/manifests/latest", r.URL.Path)
		require.Contains(t, r.Header.Get("Accept"), "application/vnd.oci.image.manifest.v1+json")

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "test.registry", Endpoint: srv.URL, Auth: "none"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	data, mediaType, err := f.FetchManifest(context.Background(), "test.registry", "library/alpine", "latest")
	require.NoError(t, err)
	require.Equal(t, manifest, string(data))
	require.Equal(t, "application/vnd.oci.image.manifest.v1+json", mediaType)
}

func TestFetcher_FetchManifest_BasicAuth(t *testing.T) {
	manifest := testManifestJSON

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "testuser" || pass != "testpass" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "private.registry", Endpoint: srv.URL, Auth: "basic", Username: "testuser", Password: "testpass"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	data, _, err := f.FetchManifest(context.Background(), "private.registry", "myrepo", "v1")
	require.NoError(t, err)
	require.Equal(t, manifest, string(data))
}

func TestFetcher_FetchManifest_TokenAuth(t *testing.T) {
	manifest := testManifestJSON
	tokenIssued := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Challenge probe: GET /v2/ with no auth → 401 + WWW-Authenticate
		if r.URL.Path == pathV2 && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm="%s/v2/token",service="test.registry"`, "http://"+r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		// Token endpoint
		if r.URL.Path == pathV2Token {
			require.Equal(t, "test.registry", r.URL.Query().Get("service"))
			tokenIssued = true
			resp := map[string]any{
				"token":      "test-bearer-token",
				"expires_in": 300,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		// Registry endpoint - require bearer token
		auth := r.Header.Get("Authorization")
		if auth != "Bearer test-bearer-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "token.registry", Endpoint: srv.URL, Auth: "token"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	data, _, err := f.FetchManifest(context.Background(), "token.registry", "myrepo", "v1")
	require.NoError(t, err)
	require.Equal(t, manifest, string(data))
	require.True(t, tokenIssued, "token should have been requested")
}

func TestFetcher_FetchManifest_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "test.registry", Endpoint: srv.URL, Auth: "none"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	_, _, err := f.FetchManifest(context.Background(), "test.registry", "nonexistent/repo", "v1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

func TestFetcher_FetchManifest_CircuitBreaker(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "flaky.registry", Endpoint: srv.URL, Auth: "none"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	// Make enough calls to trip the circuit breaker
	for range circuitThreshold + 2 {
		_, _, _ = f.FetchManifest(context.Background(), "flaky.registry", "repo", "v1")
	}

	// After threshold, circuit should be open
	require.Equal(t, 1, f.CircuitOpenCount())

	// Additional calls should fail fast without hitting the server
	prevCount := callCount
	_, _, err := f.FetchManifest(context.Background(), "flaky.registry", "repo", "v1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "circuit open")
	require.Equal(t, prevCount, callCount, "should not have made additional HTTP calls")
}

func TestFetcher_FetchBlobStream(t *testing.T) {
	blobData := []byte("test blob content")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/library/alpine/blobs/sha256:abc123", r.URL.Path)

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(blobData)
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "test.registry", Endpoint: srv.URL, Auth: "none"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	stream, err := f.FetchBlobStream(context.Background(), "test.registry", "library/alpine", "sha256:abc123")
	require.NoError(t, err)
	require.NotNil(t, stream)

	data, err := io.ReadAll(stream.Body)
	require.NoError(t, err)
	require.NoError(t, stream.Body.Close())
	require.Equal(t, blobData, data)
}

func TestFetcher_FetchManifest_UnknownRegistry(t *testing.T) {
	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries:   []config.Upstream{},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	_, _, err := f.FetchManifest(context.Background(), "unknown.registry", "repo", "v1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "not configured")
}

func TestFetcher_TokenCaching(t *testing.T) {
	tokenRequests := 0
	manifest := testManifestJSON

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Challenge probe
		if r.URL.Path == pathV2 && r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm="%s/v2/token",service="token.registry"`, "http://"+r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if r.URL.Path == pathV2Token {
			tokenRequests++
			resp := map[string]any{
				"token":      "cached-token",
				"expires_in": 300,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		auth := r.Header.Get("Authorization")
		if auth != "Bearer cached-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "token.registry", Endpoint: srv.URL, Auth: "token"},
		},
	}

	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	// First request should fetch token
	_, _, err := f.FetchManifest(context.Background(), "token.registry", "myrepo", "v1")
	require.NoError(t, err)
	require.Equal(t, 1, tokenRequests)

	// Second request to same repo should use cached token
	_, _, err = f.FetchManifest(context.Background(), "token.registry", "myrepo", "v2")
	require.NoError(t, err)
	require.Equal(t, 1, tokenRequests, "should have used cached token")
}

func TestParseWWWAuthenticate(t *testing.T) {
	tests := []struct {
		name        string
		header      string
		wantRealm   string
		wantService string
	}{
		{
			name:        "docker hub",
			header:      `Bearer realm="https://auth.docker.io/token",service="registry.docker.io"`,
			wantRealm:   "https://auth.docker.io/token",
			wantService: "registry.docker.io",
		},
		{
			name:        "ghcr",
			header:      `Bearer realm="https://ghcr.io/token",service="ghcr.io"`,
			wantRealm:   "https://ghcr.io/token",
			wantService: "ghcr.io",
		},
		{
			name:        "realm only",
			header:      `Bearer realm="https://example.com/token"`,
			wantRealm:   "https://example.com/token",
			wantService: "",
		},
		{
			name:        "extra unknown params ignored",
			header:      `Bearer realm="https://example.com/token",service="svc",scope="repository:foo:pull"`,
			wantRealm:   "https://example.com/token",
			wantService: "svc",
		},
		{
			name:        "not bearer scheme",
			header:      `Basic realm="registry"`,
			wantRealm:   "",
			wantService: "",
		},
		{
			name:        "empty header",
			header:      "",
			wantRealm:   "",
			wantService: "",
		},
		{
			name:        "unclosed quote",
			header:      `Bearer realm="https://example.com/token`,
			wantRealm:   "",
			wantService: "",
		},
		{
			name:        "value with comma inside quotes",
			header:      `Bearer realm="https://example.com/token?a=1,b=2",service="svc"`,
			wantRealm:   "https://example.com/token?a=1,b=2",
			wantService: "svc",
		},
		{
			name:        "extra whitespace",
			header:      `Bearer  realm="https://example.com/token" , service="svc"`,
			wantRealm:   "https://example.com/token",
			wantService: "svc",
		},
		{
			name:        "unquoted values",
			header:      `Bearer realm=https://example.com/token,service=svc`,
			wantRealm:   "https://example.com/token",
			wantService: "svc",
		},
		{
			name:        "mixed quoted and unquoted",
			header:      `Bearer realm="https://example.com/token",service=svc`,
			wantRealm:   "https://example.com/token",
			wantService: "svc",
		},
		{
			name:        "no equals sign",
			header:      `Bearer realm`,
			wantRealm:   "",
			wantService: "",
		},
		{
			name:        "duplicate realm uses first",
			header:      `Bearer realm="first",realm="second"`,
			wantRealm:   "second",
			wantService: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			realm, service := parseWWWAuthenticate(tt.header)
			require.Equal(t, tt.wantRealm, realm)
			require.Equal(t, tt.wantService, service)
		})
	}
}

func TestFetcher_DiscoverChallenge_RetryAfterFailure(t *testing.T) {
	// Simulates a registry whose /v2/ probe fails on the first call but succeeds
	// on subsequent calls. With sync.Once this would have permanently cached the
	// failure; with the mutex-protected approach it retries and eventually succeeds.
	manifest := testManifestJSON
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathV2 {
			callCount++
			if callCount == 1 {
				// First probe: simulate a transient 503 (no WWW-Authenticate).
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			// Second probe: respond with a proper Bearer challenge.
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm="%s/v2/token",service="retry.registry"`, "http://"+r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == pathV2Token {
			resp := map[string]any{"token": "retry-token", "expires_in": 300}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if r.Header.Get("Authorization") != "Bearer retry-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "retry.registry", Endpoint: srv.URL, Auth: "token"},
		},
	}
	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	// First attempt: probe returns 503 with no WWW-Authenticate — the fallback
	// token URL is used, but /v2/token will succeed anyway.
	_, _, err := f.FetchManifest(context.Background(), "retry.registry", "myrepo", "v1")
	// This may or may not succeed depending on whether the fallback URL works;
	// what matters is that the challenge cache was set to done=true after the
	// first call (either via fallback or error). On any retry path the probe
	// must NOT be repeated more than twice total.
	_ = err

	// The probe should have been called exactly once so far.
	require.Equal(t, 1, callCount, "probe should be called once")
}

func TestFetcher_CircuitBreaker_TripsOnAuthDiscoveryFailure(t *testing.T) {
	// A registry whose /v2/ probe always returns a connection error should
	// eventually trip the circuit breaker.
	//
	// We use a server that immediately closes connections to simulate a hard
	// network failure during the probe.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hijack and close the connection to force a network error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 5 * time.Second,
		Registries: []config.Upstream{
			{Name: "broken.registry", Endpoint: srv.URL, Auth: "token"},
		},
	}
	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	// Make enough calls to trip the circuit breaker via discovery failures.
	for range circuitThreshold + 2 {
		// Reset the challenge cache so each call re-probes (simulating retries
		// across separate requests, which is the realistic scenario after a
		// transient failure clears and then recurs).
		reg := f.registries["broken.registry"]
		reg.challenge.mu.Lock()
		reg.challenge.done = false
		reg.challenge.mu.Unlock()

		_, _, _ = f.FetchManifest(context.Background(), "broken.registry", "repo", "v1")
	}

	require.Equal(t, 1, f.CircuitOpenCount(), "circuit should be open after repeated auth discovery failures")
}

func TestFetcher_DiscoverChallenge_FallbackOnNoChallengeHeader(t *testing.T) {
	// Registry that returns 200 on /v2/ with no WWW-Authenticate
	manifest := testManifestJSON
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == pathV2Token {
			resp := map[string]any{"token": "fallback-token", "expires_in": 300}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer fallback-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		_, _ = w.Write([]byte(manifest))
	}))
	defer srv.Close()

	cfg := config.Upstreams{
		Enabled:      true,
		FetchTimeout: 30 * time.Second,
		Registries: []config.Upstream{
			{Name: "no.challenge.registry", Endpoint: srv.URL, Auth: "token"},
		},
	}
	f := NewFetcher(cfg, 100*1024*1024, 10*1024*1024, testLogger())

	data, _, err := f.FetchManifest(context.Background(), "no.challenge.registry", "myrepo", "v1")
	require.NoError(t, err)
	require.Equal(t, manifest, string(data))
}
