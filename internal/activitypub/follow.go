package activitypub

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

type EnqueueFunc func(ctx context.Context, activityID, inboxURL string, activityJSON []byte) error

// deliverOrEnqueue routes delivery through the persistent queue when available,
// falling back to direct delivery (used by CLI where no queue is running).
func deliverOrEnqueue(ctx context.Context, identity *Identity, enqueue EnqueueFunc, activityID, inboxURL string, activityJSON []byte) error {
	if enqueue != nil {
		return enqueue(ctx, activityID, inboxURL, activityJSON)
	}
	return DeliverActivity(ctx, inboxURL, activityJSON, identity)
}

// SendAccept builds and delivers an Accept(Follow) activity to the follower's
// inbox. The caller is responsible for any DB state transitions (e.g.
// AcceptFollowRequest) before calling this function.
func SendAccept(ctx context.Context, identity *Identity, followerActorURL string, enqueue EnqueueFunc) error {
	actor, err := FetchActor(ctx, followerActorURL)
	if err != nil {
		return fmt.Errorf("fetching actor %s: %w", followerActorURL, err)
	}

	activityID := identity.ActorURL + "#accept-" + uuid.New().String()
	accept := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       activityID,
		"type":     "Accept",
		"actor":    identity.ActorURL,
		"object": map[string]any{
			"type":   "Follow",
			"actor":  followerActorURL,
			"object": identity.ActorURL,
		},
	}

	acceptJSON, err := json.Marshal(accept)
	if err != nil {
		return fmt.Errorf("marshaling Accept: %w", err)
	}

	if err := deliverOrEnqueue(ctx, identity, enqueue, activityID, actor.Inbox, acceptJSON); err != nil {
		return fmt.Errorf("delivering accept to %s: %w", actor.Inbox, err)
	}

	return nil
}

// SendReject builds and delivers a Reject(Follow) activity to the follower's
// inbox. The caller is responsible for any DB state transitions (e.g.
// RejectFollowRequest) before calling this function.
func SendReject(ctx context.Context, identity *Identity, followerActorURL string, enqueue EnqueueFunc) error {
	actor, err := FetchActor(ctx, followerActorURL)
	if err != nil {
		return fmt.Errorf("fetching actor %s: %w", followerActorURL, err)
	}

	activityID := identity.ActorURL + "#reject-" + uuid.New().String()
	reject := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       activityID,
		"type":     "Reject",
		"actor":    identity.ActorURL,
		"object": map[string]any{
			"type":   "Follow",
			"actor":  followerActorURL,
			"object": identity.ActorURL,
		},
	}

	rejectJSON, err := json.Marshal(reject)
	if err != nil {
		return fmt.Errorf("marshaling Reject: %w", err)
	}

	// Best-effort delivery — caller already handled the DB side.
	_ = deliverOrEnqueue(ctx, identity, enqueue, activityID, actor.Inbox, rejectJSON)

	return nil
}

// SendFollow builds and delivers a Follow activity to the target actor's inbox.
// Returns the actor's canonical ID. The caller is responsible for recording the
// outgoing follow and peer in the database.
func SendFollow(ctx context.Context, identity *Identity, targetActorURL string, enqueue EnqueueFunc) (string, error) {
	actor, err := FetchActor(ctx, targetActorURL)
	if err != nil {
		return "", fmt.Errorf("fetching actor %s: %w", targetActorURL, err)
	}

	activityID := identity.ActorURL + "#follow-" + uuid.New().String()
	follow := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       activityID,
		"type":     "Follow",
		"actor":    identity.ActorURL,
		"object":   actor.ID,
	}

	followJSON, err := json.Marshal(follow)
	if err != nil {
		return "", fmt.Errorf("marshaling Follow: %w", err)
	}

	if err := deliverOrEnqueue(ctx, identity, enqueue, activityID, actor.Inbox, followJSON); err != nil {
		return "", fmt.Errorf("delivering follow to %s: %w", actor.Inbox, err)
	}

	return actor.ID, nil
}

// SendUndo builds and delivers an Undo(Follow) activity to the peer. Best-effort:
// returns an error but the caller should still proceed with the local cleanup.
func SendUndo(ctx context.Context, identity *Identity, peerActorURL string, enqueue EnqueueFunc) error {
	actor, err := FetchActor(ctx, peerActorURL)
	if err != nil {
		return fmt.Errorf("fetching actor %s: %w", peerActorURL, err)
	}

	activityID := identity.ActorURL + "#undo-" + uuid.New().String()
	undo := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       activityID,
		"type":     "Undo",
		"actor":    identity.ActorURL,
		"object": map[string]any{
			"type":   "Follow",
			"actor":  identity.ActorURL,
			"object": actor.ID,
		},
	}

	undoJSON, err := json.Marshal(undo)
	if err != nil {
		return fmt.Errorf("marshaling Undo: %w", err)
	}

	if err := deliverOrEnqueue(ctx, identity, enqueue, activityID, actor.Inbox, undoJSON); err != nil {
		return fmt.Errorf("delivering undo to %s: %w", actor.Inbox, err)
	}
	return nil
}
