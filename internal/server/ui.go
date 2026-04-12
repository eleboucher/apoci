package server

import (
	"cmp"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/server/ui"
)

type RepoView struct {
	Name       string
	Tags       []string
	SizeHuman  string
	UpdatedAgo string
}

type FederatedGroup struct {
	PeerDomain string
	Repos      []RepoView
}

type IndexData struct {
	Title           string
	RegistryName    string
	Endpoint        string
	TotalRepos      int
	Query           string
	LocalRepos      []RepoView
	FederatedGroups []FederatedGroup
}

func (s *Server) initUITemplates() error {
	tmplFS, err := fs.Sub(ui.TemplatesFS, "templates")
	if err != nil {
		return fmt.Errorf("getting templates sub-fs: %w", err)
	}

	s.uiTemplates, err = template.ParseFS(tmplFS, "*.tmpl")
	if err != nil {
		return fmt.Errorf("parsing UI templates: %w", err)
	}
	return nil
}

func (s *Server) handleUIIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	repos, err := s.db.ListReposWithStats(ctx, "")
	if err != nil {
		s.logger.Error("failed to list repos for UI", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.buildIndexData(repos, "")
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

	repos, err := s.db.ListReposWithStats(ctx, query)
	if err != nil {
		s.logger.Error("failed to search repos for UI", "error", err, "query", query)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.buildIndexData(repos, query)
	s.renderTemplate(w, "_repo_list.html.tmpl", data)
}

func (s *Server) handleMinimalRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"name":%q,"status":"ok"}`, s.cfg.Name)
}

func (s *Server) buildIndexData(repos []database.RepoWithStats, query string) IndexData {
	selfActor := s.identity.ActorURL
	var localRepos []RepoView
	federatedMap := make(map[string][]RepoView)

	for _, r := range repos {
		rv := RepoView{
			Name:       r.Name,
			Tags:       r.Tags,
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
		TotalRepos:      len(repos),
		Query:           query,
		LocalRepos:      localRepos,
		FederatedGroups: federatedGroups,
	}
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
