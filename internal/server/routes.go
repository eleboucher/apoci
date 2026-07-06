package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/server/ui"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// UI routes
	if s.cfg.UI.Enabled {
		staticFS, err := fs.Sub(ui.StaticFS, "static")
		if err != nil {
			panic(fmt.Sprintf("failed to get static sub-fs: %v", err))
		}
		mux.HandleFunc("GET /{$}", s.handleUIIndex)
		mux.HandleFunc("GET /ui/search", s.handleUISearch)
		mux.HandleFunc("GET /ui/tags/{repo...}", s.handleUIRepoTags)
		mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServer(http.FS(staticFS))))
	} else {
		mux.HandleFunc("GET /{$}", s.handleMinimalRoot)
	}

	// Browsers request /favicon.ico regardless of the page's <link> icons (and
	// on pages without any, e.g. JSON endpoints), so serve the embedded icon at
	// the conventional root path even when the UI is disabled.
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, ui.StaticFS, "static/favicon.ico")
	})

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	// Rate-limit the auth oracle so failed Basic-auth attempts can't be
	// brute-forced at full speed.
	mux.Handle("/v2/auth", registryPushRateLimitMiddleware(s.registryPushLimiter)(http.HandlerFunc(s.handleRegistryAuth)))
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
			// OCI clients read an unconditional 200 ping as "no auth needed" and
			// never arm their bearer-token flow, so they can't push. Challenge an
			// anonymous ping; a client re-pinging with a token gets 200. Anonymous
			// pulls still work since reads are anon-allowed regardless of the token.
			if r.Header.Get("Authorization") == "" {
				setBearerChallenge(w, s.cfg.Endpoint)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
			return
		}
		// Manifest PUTs are buffered whole into memory by the OCI handler, so
		// cap the body before it is read. Blob uploads stream and are left
		// uncapped here (they are bounded elsewhere by MaxBlobSize).
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Limits.MaxManifestSize)
		}
		registryPushRateLimitMiddleware(s.registryPushLimiter)(
			registryAuthMiddleware(s.cfg.RegistryToken, s.cfg.Endpoint, s.isPrivateRead)(s.ociHandler),
		).ServeHTTP(w, r)
	})
	mux.Handle("GET /.well-known/webfinger", s.webfingerHandler)
	mux.Handle("GET /.well-known/nodeinfo", http.HandlerFunc(s.nodeinfoHandler.ServeWellKnown))
	mux.Handle("GET /ap/nodeinfo/2.1", http.HandlerFunc(s.nodeinfoHandler.ServeNodeInfo))
	mux.Handle("GET /ap/actor", s.actorHandler)
	mux.Handle("POST /ap/inbox", rateLimitMiddleware(s.inboxLimiter)(s.inboxHandler))
	mux.Handle("GET /ap/outbox", s.outboxHandler)
	mux.Handle("GET /ap/followers", s.followersHandler)
	mux.Handle("GET /ap/following", s.followingHandler)

	for _, b := range s.packageBackends.Backends() {
		prefix := b.RoutePrefix()
		mux.Handle(prefix+"/", b.Handler())
		mux.Handle(prefix, b.Handler())
	}

	// Swagger UI + OpenAPI spec for the admin API. Served unauthenticated: it
	// exposes only the API schema, not any registry data. More specific than the
	// "/api/admin/" pattern below, so ServeMux routes it here first.
	mux.Handle("GET /api/admin/docs/", swaggerHandler())
	mux.Handle("/api/admin/", http.StripPrefix("/api/admin", s.adminRouter()))

	var handler http.Handler = mux
	handler = loggingMiddleware(s.logger)(handler)
	handler = requestIDMiddleware(handler)
	handler = securityHeadersMiddleware(handler)
	handler = recoveryMiddleware(s.logger)(handler)

	return handler
}

// ociRepoFromPath extracts the full repository name from an OCI v2 API path.
// E.g. "/v2/ghcr.io/user/repo/manifests/latest" → "ghcr.io/user/repo", true.
// Uses the last occurrence across all OCI verb separators so that repo names
// containing verb words as path components (e.g. "org/blobs/repo") are handled
// correctly.
func ociRepoFromPath(path string) (string, bool) {
	tail, ok := strings.CutPrefix(path, "/v2/")
	if !ok {
		return "", false
	}
	latest := -1
	for _, suffix := range []string{"/manifests/", "/blobs/uploads/", "/blobs/", "/tags/"} {
		if idx := strings.LastIndex(tail, suffix); idx > latest {
			latest = idx
		}
	}
	if latest < 0 {
		return "", false
	}
	return tail[:latest], true
}

// isPrivateRead reports whether a GET/HEAD request to the given OCI path
// requires authentication. Config overrides take precedence; for token-auth
// registries the per-repo flag is read from the DB (populated by the fetcher
// after the first upstream pull). Fails closed on DB errors.
func (s *Server) isPrivateRead(ctx context.Context, path string) bool {
	// The catalog enumerates every repository name, including private and
	// upstream-mirrored repos, so it must not be served anonymously.
	if path == "/v2/_catalog" || strings.HasSuffix(path, "/v2/_catalog") {
		return true
	}
	repoName, ok := ociRepoFromPath(path)
	if !ok {
		return false
	}
	return s.isPrivateReadRepo(ctx, repoName)
}

// isPrivateReadRepo reports whether anonymous reads of the named OCI repository
// require authentication. Fails closed on DB errors.
func (s *Server) isPrivateReadRepo(ctx context.Context, repoName string) bool {
	firstSeg, _, _ := strings.Cut(repoName, "/")
	if !strings.Contains(firstSeg, ".") {
		return false // local repo, not an upstream
	}

	for _, u := range s.cfg.Upstreams.Registries {
		if u.Name != firstSeg {
			continue
		}
		if u.Private || (u.Auth == "basic" && u.Username != "") {
			return true
		}
		if u.Username == "" {
			return false // no credentials — all repos are public
		}
		// Token auth with credentials: check per-repo privacy from DB.
		// Require auth conservatively until the first pull records the actual state.
		repo, err := s.db.GetRepository(ctx, repoName)
		if err != nil {
			s.logger.Warn("failed to check repository privacy", "repo", repoName, "error", err)
			return true // fail closed: deny anonymous access on transient DB error
		}
		if repo == nil {
			return true
		}
		return repo.Private
	}
	return false // not a configured upstream
}

const keyStatus = "status"

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{keyStatus: "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(); err != nil {
		s.logger.Warn("readyz check failed", "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{keyStatus: "not ready"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{keyStatus: "ready"})
}

func (s *Server) handleRegistryAuth(w http.ResponseWriter, r *http.Request) {
	_, pass, ok := r.BasicAuth()
	switch {
	case !ok:
		// Strict clients (Flux, go-containerregistry) fetch a token here before
		// an anonymous pull. Hand pull-only scopes a random token that reads
		// don't validate and pushes can't use; refuse everything else.
		if s.anonymousPullAllowed(r.Context(), r.URL.Query().Get("scope")) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"token": rand.Text()})
			return
		}
		setBearerChallenge(w, s.cfg.Endpoint)
		http.Error(w, "registry write access requires a token", http.StatusUnauthorized)
		return
	case subtle.ConstantTimeCompare([]byte(pass), []byte(s.cfg.RegistryToken)) != 1:
		w.Header().Set("WWW-Authenticate", `Basic realm="apoci"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	case s.cfg.RegistryToken == "":
		// Empty token would otherwise be encoded as {"token":""}, which
		// moby's parseTokenResponse surfaces to buildx as the confusing
		// "authorization server did not include a token in the response".
		// Surface the real state so operators get an actionable signal.
		s.logger.Error("apoci registryToken is empty; refusing /v2/auth",
			"request_id", w.Header().Get("X-Request-ID"))
		http.Error(w, "registry is not properly configured", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": s.cfg.RegistryToken})
}

// anonymousPullAllowed reports whether an unauthenticated /v2/auth request may be
// granted a token: every scope must be pull-only on an anonymously readable repo.
func (s *Server) anonymousPullAllowed(ctx context.Context, scope string) bool {
	scope = strings.TrimSpace(scope)
	if scope == "" {
		return false
	}
	for sc := range strings.FieldsSeq(scope) {
		repo, actions, ok := parseRepositoryScope(sc)
		if !ok {
			return false
		}
		for _, a := range actions {
			if a != "pull" {
				return false // push or unknown action needs real credentials
			}
		}
		if s.isPrivateReadRepo(ctx, repo) {
			return false
		}
	}
	return true
}

// parseRepositoryScope splits an OCI "repository:<name>:<actions>" scope, using
// the final colon so repo names containing colons are preserved.
func parseRepositoryScope(scope string) (repo string, actions []string, ok bool) {
	rest, found := strings.CutPrefix(scope, "repository:")
	if !found {
		return "", nil, false
	}
	i := strings.LastIndex(rest, ":")
	if i <= 0 || i == len(rest)-1 {
		return "", nil, false
	}
	return rest[:i], strings.Split(rest[i+1:], ","), true
}
