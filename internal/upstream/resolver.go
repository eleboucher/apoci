package upstream

import "strings"

// ParseUpstreamRepo extracts the upstream registry and repo path from a namespaced path.
// Example: "docker.io/library/nginx" -> ("docker.io", "library/nginx", true)
// Example: "myrepo/myimage" -> ("", "", false) - not an upstream path
func ParseUpstreamRepo(repo string) (registry, path string, ok bool) {
	parts := strings.SplitN(repo, "/", 2)
	if len(parts) < 2 {
		return "", "", false
	}
	// Check if first part looks like a registry (contains a dot)
	if !strings.Contains(parts[0], ".") {
		return "", "", false
	}
	return parts[0], parts[1], true
}
