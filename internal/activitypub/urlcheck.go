package activitypub

import (
	"fmt"
	"net"
	"net/url"
	"sync/atomic"
)

// allowInsecureHTTP controls whether HTTP (non-TLS) federation URLs are permitted.
// Use SetAllowInsecureHTTP to mutate it safely; intended only for testing/development.
var allowInsecureHTTP atomic.Bool

// SetAllowInsecureHTTP enables or disables insecure HTTP federation URLs.
func SetAllowInsecureHTTP(v bool) {
	allowInsecureHTTP.Store(v)
}

// EndpointFromActorURL extracts the scheme+host base URL from an actor URL.
// It returns the original string unchanged when parsing fails.
func EndpointFromActorURL(actorURL string) string {
	u, err := url.Parse(actorURL)
	if err != nil {
		return actorURL
	}
	return u.Scheme + "://" + u.Host
}

func validateFederationURL(rawURL string) error {
	insecure := allowInsecureHTTP.Load()

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "https" && (!insecure || u.Scheme != "http") {
		return fmt.Errorf("URL must use https, got %q", u.Scheme)
	}

	host := u.Hostname()

	// L-1: Use net.ParseIP for robust loopback/private detection instead of string matching.
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			if !insecure {
				return fmt.Errorf("loopback addresses not allowed")
			}
			return nil
		}
		if !insecure && (ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()) {
			return fmt.Errorf("private/internal IP not allowed: %s", ip)
		}
		return nil
	}

	// hostname-based loopback check
	if host == "localhost" && !insecure {
		return fmt.Errorf("loopback addresses not allowed")
	}

	return nil
}
