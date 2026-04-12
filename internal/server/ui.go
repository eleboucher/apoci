package server

import (
	"cmp"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/server/ui"
)

const digestDisplayLen = 19 // sha256:abc... truncated for display

type RepoView struct {
	Name       string
	Tags       []string
	TagCount   int
	FirstTag   string
	SizeHuman  string
	UpdatedAgo string
}

type TagView struct {
	Name        string
	Digest      string
	DigestShort string
	SizeHuman   string
	UpdatedAgo  string
	IsOCI       bool   // true if artifact_type is set (use oras pull)
	Platform    string // e.g., "linux/amd64" or "linux/amd64, linux/arm64"
}

type RepoTagsData struct {
	Title        string
	RegistryName string
	Endpoint     string
	RegistryHost string // Endpoint without scheme, for docker pull commands
	RepoName     string
	Tags         []TagView
	Page         int
	TotalPages   int
	TotalCount   int
	HasPrev      bool
	HasNext      bool
}

type FederatedGroup struct {
	PeerDomain string
	Repos      []RepoView
}

type IndexData struct {
	Title           string
	RegistryName    string
	Endpoint        string
	RegistryHost    string // Endpoint without scheme, for docker pull commands
	TotalRepos      int
	FollowerCount   int
	FollowingCount  int
	Query           string
	LocalRepos      []RepoView
	FederatedGroups []FederatedGroup
	Page            int
	TotalPages      int
	HasPrev         bool
	HasNext         bool
}

func (s *Server) initUITemplates() error {
	tmplFS, err := fs.Sub(ui.TemplatesFS, "templates")
	if err != nil {
		return fmt.Errorf("getting templates sub-fs: %w", err)
	}

	funcs := template.FuncMap{
		"add":      func(a, b int) int { return a + b },
		"subtract": func(a, b int) int { return a - b },
	}

	s.uiTemplates, err = template.New("").Funcs(funcs).ParseFS(tmplFS, "*.tmpl")
	if err != nil {
		return fmt.Errorf("parsing UI templates: %w", err)
	}
	return nil
}

const reposPageSize = 20

func (s *Server) handleUIIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}

	reposPage, err := s.db.ListReposWithStatsPaginated(ctx, "", page, reposPageSize)
	if err != nil {
		s.logger.Error("failed to list repos for UI", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	followerCount, err := s.db.CountFollows(ctx)
	if err != nil {
		s.logger.Error("failed to count followers for UI", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	followingCount, err := s.db.CountPeers(ctx)
	if err != nil {
		s.logger.Error("failed to count following for UI", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.buildIndexData(reposPage, "", followerCount, followingCount)
	s.renderTemplate(w, "layout.html.tmpl", data)
}

func (s *Server) handleUISearch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	// Ignore very short queries
	if len(query) > 0 && len(query) < 2 {
		w.WriteHeader(http.StatusOK)
		return
	}

	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}

	reposPage, err := s.db.ListReposWithStatsPaginated(ctx, query, page, reposPageSize)
	if err != nil {
		s.logger.Error("failed to search repos for UI", "error", err, "query", query)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.buildIndexData(reposPage, query, 0, 0)
	s.renderTemplate(w, "_repo_list.html.tmpl", data)
}

func (s *Server) handleMinimalRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"name":%q,"status":"ok"}`, s.cfg.Name)
}

func (s *Server) handleUIRepoTags(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repoName := r.PathValue("repo")

	repo, err := s.db.GetRepository(ctx, repoName)
	if err != nil {
		s.logger.Error("failed to get repository for tags UI", "error", err, "repo", repoName)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if repo == nil {
		http.NotFound(w, r)
		return
	}

	// Parse pagination params
	page := 1
	if p := r.URL.Query().Get("page"); p != "" {
		if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
			page = parsed
		}
	}
	const pageSize = 20

	tagsPage, err := s.db.ListTagsWithDetails(ctx, repo.ID, page, pageSize)
	if err != nil {
		s.logger.Error("failed to list tags for UI", "error", err, "repo", repoName)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	tagViews := make([]TagView, len(tagsPage.Tags))
	for i, t := range tagsPage.Tags {
		digestShort := t.Digest
		if len(digestShort) > digestDisplayLen {
			digestShort = digestShort[:digestDisplayLen] + "..."
		}
		tagViews[i] = TagView{
			Name:        t.Name,
			Digest:      t.Digest,
			DigestShort: digestShort,
			SizeHuman:   humanizeBytes(t.SizeBytes),
			UpdatedAgo:  humanizeTime(t.UpdatedAt),
			IsOCI:       t.ArtifactType != nil,
			Platform:    extractPlatforms(t.ManifestContent, t.MediaType),
		}
	}

	data := RepoTagsData{
		Title:        repoName + " - Tags",
		RegistryName: s.cfg.Name,
		Endpoint:     s.cfg.Endpoint,
		RegistryHost: stripScheme(s.cfg.Endpoint),
		RepoName:     repoName,
		Tags:         tagViews,
		Page:         tagsPage.Page,
		TotalPages:   tagsPage.TotalPages,
		TotalCount:   tagsPage.TotalCount,
		HasPrev:      tagsPage.Page > 1,
		HasNext:      tagsPage.Page < tagsPage.TotalPages,
	}
	s.renderTemplate(w, "repo_tags.html.tmpl", data)
}

func (s *Server) buildIndexData(reposPage *database.ReposPage, query string, followerCount, followingCount int) IndexData {
	selfActor := s.identity.ActorURL
	var localRepos []RepoView
	federatedMap := make(map[string][]RepoView)

	for _, r := range reposPage.Repos {
		var firstTag string
		if len(r.Tags) > 0 {
			firstTag = r.Tags[0]
		}
		rv := RepoView{
			Name:       r.Name,
			Tags:       r.Tags,
			TagCount:   len(r.Tags),
			FirstTag:   firstTag,
			SizeHuman:  humanizeBytes(r.SizeBytes),
			UpdatedAgo: humanizeTime(r.UpdatedAt),
		}

		if r.OwnerID == selfActor {
			localRepos = append(localRepos, rv)
		} else {
			// Extract peer domain from owner ID (actor URL)
			peerDomain := extractDomain(r.OwnerID)
			federatedMap[peerDomain] = append(federatedMap[peerDomain], rv)
		}
	}

	// Convert map to sorted slice
	federatedGroups := make([]FederatedGroup, 0, len(federatedMap))
	for domain, domainRepos := range federatedMap {
		federatedGroups = append(federatedGroups, FederatedGroup{
			PeerDomain: domain,
			Repos:      domainRepos,
		})
	}
	slices.SortFunc(federatedGroups, func(a, b FederatedGroup) int {
		return cmp.Compare(a.PeerDomain, b.PeerDomain)
	})

	return IndexData{
		Title:           s.cfg.Name + " - Image Browser",
		RegistryName:    s.cfg.Name,
		Endpoint:        s.cfg.Endpoint,
		RegistryHost:    stripScheme(s.cfg.Endpoint),
		TotalRepos:      reposPage.TotalCount,
		FollowerCount:   followerCount,
		FollowingCount:  followingCount,
		Query:           query,
		LocalRepos:      localRepos,
		FederatedGroups: federatedGroups,
		Page:            reposPage.Page,
		TotalPages:      reposPage.TotalPages,
		HasPrev:         reposPage.Page > 1,
		HasNext:         reposPage.Page < reposPage.TotalPages,
	}
}

func stripScheme(endpoint string) string {
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	return endpoint
}

func extractPlatforms(content []byte, mediaType string) string {
	if len(content) == 0 {
		return ""
	}

	if !strings.Contains(mediaType, "index") && !strings.Contains(mediaType, "manifest.list") {
		return ""
	}

	var index struct {
		Manifests []struct {
			Platform *struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
				Variant      string `json:"variant,omitempty"`
			} `json:"platform,omitempty"`
		} `json:"manifests"`
	}

	if err := json.Unmarshal(content, &index); err != nil {
		return ""
	}

	var platforms []string
	seen := make(map[string]bool)
	for _, m := range index.Manifests {
		if m.Platform == nil {
			continue
		}
		p := m.Platform.OS + "/" + m.Platform.Architecture
		if m.Platform.Variant != "" {
			p += "/" + m.Platform.Variant
		}
		if !seen[p] {
			seen[p] = true
			platforms = append(platforms, p)
		}
	}

	return strings.Join(platforms, ", ")
}

func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.uiTemplates.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("failed to render template", "template", name, "error", err)
	}
}

func humanizeBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

func humanizeTime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2, 2006")
	}
}

func extractDomain(actorURL string) string {
	// Actor URL format: https://domain.com/ap/actor
	actorURL = strings.TrimPrefix(actorURL, "https://")
	actorURL = strings.TrimPrefix(actorURL, "http://")
	if idx := strings.Index(actorURL, "/"); idx > 0 {
		return actorURL[:idx]
	}
	return actorURL
}
