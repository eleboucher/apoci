package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const adminMaxBody int64 = 4 * 1024 // 4 KB

func (s *Server) adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(bearerAuthMiddleware(s.cfg.AdminToken))

	r.Get("/identity", s.adminGetIdentity)
	r.Get("/follows", s.adminListFollows)
	r.Get("/follows/pending", s.adminListPending)
	r.Get("/follows/outgoing", s.adminListOutgoingFollows)
	r.Post("/follows", s.adminAddFollow)
	r.Post("/follows/accept", s.adminAcceptFollow)
	r.Post("/follows/reject", s.adminRejectFollow)
	r.Delete("/follows", s.adminRemoveFollow)

	return r
}

func (s *Server) adminGetIdentity(w http.ResponseWriter, r *http.Request) {
	pubPEM, err := s.identity.PublicKeyPEM()
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{
		"name":          s.cfg.Name,
		"actorURL":      s.identity.ActorURL,
		"keyID":         s.identity.KeyID(),
		"domain":        s.identity.Domain,
		"accountDomain": s.identity.AccountDomain,
		"endpoint":      s.cfg.Endpoint,
		"publicKey":     pubPEM,
	})
}

func (s *Server) adminListFollows(w http.ResponseWriter, r *http.Request) {
	follows, err := s.db.ListFollows(r.Context())
	if err != nil {
		s.logger.Error("listing follows", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, follows)
}

func (s *Server) adminListPending(w http.ResponseWriter, r *http.Request) {
	requests, err := s.db.ListFollowRequests(r.Context())
	if err != nil {
		s.logger.Error("listing pending requests", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, requests)
}

func (s *Server) adminListOutgoingFollows(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
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
	writeJSON(w, follows)
}

type adminFollowRequest struct {
	Target string `json:"target"`
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

func (s *Server) adminAddFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}

	result, err := s.fedSvc.AddFollow(r.Context(), target)
	if err != nil {
		s.logger.Error("adding follow", "target", target, "error", err)
		http.Error(w, "could not add follow", classifyError(err))
		return
	}

	writeJSON(w, map[string]string{"followed": result.ActorID})
}

func (s *Server) adminAcceptFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}

	result, err := s.fedSvc.AcceptFollow(r.Context(), target, s.cfg.Federation.AutoAccept)
	if err != nil {
		s.logger.Error("accepting follow", "target", target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	resp := map[string]string{"accepted": result.ActorURL}
	if result.FollowedBack {
		resp["followed_back"] = result.ActorURL
	}
	writeJSON(w, resp)
}

func (s *Server) adminRejectFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}

	actorURL, err := s.fedSvc.RejectFollow(r.Context(), target)
	if err != nil {
		s.logger.Error("rejecting follow", "target", target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"rejected": actorURL})
}

func (s *Server) adminRemoveFollow(w http.ResponseWriter, r *http.Request) {
	target, ok := decodeTarget(w, r)
	if !ok {
		return
	}

	actorURL, err := s.fedSvc.RemoveFollow(r.Context(), target)
	if err != nil {
		s.logger.Error("removing follow", "target", target, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"removed": actorURL})
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
