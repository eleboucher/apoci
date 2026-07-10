package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/admin"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

const (
	adminMaxBody   int64 = 4 * 1024 // 4 KB
	keyBlobsPurged       = "blobsPurged"
)

func (s *Server) adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(bearerAuthMiddleware(s.cfg.AdminToken))

	r.Get("/identity", s.adminGetIdentity)
	r.Get("/images", s.adminListImages)
	r.Get("/actors", s.adminListActors)
	r.Get("/follows", s.adminListFollows)
	r.Get("/follows/pending", s.adminListPending)
	r.Get("/follows/outgoing", s.adminListOutgoingFollows)
	r.Post("/follows", s.adminAddFollow)
	r.Post("/follows/accept", s.adminAcceptFollow)
	r.Post("/follows/reject", s.adminRejectFollow)
	r.Delete("/follows", s.adminRemoveFollow)
	r.Patch("/follows", s.adminUpdateFollowFilter)
	r.Delete("/mirrors/*", s.adminEvictMirror)
	r.Delete("/repos/*", s.adminDeleteRepo)
	r.Get("/uploads", s.adminListUploads)
	r.Delete("/uploads", s.adminPurgeUploads)
	r.Get("/gc", s.adminGCStatus)
	r.Post("/gc", s.adminRunGC)
	r.Get("/peers/blocked", s.adminListBlocked)
	r.Post("/peers/pause", s.adminPausePeer)
	r.Post("/peers/resume", s.adminResumePeer)
	r.Get("/replication", s.adminReplicationStatus)

	return r
}

// adminGetIdentity godoc
// @Summary  Get node identity
// @Tags     identity
// @Produce  json
// @Success  200  {object}  admin.IdentityResponse
// @Failure  500  {string}  string  "internal error"
// @Security Bearer
// @Router   /identity [get]
func (s *Server) adminGetIdentity(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /identity")
	pubPEM, err := s.identity.PublicKeyPEM()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, admin.IdentityResponse{
		Name:          s.cfg.Name,
		ActorURL:      s.identity.ActorURL,
		KeyID:         s.identity.KeyID(),
		Domain:        s.identity.Domain,
		AccountDomain: s.identity.AccountDomain,
		Endpoint:      s.cfg.Endpoint,
		PublicKey:     pubPEM,
	})
}

// adminListImages godoc
// @Summary  List locally hosted images
// @Tags     images
// @Produce  json
// @Success  200  {array}   admin.ImageEntry
// @Failure  500  {string}  string  "internal error"
// @Security Bearer
// @Router   /images [get]
func (s *Server) adminListImages(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /images")
	repos, err := s.db.ListLocallyHostedRepos(r.Context())
	if err != nil {
		s.logger.Error("listing locally hosted repos", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	entries := make([]admin.ImageEntry, len(repos))
	for i, repo := range repos {
		entries[i] = admin.ImageEntry{
			Name:      repo.Name,
			Tags:      repo.Tags,
			SizeBytes: repo.SizeBytes,
			UpdatedAt: repo.UpdatedAt,
		}
	}
	s.logger.Debug("admin: GET /images done", "count", len(entries))
	writeJSON(w, entries)
}

// adminListActors godoc
// @Summary  List known actors
// @Tags     actors
// @Produce  json
// @Success  200  {array}   database.Actor
// @Failure  500  {string}  string  "internal error"
// @Security Bearer
// @Router   /actors [get]
func (s *Server) adminListActors(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /actors")
	actors, err := s.db.ListActors(r.Context())
	if err != nil {
		s.logger.Error("listing actors", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /actors done", "count", len(actors))
	writeJSON(w, actors)
}

// adminListFollows godoc
// @Summary  List followers
// @Tags     follows
// @Produce  json
// @Success  200  {array}   database.Actor
// @Failure  500  {string}  string  "internal error"
// @Security Bearer
// @Router   /follows [get]
func (s *Server) adminListFollows(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /follows")
	follows, err := s.db.ListFollows(r.Context())
	if err != nil {
		s.logger.Error("listing follows", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /follows done", "count", len(follows))
	writeJSON(w, follows)
}

// adminListPending godoc
// @Summary  List pending follow requests
// @Tags     follows
// @Produce  json
// @Success  200  {array}   database.FollowRequest
// @Failure  500  {string}  string  "internal error"
// @Security Bearer
// @Router   /follows/pending [get]
func (s *Server) adminListPending(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /follows/pending")
	requests, err := s.db.ListFollowRequests(r.Context())
	if err != nil {
		s.logger.Error("listing pending requests", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /follows/pending done", "count", len(requests))
	writeJSON(w, requests)
}

// adminListOutgoingFollows godoc
// @Summary  List outgoing follows
// @Tags     follows
// @Produce  json
// @Param    status  query     string  false  "Filter by status"  Enums(pending, accepted, rejected)
// @Success  200     {array}   database.Actor
// @Failure  500     {string}  string  "internal error"
// @Security Bearer
// @Router   /follows/outgoing [get]
func (s *Server) adminListOutgoingFollows(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	s.logger.Debug("admin: GET /follows/outgoing", "status", status)
	var follows []database.Actor
	var err error

	if status != "" {
		follows, err = s.db.ListOutgoingFollows(r.Context(), status)
	} else {
		follows, err = s.db.ListAllOutgoingFollows(r.Context())
	}
	if err != nil {
		s.logger.Error("listing outgoing follows", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: GET /follows/outgoing done", "count", len(follows))
	writeJSON(w, follows)
}

type adminFollowRequest struct {
	Target string `json:"target"`
	Force  bool   `json:"force,omitempty"`
}

// decodeTarget reads the follow request body and returns the raw target string.
func decodeTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return "", false
	}
	return req.Target, true
}

// adminAddFollow godoc
// @Summary  Follow a peer
// @Tags     follows
// @Accept   json
// @Produce  json
// @Param    request  body      adminFollowRequest  true  "Target to follow"
// @Success  200      {object}  map[string]string   "followed"
// @Failure  400      {string}  string              "missing target"
// @Failure  502      {string}  string              "could not add follow"
// @Security Bearer
// @Router   /follows [post]
func (s *Server) adminAddFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /follows", "target", target)

	result, err := s.fedSvc.AddFollow(r.Context(), target)
	if err != nil {
		s.logger.Error("adding follow", "target", target, "error", err)
		http.Error(w, "could not add follow", classifyError(err))
		return
	}
	s.logger.Debug("admin: POST /follows done", "target", target, "actorID", result.ActorID)

	writeJSON(w, map[string]string{"followed": result.ActorID})
}

// adminAcceptFollow godoc
// @Summary  Accept a follow request
// @Tags     follows
// @Accept   json
// @Produce  json
// @Param    request  body      adminFollowRequest  true  "Target to accept"
// @Success  200      {object}  map[string]string   "accepted, optional followed_back"
// @Failure  400      {string}  string              "missing target"
// @Failure  500      {string}  string              "internal error"
// @Security Bearer
// @Router   /follows/accept [post]
func (s *Server) adminAcceptFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /follows/accept", "target", target)

	result, err := s.fedSvc.AcceptFollow(r.Context(), target, s.cfg.Federation.AutoAccept)
	if err != nil {
		s.logger.Error("accepting follow", "target", target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: POST /follows/accept done", "actorURL", result.ActorURL, "followedBack", result.FollowedBack)

	resp := map[string]string{"accepted": result.ActorURL}
	if result.FollowedBack {
		resp["followed_back"] = result.ActorURL
	}
	writeJSON(w, resp)
}

// adminRejectFollow godoc
// @Summary  Reject a follow request
// @Tags     follows
// @Accept   json
// @Produce  json
// @Param    request  body      adminFollowRequest  true  "Target to reject"
// @Success  200      {object}  map[string]string   "rejected"
// @Failure  400      {string}  string              "missing target"
// @Failure  500      {string}  string              "internal error"
// @Security Bearer
// @Router   /follows/reject [post]
func (s *Server) adminRejectFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /follows/reject", "target", target)

	actorURL, err := s.fedSvc.RejectFollow(r.Context(), target)
	if err != nil {
		s.logger.Error("rejecting follow", "target", target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: POST /follows/reject done", "actorURL", actorURL)

	writeJSON(w, map[string]string{"rejected": actorURL})
}

// adminRemoveFollow godoc
// @Summary  Unfollow a peer
// @Description  Removes a follow relationship; set force to purge local records even if the peer is unreachable.
// @Tags     follows
// @Accept   json
// @Produce  json
// @Param    request  body      adminFollowRequest  true  "Target to unfollow"
// @Success  200      {object}  map[string]string   "removed"
// @Failure  400      {string}  string              "missing target"
// @Failure  500      {string}  string              "internal error"
// @Security Bearer
// @Router   /follows [delete]
func (s *Server) adminRemoveFollow(w http.ResponseWriter, r *http.Request) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}
	s.logger.Debug("admin: DELETE /follows", "target", req.Target, "force", req.Force)

	actorURL, err := s.fedSvc.RemoveFollow(r.Context(), req.Target, req.Force)
	if err != nil {
		s.logger.Error("removing follow", "target", req.Target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.logger.Debug("admin: DELETE /follows done", "actorURL", actorURL)

	writeJSON(w, map[string]string{"removed": actorURL})
}

type adminFollowFilterRequest struct {
	Target   string   `json:"target"`
	TagGlobs []string `json:"tag_globs"`
}

// adminUpdateFollowFilter godoc
// @Summary  Set follower tag filter
// @Description  Sets the tag-glob filter controlling which tags are delivered to an inbound follower.
// @Tags     follows
// @Accept   json
// @Produce  json
// @Param    request  body      adminFollowFilterRequest  true  "Target and tag globs"
// @Success  200      {object}  map[string]interface{}    "updated, tag_globs"
// @Failure  400      {string}  string                    "missing target or invalid glob"
// @Failure  404      {string}  string                    "follower not found"
// @Failure  500      {string}  string                    "internal error"
// @Security Bearer
// @Router   /follows [patch]
func (s *Server) adminUpdateFollowFilter(w http.ResponseWriter, r *http.Request) {
	var req adminFollowFilterRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}
	s.logger.Debug("admin: PATCH /follows", "target", req.Target, "tag_globs", req.TagGlobs)

	if err := s.db.UpdateFollowFilter(r.Context(), req.Target, req.TagGlobs); err != nil {
		s.logger.Error("updating follow filter", "target", req.Target, "error", err)
		switch {
		case errors.Is(err, database.ErrInvalidGlob):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case errors.Is(err, database.ErrFollowerNotFound):
			http.Error(w, err.Error(), http.StatusNotFound)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	writeJSON(w, map[string]any{"updated": req.Target, "tag_globs": req.TagGlobs})
}

// adminEvictMirror godoc
// @Summary  Evict a mirrored repository
// @Description  Drops a locally-mirrored upstream repository (or a single manifest by digest). Does not affect the upstream.
// @Tags     mirrors
// @Produce  json
// @Param    repo    path      string                  true   "Repository name"
// @Param    digest  query     string                  false  "Evict only this manifest digest (sha256:...)"
// @Success  200     {object}  map[string]interface{}  "evicted, blobsPurged"
// @Failure  400     {string}  string                  "invalid request or locally-owned repository"
// @Failure  404     {string}  string                  "repository not found"
// @Failure  500     {string}  string                  "internal error"
// @Security Bearer
// @Router   /mirrors/{repo} [delete]
func (s *Server) adminEvictMirror(w http.ResponseWriter, r *http.Request) {
	repo := chi.URLParam(r, "*")
	if repo == "" {
		http.Error(w, "missing repository", http.StatusBadRequest)
		return
	}
	if err := validate.RepoName(repo); err != nil {
		http.Error(w, "invalid repository name", http.StatusBadRequest)
		return
	}
	digest := r.URL.Query().Get("digest")
	if digest != "" {
		if err := validate.Digest(digest); err != nil {
			http.Error(w, "invalid digest", http.StatusBadRequest)
			return
		}
	}
	s.logger.Debug("admin: DELETE /mirrors", "repo", repo, "digest", digest)

	repoObj, err := s.db.GetRepository(r.Context(), repo)
	if err != nil {
		s.logger.Error("looking up repo for eviction", "repo", repo, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if repoObj == nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	if repoObj.OwnerID == s.identity.ActorURL {
		http.Error(w, "repository is locally owned; use the /v2/ delete API to remove it", http.StatusBadRequest)
		return
	}

	if digest != "" {
		purged, err := s.db.DeletePackageVersionWithBlobs(r.Context(), repoObj.ID, digest)
		if err != nil {
			s.logger.Error("evicting mirror manifest", "repo", repo, "digest", digest, "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, d := range purged {
			if err := s.blobs.Delete(r.Context(), d); err != nil {
				s.logger.Warn("evict: failed to delete blob bytes", "digest", d, "error", err)
			}
		}
		writeJSON(w, map[string]any{"evicted": repo, "digest": digest, keyBlobsPurged: len(purged)})
		return
	}

	purged, err := s.db.DeletePackageWithBlobs(r.Context(), repoObj.ID)
	if err != nil {
		s.logger.Error("evicting mirror repository", "repo", repo, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	for _, d := range purged {
		if err := s.blobs.Delete(r.Context(), d); err != nil {
			s.logger.Warn("evict: failed to delete blob bytes", "digest", d, "error", err)
		}
	}
	writeJSON(w, map[string]any{"evicted": repo, keyBlobsPurged: len(purged)})
}

// adminDeleteRepo godoc
// @Summary  Delete a locally-owned repository
// @Description  Removes a locally-owned repository in one call: manifests, tags, and its catalog entry, with unreferenced blobs purged. Tombstones are recorded so federation peers cannot resurrect the content, and deletes are published to followers. Refuses while an upload session is open (409). For mirrored repositories use /mirrors instead.
// @Tags     repos
// @Produce  json
// @Param    repo  path      string                  true  "Repository name"
// @Success  200   {object}  map[string]interface{}  "deleted, manifests, tags, blobsPurged"
// @Failure  400   {string}  string                  "invalid request or not locally owned"
// @Failure  404   {string}  string                  "repository not found"
// @Failure  409   {string}  string                  "repository has open upload sessions"
// @Failure  500   {string}  string                  "internal error"
// @Security Bearer
// @Router   /repos/{repo} [delete]
func (s *Server) adminDeleteRepo(w http.ResponseWriter, r *http.Request) {
	repo := chi.URLParam(r, "*")
	if repo == "" {
		http.Error(w, "missing repository", http.StatusBadRequest)
		return
	}
	if err := validate.RepoName(repo); err != nil {
		http.Error(w, "invalid repository name", http.StatusBadRequest)
		return
	}
	s.logger.Debug("admin: DELETE /repos", "repo", repo)

	repoObj, err := s.db.GetRepository(r.Context(), repo)
	if err != nil {
		s.logger.Error("looking up repo for delete", "repo", repo, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if repoObj == nil {
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}
	if repoObj.OwnerID != s.identity.ActorURL {
		http.Error(w, "repository is not locally owned; use /api/admin/mirrors to evict a mirror", http.StatusBadRequest)
		return
	}

	res, err := s.db.DeleteOwnedRepositoryWithBlobs(r.Context(), repoObj.ID, s.identity.ActorURL)
	switch {
	case errors.Is(err, database.ErrRepositoryBusy):
		http.Error(w, "repository has open upload sessions; retry after they finish or expire", http.StatusConflict)
		return
	case errors.Is(err, database.ErrPackageOwnerMismatch):
		http.Error(w, "repository is not locally owned; use /api/admin/mirrors to evict a mirror", http.StatusBadRequest)
		return
	case err != nil:
		s.logger.Error("deleting repository", "repo", repo, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	case res == nil:
		http.Error(w, "repository not found", http.StatusNotFound)
		return
	}

	for _, d := range res.PurgedBlobs {
		if err := s.blobs.Delete(r.Context(), d); err != nil {
			s.logger.Warn("repo delete: failed to delete blob bytes", "digest", d, "error", err)
		}
	}
	// Best-effort, post-commit — same stance as the v2 per-manifest delete.
	if s.publisher != nil {
		if err := s.publisher.WithdrawRepo(r.Context(), res.Name, res.TagNames, res.ManifestDigests); err != nil {
			s.logger.Warn("repo delete: failed to publish deletes to federation", "repo", res.Name, "error", err)
		}
	}

	s.logger.Info("repository deleted", "repo", repo,
		"manifests", len(res.ManifestDigests), "tags", len(res.TagNames), keyBlobsPurged, len(res.PurgedBlobs))
	writeJSON(w, map[string]any{
		"deleted":      repo,
		"manifests":    len(res.ManifestDigests),
		"tags":         len(res.TagNames),
		keyBlobsPurged: len(res.PurgedBlobs),
	})
}

type adminPeerBlockRequest struct {
	Domain string `json:"domain,omitempty"`
	Actor  string `json:"actor,omitempty"`
}

func decodePeerBlock(w http.ResponseWriter, r *http.Request) (adminPeerBlockRequest, bool) {
	var req adminPeerBlockRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Domain == "" && req.Actor == "") {
		http.Error(w, "missing domain or actor", http.StatusBadRequest)
		return adminPeerBlockRequest{}, false
	}
	// Normalize the domain to match how isBlocked compares against url.Hostname()
	// (lowercased, bare host); otherwise a pause silently no-ops. Actor URLs are
	// matched exactly, so they are only trimmed.
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	req.Actor = strings.TrimSpace(req.Actor)
	if req.Domain != "" && (strings.ContainsAny(req.Domain, "/:")) {
		http.Error(w, "domain must be a bare hostname (no scheme or path)", http.StatusBadRequest)
		return adminPeerBlockRequest{}, false
	}
	return req, true
}

// adminPausePeer godoc
// @Summary  Pause a peer
// @Description  Blocks inbound federation from a domain or actor without a restart. The block is in-memory and does not survive a config reload.
// @Tags     peers
// @Accept   json
// @Produce  json
// @Param    request  body      adminPeerBlockRequest   true  "Domain or actor to block"
// @Success  200      {object}  map[string]interface{}  "paused"
// @Failure  400      {string}  string                  "missing or invalid domain/actor"
// @Security Bearer
// @Router   /peers/pause [post]
func (s *Server) adminPausePeer(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePeerBlock(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /peers/pause", "domain", req.Domain, "actor", req.Actor)
	if req.Domain != "" {
		s.inboxHandler.PauseDomain(req.Domain)
	}
	if req.Actor != "" {
		s.inboxHandler.PauseActor(req.Actor)
	}
	writeJSON(w, map[string]any{"paused": req})
}

// adminResumePeer godoc
// @Summary  Resume a peer
// @Tags     peers
// @Accept   json
// @Produce  json
// @Param    request  body      adminPeerBlockRequest   true  "Domain or actor to unblock"
// @Success  200      {object}  map[string]interface{}  "resumed"
// @Failure  400      {string}  string                  "missing or invalid domain/actor"
// @Security Bearer
// @Router   /peers/resume [post]
func (s *Server) adminResumePeer(w http.ResponseWriter, r *http.Request) {
	req, ok := decodePeerBlock(w, r)
	if !ok {
		return
	}
	s.logger.Debug("admin: POST /peers/resume", "domain", req.Domain, "actor", req.Actor)
	if req.Domain != "" {
		s.inboxHandler.ResumeDomain(req.Domain)
	}
	if req.Actor != "" {
		s.inboxHandler.ResumeActor(req.Actor)
	}
	writeJSON(w, map[string]any{"resumed": req})
}

// adminListBlocked godoc
// @Summary  List blocked peers
// @Tags     peers
// @Produce  json
// @Success  200  {object}  map[string][]string  "domains, actors"
// @Security Bearer
// @Router   /peers/blocked [get]
func (s *Server) adminListBlocked(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /peers/blocked")
	writeJSON(w, map[string][]string{
		"domains": s.inboxHandler.BlockedDomains(),
		"actors":  s.inboxHandler.BlockedActors(),
	})
}

// adminReplicationStatus godoc
// @Summary  Replication status
// @Tags     replication
// @Produce  json
// @Success  200  {object}  map[string]interface{}  "enabled, targets"
// @Security Bearer
// @Router   /replication [get]
func (s *Server) adminReplicationStatus(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /replication")
	if s.replication == nil {
		writeJSON(w, map[string]any{"enabled": false, "targets": []any{}})
		return
	}
	writeJSON(w, map[string]any{"enabled": true, "targets": s.replication.Status()})
}

// adminGCStatus godoc
// @Summary  Garbage collector status
// @Tags     gc
// @Produce  json
// @Success  200  {object}  GCStatusResponse
// @Security Bearer
// @Router   /gc [get]
func (s *Server) adminGCStatus(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /gc")
	writeJSON(w, s.gc.Status())
}

// adminRunGC godoc
// @Summary  Run garbage collection
// @Description  Runs a single garbage-collection cycle synchronously.
// @Tags     gc
// @Produce  json
// @Success  200  {object}  map[string]string  "status"
// @Security Bearer
// @Router   /gc [post]
func (s *Server) adminRunGC(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: POST /gc")
	s.gc.RunOnce(r.Context())
	writeJSON(w, map[string]string{"status": "ok"})
}

// adminListUploads godoc
// @Summary  List in-progress upload sessions
// @Tags     uploads
// @Produce  json
// @Success  200  {array}   admin.UploadSessionEntry
// @Failure  500  {string}  string  "internal error"
// @Security Bearer
// @Router   /uploads [get]
func (s *Server) adminListUploads(w http.ResponseWriter, r *http.Request) {
	s.logger.Debug("admin: GET /uploads")
	sessions, err := s.db.ListUploadSessions(r.Context())
	if err != nil {
		s.logger.Error("listing upload sessions", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	out := make([]admin.UploadSessionEntry, len(sessions))
	for i, sess := range sessions {
		out[i] = admin.UploadSessionEntry{
			UUID:          sess.UUID,
			RepositoryID:  sess.RepositoryID,
			BytesReceived: sess.BytesReceived,
			CreatedAt:     sess.CreatedAt,
			ExpiresAt:     sess.ExpiresAt,
		}
	}
	writeJSON(w, out)
}

// adminPurgeUploads godoc
// @Summary  Force-clear upload sessions to recover a wedged repository
// @Tags     uploads
// @Produce  json
// @Param    repo  query     string  false  "only clear sessions for this repository; omit to clear all"
// @Success  200   {object}  map[string]any
// @Failure  400   {string}  string  "invalid repository name"
// @Failure  500   {string}  string  "internal error"
// @Security Bearer
// @Router   /uploads [delete]
func (s *Server) adminPurgeUploads(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo != "" {
		if err := validate.RepoName(repo); err != nil {
			http.Error(w, "invalid repository name", http.StatusBadRequest)
			return
		}
	}
	s.logger.Debug("admin: DELETE /uploads", "repo", repo)

	purged, err := s.registry.PurgeUploadSessions(r.Context(), repo)
	if err != nil {
		s.logger.Error("purging upload sessions", "repo", repo, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"purged": purged, "repo": repo})
}

// classifyError maps service errors to HTTP status codes.
// Errors from resolving or fetching remote actors are gateway errors;
// everything else is an internal error.
func classifyError(err error) int {
	msg := err.Error()
	for _, prefix := range []string{"resolving target:", "fetching actor:", "delivering follow:"} {
		if strings.HasPrefix(msg, prefix) {
			return http.StatusBadGateway
		}
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writeJSON: failed to encode response", "error", err)
	}
}
