// Package federation provides the shared business logic for follow management.
// Both the HTTP admin API and the CLI call into these functions, keeping
// transport concerns (HTTP decoding, terminal output) in their own layers.
//
// The activitypub package handles only protocol mechanics (build and deliver
// activities). All database state transitions live here.
package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

// Federator abstracts the ActivityPub network operations, enabling injection
// of test doubles. None of these methods touch the database.
type Federator interface {
	ResolveFollowTarget(ctx context.Context, input string) (string, error)
	FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error)
	DeliverActivity(ctx context.Context, inboxURL string, activityJSON []byte) error
	SendAccept(ctx context.Context, followerActorURL string) error
	SendReject(ctx context.Context, followerActorURL string) error
	SendUndo(ctx context.Context, peerActorURL string) error
	SendFollow(ctx context.Context, targetActorURL string) (string, error)
}

// RealFederator implements Federator using the activitypub package.
type RealFederator struct {
	Identity *activitypub.Identity
	Enqueue  activitypub.EnqueueFunc
}

func (f *RealFederator) ResolveFollowTarget(ctx context.Context, input string) (string, error) {
	return activitypub.ResolveFollowTarget(ctx, input)
}

func (f *RealFederator) FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error) {
	return activitypub.FetchActor(ctx, actorURL)
}

func (f *RealFederator) DeliverActivity(ctx context.Context, inboxURL string, activityJSON []byte) error {
	return activitypub.DeliverActivity(ctx, inboxURL, activityJSON, f.Identity)
}

func (f *RealFederator) SendAccept(ctx context.Context, followerActorURL string) error {
	return activitypub.SendAccept(ctx, f.Identity, followerActorURL, f.Enqueue)
}

func (f *RealFederator) SendReject(ctx context.Context, followerActorURL string) error {
	return activitypub.SendReject(ctx, f.Identity, followerActorURL, f.Enqueue)
}

func (f *RealFederator) SendUndo(ctx context.Context, peerActorURL string) error {
	return activitypub.SendUndo(ctx, f.Identity, peerActorURL, f.Enqueue)
}

func (f *RealFederator) SendFollow(ctx context.Context, targetActorURL string) (string, error) {
	return activitypub.SendFollow(ctx, f.Identity, targetActorURL, f.Enqueue)
}

// Service orchestrates follow management operations.
type Service struct {
	Fed      Federator
	DB       *database.DB
	ActorURL string // this node's actor URL
	Logger   *slog.Logger
}

// AddFollowResult is returned by AddFollow on success.
type AddFollowResult struct {
	ActorID string
}

// AddFollow resolves the target, sends a Follow activity, and records the
// outgoing follow and peer. The outgoing follow is stored before delivery so
// that an immediate Accept from the peer can be matched.
func (s *Service) AddFollow(ctx context.Context, input string) (*AddFollowResult, error) {
	actorURL, err := s.Fed.ResolveFollowTarget(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("resolving target: %w", err)
	}

	actor, err := s.Fed.FetchActor(ctx, actorURL)
	if err != nil {
		return nil, fmt.Errorf("fetching actor: %w", err)
	}

	// Store outgoing follow BEFORE delivery so that an immediate Accept from
	// the peer can be matched by the inbox handler.
	if err := s.DB.AddOutgoingFollow(ctx, actor.ID); err != nil {
		return nil, fmt.Errorf("storing outgoing follow: %w", err)
	}

	followActivity := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       s.ActorURL + "#follow-" + url.QueryEscape(actor.ID),
		"type":     "Follow",
		"actor":    s.ActorURL,
		"object":   actor.ID,
	}
	activityJSON, err := json.Marshal(followActivity)
	if err != nil {
		return nil, fmt.Errorf("marshaling follow: %w", err)
	}

	if err := s.Fed.DeliverActivity(ctx, actor.Inbox, activityJSON); err != nil {
		return nil, fmt.Errorf("delivering follow: %w", err)
	}

	if err := s.DB.UpsertPeer(ctx, &database.Peer{
		ActorURL:          actor.ID,
		Endpoint:          activitypub.EndpointFromActorURL(actor.ID),
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}); err != nil {
		s.Logger.Warn("recording peer after follow", "actor", actor.ID, "error", err)
	}

	return &AddFollowResult{ActorID: actor.ID}, nil
}

// RemoveFollow sends an Undo(Follow) and removes both inbound and outgoing
// follow records. Returns an error only when neither table had a record.
func (s *Service) RemoveFollow(ctx context.Context, input string) (string, error) {
	actorURL, err := s.Fed.ResolveFollowTarget(ctx, input)
	if err != nil {
		return "", fmt.Errorf("resolving target: %w", err)
	}

	if err := s.Fed.SendUndo(ctx, actorURL); err != nil {
		s.Logger.Warn("failed to send Undo to peer", "actor", actorURL, "error", err)
	}

	errFollow := s.DB.RemoveFollow(ctx, actorURL)
	errOutgoing := s.DB.RemoveOutgoingFollow(ctx, actorURL)

	if errFollow != nil && errOutgoing != nil {
		return "", fmt.Errorf("removing follow: follow=%w, outgoing=%w", errFollow, errOutgoing)
	}
	return actorURL, nil
}

// AcceptFollowResult is returned by AcceptFollow on success.
type AcceptFollowResult struct {
	ActorURL     string
	FollowedBack bool
}

// AcceptFollow accepts a pending follow request: promotes the DB record, delivers
// an Accept activity, and optionally sends a mutual follow-back.
func (s *Service) AcceptFollow(ctx context.Context, input, autoAccept string) (*AcceptFollowResult, error) {
	actorURL, err := s.Fed.ResolveFollowTarget(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("resolving target: %w", err)
	}

	fr, err := s.DB.GetFollowRequest(ctx, actorURL)
	if err != nil {
		return nil, fmt.Errorf("looking up follow request: %w", err)
	}
	if fr == nil {
		return nil, fmt.Errorf("no pending follow request from %s", actorURL)
	}

	// Promote locally first — this is the source of truth.
	if err := s.DB.AcceptFollowRequest(ctx, actorURL); err != nil {
		return nil, fmt.Errorf("promoting follow: %w", err)
	}

	if err := s.Fed.SendAccept(ctx, actorURL); err != nil {
		return nil, fmt.Errorf("delivering accept (accepted locally): %w", err)
	}

	result := &AcceptFollowResult{ActorURL: actorURL}

	if autoAccept == activitypub.AutoAcceptMutual {
		sent, err := s.sendMutualFollowBack(ctx, actorURL)
		if err != nil {
			s.Logger.Warn("mutual follow-back failed", "actor", actorURL, "error", err)
		} else if sent {
			result.FollowedBack = true
		}
	}

	return result, nil
}

// RejectFollow rejects a pending follow request: marks it rejected in the DB
// and delivers a Reject activity (best-effort).
func (s *Service) RejectFollow(ctx context.Context, input string) (string, error) {
	actorURL, err := s.Fed.ResolveFollowTarget(ctx, input)
	if err != nil {
		return "", fmt.Errorf("resolving target: %w", err)
	}

	fr, err := s.DB.GetFollowRequest(ctx, actorURL)
	if err != nil {
		return "", fmt.Errorf("looking up follow request: %w", err)
	}
	if fr == nil {
		return "", fmt.Errorf("no pending follow request from %s", actorURL)
	}

	// Reject locally first — this is the source of truth.
	if err := s.DB.RejectFollowRequest(ctx, actorURL); err != nil {
		return "", fmt.Errorf("rejecting follow request: %w", err)
	}

	// Best-effort delivery.
	if err := s.Fed.SendReject(ctx, actorURL); err != nil {
		s.Logger.Warn("reject delivery failed (rejected locally)", "actor", actorURL, "error", err)
	}

	return actorURL, nil
}

// sendMutualFollowBack sends a Follow back to an actor we just accepted,
// recording the outgoing follow and peer. It is a no-op when an outgoing
// follow already exists. Returns true when a follow-back was sent.
func (s *Service) sendMutualFollowBack(ctx context.Context, actorURL string) (bool, error) {
	existing, err := s.DB.GetOutgoingFollow(ctx, actorURL)
	if err != nil {
		return false, fmt.Errorf("checking existing outgoing follow: %w", err)
	}
	if existing != nil {
		return false, nil
	}

	actorID, err := s.Fed.SendFollow(ctx, actorURL)
	if err != nil {
		return false, fmt.Errorf("sending follow-back: %w", err)
	}

	if err := s.DB.AddOutgoingFollow(ctx, actorID); err != nil {
		return false, fmt.Errorf("storing outgoing follow: %w", err)
	}

	if err := s.DB.UpsertPeer(ctx, &database.Peer{
		ActorURL:          actorID,
		Endpoint:          activitypub.EndpointFromActorURL(actorID),
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}); err != nil {
		// Follow was sent and outgoing follow recorded; peer upsert is best-effort.
		return true, fmt.Errorf("upserting peer (follow-back sent): %w", err)
	}

	return true, nil
}
