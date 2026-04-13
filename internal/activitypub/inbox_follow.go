package activitypub

import (
	"context"
	"fmt"
	"net/url"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

// processFollow handles incoming Follow requests.
func (h *InboxHandler) processFollow(ctx context.Context, activity *RawActivity, pubKeyPEM string) error {
	target, ok := activity.Object.(string)
	if !ok {
		return fmt.Errorf("invalid Follow object")
	}

	if target != h.identity.ActorURL {
		return fmt.Errorf("follow target mismatch")
	}

	if activity.Actor == h.identity.ActorURL {
		return fmt.Errorf("cannot follow yourself")
	}

	actorEndpoint := EndpointFromActorURL(activity.Actor)

	var alias *string
	if actor, err := FetchActor(ctx, activity.Actor); err == nil {
		if a := ActorAlias(actor); a != "" {
			alias = &a
		}
	}

	if err := h.db.AddFollowRequest(ctx, activity.Actor, pubKeyPEM, actorEndpoint, alias); err != nil {
		return fmt.Errorf("storing follow request: %w", err)
	}

	if h.shouldAutoAccept(ctx, activity.Actor) {
		if err := h.db.AcceptFollowRequest(ctx, activity.Actor); err != nil {
			h.logger.Warn("inbox: auto-accept DB promotion failed, request remains pending", "from", activity.Actor, "error", err)
		} else if err := SendAccept(ctx, h.identity, activity.Actor, h.enqueue); err != nil {
			h.logger.Warn("inbox: auto-accept delivery failed (accepted locally)", "from", activity.Actor, "error", err)
			return nil
		} else {
			h.logger.Info("inbox: auto-accepted follow request", "from", activity.Actor)
			return nil
		}
	}

	h.logger.Info("inbox: received follow request (pending operator approval)", "from", activity.Actor)
	if h.notifier != nil {
		h.notifier.Send(notify.EventFollowRequest, fmt.Sprintf("New follow request from %s (pending approval)", activity.Actor))
	}
	return nil
}

// processAccept handles Accept(Follow) activities.
func (h *InboxHandler) processAccept(ctx context.Context, activity *RawActivity) error {
	followType, ok := extractObjectType(activity.Object)
	if !ok {
		return fmt.Errorf("invalid Accept object")
	}

	if followType != ActivityFollow {
		return nil
	}

	outgoing, err := h.db.GetOutgoingFollow(ctx, activity.Actor)
	if err != nil || !outgoing.HasPendingOutgoingFollow() {
		h.logger.Warn("inbox: Accept(Follow) from actor we have no pending follow to", "actor", activity.Actor)
		return nil
	}

	if err := h.db.AcceptOutgoingFollow(ctx, activity.Actor); err != nil {
		h.logger.Warn("inbox: failed to record outgoing follow acceptance", "by", activity.Actor, "error", err)
	} else {
		h.logger.Info("inbox: outgoing follow accepted", "by", activity.Actor)
	}

	if h.autoAccept == AutoAcceptMutual {
		fr, err := h.db.GetFollowRequest(ctx, activity.Actor)
		if err == nil && fr != nil {
			if err := h.db.AcceptFollowRequest(ctx, activity.Actor); err != nil {
				h.logger.Warn("inbox: mutual auto-accept DB promotion failed", "from", activity.Actor, "error", err)
			} else if err := SendAccept(ctx, h.identity, activity.Actor, h.enqueue); err != nil {
				h.logger.Warn("inbox: mutual auto-accept delivery failed (accepted locally)", "from", activity.Actor, "error", err)
			} else {
				h.logger.Info("inbox: mutual auto-accepted pending inbound follow", "from", activity.Actor)
			}
		}
	}

	return nil
}

// processReject handles Reject(Follow) activities.
func (h *InboxHandler) processReject(ctx context.Context, activity *RawActivity) error {
	followType, ok := extractObjectType(activity.Object)
	if !ok || followType != ActivityFollow {
		return nil
	}

	if err := h.db.RejectOutgoingFollow(ctx, activity.Actor); err != nil {
		h.logger.Warn("inbox: failed to record outgoing follow rejection", "by", activity.Actor, "error", err)
	}

	if err := h.db.RejectFollowRequest(ctx, activity.Actor); err != nil {
		h.logger.Debug("inbox: no pending inbound follow request to reject", "actor", activity.Actor, "error", err)
	}

	h.logger.Info("inbox: follow rejected", "by", activity.Actor)
	return nil
}

// processUndo handles Undo(Follow) activities.
func (h *InboxHandler) processUndo(ctx context.Context, activity *RawActivity) error {
	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		return nil
	}

	undoType, _ := objectMap["type"].(string)
	if undoType != ActivityFollow {
		return nil
	}

	if err := h.db.RemoveFollow(ctx, activity.Actor); err != nil {
		h.logger.Debug("inbox: Undo(Follow) no-op", "actor", activity.Actor, "error", err)
	}

	h.logger.Info("inbox: follow undone", "by", activity.Actor)
	return nil
}

// followGone returns true when actorURL has no follow or pending request.
func (h *InboxHandler) followGone(ctx context.Context, actorURL string) bool {
	f, _ := h.db.GetFollow(ctx, actorURL)
	if f != nil {
		return false
	}
	fr, _ := h.db.GetFollowRequest(ctx, actorURL)
	return fr == nil
}

func (h *InboxHandler) shouldAutoAccept(ctx context.Context, actorURL string) bool {
	if h.autoAccept == AutoAcceptAll {
		return true
	}

	// Mutual: auto-accept if we have a pending or accepted outgoing follow.
	if h.autoAccept == AutoAcceptMutual {
		of, err := h.db.GetOutgoingFollow(ctx, actorURL)
		if err == nil && of.HasPendingOrAcceptedOutgoingFollow() {
			return true
		}
	}

	if len(h.allowedDomains) > 0 {
		u, err := url.Parse(actorURL)
		if err == nil {
			host := u.Hostname()
			for _, allowed := range h.allowedDomains {
				if matchesDomain(host, allowed) {
					return true
				}
			}
		}
	}

	return false
}
