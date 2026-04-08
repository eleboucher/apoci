package server

import (
	"context"
	"encoding/json"
	"log/slog"
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
	SendUndo(ctx context.Context, peerActorURL string) error
	SendMutualFollow(ctx context.Context, actorURL string) (bool, error)
}

type realAPFederator struct {
	identity *activitypub.Identity
	db       *database.DB
	enqueue  activitypub.EnqueueFunc
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
	return activitypub.SendAccept(ctx, f.identity, f.db, followerActorURL, f.enqueue)
}

func (f *realAPFederator) SendReject(ctx context.Context, followerActorURL string) error {
	return activitypub.SendReject(ctx, f.identity, f.db, followerActorURL, f.enqueue)
}

func (f *realAPFederator) SendUndo(ctx context.Context, peerActorURL string) error {
	return activitypub.SendUndo(ctx, f.identity, peerActorURL, f.enqueue)
}

func (f *realAPFederator) SendMutualFollow(ctx context.Context, actorURL string) (bool, error) {
	return activitypub.SendMutualFollow(ctx, f.identity, f.db, actorURL, f.enqueue)
}

func (s *Server) adminRouter() http.Handler {
	r := chi.NewRouter()
	r.Use(bearerAuthMiddleware(s.cfg.AdminToken))

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

// decodeAndResolveTarget reads the follow request body and resolves the actor URL.
// Returns the actor URL and true on success, or writes an HTTP error and returns false.
func (s *Server) decodeAndResolveTarget(w http.ResponseWriter, r *http.Request) (string, bool) {
	var req adminFollowRequest
	r.Body = http.MaxBytesReader(w, r.Body, adminMaxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Target == "" {
		http.Error(w, "missing target", http.StatusBadRequest)
		return "", false
	}

	actorURL, err := s.apFed.ResolveFollowTarget(r.Context(), req.Target)
	if err != nil {
		s.logger.Error("resolving follow target", "target", req.Target, "error", err)
		http.Error(w, "could not resolve target", http.StatusBadGateway)
		return "", false
	}
	return actorURL, true
}

func (s *Server) adminAddFollow(w http.ResponseWriter, r *http.Request) {
	actorURL, ok := s.decodeAndResolveTarget(w, r)
	if !ok {
		return
	}

	ctx := r.Context()

	actor, err := s.apFed.FetchActor(ctx, actorURL)
	if err != nil {
		s.logger.Error("fetching actor", "actor_url", actorURL, "error", err)
		http.Error(w, "could not fetch actor", http.StatusBadGateway)
		return
	}

	if err := s.db.AddOutgoingFollow(ctx, actor.ID); err != nil {
		s.logger.Error("storing outgoing follow", "error", err)
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
		Endpoint:          activitypub.EndpointFromActorURL(actor.ID),
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}); err != nil {
		s.logger.Warn("recording peer after follow", "actor", actor.ID, "error", err)
	}

	writeJSON(w, map[string]string{"followed": actor.ID})
}

func (s *Server) adminAcceptFollow(w http.ResponseWriter, r *http.Request) {
	actorURL, ok := s.decodeAndResolveTarget(w, r)
	if !ok {
		return
	}

	ctx := r.Context()

	if err := s.apFed.SendAccept(ctx, actorURL); err != nil {
		s.logger.Error("sending accept", "actor_url", actorURL, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	result := map[string]string{"accepted": actorURL}

	if s.cfg.Federation.AutoAccept == activitypub.AutoAcceptMutual {
		sent, err := s.apFed.SendMutualFollow(ctx, actorURL)
		if err != nil {
			s.logger.Warn("mutual follow-back failed", "actor", actorURL, "error", err)
		} else if sent {
			result["followed_back"] = actorURL
		}
	}

	writeJSON(w, result)
}

func (s *Server) adminRejectFollow(w http.ResponseWriter, r *http.Request) {
	actorURL, ok := s.decodeAndResolveTarget(w, r)
	if !ok {
		return
	}

	if err := s.apFed.SendReject(r.Context(), actorURL); err != nil {
		s.logger.Error("sending reject", "actor_url", actorURL, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"rejected": actorURL})
}

func (s *Server) adminRemoveFollow(w http.ResponseWriter, r *http.Request) {
	actorURL, ok := s.decodeAndResolveTarget(w, r)
	if !ok {
		return
	}

	ctx := r.Context()

	if err := s.apFed.SendUndo(ctx, actorURL); err != nil {
		s.logger.Warn("failed to send Undo to peer", "actor", actorURL, "error", err)
	}

	// Clean up both inbound follow (if they followed us back) and outgoing follow record.
	errFollow := s.db.RemoveFollow(ctx, actorURL)
	errOutgoing := s.db.RemoveOutgoingFollow(ctx, actorURL)

	// If neither table had a record, report the error.
	if errFollow != nil && errOutgoing != nil {
		s.logger.Error("removing follow", "actor_url", actorURL, "error_follow", errFollow, "error_outgoing", errOutgoing)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, map[string]string{"removed": actorURL})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("writeJSON: failed to encode response", "error", err)
	}
}
