package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"golang.org/x/time/rate"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

type Notifier interface {
	Send(event, message string)
}

type InboxRepository interface {
	GetActivity(ctx context.Context, activityID string) (*database.Activity, error)
	PutActivity(ctx context.Context, activityID, activityType, actorURL string, activityJSON []byte) error
	AddFollowRequest(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error
	AcceptFollowRequest(ctx context.Context, actorURL string) error
	RejectFollowRequest(ctx context.Context, actorURL string) error
	GetFollowRequest(ctx context.Context, actorURL string) (*database.FollowRequest, error)
	GetFollow(ctx context.Context, actorURL string) (*database.Follow, error)
	RemoveFollow(ctx context.Context, actorURL string) error
	GetOutgoingFollow(ctx context.Context, actorURL string) (*database.OutgoingFollow, error)
	AcceptOutgoingFollow(ctx context.Context, actorURL string) error
	RejectOutgoingFollow(ctx context.Context, actorURL string) error
	GetRepository(ctx context.Context, name string) (*database.Repository, error)
	GetOrCreateRepository(ctx context.Context, name, ownerDID string) (*database.Repository, error)
	IsRepositoryOwner(ctx context.Context, repoID int64, did string) (bool, error)
	PutManifest(ctx context.Context, m *database.Manifest) error
	GetManifestByDigest(ctx context.Context, repoID int64, digest string) (*database.Manifest, error)
	PutManifestLayers(ctx context.Context, manifestID int64, blobDigests []string) error
	DeleteManifest(ctx context.Context, repoID int64, digest string) error
	RecordDeletedManifest(ctx context.Context, digest, repoName, sourceActor string) error
	PutTag(ctx context.Context, repoID int64, name, manifestDigest string) error
	DeleteTag(ctx context.Context, repoID int64, name string) error
	FindPeersWithBlob(ctx context.Context, digest string) ([]database.PeerBlob, error)
	GetBlob(ctx context.Context, digest string) (*database.Blob, error)
	PutBlob(ctx context.Context, digest string, sizeBytes int64, mediaType *string, storedLocally bool) error
	PutPeerBlob(ctx context.Context, peerActor, blobDigest, peerEndpoint string) error
}

type BlobReplicator interface {
	ReplicateBlob(ctx context.Context, peerEndpoint, digest string, size int64)
}

type ActorInvalidator interface {
	Invalidate(actorURL string)
}

// ActivityEnqueuer pushes validated activities for async processing.
type ActivityEnqueuer interface {
	Enqueue(task InboxTask) bool
}

type InboxHandler struct {
	identity       *Identity
	db             InboxRepository
	blobReplicator BlobReplicator
	actorCache     ActorInvalidator
	enqueue        EnqueueFunc
	worker         ActivityEnqueuer
	notifier       Notifier

	maxManifestSize int64
	maxBlobSize     int64

	autoAccept     string
	allowedDomains []string
	blockedDomains map[string]struct{}
	blockedActors  map[string]struct{}
	actorLimiters  *ttlcache.Cache[string, *rate.Limiter]
	domainLimiters *ttlcache.Cache[string, *rate.Limiter]
	fetchFailures  *ttlcache.Cache[string, struct{}]
	nsCache        *ttlcache.Cache[string, string]
	sigCache       *SignatureCache

	logger *slog.Logger
}

// InboxTask is a validated activity ready for processing.
type InboxTask struct {
	Activity  RawActivity
	PubKeyPEM string
	RawBody   []byte
}

type InboxConfig struct {
	MaxManifestSize int64
	MaxBlobSize     int64
	AutoAccept      string
	AllowedDomains  []string
	BlockedDomains  []string
	BlockedActors   []string
}

func NewInboxHandler(identity *Identity, db InboxRepository, cfg InboxConfig, logger *slog.Logger) *InboxHandler {
	blockedDomainSet := make(map[string]struct{}, len(cfg.BlockedDomains))
	for _, d := range cfg.BlockedDomains {
		blockedDomainSet[d] = struct{}{}
	}
	blockedActorSet := make(map[string]struct{}, len(cfg.BlockedActors))
	for _, a := range cfg.BlockedActors {
		blockedActorSet[a] = struct{}{}
	}

	actorLimiters := ttlcache.New[string, *rate.Limiter](
		ttlcache.WithTTL[string, *rate.Limiter](10 * time.Minute),
	)
	go actorLimiters.Start()

	domainLimiters := ttlcache.New[string, *rate.Limiter](
		ttlcache.WithTTL[string, *rate.Limiter](10 * time.Minute),
	)
	go domainLimiters.Start()

	fetchFailures := ttlcache.New[string, struct{}](
		ttlcache.WithTTL[string, struct{}](5 * time.Minute),
	)
	go fetchFailures.Start()

	nsCache := ttlcache.New[string, string](
		ttlcache.WithTTL[string, string](1 * time.Hour),
	)
	go nsCache.Start()

	return &InboxHandler{
		identity:        identity,
		db:              db,
		maxManifestSize: cfg.MaxManifestSize,
		maxBlobSize:     cfg.MaxBlobSize,
		autoAccept:      cfg.AutoAccept,
		allowedDomains:  cfg.AllowedDomains,
		blockedDomains:  blockedDomainSet,
		blockedActors:   blockedActorSet,
		actorLimiters:   actorLimiters,
		domainLimiters:  domainLimiters,
		fetchFailures:   fetchFailures,
		nsCache:         nsCache,
		sigCache:        NewSignatureCache(),
		logger:          logger,
	}
}

func (h *InboxHandler) Stop() {
	h.actorLimiters.Stop()
	h.domainLimiters.Stop()
	h.fetchFailures.Stop()
	h.nsCache.Stop()
	h.sigCache.Stop()
}

func (h *InboxHandler) SetBlobReplicator(r BlobReplicator) {
	h.blobReplicator = r
}

func (h *InboxHandler) SetActorCache(c ActorInvalidator) {
	h.actorCache = c
}

func (h *InboxHandler) SetEnqueueFunc(fn EnqueueFunc) {
	h.enqueue = fn
}

func (h *InboxHandler) SetWorker(w ActivityEnqueuer) {
	h.worker = w
}

func (h *InboxHandler) SetNotifier(n Notifier) {
	h.notifier = n
}

// SetNamespaceForActor pre-populates the namespace cache for a given actor,
// bypassing the actor fetch. Intended for testing.
func (h *InboxHandler) SetNamespaceForActor(actorURL, namespace string) {
	h.nsCache.Set(actorURL, namespace, ttlcache.DefaultTTL)
}

const (
	ActivityFollow   = "Follow"
	ActivityAccept   = "Accept"
	ActivityReject   = "Reject"
	ActivityUndo     = "Undo"
	ActivityCreate   = "Create"
	ActivityUpdate   = "Update"
	ActivityAnnounce = "Announce"
	ActivityDelete   = "Delete"

	AutoAcceptNone   = "none"
	AutoAcceptAll    = "all"
	AutoAcceptMutual = "mutual"
)

type RawActivity struct {
	Context any    `json:"@context,omitempty"`
	ID      string `json:"id"`
	Type    string `json:"type"`
	Actor   string `json:"actor"`
	Object  any    `json:"object"`
}

func (h *InboxHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if ct := r.Header.Get("Content-Type"); !isActivityPubContentType(ct) {
		http.Error(w, "unsupported content type", http.StatusUnsupportedMediaType)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 256*1024))
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	keyID, err := ExtractKeyID(r)
	if err != nil {
		h.logger.Warn("inbox: missing signature", "error", err)
		http.Error(w, "missing HTTP signature", http.StatusUnauthorized)
		return
	}

	actorURL := keyIDToActorURL(keyID)
	pubKeyPEM, err := h.fetchActorPublicKey(r.Context(), actorURL)
	if err != nil {
		h.logger.Warn("inbox: failed to fetch actor key", "actor", actorURL, "error", err)
		http.Error(w, "failed to verify signature", http.StatusUnauthorized)
		return
	}

	if err := VerifyRequest(r, pubKeyPEM, body, h.sigCache); err != nil {
		// The cached key may be stale (e.g. the remote actor rotated keys).
		// Re-fetch the actor and retry verification once.
		freshPEM, fetchErr := h.refetchActorPublicKey(r.Context(), actorURL)
		if fetchErr != nil || freshPEM == pubKeyPEM {
			h.logger.Warn("inbox: invalid signature", "actor", actorURL, "error", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		if err := VerifyRequest(r, freshPEM, body, h.sigCache); err != nil {
			h.logger.Warn("inbox: invalid signature after key refetch", "actor", actorURL, "error", err)
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		pubKeyPEM = freshPEM
		h.logger.Info("inbox: signature verified after key refetch", "actor", actorURL)
	}

	var activity RawActivity
	if err := json.Unmarshal(body, &activity); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	normClaimed, err := normaliseActorURL(activity.Actor)
	if err != nil {
		http.Error(w, "invalid actor URL", http.StatusBadRequest)
		return
	}
	normSigned, err := normaliseActorURL(actorURL)
	if err != nil {
		http.Error(w, "invalid actor URL", http.StatusBadRequest)
		return
	}
	if normClaimed != normSigned {
		h.logger.Warn("inbox: actor mismatch", "signed", actorURL, "claimed", activity.Actor)
		http.Error(w, "actor mismatch", http.StatusForbidden)
		return
	}

	if h.isBlocked(activity.Actor) {
		h.logger.Debug("inbox: dropped activity from blocked actor", "actor", activity.Actor)
		w.WriteHeader(http.StatusAccepted) // silent drop — don't reveal block
		return
	}

	if !h.actorAllowed(activity.Actor) {
		metrics.InboxRateLimited.Add(1)
		h.logger.Warn("inbox: actor rate limited", "actor", activity.Actor)
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	if activity.ID == "" {
		http.Error(w, "activity missing id", http.StatusBadRequest)
		return
	}

	if err := validate.ActivityID(activity.ID); err != nil {
		http.Error(w, "activity ID too long", http.StatusBadRequest)
		return
	}

	// Dedup: AP spec requires processing an activity at most once.
	existing, err := h.db.GetActivity(r.Context(), activity.ID)
	if err != nil {
		h.logger.Error("inbox: failed to check activity dedup", "id", activity.ID, "error", err)
		http.Error(w, "temporary error", http.StatusServiceUnavailable)
		return
	}
	if existing != nil {
		// Follow uses deterministic IDs (actor#follow-target), so a re-follow
		// after removal reuses the same ID. Allow reprocessing when the
		// relationship no longer exists.
		if activity.Type == ActivityFollow && h.followGone(r.Context(), activity.Actor) {
			h.logger.Info("inbox: re-processing Follow (previous relationship removed)", "from", activity.Actor)
		} else {
			metrics.InboxDedupHits.Add(1)
			h.logger.Debug("inbox: duplicate activity, skipping", "id", activity.ID)
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}

	metrics.InboxActivities.WithLabelValues(activity.Type).Inc()

	task := InboxTask{
		Activity:  activity,
		PubKeyPEM: pubKeyPEM,
		RawBody:   body,
	}

	if h.worker != nil {
		if !h.worker.Enqueue(task) {
			h.logger.Warn("inbox: worker queue full", "id", activity.ID)
			http.Error(w, "busy, retry later", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// No async worker configured, process synchronously.
	h.storeActivity(r.Context(), activity.ID, activity.Type, activity.Actor, body)
	if err := h.dispatch(r.Context(), task); err != nil {
		h.logger.Warn("inbox: processing failed", "type", activity.Type, "id", activity.ID, "error", err)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (h *InboxHandler) dispatch(ctx context.Context, task InboxTask) error {
	switch task.Activity.Type {
	case ActivityFollow:
		return h.processFollow(ctx, &task.Activity, task.PubKeyPEM)
	case ActivityAccept:
		return h.processAccept(ctx, &task.Activity)
	case ActivityReject:
		return h.processReject(ctx, &task.Activity)
	case ActivityUndo:
		return h.processUndo(ctx, &task.Activity)
	case ActivityCreate:
		return h.processCreate(ctx, &task.Activity)
	case ActivityUpdate:
		return h.processUpdate(ctx, &task.Activity)
	case ActivityAnnounce:
		return h.processAnnounce(ctx, &task.Activity)
	case ActivityDelete:
		return h.processDelete(ctx, &task.Activity)
	default:
		h.logger.Debug("inbox: unhandled activity type", "type", task.Activity.Type)
		return nil
	}
}

func (h *InboxHandler) fetchActorPublicKey(ctx context.Context, actorURL string) (string, error) {
	follow, err := h.db.GetFollow(ctx, actorURL)
	if err == nil && follow != nil && follow.PublicKeyPEM != "" {
		return follow.PublicKeyPEM, nil
	}

	fr, err := h.db.GetFollowRequest(ctx, actorURL)
	if err == nil && fr != nil && fr.PublicKeyPEM != "" {
		return fr.PublicKeyPEM, nil
	}

	return h.doFetchActorKey(ctx, actorURL)
}

// refetchActorPublicKey bypasses the local follow/request cache and fetches
// directly from the remote actor. Used for key rotation retry.
func (h *InboxHandler) refetchActorPublicKey(ctx context.Context, actorURL string) (string, error) {
	return h.doFetchActorKey(ctx, actorURL)
}

func (h *InboxHandler) doFetchActorKey(ctx context.Context, actorURL string) (string, error) {
	if err := h.checkActorDomainAllowed(actorURL); err != nil {
		return "", err
	}

	if h.fetchFailures.Has(actorURL) {
		return "", fmt.Errorf("actor %s recently failed to fetch (cached)", actorURL)
	}

	actor, err := FetchActor(ctx, actorURL)
	if err != nil {
		h.fetchFailures.Set(actorURL, struct{}{}, ttlcache.DefaultTTL)
		return "", err
	}
	return actor.PublicKey.PublicKeyPEM, nil
}

// checkActorDomainAllowed returns an error if the actor URL's domain is not
// in the allowed list (when an allowlist is configured).
func (h *InboxHandler) checkActorDomainAllowed(actorURL string) error {
	if len(h.allowedDomains) == 0 {
		return nil
	}
	u, err := url.Parse(actorURL)
	if err != nil {
		return fmt.Errorf("invalid actor URL: %w", err)
	}
	host := u.Hostname()
	for _, d := range h.allowedDomains {
		if matchesDomain(host, d) {
			return nil
		}
	}
	return fmt.Errorf("actor domain %q not in allowed list", host)
}

func keyIDToActorURL(keyID string) string {
	if base, _, ok := strings.Cut(keyID, "#"); ok {
		return base
	}
	return keyID
}

// normaliseActorURL strips trailing slashes and lowercases the scheme+host
// so that URL comparison is not sensitive to minor formatting differences.
func normaliseActorURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid actor URL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("actor URL %q missing scheme or host", raw)
	}
	u.Fragment = ""
	u.RawQuery = ""
	u.Host = strings.ToLower(u.Host)
	u.Scheme = strings.ToLower(u.Scheme)
	return strings.TrimRight(u.String(), "/"), nil
}

// extractObjectType returns the "type" field from the activity object.
// It handles both map objects and bare string references (some implementations
// send just the activity ID as a string — treated as a Follow).
// Returns false if the object type is unrecognised.
func extractObjectType(obj any) (string, bool) {
	switch v := obj.(type) {
	case map[string]any:
		t, _ := v["type"].(string)
		return t, true
	case string:
		return ActivityFollow, true
	default:
		return "", false
	}
}

// matchesDomain reports whether host equals domain or is a subdomain of it.
func matchesDomain(host, domain string) bool {
	return host == domain || strings.HasSuffix(host, "."+domain)
}

func (h *InboxHandler) storeActivity(ctx context.Context, activityID, activityType, actorURL string, body []byte) {
	if err := h.db.PutActivity(ctx, activityID, activityType, actorURL, body); err != nil {
		h.logger.Warn("inbox: failed to store activity for dedup",
			"activity_id", activityID,
			"type", activityType,
			"error", err,
		)
	}
}

func (h *InboxHandler) isFollowed(ctx context.Context, actorURL string) bool {
	follow, err := h.db.GetFollow(ctx, actorURL)
	return err == nil && follow != nil
}

func (h *InboxHandler) isBlocked(actorURL string) bool {
	if _, ok := h.blockedActors[actorURL]; ok {
		return true
	}
	u, err := url.Parse(actorURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	for blocked := range h.blockedDomains {
		if matchesDomain(host, blocked) {
			return true
		}
	}
	return false
}

func (h *InboxHandler) actorAllowed(actorURL string) bool {
	if !h.checkLimiter(h.actorLimiters, actorURL, 5, 20) {
		return false
	}
	// Per-domain budget prevents bypassing per-actor limits via actor rotation.
	u, err := url.Parse(actorURL)
	if err != nil {
		return false
	}
	domain := u.Hostname()
	return h.checkLimiter(h.domainLimiters, domain, 20, 100)
}

func (h *InboxHandler) checkLimiter(cache *ttlcache.Cache[string, *rate.Limiter], key string, r rate.Limit, burst int) bool {
	item, _ := cache.GetOrSet(key, rate.NewLimiter(r, burst))
	return item.Value().Allow()
}

// followGone returns true when actorURL has no follow or pending request.

// isActivityPubContentType returns true if ct is a valid ActivityPub content
// type. Both application/activity+json and application/ld+json (with or
// without a profile parameter) are accepted per the AP spec.
func isActivityPubContentType(ct string) bool {
	// Trim optional parameters (e.g. charset, profile).
	mediaType, _, _ := strings.Cut(ct, ";")
	mediaType = strings.TrimSpace(mediaType)
	return mediaType == "application/activity+json" || mediaType == "application/ld+json"
}
