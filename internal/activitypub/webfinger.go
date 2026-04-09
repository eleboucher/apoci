package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"golang.org/x/sync/singleflight"
)

const webfingerCacheTTL = 15 * time.Minute

// WebFingerClient handles WebFinger lookups with caching and deduplication.
// Call Stop() when done to release the background goroutine.
type WebFingerClient struct {
	cache *ttlcache.Cache[string, string]
	group singleflight.Group
}

func NewWebFingerClient() *WebFingerClient {
	cache := ttlcache.New[string, string](
		ttlcache.WithTTL[string, string](webfingerCacheTTL),
		ttlcache.WithCapacity[string, string](1000),
	)
	go cache.Start()
	return &WebFingerClient{cache: cache}
}

func (c *WebFingerClient) Stop() {
	c.cache.Stop()
}

var (
	defaultWebFingerClient     *WebFingerClient
	defaultWebFingerClientOnce sync.Once
)

func getDefaultWebFingerClient() *WebFingerClient {
	defaultWebFingerClientOnce.Do(func() {
		defaultWebFingerClient = NewWebFingerClient()
	})
	return defaultWebFingerClient
}

func StopDefaultWebFingerClient() {
	if defaultWebFingerClient != nil {
		defaultWebFingerClient.Stop()
	}
}

type WebFingerResponse struct {
	Subject string          `json:"subject"`
	Aliases []string        `json:"aliases,omitempty"`
	Links   []WebFingerLink `json:"links"`
}

type WebFingerLink struct {
	Rel  string `json:"rel"`
	Type string `json:"type,omitempty"`
	Href string `json:"href,omitempty"`
}

// Lookup resolves a resource via WebFinger and returns the AP actor URL.
// Results are cached for webfingerCacheTTL to avoid repeated lookups.
func (c *WebFingerClient) Lookup(_ context.Context, domain, resource string) (string, error) {
	if strings.ContainsAny(domain, "/:@") {
		return "", fmt.Errorf("invalid domain %q: must be a bare hostname", domain)
	}

	if item := c.cache.Get(resource); item != nil {
		return item.Value(), nil
	}

	v, err, _ := c.group.Do(resource, func() (any, error) {
		if item := c.cache.Get(resource); item != nil {
			return item.Value(), nil
		}
		// Use a fresh context so a cancellation by the first caller does not
		// abort concurrent callers that are still waiting on the same key.
		fetchCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		actorURL, err := fetchWebFinger(fetchCtx, domain, resource)
		if err != nil {
			return "", err
		}
		c.cache.Set(resource, actorURL, ttlcache.DefaultTTL)
		return actorURL, nil
	})
	if err != nil {
		return "", err
	}

	return v.(string), nil
}

// lookupWebFinger is a package-level convenience function using the default client.
func lookupWebFinger(ctx context.Context, domain, resource string) (string, error) {
	return getDefaultWebFingerClient().Lookup(ctx, domain, resource)
}

func fetchWebFinger(ctx context.Context, domain, resource string) (string, error) {
	wfURL := fmt.Sprintf("https://%s/.well-known/webfinger?resource=%s", domain, url.QueryEscape(resource))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wfURL, nil)
	if err != nil {
		return "", fmt.Errorf("creating webfinger request: %w", err)
	}
	req.Header.Set("Accept", "application/jrd+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("webfinger request to %s: %w", domain, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("webfinger on %s returned %d", domain, resp.StatusCode)
	}

	var wf WebFingerResponse
	if err := json.NewDecoder(resp.Body).Decode(&wf); err != nil {
		return "", fmt.Errorf("decoding webfinger response: %w", err)
	}

	for _, link := range wf.Links {
		if link.Rel == "self" && link.Type == "application/activity+json" && link.Href != "" {
			return link.Href, nil
		}
	}

	return "", fmt.Errorf("no ActivityPub actor link in webfinger response from %s", domain)
}

// ResolveFollowTarget resolves a domain, handle, or actor URL to an actor URL.
func ResolveFollowTarget(ctx context.Context, input string) (string, error) {
	if strings.HasPrefix(input, "https://") || strings.HasPrefix(input, "http://") {
		if err := validateFederationURL(input); err != nil {
			return "", fmt.Errorf("unsafe target URL: %w", err)
		}
		return input, nil
	}

	input = strings.TrimPrefix(input, "@")
	if user, domain, ok := strings.Cut(input, "@"); ok {
		return lookupWebFinger(ctx, domain, fmt.Sprintf("acct:%s@%s", user, domain))
	}

	return lookupWebFinger(ctx, input, fmt.Sprintf("acct:registry@%s", input))
}

type WebFingerHandler struct {
	identity *Identity
}

func NewWebFingerHandler(identity *Identity) *WebFingerHandler {
	return &WebFingerHandler{identity: identity}
}

func (h *WebFingerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resource := r.URL.Query().Get("resource")
	if resource == "" {
		http.Error(w, "missing resource parameter", http.StatusBadRequest)
		return
	}

	valid := false
	if resource == h.identity.ActorURL {
		valid = true
	} else if after, ok := strings.CutPrefix(resource, "acct:"); ok {
		user, domain, hasDomain := strings.Cut(after, "@")
		valid = hasDomain && user == "registry" &&
			(domain == h.identity.Domain || domain == h.identity.AccountDomain)
	}

	if !valid {
		http.Error(w, "resource not found", http.StatusNotFound)
		return
	}

	resp := WebFingerResponse{
		Subject: resource,
		Aliases: []string{h.identity.ActorURL},
		Links: []WebFingerLink{
			{
				Rel:  "self",
				Type: "application/activity+json",
				Href: h.identity.ActorURL,
			},
		},
	}

	w.Header().Set("Content-Type", "application/jrd+json")
	_ = json.NewEncoder(w).Encode(resp)
}
