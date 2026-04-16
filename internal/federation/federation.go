// Package federation provides the shared business logic for follow management.
package federation

import (
	"context"
	"fmt"
	"log/slog"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

type FederationRepository interface {
	RemoveFollow(ctx context.Context, actorURL string) error
	ListFollows(ctx context.Context) ([]database.Actor, error)
	RefreshFollow(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error
	GetFollowRequest(ctx context.Context, actorURL string) (*database.FollowRequest, error)
	FindFollowRequestByInput(ctx context.Context, input string) (*database.FollowRequest, error)
	AcceptFollowRequest(ctx context.Context, actorURL string) error
	RejectFollowRequest(ctx context.Context, actorURL string) error
	ListFollowRequests(ctx context.Context) ([]database.FollowRequest, error)
	RefreshFollowRequest(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error
	AddOutgoingFollow(ctx context.Context, actorURL string) error
	GetOutgoingFollow(ctx context.Context, actorURL string) (*database.Actor, error)
	RemoveOutgoingFollow(ctx context.Context, actorURL string) error
	ListAllOutgoingFollows(ctx context.Context) ([]database.Actor, error)
	RefreshOutgoingFollow(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error
	UpsertActor(ctx context.Context, a *database.Actor) error
	DeleteActor(ctx context.Context, actorURL string) error
	FindActorByInput(ctx context.Context, input string) (*database.Actor, error)
}

type Federator interface {
	ResolveFollowTarget(ctx context.Context, input string) (string, error)
	FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error)
	SendAccept(ctx context.Context, followerActorURL string) error
	SendReject(ctx context.Context, followerActorURL string) error
	SendUndo(ctx context.Context, peerActorURL string) error
	SendFollow(ctx context.Context, targetActorURL string) (string, error)
}

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

type Service struct {
	Fed      Federator
	DB       FederationRepository
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
	s.Logger.Debug("AddFollow", "input", input)

	actorURL, err := s.Fed.ResolveFollowTarget(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("resolving target: %w", err)
	}
	s.Logger.Debug("AddFollow resolved", "actorURL", actorURL)

	if actorURL == s.ActorURL {
		return nil, fmt.Errorf("cannot follow yourself")
	}

	actor, err := s.Fed.FetchActor(ctx, actorURL)
	if err != nil {
		return nil, fmt.Errorf("fetching actor: %w", err)
	}
	s.Logger.Debug("AddFollow fetched actor", "actorID", actor.ID, "inbox", actor.Inbox)

	if actor.ID == s.ActorURL {
		return nil, fmt.Errorf("cannot follow yourself")
	}

	// Store outgoing follow BEFORE delivery so that an immediate Accept from
	// the peer can be matched by the inbox handler.
	if err := s.DB.AddOutgoingFollow(ctx, actor.ID); err != nil {
		return nil, fmt.Errorf("storing outgoing follow: %w", err)
	}

	s.Logger.Debug("AddFollow delivering Follow activity", "actorID", actor.ID)
	if _, err := s.Fed.SendFollow(ctx, actor.ID); err != nil {
		return nil, fmt.Errorf("delivering follow: %w", err)
	}

	if err := s.DB.UpsertActor(ctx, &database.Actor{
		ActorURL:          actor.ID,
		Endpoint:          activitypub.EndpointFromActorURL(actor.ID),
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}); err != nil {
		s.Logger.Warn("recording peer after follow", "actor", actor.ID, "error", err)
	}

	s.Logger.Debug("AddFollow done", "actorID", actor.ID)
	return &AddFollowResult{ActorID: actor.ID}, nil
}

// RemoveFollow sends an Undo(Follow) and clears follow records for the given
// actor. With force=true, resolution and Undo errors are tolerated, and the
// actor row is hard-deleted.
func (s *Service) RemoveFollow(ctx context.Context, input string, force bool) (string, error) {
	s.Logger.Debug("RemoveFollow", "input", input, "force", force)

	var actorURL string
	if force {
		// Force: prioritize local DB lookup (handles WebFinger returning different URL)
		a, err := s.DB.FindActorByInput(ctx, input)
		if err != nil {
			return "", fmt.Errorf("finding actor: %w", err)
		}
		if a != nil {
			actorURL = a.ActorURL
			s.Logger.Debug("RemoveFollow found locally", "actorURL", actorURL)
		} else {
			// Not in DB; try WebFinger as fallback
			s.Logger.Debug("RemoveFollow not found locally, trying WebFinger", "input", input)
			resolved, err := s.Fed.ResolveFollowTarget(ctx, input)
			if err != nil {
				return "", fmt.Errorf("actor %q not found locally and WebFinger failed: %w", input, err)
			}
			actorURL = resolved
			s.Logger.Debug("RemoveFollow resolved via WebFinger", "actorURL", actorURL)
		}
	} else {
		resolved, err := s.Fed.ResolveFollowTarget(ctx, input)
		if err != nil {
			return "", fmt.Errorf("resolving target: %w", err)
		}
		actorURL = resolved
		s.Logger.Debug("RemoveFollow resolved", "actorURL", actorURL)
	}

	if err := s.Fed.SendUndo(ctx, actorURL); err != nil {
		s.Logger.Debug("RemoveFollow Undo failed", "actor", actorURL, "error", err)
	}

	errFollow := s.DB.RemoveFollow(ctx, actorURL)
	errOutgoing := s.DB.RemoveOutgoingFollow(ctx, actorURL)
	s.Logger.Debug("RemoveFollow DB cleanup", "actorURL", actorURL, "followErr", errFollow, "outgoingErr", errOutgoing)

	if errFollow != nil && errOutgoing != nil && !force {
		return "", fmt.Errorf("removing follow: follow=%w, outgoing=%w", errFollow, errOutgoing)
	}

	if force {
		if err := s.DB.DeleteActor(ctx, actorURL); err != nil {
			return "", fmt.Errorf("deleting actor: %w", err)
		}
		s.Logger.Debug("RemoveFollow deleted actor", "actorURL", actorURL)
	}

	return actorURL, nil
}

// AcceptFollowResult is returned by AcceptFollow on success.
type AcceptFollowResult struct {
	ActorURL     string
	FollowedBack bool
}

// AcceptFollow accepts a pending follow request: promotes the DB record, delivers
// an Accept activity (best-effort), and optionally sends a mutual follow-back.
func (s *Service) AcceptFollow(ctx context.Context, input, autoAccept string) (*AcceptFollowResult, error) {
	s.Logger.Debug("AcceptFollow", "input", input, "autoAccept", autoAccept)

	actorURL, fr, err := s.locateFollowRequest(ctx, input)
	if err != nil {
		return nil, err
	}
	if fr == nil {
		return nil, fmt.Errorf("no pending follow request for %q", input)
	}
	s.Logger.Debug("AcceptFollow located", "actorURL", actorURL)

	// Promote locally first — this is the source of truth.
	if err := s.DB.AcceptFollowRequest(ctx, actorURL); err != nil {
		return nil, fmt.Errorf("promoting follow: %w", err)
	}
	s.Logger.Debug("AcceptFollow promoted locally", "actorURL", actorURL)

	// Best-effort delivery. The delivery queue retries; a transient failure
	// must not leave the pending request irrecoverably consumed.
	if err := s.Fed.SendAccept(ctx, actorURL); err != nil {
		s.Logger.Warn("accept delivery failed (accepted locally)", "actor", actorURL, "error", err)
	} else {
		s.Logger.Debug("AcceptFollow delivered Accept", "actorURL", actorURL)
	}

	result := &AcceptFollowResult{ActorURL: actorURL}

	if autoAccept == activitypub.AutoAcceptMutual {
		sent, err := s.sendMutualFollowBack(ctx, actorURL)
		if sent {
			result.FollowedBack = true
			s.Logger.Debug("AcceptFollow mutual follow-back sent", "actorURL", actorURL)
		}
		if err != nil {
			s.Logger.Warn("mutual follow-back failed", "actor", actorURL, "error", err)
		}
	}

	return result, nil
}

// RejectFollow rejects a pending follow request: marks it rejected in the DB
// and delivers a Reject activity (best-effort).
func (s *Service) RejectFollow(ctx context.Context, input string) (string, error) {
	s.Logger.Debug("RejectFollow", "input", input)

	actorURL, fr, err := s.locateFollowRequest(ctx, input)
	if err != nil {
		return "", err
	}
	if fr == nil {
		return "", fmt.Errorf("no pending follow request for %q", input)
	}
	s.Logger.Debug("RejectFollow located", "actorURL", actorURL)

	// Reject locally first — this is the source of truth.
	if err := s.DB.RejectFollowRequest(ctx, actorURL); err != nil {
		return "", fmt.Errorf("rejecting follow request: %w", err)
	}
	s.Logger.Debug("RejectFollow rejected locally", "actorURL", actorURL)

	// Best-effort delivery.
	if err := s.Fed.SendReject(ctx, actorURL); err != nil {
		s.Logger.Warn("reject delivery failed (rejected locally)", "actor", actorURL, "error", err)
	} else {
		s.Logger.Debug("RejectFollow delivered Reject", "actorURL", actorURL)
	}

	return actorURL, nil
}

func (s *Service) locateFollowRequest(ctx context.Context, input string) (string, *database.FollowRequest, error) {
	if actorURL, err := s.Fed.ResolveFollowTarget(ctx, input); err == nil {
		fr, dbErr := s.DB.GetFollowRequest(ctx, actorURL)
		if dbErr != nil {
			return "", nil, fmt.Errorf("looking up follow request: %w", dbErr)
		}
		if fr != nil {
			return actorURL, fr, nil
		}
		s.Logger.Debug("locateFollowRequest WebFinger found no request, falling back", "webfingerURL", actorURL, "input", input)
	} else {
		s.Logger.Debug("locateFollowRequest WebFinger failed, falling back", "input", input, "error", err)
	}

	fr, err := s.DB.FindFollowRequestByInput(ctx, input)
	if err != nil {
		return "", nil, fmt.Errorf("finding follow request: %w", err)
	}
	if fr == nil {
		return "", nil, nil
	}
	return fr.ActorURL, fr, nil
}

func (s *Service) RefreshActors(ctx context.Context) {
	follows, err := s.DB.ListFollows(ctx)
	if err != nil {
		s.Logger.Warn("actor refresh: failed to list follows", "error", err)
		return
	}
	refreshed := make(map[string]struct{}, len(follows))
	for _, f := range follows {
		if ctx.Err() != nil {
			return
		}
		actor, err := s.Fed.FetchActor(ctx, f.ActorURL)
		if err != nil {
			s.Logger.Warn("actor refresh: failed to fetch actor", "actor", f.ActorURL, "error", err)
			continue
		}
		alias := actorAlias(actor)
		endpoint := activitypub.EndpointFromActorURL(f.ActorURL)
		if err := s.DB.RefreshFollow(ctx, f.ActorURL, actor.PublicKey.PublicKeyPEM, endpoint, alias); err != nil {
			s.Logger.Warn("actor refresh: failed to update follow", "actor", f.ActorURL, "error", err)
			continue
		}
		refreshed[f.ActorURL] = struct{}{}
	}

	requests, err := s.DB.ListFollowRequests(ctx)
	if err != nil {
		s.Logger.Warn("actor refresh: failed to list follow requests", "error", err)
		return
	}
	for _, fr := range requests {
		if ctx.Err() != nil {
			return
		}
		actor, err := s.Fed.FetchActor(ctx, fr.ActorURL)
		if err != nil {
			s.Logger.Warn("actor refresh: failed to fetch actor", "actor", fr.ActorURL, "error", err)
			continue
		}
		alias := actorAlias(actor)
		endpoint := activitypub.EndpointFromActorURL(fr.ActorURL)
		if err := s.DB.RefreshFollowRequest(ctx, fr.ActorURL, actor.PublicKey.PublicKeyPEM, endpoint, alias); err != nil {
			s.Logger.Warn("actor refresh: failed to update follow request", "actor", fr.ActorURL, "error", err)
		}
	}

	outgoing, err := s.DB.ListAllOutgoingFollows(ctx)
	if err != nil {
		s.Logger.Warn("actor refresh: failed to list outgoing follows", "error", err)
		return
	}
	for _, o := range outgoing {
		if ctx.Err() != nil {
			return
		}
		if _, ok := refreshed[o.ActorURL]; ok {
			continue
		}
		actor, err := s.Fed.FetchActor(ctx, o.ActorURL)
		if err != nil {
			s.Logger.Warn("actor refresh: failed to fetch actor", "actor", o.ActorURL, "error", err)
			continue
		}
		alias := actorAlias(actor)
		endpoint := activitypub.EndpointFromActorURL(o.ActorURL)
		if err := s.DB.RefreshOutgoingFollow(ctx, o.ActorURL, actor.PublicKey.PublicKeyPEM, endpoint, alias); err != nil {
			s.Logger.Warn("actor refresh: failed to update outgoing follow", "actor", o.ActorURL, "error", err)
		}
	}
}

// actorAlias returns the actor's account domain for display.
func actorAlias(actor *activitypub.Actor) *string {
	alias := activitypub.ActorAlias(actor)
	if alias == "" {
		return nil
	}
	return &alias
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

	if err := s.DB.AddOutgoingFollow(ctx, actorURL); err != nil {
		return false, fmt.Errorf("storing outgoing follow: %w", err)
	}

	if _, err := s.Fed.SendFollow(ctx, actorURL); err != nil {
		return false, fmt.Errorf("sending follow-back: %w", err)
	}

	if err := s.DB.UpsertActor(ctx, &database.Actor{
		ActorURL:          actorURL,
		Endpoint:          activitypub.EndpointFromActorURL(actorURL),
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}); err != nil {
		// Follow was sent and outgoing follow recorded; peer upsert is best-effort.
		return true, fmt.Errorf("upserting peer (follow-back sent): %w", err)
	}

	return true, nil
}
