package validate

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

var (
	digestRe = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

	repoComponentRe = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
)

func Digest(d string) error {
	if !digestRe.MatchString(d) {
		return fmt.Errorf("invalid digest format: must match sha256:<64 hex chars>")
	}
	return nil
}

func RepoName(name string) error {
	if name == "" {
		return fmt.Errorf("repository name is empty")
	}
	if len(name) > 256 {
		return fmt.Errorf("repository name too long (max 256 chars)")
	}
	parts := strings.Split(name, "/")
	if len(parts) > 8 {
		return fmt.Errorf("repository name has too many path components (max 8)")
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("repository name has empty path component")
		}
		if !repoComponentRe.MatchString(part) {
			return fmt.Errorf("invalid repository name component %q: must be lowercase alphanumeric with ._- separators", part)
		}
	}
	return nil
}

func PeerEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("peer endpoint must use http or https scheme, got %q", u.Scheme)
	}

	if u.Host == "" {
		return fmt.Errorf("peer endpoint has no host")
	}

	host := u.Hostname()

	if !AllowPrivateIPs.Load() {
		if host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "0.0.0.0" {
			return fmt.Errorf("peer endpoint must not point to loopback address")
		}

		ip := net.ParseIP(host)
		if ip != nil {
			if err := checkPrivateIP(ip); err != nil {
				return err
			}
		}
	}

	return nil
}

var privateRanges []privateRange

func mustParseCIDR(network string) *net.IPNet {
	_, cidr, err := net.ParseCIDR(network)
	if err != nil {
		panic(fmt.Sprintf("validate: invalid CIDR %q: %v", network, err))
	}
	return cidr
}

func init() {
	for _, entry := range []struct {
		network string
		label   string
	}{
		{"10.0.0.0/8", "private (10.x)"},
		{"172.16.0.0/12", "private (172.16.x)"},
		{"192.168.0.0/16", "private (192.168.x)"},
		{"127.0.0.0/8", "loopback"},
		{"169.254.0.0/16", "link-local / cloud metadata"},
		{"fc00::/7", "IPv6 unique local"},
		{"fe80::/10", "IPv6 link-local"},
	} {
		privateRanges = append(privateRanges, privateRange{cidr: mustParseCIDR(entry.network), label: entry.label})
	}
}

type privateRange struct {
	cidr  *net.IPNet
	label string
}

func checkPrivateIP(ip net.IP) error {
	for _, r := range privateRanges {
		if r.cidr.Contains(ip) {
			return fmt.Errorf("peer endpoint must not point to %s address (%s)", r.label, ip)
		}
	}
	return nil
}

// AllowPrivateIPs disables DNS-rebinding and private IP checks in SafeDialContext.
// Set to true for development/testing with localhost servers.
var AllowPrivateIPs atomic.Bool

// SafeDialContext is a DialContext function that resolves DNS and rejects
// connections to private/internal IP ranges to prevent DNS rebinding attacks.
func SafeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address: %w", err)
	}

	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("DNS lookup failed for %s: %w", host, err)
	}

	if !AllowPrivateIPs.Load() {
		for _, ipAddr := range ips {
			if ipAddr.IP.IsUnspecified() {
				return nil, fmt.Errorf("connection to unspecified address blocked for host %s", host)
			}
			if err := checkPrivateIP(ipAddr.IP); err != nil {
				return nil, fmt.Errorf("DNS for %s resolved to blocked IP: %w", host, err)
			}
		}
	}

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var lastErr error
	for _, ipAddr := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ipAddr.IP.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("failed to connect to %s: %w", host, lastErr)
}

func ManifestContent(content []byte, maxSize int64) error {
	if int64(len(content)) > maxSize {
		return fmt.Errorf("manifest too large: %d bytes (max %d)", len(content), maxSize)
	}
	if len(content) == 0 {
		return fmt.Errorf("manifest content is empty")
	}
	return nil
}

var tagRe = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)

func Tag(tag string) error {
	if tag == "" {
		return nil // empty tag is valid (push by digest only)
	}
	if !tagRe.MatchString(tag) {
		return fmt.Errorf("invalid tag %q: must match [a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}", tag)
	}
	return nil
}
