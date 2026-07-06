package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
)

const (
	testRegistryToken        = "test-token"
	testDomain               = "test.example.com"
	testGHCRRepo             = "ghcr.io/user/repo"
	testFollowsAPI           = "/api/admin/follows"
	testFollowBody           = `{"target":"https://x.example.com/ap/actor"}`
	testOCIManifestMediaType = "application/vnd.oci.image.manifest.v1+json"
	testTagLatest            = "latest"
)

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	identity, err := activitypub.LoadOrCreateIdentity("https://test.example.com", testDomain, "", "", nopLog())
	require.NoError(t, err)

	cfg := &config.Config{
		Name:          "test-node",
		Endpoint:      "https://test.example.com",
		Domain:        testDomain,
		AccountDomain: testDomain,
		Listen:        ":0",
		RegistryToken: testRegistryToken,
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
			Enabled:          new(true),
			Interval:         6 * time.Hour,
			StalePeerBlobAge: 30 * 24 * time.Hour,
			OrphanBatchSize:  500,
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
	rl := newIPRateLimiter(10, 5, nil, nil)
	defer rl.Stop()

	for i := range 5 {
		require.True(t, rl.allow("1.2.3.4"), "request %d should be allowed within burst", i+1)
	}

	// Next request should be rejected (burst exhausted, no time to refill).
	require.False(t, rl.allow("1.2.3.4"), "request 6 should be rate limited")
}

func TestRateLimiterTracksSeparateIPs(t *testing.T) {
	rl := newIPRateLimiter(10, 2, nil, nil)
	defer rl.Stop()

	require.True(t, rl.allow("10.0.0.1"), "first IP should be allowed")
	require.True(t, rl.allow("10.0.0.2"), "second IP should be allowed independently")
}

func TestRateLimiterTrustedIPs(t *testing.T) {
	rl := newIPRateLimiter(10, 1, []string{"192.168.1.100", "10.0.0.0/8"}, nil)
	defer rl.Stop()

	// Trusted single IP should always be allowed
	for range 10 {
		require.True(t, rl.allow("192.168.1.100"), "trusted IP should bypass rate limit")
	}

	// Trusted CIDR should always be allowed
	for range 10 {
		require.True(t, rl.allow("10.5.5.5"), "IP in trusted CIDR should bypass rate limit")
	}

	// Non-trusted IP should be rate limited
	require.True(t, rl.allow("8.8.8.8"), "first request from untrusted IP allowed")
	require.False(t, rl.allow("8.8.8.8"), "second request from untrusted IP should be limited")
}

func TestClientIPForwardedFor(t *testing.T) {
	rl := newIPRateLimiter(10, 1, nil, []string{"10.0.0.0/8"})
	defer rl.Stop()

	newReq := func(remote, xff string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	// Untrusted direct peer: XFF is ignored, peer address is the key.
	require.Equal(t, "8.8.8.8", rl.clientIP(newReq("8.8.8.8:1234", "1.1.1.1")))

	// Trusted proxy: rightmost non-proxy XFF entry is the client.
	require.Equal(t, "1.1.1.1", rl.clientIP(newReq("10.0.0.1:443", "1.1.1.1")))

	// Chain of trusted proxies: skip them, return the real client.
	require.Equal(t, "1.1.1.1", rl.clientIP(newReq("10.0.0.1:443", "1.1.1.1, 10.0.0.2, 10.0.0.3")))

	// Trusted proxy but no XFF: fall back to the peer address.
	require.Equal(t, "10.0.0.1", rl.clientIP(newReq("10.0.0.1:443", "")))
}

func TestOCIRepoFromPath(t *testing.T) {
	tests := []struct {
		path     string
		wantRepo string
		wantOK   bool
	}{
		{"/v2/ghcr.io/user/repo/manifests/latest", testGHCRRepo, true},
		{"/v2/ghcr.io/user/repo/blobs/sha256:abc", testGHCRRepo, true},
		{"/v2/ghcr.io/user/repo/tags/list", testGHCRRepo, true},
		{"/v2/ghcr.io/user/repo/blobs/uploads/", testGHCRRepo, true},
		// Repo name contains an OCI verb as a path component — last separator wins.
		{"/v2/ghcr.io/org/blobs/repo/manifests/latest", "ghcr.io/org/blobs/repo", true},
		{"/v2/ghcr.io/org/manifests/repo/manifests/v1", "ghcr.io/org/manifests/repo", true},
		// Non-OCI paths.
		{"/v2/noslash", "", false},
		{"/healthz", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			repo, ok := ociRepoFromPath(tc.path)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.wantRepo, repo)
		})
	}
}

func TestIsPrivateRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	makeServer := func(auth, username string, cfgPrivate bool) *Server {
		return &Server{
			cfg: &config.Config{
				Upstreams: config.Upstreams{
					Registries: []config.Upstream{
						{Name: "reg.io", Auth: auth, Username: username, Private: cfgPrivate},
					},
				},
			},
			db:     db,
			logger: nopLog(),
		}
	}

	t.Run("non-OCI path returns false", func(t *testing.T) {
		s := makeServer("token", "user", false)
		require.False(t, s.isPrivateRead(ctx, "/healthz"))
	})

	t.Run("local repo (no dot in first segment) returns false", func(t *testing.T) {
		s := makeServer("token", "user", false)
		require.False(t, s.isPrivateRead(ctx, "/v2/myrepo/manifests/latest"))
	})

	t.Run("unknown upstream returns false", func(t *testing.T) {
		s := makeServer("token", "user", false)
		require.False(t, s.isPrivateRead(ctx, "/v2/other.io/user/repo/manifests/latest"))
	})

	t.Run("config private:true returns true", func(t *testing.T) {
		s := makeServer("none", "", true)
		require.True(t, s.isPrivateRead(ctx, "/v2/reg.io/user/repo/manifests/latest"))
	})

	t.Run("basic auth with username returns true", func(t *testing.T) {
		s := makeServer("basic", "user", false)
		require.True(t, s.isPrivateRead(ctx, "/v2/reg.io/user/repo/manifests/latest"))
	})

	t.Run("token auth without credentials returns false", func(t *testing.T) {
		s := makeServer("token", "", false)
		require.False(t, s.isPrivateRead(ctx, "/v2/reg.io/user/repo/manifests/latest"))
	})

	t.Run("token auth with credentials, repo not in DB returns true (conservative)", func(t *testing.T) {
		s := makeServer("token", "user", false)
		require.True(t, s.isPrivateRead(ctx, "/v2/reg.io/user/repo/manifests/latest"))
	})

	t.Run("token auth with credentials, repo in DB as public returns false", func(t *testing.T) {
		s := makeServer("token", "user", false)
		repoObj, err := db.GetOrCreateRepository(ctx, "reg.io/public/repo", "upstream:reg.io")
		require.NoError(t, err)
		require.NoError(t, db.SetRepositoryPrivate(ctx, repoObj.ID, false))
		require.False(t, s.isPrivateRead(ctx, "/v2/reg.io/public/repo/manifests/latest"))
	})

	t.Run("token auth with credentials, repo in DB as private returns true", func(t *testing.T) {
		s := makeServer("token", "user", false)
		repoObj, err := db.GetOrCreateRepository(ctx, "reg.io/priv/repo", "upstream:reg.io")
		require.NoError(t, err)
		require.NoError(t, db.SetRepositoryPrivate(ctx, repoObj.ID, true))
		require.True(t, s.isPrivateRead(ctx, "/v2/reg.io/priv/repo/manifests/latest"))
	})

	t.Run("DB error returns true (fail closed)", func(t *testing.T) {
		s := makeServer("token", "user", false)
		// Pre-create the repo so isPrivateRead reaches the DB query (not the nil-repo branch).
		repoObj, err := db.GetOrCreateRepository(ctx, "reg.io/error/repo", "upstream:reg.io")
		require.NoError(t, err)
		require.NoError(t, db.SetRepositoryPrivate(ctx, repoObj.ID, false))
		// Close the DB to force an error.
		require.NoError(t, db.Close())
		require.True(t, s.isPrivateRead(ctx, "/v2/reg.io/error/repo/manifests/latest"))
		// Reopen for other subtests (t.Cleanup will close again, which is fine).
		db, err = database.OpenSQLite(dir, 0, 0, nopLog())
		require.NoError(t, err)
		s.db = db
	})
}

func TestRegistryAuthMiddlewareAllowsReadWithoutToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := registryAuthMiddleware("secret-token", "https://registry.example.com", nil)(inner)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/v2/test/blobs/sha256:abc", nil)
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "%s should be allowed without token", method)
	}
}

func TestRegistryAuthMiddlewareAcceptsValidToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})

	handler := registryAuthMiddleware("secret-token", "https://registry.example.com", nil)(inner)

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

	handler := registryAuthMiddleware("secret-token", "https://registry.example.com", nil)(inner)

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
	require.Equal(t, testDomain, info["domain"])
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

	handler := registryAuthMiddleware("", "https://registry.example.com", nil)(inner)

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

func TestRegistryAuthMiddlewareRejectsWriteWithoutToken(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := registryAuthMiddleware("secret-token", "https://registry.example.com", nil)(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/v2/test/manifests/latest", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code, "PUT without token should be 401")
	require.Equal(t, `Bearer realm="https://registry.example.com/v2/auth",service="registry"`, rec.Header().Get("WWW-Authenticate"))
}

func TestRegistryAuthEndpointIssuesToken(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/auth", nil)
	req.SetBasicAuth("", testRegistryToken)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]string
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, testRegistryToken, body["token"])
}

func TestRegistryAuthEndpointRejectsWrongPassword(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/auth", nil)
	req.SetBasicAuth("", "wrong-password")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}

func TestRegistryAuthEndpointRejectsAnonymous(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/auth")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"anonymous /v2/auth must challenge, not issue a token")
	require.Contains(t, resp.Header.Get("WWW-Authenticate"), `Bearer realm="`,
		"anonymous /v2/auth must advertise the bearer realm")
}

func TestRegistryPingRequiresAuthChallenge(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	require.Equal(t,
		`Bearer realm="https://test.example.com/v2/auth",service="registry"`,
		resp.Header.Get("WWW-Authenticate"),
	)
}

// TestPeerHealthCheckAcceptsRealPing runs a real peering.Fetcher against the
// real server routes: CheckHealth must read this build's 401+Bearer /v2/ ping
// as healthy, or upgraded peers would mark each other unhealthy.
func TestPeerHealthCheckAcceptsRealPing(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	fetcher := peering.NewFetcher(10*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	require.NoError(t, fetcher.CheckHealth(t.Context(), srv.URL),
		"CheckHealth must treat this build's 401+Bearer /v2/ ping as healthy")
}

func TestRegistryPingWithAuthReturnsOK(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/", nil)
	req.Header.Set("Authorization", "Bearer any-token-value")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "application/json", resp.Header.Get("Content-Type"))

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(b))
}

func TestRegistryAuthEndpointReturns503OnEmptyToken(t *testing.T) {
	s := testServer(t)
	s.cfg.RegistryToken = "" // emulates a container restart with a wiped token file

	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Empty Basic auth passes constantTimeCompare against an empty token,
	// which is the path that would otherwise emit {"token":""}.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/auth", nil)
	req.SetBasicAuth("", "")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode,
		"empty RegistryToken must surface 503, not the {token:''} shape that buildx reports as 'did not include a token'")
}

func TestRegistryAuthFullFlow(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/test/blobs/uploads/", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	wwwAuth := resp.Header.Get("WWW-Authenticate")
	require.Contains(t, wwwAuth, `/v2/auth`)

	tokenReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v2/auth", nil)
	tokenReq.SetBasicAuth("", testRegistryToken)
	tokenResp, err := http.DefaultClient.Do(tokenReq)
	require.NoError(t, err)
	defer func() { _ = tokenResp.Body.Close() }()
	require.Equal(t, http.StatusOK, tokenResp.StatusCode)

	var tokenBody map[string]string
	require.NoError(t, json.NewDecoder(tokenResp.Body).Decode(&tokenBody))
	token := tokenBody["token"]
	require.NotEmpty(t, token)

	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v2/test/blobs/uploads/", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	resp2, err := http.DefaultClient.Do(req2)
	require.NoError(t, err)
	_ = resp2.Body.Close()
	require.NotEqual(t, http.StatusUnauthorized, resp2.StatusCode)
}

func TestFederationWithdrawalSweepEmitsDeletesAndStamps(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	s.cfg.Federation.ExcludedRepos = []string{"excluded/*"}

	excluded, err := s.db.GetOrCreatePackage(ctx, "oci", "excluded/repo", s.identity.ActorURL)
	require.NoError(t, err)
	dgst := "sha256:" + strings.Repeat("a", 64)
	require.NoError(t, s.db.PutPackageVersion(ctx, &database.PackageVersion{
		PackageID: excluded.ID,
		Version:   dgst,
		MediaType: testOCIManifestMediaType,
		Metadata:  []byte(`{}`),
	}))
	require.NoError(t, s.db.PutPackageTag(ctx, excluded.ID, testTagLatest, dgst))

	_, err = s.db.GetOrCreatePackage(ctx, "oci", "kept/repo", s.identity.ActorURL)
	require.NoError(t, err)

	before, err := s.db.ListActivitiesPage(ctx, s.identity.ActorURL, 0, 100)
	require.NoError(t, err)

	s.runFederationWithdrawalSweep(ctx)

	after, err := s.db.ListActivitiesPage(ctx, s.identity.ActorURL, 0, 100)
	require.NoError(t, err)
	require.Len(t, after, len(before)+2, "expected 2 Delete activities (1 tag + 1 manifest)")

	deletes := 0
	for _, a := range after {
		if a.Type == activitypub.ActivityDelete {
			deletes++
		}
	}
	require.Equal(t, 2, deletes)

	excludedAfter, err := s.db.GetPackage(ctx, "oci", "excluded/repo")
	require.NoError(t, err)
	require.NotNil(t, excludedAfter.FederationWithdrawnAt, "excluded repo should be stamped withdrawn")

	keptAfter, err := s.db.GetPackage(ctx, "oci", "kept/repo")
	require.NoError(t, err)
	require.Nil(t, keptAfter.FederationWithdrawnAt, "non-matching repo must remain pending")

	// Idempotent: a second pass must emit nothing.
	s.runFederationWithdrawalSweep(ctx)
	final, err := s.db.ListActivitiesPage(ctx, s.identity.ActorURL, 0, 100)
	require.NoError(t, err)
	require.Len(t, final, len(after), "second sweep must not re-emit")
}

func TestFaviconServedAtRoot(t *testing.T) {
	for _, uiEnabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("uiEnabled=%v", uiEnabled), func(t *testing.T) {
			s := testServer(t)
			s.cfg.UI.Enabled = uiEnabled
			srv := httptest.NewServer(s.routes())
			defer srv.Close()

			resp, err := http.Get(srv.URL + "/favicon.ico")
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()

			require.Equal(t, http.StatusOK, resp.StatusCode)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NotEmpty(t, body)
			require.Contains(t, resp.Header.Get("Content-Type"), "icon")
		})
	}
}
