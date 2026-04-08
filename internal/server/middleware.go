package server

import (
	"crypto/subtle"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"golang.org/x/time/rate"

	"github.com/google/uuid"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
)

type responseWriter struct {
	http.ResponseWriter
	statusCode int
	written    int64
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.written += int64(n)
	return n, err
}

// requestIDSafeRe matches only safe characters for the X-Request-ID header.
var requestIDSafeRe = regexp.MustCompile(`^[a-zA-Z0-9\-_]{1,128}$`)

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" || !requestIDSafeRe.MatchString(reqID) {
			reqID = uuid.New().String()
		}
		w.Header().Set("X-Request-ID", reqID)
		next.ServeHTTP(w, r)
	})
}

func loggingMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

			next.ServeHTTP(rw, r)

			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.statusCode,
				"bytes", rw.written,
				"duration", time.Since(start),
				"remote", r.RemoteAddr,
				"request_id", w.Header().Get("X-Request-ID"),
			)
		})
	}
}

// registryAuthMiddleware requires a Bearer token for mutating OCI registry requests.
// Read-only requests (GET, HEAD) are intentionally allowed without authentication
// to support anonymous image pulls from public registries.
// Basic auth is also accepted, with the password treated as the token, to support
// OCI clients (e.g. flux) that only support Basic auth.
func registryAuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			if token == "" {
				http.Error(w, "registry write access requires a configured token", http.StatusForbidden)
				return
			}

			provided := ""
			auth := r.Header.Get("Authorization")
			if t, ok := strings.CutPrefix(auth, "Bearer "); ok {
				provided = t
			} else if _, p, ok := r.BasicAuth(); ok {
				provided = p
			}

			if subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="apoci",service="registry"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// ipRateLimiter provides per-IP rate limiting using x/time/rate with automatic
// eviction of stale entries via ttlcache.
type ipRateLimiter struct {
	cache *ttlcache.Cache[string, *rate.Limiter]
	rate  rate.Limit
	burst int
}

func newIPRateLimiter(r rate.Limit, burst int) *ipRateLimiter {
	cache := ttlcache.New[string, *rate.Limiter](
		ttlcache.WithTTL[string, *rate.Limiter](10 * time.Minute),
	)
	go cache.Start()

	return &ipRateLimiter{
		cache: cache,
		rate:  r,
		burst: burst,
	}
}

func (rl *ipRateLimiter) allow(ip string) bool {
	item, _ := rl.cache.GetOrSet(ip, rate.NewLimiter(rl.rate, rl.burst))
	return item.Value().Allow()
}

func (rl *ipRateLimiter) Stop() {
	rl.cache.Stop()
}

func clientIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		return r.RemoteAddr
	}
	return ip
}

func rateLimitMiddleware(rl *ipRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.allow(clientIP(r)) {
				metrics.InboxRateLimited.Add(1)
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func registryPushRateLimitMiddleware(rl *ipRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}
			if !rl.allow(clientIP(r)) {
				metrics.RegistryPushRateLimited.Add(1)
				http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func bearerAuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				http.Error(w, "admin API requires a configured token", http.StatusUnauthorized)
				return
			}
			auth := r.Header.Get("Authorization")
			provided, ok := strings.CutPrefix(auth, "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(provided), []byte(token)) != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="apoci"`)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// securityHeadersMiddleware adds standard defensive HTTP security headers to all responses.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func recoveryMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("panic recovered",
						"panic", rec,
						"method", r.Method,
						"path", r.URL.Path,
						"stack", string(debug.Stack()),
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
