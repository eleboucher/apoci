package activitypub

import (
	"encoding/json"
	"net/http"
)

type NodeInfoWellKnown struct {
	Links []NodeInfoWellKnownLink `json:"links"`
}

type NodeInfoWellKnownLink struct {
	Rel  string `json:"rel"`
	Href string `json:"href"`
}

type NodeInfo struct {
	Version           string           `json:"version"`
	Software          NodeInfoSoftware `json:"software"`
	Protocols         []string         `json:"protocols"`
	Usage             NodeInfoUsage    `json:"usage"`
	OpenRegistrations bool             `json:"openRegistrations"`
	Metadata          map[string]any   `json:"metadata"`
}

type NodeInfoSoftware struct {
	Name       string `json:"name"`
	Version    string `json:"version"`
	Repository string `json:"repository,omitempty"`
	Homepage   string `json:"homepage,omitempty"`
}

type NodeInfoUsage struct {
	Users      NodeInfoUsers `json:"users"`
	LocalPosts int           `json:"localPosts"`
}

type NodeInfoUsers struct {
	Total          int `json:"total"`
	ActiveMonth    int `json:"activeMonth"`
	ActiveHalfyear int `json:"activeHalfyear"`
}

type NodeInfoHandler struct {
	domain  string
	version string
}

func NewNodeInfoHandler(domain, version string) *NodeInfoHandler {
	return &NodeInfoHandler{
		domain:  domain,
		version: version,
	}
}

func (h *NodeInfoHandler) ServeWellKnown(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	resp := NodeInfoWellKnown{
		Links: []NodeInfoWellKnownLink{
			{
				Rel:  "http://nodeinfo.diaspora.software/ns/schema/2.1",
				Href: "https://" + h.domain + "/ap/nodeinfo/2.1",
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *NodeInfoHandler) ServeNodeInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	info := NodeInfo{
		Version: "2.1",
		Software: NodeInfoSoftware{
			Name:    "apoci",
			Version: h.version,
		},
		Protocols:         []string{"activitypub"},
		OpenRegistrations: false,
		Usage: NodeInfoUsage{
			Users: NodeInfoUsers{
				Total:          1,
				ActiveMonth:    1,
				ActiveHalfyear: 1,
			},
			LocalPosts: 0,
		},
		Metadata: map[string]any{
			"nodeDescription": "Federated OCI artifact registry",
		},
	}

	w.Header().Set("Content-Type", "application/json; profile=\"http://nodeinfo.diaspora.software/ns/schema/2.1#\"")
	_ = json.NewEncoder(w).Encode(info)
}
