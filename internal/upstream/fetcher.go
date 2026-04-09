package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

// challengeCache holds a cached auth challenge discovery result.
// mu protects all fields. done is only set to true on success, so
// a failed discovery will be retried on the next call.
type challengeCache struct {
	mu    sync.Mutex
	done  bool
	realm string
	svc   string
}

const (
	upstreamUserAgent      = "apoci/upstream-proxy"
	defaultTokenExpirySecd = 300 // 5 min; used when expires_in is absent
	tokenCacheBufferSec    = 30  // refresh token this many seconds before expiry
)

// registry holds runtime state for a configured upstream.
type registry struct {
	config     config.Upstream
	tokenCache sync.Map // repo -> cachedToken
	challenge  challengeCache
}

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// Fetcher proxies requests to upstream OCI registries.
type Fetcher struct {
	client          *http.Client
	registries      map[string]*registry // name -> registry
	logger          *slog.Logger
	maxBlobSize     int64
	maxManifestSize int64
	circuit         *circuitBreaker
}

// NewFetcher creates an upstream fetcher from config.
func NewFetcher(cfg config.Upstreams, maxBlobSize, maxManifestSize int64, logger *slog.Logger) *Fetcher {
	registries := make(map[string]*registry)
	for _, r := range cfg.Registries {
		registries[r.Name] = &registry{config: r}
	}
	return &Fetcher{
		client: &http.Client{
			Timeout: cfg.FetchTimeout,
			Transport: &http.Transport{
				DialContext: validate.SafeDialContext,
			},
		},
		registries:      registries,
		logger:          logger,
		maxBlobSize:     maxBlobSize,
		maxManifestSize: maxManifestSize,
		circuit:         newCircuitBreaker(),
	}
}

// HasRegistry returns true if the registry name is configured.
func (f *Fetcher) HasRegistry(name string) bool {
	_, ok := f.registries[name]
	return ok
}

// CircuitOpenCount returns the number of registries with open circuits.
func (f *Fetcher) CircuitOpenCount() int {
	return f.circuit.openCount()
}

// recordFailureWithMetrics records a failure and updates metrics if circuit opens.
func (f *Fetcher) recordFailureWithMetrics(registry string) {
	opened := f.circuit.recordFailure(registry)
	if opened {
		metrics.UpstreamCircuitOpen.WithLabelValues(registry).Set(1)
		f.logger.Warn("circuit breaker opened for upstream", "registry", registry)
	}
}

// recordSuccessWithMetrics records a success and updates metrics if circuit was open.
func (f *Fetcher) recordSuccessWithMetrics(registry string) {
	wasOpen := f.circuit.recordSuccess(registry)
	if wasOpen {
		metrics.UpstreamCircuitOpen.WithLabelValues(registry).Set(0)
		f.logger.Info("circuit breaker closed for upstream", "registry", registry)
	}
}

// FetchManifest fetches a manifest from an upstream registry.
func (f *Fetcher) FetchManifest(ctx context.Context, registryName, repo, reference string) ([]byte, string, error) {
	return f.fetchManifestWithRetry(ctx, registryName, repo, reference, false)
}

func (f *Fetcher) fetchManifestWithRetry(ctx context.Context, registryName, repo, reference string, retried bool) ([]byte, string, error) {
	reg, ok := f.registries[registryName]
	if !ok {
		return nil, "", fmt.Errorf("upstream registry %q not configured", registryName)
	}

	if open, expired := f.circuit.isOpen(registryName); open {
		return nil, "", fmt.Errorf("circuit open for upstream %s", registryName)
	} else if expired {
		metrics.UpstreamCircuitOpen.WithLabelValues(registryName).Set(0)
		f.logger.Info("circuit breaker expired for upstream", "registry", registryName)
	}

	reqURL := fmt.Sprintf("%s/v2/%s/manifests/%s",
		strings.TrimRight(reg.config.Endpoint, "/"),
		escapePathSegments(repo),
		url.PathEscape(reference))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}

	// OCI manifest accept header
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, */*")
	req.Header.Set("User-Agent", upstreamUserAgent)

	if err := f.addAuth(ctx, req, reg, repo); err != nil {
		return nil, "", fmt.Errorf("adding auth: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.recordFailureWithMetrics(registryName)
		return nil, "", fmt.Errorf("fetching manifest: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized && !retried {
		_ = resp.Body.Close()
		reg.tokenCache.Delete(repo)
		return f.fetchManifestWithRetry(ctx, registryName, repo, reference, true)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, "", fmt.Errorf("manifest not found on upstream")
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			f.recordFailureWithMetrics(registryName)
		}
		return nil, "", fmt.Errorf("upstream returned %d for %s", resp.StatusCode, reqURL)
	}

	f.recordSuccessWithMetrics(registryName)

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxManifestSize+1))
	_ = resp.Body.Close()
	if err != nil {
		return nil, "", fmt.Errorf("reading manifest: %w", err)
	}
	if int64(len(data)) > f.maxManifestSize {
		return nil, "", fmt.Errorf("manifest exceeds max size")
	}

	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "application/vnd.oci.image.manifest.v1+json"
	}

	f.logger.Debug("fetched manifest from upstream",
		"registry", registryName,
		"repo", repo,
		"reference", reference,
		"size", len(data),
	)

	return data, mediaType, nil
}

// FetchBlobStream fetches a blob from an upstream registry.
func (f *Fetcher) FetchBlobStream(ctx context.Context, registryName, repo, digest string) (*peering.BlobStream, error) {
	return f.fetchBlobStreamWithRetry(ctx, registryName, repo, digest, false)
}

func (f *Fetcher) fetchBlobStreamWithRetry(ctx context.Context, registryName, repo, digest string, retried bool) (*peering.BlobStream, error) {
	reg, ok := f.registries[registryName]
	if !ok {
		return nil, fmt.Errorf("upstream registry %q not configured", registryName)
	}

	if open, expired := f.circuit.isOpen(registryName); open {
		return nil, fmt.Errorf("circuit open for upstream %s", registryName)
	} else if expired {
		metrics.UpstreamCircuitOpen.WithLabelValues(registryName).Set(0)
		f.logger.Info("circuit breaker expired for upstream", "registry", registryName)
	}

	reqURL := fmt.Sprintf("%s/v2/%s/blobs/%s",
		strings.TrimRight(reg.config.Endpoint, "/"),
		escapePathSegments(repo),
		url.PathEscape(digest))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("User-Agent", upstreamUserAgent)

	if err := f.addAuth(ctx, req, reg, repo); err != nil {
		return nil, fmt.Errorf("adding auth: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		f.recordFailureWithMetrics(registryName)
		return nil, fmt.Errorf("fetching blob: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized && !retried {
		_ = resp.Body.Close()
		reg.tokenCache.Delete(repo)
		return f.fetchBlobStreamWithRetry(ctx, registryName, repo, digest, true)
	}

	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("blob not found on upstream")
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		if resp.StatusCode >= 500 {
			f.recordFailureWithMetrics(registryName)
		}
		return nil, fmt.Errorf("upstream returned %d", resp.StatusCode)
	}

	f.recordSuccessWithMetrics(registryName)

	f.logger.Debug("streaming blob from upstream",
		"registry", registryName,
		"repo", repo,
		"digest", digest,
	)

	return &peering.BlobStream{
		Body: &limitedReadCloser{
			Reader: io.LimitReader(resp.Body, f.maxBlobSize+1),
			closer: resp.Body,
		},
	}, nil
}

// addAuth adds authentication to the request based on registry config.
func (f *Fetcher) addAuth(ctx context.Context, req *http.Request, reg *registry, repo string) error {
	switch reg.config.Auth {
	case "none":
		return nil
	case "basic":
		req.SetBasicAuth(reg.config.Username, reg.config.Password)
		return nil
	case "token":
		token, err := f.getToken(ctx, reg, repo)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	default:
		return fmt.Errorf("unknown auth type: %s", reg.config.Auth)
	}
}

// getToken fetches a bearer token for the given repo using RFC 6750 /
// Docker Registry v2 auth. It first discovers the token endpoint via a
// WWW-Authenticate challenge on GET /v2/ (result is cached per registry),
// then exchanges credentials for a scoped pull token (cached per repo).
func (f *Fetcher) getToken(ctx context.Context, reg *registry, repo string) (string, error) {
	// Check per-repo token cache first.
	if cached, ok := reg.tokenCache.Load(repo); ok {
		ct := cached.(cachedToken)
		if time.Now().Before(ct.expiresAt) {
			return ct.token, nil
		}
	}

	// Discover the token endpoint via WWW-Authenticate challenge (once per registry).
	realm, service, err := f.discoverChallenge(ctx, reg)
	if err != nil {
		f.recordFailureWithMetrics(reg.config.Name)
		return "", fmt.Errorf("discovering auth challenge: %w", err)
	}

	// Build the token request URL.
	scope := fmt.Sprintf("repository:%s:pull", repo)
	tokenURL, err := url.Parse(realm)
	if err != nil {
		return "", fmt.Errorf("parsing realm URL %q: %w", realm, err)
	}
	q := tokenURL.Query()
	if service != "" {
		q.Set("service", service)
	}
	q.Set("scope", scope)
	tokenURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("creating token request: %w", err)
	}

	req.Header.Set("User-Agent", upstreamUserAgent)

	if reg.config.Username != "" {
		req.SetBasicAuth(reg.config.Username, reg.config.Password)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetching token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", fmt.Errorf("decoding token response: %w", err)
	}

	token := tokenResp.Token
	if token == "" {
		token = tokenResp.AccessToken
	}
	if token == "" {
		return "", fmt.Errorf("no token in response")
	}

	expiresIn := tokenResp.ExpiresIn
	if expiresIn == 0 {
		expiresIn = defaultTokenExpirySecd
	}

	// Cache with some buffer before expiry (min 1 second to avoid negative duration).
	cacheBuffer := tokenCacheBufferSec
	if expiresIn <= cacheBuffer {
		cacheBuffer = expiresIn / 2
	}
	if cacheBuffer < 1 {
		cacheBuffer = 1
	}
	reg.tokenCache.Store(repo, cachedToken{
		token:     token,
		expiresAt: time.Now().Add(time.Duration(expiresIn-cacheBuffer) * time.Second),
	})

	f.logger.Debug("obtained token from upstream",
		"registry", reg.config.Name,
		"repo", repo,
		"expiresIn", expiresIn,
	)

	return token, nil
}

// discoverChallenge issues an unauthenticated GET /v2/ and parses the
// WWW-Authenticate header from the expected 401 response to discover the
// token realm and service. The result is cached on the registry struct so
// the probe request is only made once on success. A failed discovery is
// not cached and will be retried on the next call.
func (f *Fetcher) discoverChallenge(ctx context.Context, reg *registry) (realm, service string, err error) {
	c := &reg.challenge
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.done {
		return c.realm, c.svc, nil
	}

	probeURL := strings.TrimRight(reg.config.Endpoint, "/") + "/v2/"
	req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if reqErr != nil {
		return "", "", fmt.Errorf("creating challenge probe request: %w", reqErr)
	}
	req.Header.Set("User-Agent", upstreamUserAgent)

	resp, doErr := f.client.Do(req)
	if doErr != nil {
		return "", "", fmt.Errorf("probing registry for auth challenge: %w", doErr)
	}
	_ = resp.Body.Close()

	www := resp.Header.Get("WWW-Authenticate")
	if www == "" {
		// Registry returned 200 without a challenge — no auth required,
		// or it's non-standard. Fall back to a /v2/token endpoint guess.
		c.realm = strings.TrimRight(reg.config.Endpoint, "/") + "/v2/token"
		c.done = true
		return c.realm, c.svc, nil
	}

	parsedRealm, parsedSvc := parseWWWAuthenticate(www)
	if parsedRealm == "" {
		// Unparseable header — fall back.
		c.realm = strings.TrimRight(reg.config.Endpoint, "/") + "/v2/token"
		c.done = true
		f.logger.Warn("could not parse WWW-Authenticate header, using fallback token URL",
			"registry", reg.config.Name,
			"header", www,
		)
		return c.realm, c.svc, nil
	}

	c.realm = parsedRealm
	c.svc = parsedSvc
	c.done = true
	f.logger.Debug("discovered auth challenge",
		"registry", reg.config.Name,
		"realm", parsedRealm,
		"service", parsedSvc,
	)

	return c.realm, c.svc, nil
}

// parseWWWAuthenticate extracts the realm and service from a Bearer
// WWW-Authenticate header value, e.g.:
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io"
func parseWWWAuthenticate(header string) (realm, service string) {
	// Strip the scheme prefix ("Bearer ").
	after, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return "", ""
	}
	// Parse key="value" pairs. Values may contain commas inside quotes so we
	// walk character-by-character rather than splitting on commas.
	for len(after) > 0 {
		after = strings.TrimLeft(after, " ,")
		eqIdx := strings.IndexByte(after, '=')
		if eqIdx < 0 {
			break
		}
		key := strings.TrimSpace(after[:eqIdx])
		after = after[eqIdx+1:]

		var value string
		if len(after) > 0 && after[0] == '"' {
			// Quoted value.
			end := strings.IndexByte(after[1:], '"')
			if end < 0 {
				break
			}
			value = after[1 : end+1]
			after = after[end+2:]
		} else {
			// Unquoted value — ends at next comma or end of string.
			end := strings.IndexByte(after, ',')
			if end < 0 {
				value = after
				after = ""
			} else {
				value = after[:end]
				after = after[end+1:]
			}
		}

		switch key {
		case "realm":
			realm = value
		case "service":
			service = value
		}
	}
	return realm, service
}

type limitedReadCloser struct {
	io.Reader
	closer io.Closer
}

func (r *limitedReadCloser) Close() error {
	return r.closer.Close()
}

// escapePathSegments escapes each segment of a slash-delimited path individually,
// preserving the slashes so that multi-component repo names (e.g. "library/nginx")
// remain valid OCI URL paths rather than being collapsed into a single encoded segment.
func escapePathSegments(path string) string {
	segments := strings.Split(path, "/")
	for i, s := range segments {
		segments[i] = url.PathEscape(s)
	}
	return strings.Join(segments, "/")
}
