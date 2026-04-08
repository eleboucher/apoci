package activitypub

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
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

func SendAccept(ctx context.Context, identity *Identity, db *database.DB, followerActorURL string, enqueue EnqueueFunc) error {
	fr, err := db.GetFollowRequest(ctx, followerActorURL)
	if err != nil {
		return fmt.Errorf("looking up follow request: %w", err)
	}
	if fr == nil {
		return fmt.Errorf("no pending follow request from %s", followerActorURL)
	}

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

	if err := db.AcceptFollowRequest(ctx, followerActorURL); err != nil {
		return fmt.Errorf("promoting follow: %w", err)
	}

	if err := deliverOrEnqueue(ctx, identity, enqueue, activityID, actor.Inbox, acceptJSON); err != nil {
		return fmt.Errorf("delivering accept to %s (accepted locally): %w", actor.Inbox, err)
	}

	return nil
}

func SendReject(ctx context.Context, identity *Identity, db *database.DB, followerActorURL string, enqueue EnqueueFunc) error {
	fr, err := db.GetFollowRequest(ctx, followerActorURL)
	if err != nil {
		return fmt.Errorf("looking up follow request: %w", err)
	}
	if fr == nil {
		return fmt.Errorf("no pending follow request from %s", followerActorURL)
	}

	actor, err := FetchActor(ctx, followerActorURL)
	if err != nil {
		// Still reject locally even if we can't reach the peer
		_ = db.RejectFollowRequest(ctx, followerActorURL)
		return fmt.Errorf("fetching actor %s (rejected locally): %w", followerActorURL, err)
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
		_ = db.RejectFollowRequest(ctx, followerActorURL)
		return fmt.Errorf("marshaling Reject: %w", err)
	}

	// Reject locally first — this is the source of truth.
	if err := db.RejectFollowRequest(ctx, followerActorURL); err != nil {
		return fmt.Errorf("rejecting follow request: %w", err)
	}

	// Best-effort delivery — already rejected locally.
	_ = deliverOrEnqueue(ctx, identity, enqueue, activityID, actor.Inbox, rejectJSON)

	return nil
}

// SendUndo delivers an Undo(Follow) to the peer. Best-effort: returns an error
// but the caller should still proceed with the local unfollow.
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
