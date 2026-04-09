package validate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDigest(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"valid", "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", false},
		{"missing prefix", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"wrong algo", "sha512:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", true},
		{"too short", "sha256:abcd", true},
		{"too long", "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855aa", true},
		{"uppercase", "sha256:E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", true},
		{"empty", "", true},
		{"path traversal", "sha256:../../../etc/passwd" + strings.Repeat("a", 40), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Digest(tt.input)
			if tt.wantErr {
				assert.Error(t, err, "Digest(%q)", tt.input)
			} else {
				assert.NoError(t, err, "Digest(%q)", tt.input)
			}
		})
	}
}

func TestRepoName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"simple", "myapp", false},
		{"with slash", "myorg/myapp", false},
		{"with dots", "my.app", false},
		{"with dash", "my-app", false},
		{"with underscore", "my_app", false},
		{"multi-level", "org/team/app", false},
		{"max components", "a/b/c/d/e/f/g/h", false},
		{"empty", "", true},
		{"too many components", "a/b/c/d/e/f/g/h/i", true},
		{"uppercase", "MyApp", true},
		{"empty component", "org//app", true},
		{"leading slash", "/app", true},
		{"trailing slash", "app/", true},
		{"special chars", "my@app", true},
		{"too long", strings.Repeat("a", 257), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RepoName(tt.input)
			if tt.wantErr {
				assert.Error(t, err, "RepoName(%q)", tt.input)
			} else {
				assert.NoError(t, err, "RepoName(%q)", tt.input)
			}
		})
	}
}

func TestPeerEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"https", "https://registry.example.com", false},
		{"http", "http://registry.example.com", false},
		{"with port", "https://registry.example.com:5000", false},
		{"empty", "", true},
		{"ftp scheme", "ftp://example.com", true},
		{"no scheme", "example.com", true},
		{"localhost", "http://localhost:5000", true},
		{"loopback v4", "http://127.0.0.1:5000", true},
		{"loopback v6", "http://[::1]:5000", true},
		{"zero addr", "http://0.0.0.0:5000", true},
		{"private 10.x", "http://10.0.0.1:5000", true},
		{"private 172.16.x", "http://172.16.0.1:5000", true},
		{"private 192.168.x", "http://192.168.1.1:5000", true},
		{"link-local", "http://169.254.1.1:5000", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := PeerEndpoint(tt.input)
			if tt.wantErr {
				assert.Error(t, err, "PeerEndpoint(%q)", tt.input)
			} else {
				assert.NoError(t, err, "PeerEndpoint(%q)", tt.input)
			}
		})
	}
}

func TestManifestContent(t *testing.T) {
	tests := []struct {
		name    string
		input   []byte
		wantErr bool
	}{
		{"valid", []byte(`{"schemaVersion":2}`), false},
		{"empty", []byte{}, true},
		{"nil", nil, true},
		{"too large", make([]byte, 10*1024*1024+1), true},
		{"at limit", make([]byte, 10*1024*1024), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ManifestContent(tt.input, 10*1024*1024)
			if tt.wantErr {
				assert.Error(t, err, "ManifestContent(len=%d)", len(tt.input))
			} else {
				assert.NoError(t, err, "ManifestContent(len=%d)", len(tt.input))
			}
		})
	}
}

func TestSanitizeText(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{"plain", "hello world", 20, "hello world"},
		{"truncate", "abcdef", 3, "abc"},
		{"strip null", "ab\x00cd", 10, "abcd"},
		{"strip control", "ab\x01\x1fcd", 10, "abcd"},
		{"keep tab", "a\tb", 10, "a\tb"},
		{"keep newline", "a\nb", 10, "a\nb"},
		{"keep cr", "a\rb", 10, "a\rb"},
		{"strip del", "ab\x7fcd", 10, "abcd"},
		{"empty", "", 10, ""},
		{"unicode ok", "héllo", 10, "héllo"},
		// Permitted control chars must not bypass the length guard.
		{"tab at boundary", "aa\tb", 2, "aa"},
		{"newline at boundary", "aa\nb", 2, "aa"},
		{"cr at boundary", "aa\rb", 2, "aa"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeText(tt.input, tt.maxLen)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTag(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"latest", "latest", false},
		{"semver", "v1.2.3", false},
		{"with dots", "1.0.0-rc.1", false},
		{"with underscore", "_hidden", false},
		{"empty allowed", "", false},
		{"starts with dash", "-invalid", true},
		{"starts with dot", ".invalid", true},
		{"too long", strings.Repeat("a", 129), true},
		{"max length", strings.Repeat("a", 128), false},
		{"special chars", "tag@latest", true},
		{"space", "my tag", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Tag(tt.input)
			if tt.wantErr {
				assert.Error(t, err, "Tag(%q)", tt.input)
			} else {
				assert.NoError(t, err, "Tag(%q)", tt.input)
			}
		})
	}
}
