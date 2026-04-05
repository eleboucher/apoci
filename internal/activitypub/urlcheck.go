package activitypub

import (
	"fmt"
	"net"
	"net/url"
)

// AllowInsecureHTTP can be set to true for development/testing to allow HTTP URLs.
var AllowInsecureHTTP = false

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
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "https" && (!AllowInsecureHTTP || u.Scheme != "http") {
		return fmt.Errorf("URL must use https, got %q", u.Scheme)
	}

	host := u.Hostname()

	if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]" {
		if !AllowInsecureHTTP {
			return fmt.Errorf("loopback addresses not allowed")
		}
		return nil
	}

	ip := net.ParseIP(host)
	if ip != nil && !AllowInsecureHTTP {
		if ip.IsPrivate() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			return fmt.Errorf("private/internal IP not allowed: %s", ip)
		}
	}

	return nil
}
