package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

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

// lookupWebFinger resolves a resource via WebFinger and returns the AP actor URL.
func lookupWebFinger(ctx context.Context, domain, resource string) (string, error) {
	if strings.ContainsAny(domain, "/:@") {
		return "", fmt.Errorf("invalid domain %q: must be a bare hostname", domain)
	}

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
