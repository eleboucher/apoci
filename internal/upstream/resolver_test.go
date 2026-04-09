package upstream

import "testing"

func TestParseUpstreamRepo(t *testing.T) {
	tests := []struct {
		name         string
		repo         string
		wantRegistry string
		wantPath     string
		wantOK       bool
	}{
		{
			name:         "docker hub library image",
			repo:         "docker.io/library/nginx",
			wantRegistry: "docker.io",
			wantPath:     "library/nginx",
			wantOK:       true,
		},
		{
			name:         "docker hub user image",
			repo:         "docker.io/myuser/myimage",
			wantRegistry: "docker.io",
			wantPath:     "myuser/myimage",
			wantOK:       true,
		},
		{
			name:         "ghcr.io image",
			repo:         "ghcr.io/owner/repo",
			wantRegistry: "ghcr.io",
			wantPath:     "owner/repo",
			wantOK:       true,
		},
		{
			name:         "quay.io image",
			repo:         "quay.io/org/image",
			wantRegistry: "quay.io",
			wantPath:     "org/image",
			wantOK:       true,
		},
		{
			name:         "deeply nested path",
			repo:         "gcr.io/project/sub/path/image",
			wantRegistry: "gcr.io",
			wantPath:     "project/sub/path/image",
			wantOK:       true,
		},
		{
			name:         "custom registry with port",
			repo:         "registry.example.com:5000/myimage",
			wantRegistry: "registry.example.com:5000",
			wantPath:     "myimage",
			wantOK:       true,
		},
		{
			name:         "local repo without domain",
			repo:         "myrepo/myimage",
			wantRegistry: "",
			wantPath:     "",
			wantOK:       false,
		},
		{
			name:         "bare image name",
			repo:         "nginx",
			wantRegistry: "",
			wantPath:     "",
			wantOK:       false,
		},
		{
			name:         "localhost without dot",
			repo:         "localhost/myimage",
			wantRegistry: "",
			wantPath:     "",
			wantOK:       false,
		},
		{
			name:         "localhost with port",
			repo:         "localhost:5000/myimage",
			wantRegistry: "",
			wantPath:     "",
			wantOK:       false,
		},
		{
			name:         "empty string",
			repo:         "",
			wantRegistry: "",
			wantPath:     "",
			wantOK:       false,
		},
		{
			name:         "only slash",
			repo:         "/",
			wantRegistry: "",
			wantPath:     "",
			wantOK:       false,
		},
		{
			name:         "registry only no path",
			repo:         "docker.io",
			wantRegistry: "",
			wantPath:     "",
			wantOK:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRegistry, gotPath, gotOK := ParseUpstreamRepo(tt.repo)
			if gotRegistry != tt.wantRegistry {
				t.Errorf("registry = %q, want %q", gotRegistry, tt.wantRegistry)
			}
			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
			if gotOK != tt.wantOK {
				t.Errorf("ok = %v, want %v", gotOK, tt.wantOK)
			}
		})
	}
}
