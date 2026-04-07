package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	// PublicCollection is the special ActivityStreams address for public content.
	PublicCollection = "https://www.w3.org/ns/activitystreams#Public"

	maxConcurrentDeliveries = 50
)

const followerBatchSize = 100

type APPublisher struct {
	identity   *Identity
	db         *database.DB
	actorCache *ActorCache
	endpoint   string
	logger     *slog.Logger
	onEnqueue  func()
}

// SetNotifyFunc sets a callback invoked after deliveries are enqueued,
// allowing the delivery queue to wake up immediately.
func (p *APPublisher) SetNotifyFunc(fn func()) {
	p.onEnqueue = fn
}

func NewAPPublisher(ctx context.Context, identity *Identity, db *database.DB, endpoint string, logger *slog.Logger) *APPublisher {
	return &APPublisher{
		identity:   identity,
		db:         db,
		actorCache: NewActorCache(ctx, identity),
		endpoint:   endpoint,
		logger:     logger,
	}
}

func (p *APPublisher) PublishManifest(ctx context.Context, repo, digest, mediaType string, size int64, content []byte, subjectDigest *string) error {
	objectID := p.objectURL("manifest", digest)

	object := OCIManifest{
		Context:      ociContext(),
		Type:         "OCIManifest",
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    nowRFC3339(),
		Repository:   repo,
		Digest:       digest,
		MediaType:    mediaType,
		Size:         size,
		Content:      EncodeContent(content),
	}
	if subjectDigest != nil {
		object.SubjectDigest = *subjectDigest
	}

	return p.createAndDeliver(ctx, "Create", object)
}

func (p *APPublisher) PublishTag(ctx context.Context, repo, tag, digest string) error {
	objectID := p.objectURL("tag", repo+"/"+tag)

	object := OCITag{
		Context:      ociContext(),
		Type:         "OCITag",
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    nowRFC3339(),
		Repository:   repo,
		Tag:          tag,
		Digest:       digest,
	}

	return p.createAndDeliver(ctx, "Update", object)
}

func (p *APPublisher) PublishBlobRef(ctx context.Context, digest string, size int64) error {
	objectID := p.objectURL("blob", digest)

	object := OCIBlob{
		Context:      ociContext(),
		Type:         "OCIBlob",
		ID:           objectID,
		AttributedTo: p.identity.ActorURL,
		Published:    nowRFC3339(),
		Digest:       digest,
		Size:         size,
		Endpoint:     p.endpoint,
	}

	return p.createAndDeliver(ctx, "Announce", object)
}

// Stop releases background resources (actor cache eviction).
func (p *APPublisher) Stop() {
	p.actorCache.Stop()
}

func (p *APPublisher) createAndDeliver(ctx context.Context, activityType string, object any) error {
	metrics.OutboundActivities.Add(activityType, 1)
	activityID := p.activityURL()
	followersURL := p.endpoint + "/ap/followers"

	activity := map[string]any{
		"@context": ContextActivityStreams,
		"id":       activityID,
		"type":     activityType,
		"actor":    p.identity.ActorURL,
		"to":       []string{PublicCollection},
		"cc":       []string{followersURL},
		"object":   object,
	}

	activityJSON, err := json.Marshal(activity)
	if err != nil {
		return fmt.Errorf("marshaling activity: %w", err)
	}

	if err := p.db.PutActivity(ctx, activityID, activityType, p.identity.ActorURL, activityJSON); err != nil {
		return fmt.Errorf("storing activity: %w", err)
	}

	return p.enqueueToFollowers(ctx, activityID, activityJSON)
}

// enqueueToFollowers resolves follower inboxes (using shared inbox when available)
// and enqueues deliveries to the persistent delivery queue.
// Followers are loaded in batches to avoid unbounded memory usage.
func (p *APPublisher) enqueueToFollowers(ctx context.Context, activityID string, activityJSON []byte) error {
	// Deduplicate by shared inbox to reduce deliveries.
	inboxes := make(map[string]struct{})
	var afterID int64
	for {
		batch, err := p.db.ListFollowsBatch(ctx, afterID, followerBatchSize)
		if err != nil {
			return fmt.Errorf("listing followers: %w", err)
		}
		if len(batch) == 0 {
			break
		}
		for _, f := range batch {
			inbox, err := p.resolveInbox(ctx, f.ActorURL)
			if err != nil {
				p.logger.Warn("failed to resolve inbox for follower", "actor", f.ActorURL, "error", err)
				continue
			}
			inboxes[inbox] = struct{}{}
		}
		afterID = batch[len(batch)-1].ID
		if len(batch) < followerBatchSize {
			break
		}
	}

	for inbox := range inboxes {
		if err := p.db.EnqueueDelivery(ctx, activityID, inbox, activityJSON); err != nil {
			p.logger.Error("failed to enqueue delivery", "inbox", inbox, "error", err)
		} else {
			metrics.DeliveryEnqueued.Add(1)
		}
	}

	if len(inboxes) > 0 && p.onEnqueue != nil {
		p.onEnqueue()
	}

	return nil
}

// resolveInbox returns the shared inbox if available, otherwise the actor's personal inbox.
func (p *APPublisher) resolveInbox(ctx context.Context, actorURL string) (string, error) {
	actor, err := p.actorCache.Get(ctx, actorURL)
	if err != nil {
		return "", err
	}

	if sharedInbox, ok := actor.Endpoints["sharedInbox"]; ok && sharedInbox != "" {
		return sharedInbox, nil
	}

	return actor.Inbox, nil
}

func (p *APPublisher) objectURL(kind, ref string) string {
	return p.endpoint + "/ap/objects/" + kind + "/" + ref
}

func (p *APPublisher) activityURL() string {
	return p.endpoint + "/ap/activities/" + uuid.New().String()
}
