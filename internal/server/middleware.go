package server

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"runtime/debug"
	"slices"
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

// ReadFrom delegates to the underlying ResponseWriter's io.ReaderFrom so blob
// downloads keep the os.File sendfile fast path instead of being copied through
// a userspace buffer. Falls back to a generic copy when unsupported.
func (rw *responseWriter) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := rw.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(src)
		rw.written += n
		return n, err
	}
	n, err := io.Copy(rw.ResponseWriter, src)
	rw.written += n
	return n, err
}

// Unwrap exposes the underlying ResponseWriter for http.ResponseController.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
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

			if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
				return
			}

			logger.Info("request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", rw.statusCode,
				"bytes", rw.written,
				"duration", time.Since(start),
				"remote", remoteIP(r),
				"request_id", w.Header().Get("X-Request-ID"),
			)
		})
	}
}

// registryAuthMiddleware requires a Bearer token for mutating OCI registry requests.
// Read-only requests (GET, HEAD) are allowed without authentication to support
// anonymous image pulls from public registries, unless isPrivateRead returns true
// for the request path (used to protect private upstream images stored in apoci).
// Basic auth is also accepted, with the password treated as the token, to support
// OCI clients (e.g. flux) that only support Basic auth.
func registryAuthMiddleware(token, endpoint string, isPrivateRead func(context.Context, string) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				if isPrivateRead == nil || !isPrivateRead(r.Context(), r.URL.Path) {
					next.ServeHTTP(w, r)
					return
				}
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
				setBearerChallenge(w, endpoint)
				http.Error(w, "authentication required", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// setBearerChallenge advertises the registry's bearer-token endpoint via the
// WWW-Authenticate header so OCI clients (skopeo/oras/crane) arm their
// token-exchange flow. It is the single source of the challenge format, shared
// by registryAuthMiddleware and the GET /v2/ ping handler.
func setBearerChallenge(w http.ResponseWriter, endpoint string) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/v2/auth",service="registry"`, endpoint))
}

// ipRateLimiter provides per-IP rate limiting using x/time/rate with automatic
// eviction of stale entries via ttlcache.
type ipRateLimiter struct {
	cache          *ttlcache.Cache[string, *rate.Limiter]
	rate           rate.Limit
	burst          int
	trustedIPs     []net.IPNet
	trustedProxies []net.IPNet
}

// parseCIDRs turns a list of IPs/CIDRs into IPNets, skipping unparseable entries.
func parseCIDRs(entries []string) []net.IPNet {
	var out []net.IPNet
	for _, entry := range entries {
		if strings.Contains(entry, "/") {
			_, ipNet, err := net.ParseCIDR(entry)
			if err == nil {
				out = append(out, *ipNet)
			}
			continue
		}
		ip := net.ParseIP(entry)
		if ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			out = append(out, net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
		}
	}
	return out
}

func newIPRateLimiter(r rate.Limit, burst int, trustedIPs, trustedProxies []string) *ipRateLimiter {
	cache := ttlcache.New[string, *rate.Limiter](
		ttlcache.WithTTL[string, *rate.Limiter](10 * time.Minute),
	)
	go cache.Start()

	return &ipRateLimiter{
		cache:          cache,
		rate:           r,
		burst:          burst,
		trustedIPs:     parseCIDRs(trustedIPs),
		trustedProxies: parseCIDRs(trustedProxies),
	}
}

func (rl *ipRateLimiter) isTrusted(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}
	for _, trusted := range rl.trustedIPs {
		if trusted.Contains(ip) {
			return true
		}
	}
	return false
}

// remoteIP returns the direct peer address, for logging. Unlike clientIP it does
// not consult X-Forwarded-For.
func remoteIP(r *http.Request) string {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip == "" {
		return r.RemoteAddr
	}
	return ip
}

func (rl *ipRateLimiter) isTrustedProxy(ip net.IP) bool {
	for _, proxy := range rl.trustedProxies {
		if proxy.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP returns the rate-limit key. Behind a trusted proxy it takes the
// rightmost X-Forwarded-For entry that isn't itself a trusted proxy; otherwise
// it uses the direct peer, so an untrusted caller can't forge the header.
func (rl *ipRateLimiter) clientIP(r *http.Request) string {
	peer, _, _ := net.SplitHostPort(r.RemoteAddr)
	if peer == "" {
		peer = r.RemoteAddr
	}
	peerIP := net.ParseIP(peer)
	if peerIP == nil || !rl.isTrustedProxy(peerIP) {
		return peer
	}
	xff := r.Header.Get("X-Forwarded-For")
	if xff == "" {
		return peer
	}
	parts := strings.Split(xff, ",")
	for _, raw := range slices.Backward(parts) {
		ipStr := strings.TrimSpace(raw)
		ip := net.ParseIP(ipStr)
		if ip == nil {
			continue
		}
		if !rl.isTrustedProxy(ip) {
			return ipStr
		}
	}
	return peer
}

func (rl *ipRateLimiter) allow(ip string) bool {
	if rl.isTrusted(ip) {
		return true
	}
	// Get first so the common (cache-hit) path doesn't allocate a throwaway
	// limiter on every request; only construct one on an actual miss.
	if item := rl.cache.Get(ip); item != nil {
		return item.Value().Allow()
	}
	item, _ := rl.cache.GetOrSet(ip, rate.NewLimiter(rl.rate, rl.burst))
	return item.Value().Allow()
}

func (rl *ipRateLimiter) Stop() {
	rl.cache.Stop()
}

func rateLimitMiddleware(rl *ipRateLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !rl.allow(rl.clientIP(r)) {
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
			if !rl.allow(rl.clientIP(r)) {
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
		// Only assert HSTS when the request actually arrived over TLS. Sending it
		// on plain HTTP is a foot-gun (a 1-year includeSubDomains pin can break
		// sibling services on a shared parent domain).
		if r.TLS != nil {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		// UI routes need a more permissive CSP to load styles and scripts
		if r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/ui/") {
			w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'self' 'unsafe-inline'; script-src 'self'")
		} else {
			w.Header().Set("Content-Security-Policy", "default-src 'none'")
		}
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
