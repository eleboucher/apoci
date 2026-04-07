package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

type EnqueueFunc func(ctx context.Context, activityID, inboxURL string, activityJSON []byte) error

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

	activityID := fmt.Sprintf("%s#accept-%d", identity.ActorURL, time.Now().UnixNano())
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

	// Route delivery through the persistent queue when available, so
	// transient failures are retried automatically.
	if enqueue != nil {
		if err := enqueue(ctx, activityID, actor.Inbox, acceptJSON); err != nil {
			return fmt.Errorf("enqueuing Accept to %s (accepted locally): %w", actor.Inbox, err)
		}
		return nil
	}

	// Fallback: direct delivery (used by CLI where no queue is running).
	if err := DeliverActivity(ctx, actor.Inbox, acceptJSON, identity); err != nil {
		return fmt.Errorf("delivering Accept to %s (accepted locally): %w", actor.Inbox, err)
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

	activityID := fmt.Sprintf("%s#reject-%d", identity.ActorURL, time.Now().UnixNano())
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

	// Route delivery through the persistent queue when available.
	if enqueue != nil {
		if err := enqueue(ctx, activityID, actor.Inbox, rejectJSON); err != nil {
			// Already rejected locally, just log the error
			return fmt.Errorf("enqueuing Reject to %s (rejected locally): %w", actor.Inbox, err)
		}
		return nil
	}

	// Fallback: best-effort direct delivery (used by CLI).
	_ = DeliverActivity(ctx, actor.Inbox, rejectJSON, identity)

	return nil
}
