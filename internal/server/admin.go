package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const adminMaxBody int64 = 4 * 1024 // 4 KB

// apFederator abstracts the ActivityPub network operations used by admin
// handlers, enabling injection of test doubles.
type apFederator interface {
	ResolveFollowTarget(ctx context.Context, input string) (string, error)
	FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error)
	DeliverActivity(ctx context.Context, inboxURL string, activityJSON []byte) error
	SendAccept(ctx context.Context, followerActorURL string) error
	SendReject(ctx context.Context, followerActorURL string) error
}

type realAPFederator struct {
	identity *activitypub.Identity
	db       *database.DB
}

func (f *realAPFederator) ResolveFollowTarget(ctx context.Context, input string) (string, error) {
	return activitypub.ResolveFollowTarget(ctx, input)
}

func (f *realAPFederator) FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error) {
	return activitypub.FetchActor(ctx, actorURL)
}

func (f *realAPFederator) DeliverActivity(ctx context.Context, inboxURL string, activityJSON []byte) error {
	return activitypub.DeliverActivity(ctx, inboxURL, activityJSON, f.identity)
}

func (f *realAPFederator) SendAccept(ctx context.Context, followerActorURL string) error {
	return activitypub.SendAccept(ctx, f.identity, f.db, followerActorURL)
}

func (f *realAPFederator) SendReject(ctx context.Context, followerActorURL string) error {
	return activitypub.SendReject(ctx, f.identity, f.db, followerActorURL)
}

func (s *Server) adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(bearerAuthMiddleware(s.cfg.RegistryToken))

	r.Get("/identity", s.adminGetIdentity)
	r.Get("/follows", s.adminListFollows)
	r.Get("/follows/pending", s.adminListPending)
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

type adminFollowRequest struct {
	Target string `json:"target"`
}

func (s *Server) adminAddFollow(w http.ResponseWriter, r *http.Request) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	actorURL, err := s.apFed.ResolveFollowTarget(ctx, req.Target)
	if err != nil {
		s.logger.Error("resolving follow target", "target", req.Target, "error", err)
		http.Error(w, "could not resolve target", http.StatusBadGateway)
		return
	}

	actor, err := s.apFed.FetchActor(ctx, actorURL)
	if err != nil {
		s.logger.Error("fetching actor", "actor_url", actorURL, "error", err)
		http.Error(w, "could not fetch actor", http.StatusBadGateway)
		return
	}

	endpoint := activitypub.EndpointFromActorURL(actor.ID)
	if err := s.db.AddFollowRequest(ctx, actor.ID, actor.PublicKey.PublicKeyPEM, endpoint); err != nil {
		s.logger.Error("storing follow request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	followActivity := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       s.identity.ActorURL + "#follow-" + url.QueryEscape(actor.ID),
		"type":     "Follow",
		"actor":    s.identity.ActorURL,
		"object":   actor.ID,
	}

	activityJSON, err := json.Marshal(followActivity)
	if err != nil {
		s.logger.Error("marshaling follow activity", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := s.apFed.DeliverActivity(ctx, actor.Inbox, activityJSON); err != nil {
		s.logger.Error("delivering follow activity", "inbox", actor.Inbox, "error", err)
		http.Error(w, "could not deliver follow activity", http.StatusBadGateway)
		return
	}

	if err := s.db.UpsertPeer(ctx, &database.Peer{
		ActorURL:          actor.ID,
		Endpoint:          endpoint,
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}); err != nil {
		s.logger.Warn("recording peer after follow", "actor", actor.ID, "error", err)
	}

	writeJSON(w, map[string]string{"followed": actor.ID})
}

func (s *Server) adminAcceptFollow(w http.ResponseWriter, r *http.Request) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	actorURL, err := s.apFed.ResolveFollowTarget(ctx, req.Target)
	if err != nil {
		s.logger.Error("resolving follow target", "target", req.Target, "error", err)
		http.Error(w, "could not resolve target", http.StatusBadGateway)
		return
	}

	if err := s.apFed.SendAccept(ctx, actorURL); err != nil {
		s.logger.Error("sending accept", "actor_url", actorURL, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"accepted": actorURL})
}

func (s *Server) adminRejectFollow(w http.ResponseWriter, r *http.Request) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	actorURL, err := s.apFed.ResolveFollowTarget(ctx, req.Target)
	if err != nil {
		s.logger.Error("resolving follow target", "target", req.Target, "error", err)
		http.Error(w, "could not resolve target", http.StatusBadGateway)
		return
	}

	if err := s.apFed.SendReject(ctx, actorURL); err != nil {
		s.logger.Error("sending reject", "actor_url", actorURL, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"rejected": actorURL})
}

func (s *Server) adminRemoveFollow(w http.ResponseWriter, r *http.Request) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	actorURL, err := s.apFed.ResolveFollowTarget(ctx, req.Target)
	if err != nil {
		s.logger.Error("resolving follow target", "target", req.Target, "error", err)
		http.Error(w, "could not resolve target", http.StatusBadGateway)
		return
	}

	if err := s.db.RemoveFollow(ctx, actorURL); err != nil {
		s.logger.Error("removing follow", "actor_url", actorURL, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"removed": actorURL})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
