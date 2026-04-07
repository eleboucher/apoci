package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func SendAccept(ctx context.Context, identity *Identity, db *database.DB, followerActorURL string) error {
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

	accept := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       fmt.Sprintf("%s#accept-%d", identity.ActorURL, time.Now().UnixNano()),
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

	// Commit DB first — this is the source of truth. Delivery is best-effort.
	if err := db.AcceptFollowRequest(ctx, followerActorURL); err != nil {
		return fmt.Errorf("promoting follow: %w", err)
	}

	if err := DeliverActivity(ctx, actor.Inbox, acceptJSON, identity); err != nil {
		return fmt.Errorf("delivering Accept to %s (accepted locally): %w", actor.Inbox, err)
	}

	return nil
}

func SendReject(ctx context.Context, identity *Identity, db *database.DB, followerActorURL string) error {
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

	reject := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       fmt.Sprintf("%s#reject-%d", identity.ActorURL, time.Now().UnixNano()),
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

	// Best-effort delivery — reject locally regardless
	_ = DeliverActivity(ctx, actor.Inbox, rejectJSON, identity)

	return db.RejectFollowRequest(ctx, followerActorURL)
}
