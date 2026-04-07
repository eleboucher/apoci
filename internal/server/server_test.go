package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const testRegistryToken = "test-token"

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	identity, err := activitypub.LoadOrCreateIdentity("test.example.com", "", "", nopLog())
	require.NoError(t, err)

	cfg := &config.Config{
		Name:          "test-node",
		Endpoint:      "https://test.example.com",
		Domain:        "test.example.com",
		AccountDomain: "test.example.com",
		Listen:        ":0",
		RegistryToken: testRegistryToken,
		ImmutableTags: `^v[0-9]`,
		Peering: config.Peering{
			HealthCheckInterval: 30 * time.Second,
			FetchTimeout:        10 * time.Second,
		},
		Limits: config.Limits{
			MaxManifestSize: config.DefaultMaxManifestSize,
			MaxBlobSize:     config.DefaultMaxBlobSize,
		},
	}

	s, err := New(cfg, db, blobs, identity, "test", nopLog())
	require.NoError(t, err)
	return s
}

func TestHealthz(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "ok", body["status"])
}

func TestReadyz(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/readyz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "ready", body["status"])
}

func TestRequestIDMiddlewareAddsHeader(t *testing.T) {
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)

	handler.ServeHTTP(rec, req)

	reqID := rec.Header().Get("X-Request-ID")
	require.NotEmpty(t, reqID, "expected X-Request-ID header to be set")
	require.Len(t, reqID, 36, "expected UUID-length request ID (36 chars)")
}

func TestRequestIDMiddlewarePreservesExisting(t *testing.T) {
	handler := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-ID", "my-custom-id-123")

	handler.ServeHTTP(rec, req)

	reqID := rec.Header().Get("X-Request-ID")
	require.Equal(t, "my-custom-id-123", reqID)
}

func TestRecoveryMiddlewareCatchesPanic(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	})

	handler := recoveryMiddleware(nopLog())(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/panic", nil)

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotEmpty(t, rec.Body.String(), "expected non-empty error body")
}

func TestRecoveryMiddlewarePassesThrough(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	handler := recoveryMiddleware(nopLog())(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/normal", nil)

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "ok", rec.Body.String())
}

func TestRequestIDAppearsOnRoutes(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	reqID := resp.Header.Get("X-Request-ID")
	require.NotEmpty(t, reqID, "expected X-Request-ID header on /healthz response")
}

func TestAPEndpointsExist(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// WebFinger
	resp, err := http.Get(srv.URL + "/.well-known/webfinger?resource=acct:registry@test.example.com")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "webfinger")

	// Actor
	req, _ := http.NewRequest("GET", srv.URL+"/ap/actor", nil)
	req.Header.Set("Accept", "application/activity+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "actor")

	// Outbox
	resp, err = http.Get(srv.URL + "/ap/outbox")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "outbox")

	// Followers
	resp, err = http.Get(srv.URL + "/ap/followers")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "followers")
}

func TestRateLimiterAllowsUpToBurst(t *testing.T) {
	rl := newIPRateLimiter(10, 5)
	defer rl.Stop()

	for i := range 5 {
		require.True(t, rl.allow("1.2.3.4"), "request %d should be allowed within burst", i+1)
	}

	// Next request should be rejected (burst exhausted, no time to refill).
	require.False(t, rl.allow("1.2.3.4"), "request 6 should be rate limited")
}

func TestRateLimiterTracksSeparateIPs(t *testing.T) {
	rl := newIPRateLimiter(10, 2)
	defer rl.Stop()

	require.True(t, rl.allow("10.0.0.1"), "first IP should be allowed")
	require.True(t, rl.allow("10.0.0.2"), "second IP should be allowed independently")
}

func TestRegistryAuthMiddlewareAllowsReadWithoutToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := registryAuthMiddleware("secret-token")(inner)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v2/test/blobs/sha256:abc", nil)
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "%s should be allowed without token", method)
	}
}

func TestRegistryAuthMiddlewareRejectsWriteWithoutToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := registryAuthMiddleware("secret-token")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/test/manifests/latest", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "PUT without token should be 401")
	require.NotEmpty(t, rec.Header().Get("WWW-Authenticate"), "expected WWW-Authenticate header on 401")
}

func TestRegistryAuthMiddlewareAcceptsValidToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	handler := registryAuthMiddleware("secret-token")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/test/manifests/latest", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, "PUT with valid token should be 201")
}

func TestRegistryAuthMiddlewareRejectsInvalidToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := registryAuthMiddleware("secret-token")(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v2/test/blobs/uploads/", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "POST with wrong token should be 401")
}

func TestAdminIdentityRequiresAuth(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/admin/identity")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestAdminIdentityWithToken(t *testing.T) {
	s := testServer(t)
	s.cfg.RegistryToken = testRegistryToken
	s.cfg.AdminToken = testRegistryToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/identity", nil)
	req.Header.Set("Authorization", "Bearer "+testRegistryToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var info map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	require.Equal(t, "test-node", info["name"])
	require.Equal(t, "test.example.com", info["domain"])
}

func TestAdminFollowsListEmpty(t *testing.T) {
	s := testServer(t)
	s.cfg.RegistryToken = testRegistryToken
	s.cfg.AdminToken = testRegistryToken
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/api/admin/follows", nil)
	req.Header.Set("Authorization", "Bearer "+testRegistryToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestRegistryAuthMiddlewareEmptyTokenBlocksWrites(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := registryAuthMiddleware("")(inner)

	// Writes should be blocked when no token is configured
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/test/manifests/latest", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "empty token config should block writes")

	// Reads should still be allowed
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v2/test/manifests/latest", nil)
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "empty token config should allow reads")
}
