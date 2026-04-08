package activitypub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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

type BlobReplicator interface {
	ReplicateBlob(ctx context.Context, peerEndpoint, digest string, size int64)
}

type ActorInvalidator interface {
	Invalidate(actorURL string)
}

type InboxHandler struct {
	identity       *Identity
	db             *database.DB
	blobReplicator BlobReplicator
	actorCache     ActorInvalidator
	enqueue        EnqueueFunc

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

type InboxConfig struct {
	MaxManifestSize int64
	MaxBlobSize     int64
	AutoAccept      string
	AllowedDomains  []string
	BlockedDomains  []string
	BlockedActors   []string
}

func NewInboxHandler(identity *Identity, db *database.DB, cfg InboxConfig, logger *slog.Logger) *InboxHandler {
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
		h.logger.Warn("inbox: invalid signature", "actor", actorURL, "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
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
		metrics.InboxDedupHits.Add(1)
		h.logger.Debug("inbox: duplicate activity, skipping", "id", activity.ID)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	metrics.InboxActivities.WithLabelValues(activity.Type).Inc()

	switch activity.Type {
	case ActivityFollow:
		h.handleFollow(r.Context(), w, &activity, pubKeyPEM)
	case ActivityAccept:
		h.handleAccept(r.Context(), w, &activity)
	case ActivityReject:
		h.handleReject(r.Context(), w, &activity)
	case ActivityUndo:
		h.handleUndo(r.Context(), w, &activity)
	case ActivityCreate:
		h.handleCreate(r.Context(), w, &activity, body)
	case ActivityUpdate:
		h.handleUpdate(r.Context(), w, &activity, body)
	case ActivityAnnounce:
		h.handleAnnounce(r.Context(), w, &activity, body)
	case ActivityDelete:
		h.handleDelete(r.Context(), w, &activity)
	default:
		h.logger.Debug("inbox: unhandled activity type", "type", activity.Type)
		w.WriteHeader(http.StatusAccepted)
	}
}

func (h *InboxHandler) handleFollow(ctx context.Context, w http.ResponseWriter, activity *RawActivity, pubKeyPEM string) {
	target, ok := activity.Object.(string)
	if !ok {
		http.Error(w, "invalid Follow object", http.StatusBadRequest)
		return
	}

	if target != h.identity.ActorURL {
		http.Error(w, "follow target mismatch", http.StatusBadRequest)
		return
	}

	if activity.Actor == h.identity.ActorURL {
		http.Error(w, "cannot follow yourself", http.StatusBadRequest)
		return
	}

	actorEndpoint := EndpointFromActorURL(activity.Actor)
	if err := h.db.AddFollowRequest(ctx, activity.Actor, pubKeyPEM, actorEndpoint); err != nil {
		h.logger.Error("inbox: failed to store follow request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	activityJSON, err := json.Marshal(activity)
	if err != nil {
		h.logger.Error("inbox: failed to marshal Follow activity", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.storeActivity(ctx, activity.ID, ActivityFollow, activity.Actor, activityJSON)

	if h.shouldAutoAccept(ctx, activity.Actor) {
		if err := SendAccept(ctx, h.identity, h.db, activity.Actor, h.enqueue); err != nil {
			h.logger.Warn("inbox: auto-accept failed, request remains pending", "from", activity.Actor, "error", err)
		} else {
			h.logger.Info("inbox: auto-accepted follow request", "from", activity.Actor)
			w.WriteHeader(http.StatusAccepted)
			return
		}
	}

	h.logger.Info("inbox: received follow request (pending operator approval)", "from", activity.Actor)
	w.WriteHeader(http.StatusAccepted)
}

func (h *InboxHandler) handleAccept(ctx context.Context, w http.ResponseWriter, activity *RawActivity) {
	followType := ""

	switch obj := activity.Object.(type) {
	case map[string]any:
		followType, _ = obj["type"].(string)
	case string:
		// Some implementations send just the Follow activity ID as a string.
		// Treat it as accepting a Follow.
		followType = ActivityFollow
	default:
		http.Error(w, "invalid Accept object", http.StatusBadRequest)
		return
	}

	if followType != ActivityFollow {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Verify we actually have a pending outgoing follow to this actor before
	// accepting. Without this check, a spurious Accept(Follow) from any actor
	// with a valid signature would create an unauthorized follow relationship.
	outgoing, err := h.db.GetOutgoingFollow(ctx, activity.Actor)
	if err != nil || outgoing == nil || outgoing.Status != "pending" {
		h.logger.Warn("inbox: Accept(Follow) from actor we have no pending follow to", "actor", activity.Actor)
		w.WriteHeader(http.StatusAccepted) // silent accept per AP convention
		return
	}

	if err := h.db.AcceptOutgoingFollow(ctx, activity.Actor); err != nil {
		h.logger.Warn("inbox: failed to record outgoing follow acceptance", "by", activity.Actor, "error", err)
	} else {
		h.logger.Info("inbox: outgoing follow accepted", "by", activity.Actor)
	}

	// In mutual mode, auto-accept any pending inbound follow request from this
	// actor now that our outgoing follow has been accepted.
	if h.autoAccept == AutoAcceptMutual {
		fr, err := h.db.GetFollowRequest(ctx, activity.Actor)
		if err == nil && fr != nil {
			if err := SendAccept(ctx, h.identity, h.db, activity.Actor, h.enqueue); err != nil {
				h.logger.Warn("inbox: mutual auto-accept of pending inbound follow failed", "from", activity.Actor, "error", err)
			} else {
				h.logger.Info("inbox: mutual auto-accepted pending inbound follow", "from", activity.Actor)
			}
		}
	}

	activityJSON, err := json.Marshal(activity)
	if err != nil {
		h.logger.Error("inbox: failed to marshal Accept activity", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.storeActivity(ctx, activity.ID, ActivityAccept, activity.Actor, activityJSON)

	w.WriteHeader(http.StatusAccepted)
}

func (h *InboxHandler) handleReject(ctx context.Context, w http.ResponseWriter, activity *RawActivity) {
	followType := ""

	switch obj := activity.Object.(type) {
	case map[string]any:
		followType, _ = obj["type"].(string)
	case string:
		followType = ActivityFollow
	default:
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if followType != ActivityFollow {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// Peer rejected our outgoing follow.
	if err := h.db.RejectOutgoingFollow(ctx, activity.Actor); err != nil {
		h.logger.Warn("inbox: failed to record outgoing follow rejection", "by", activity.Actor, "error", err)
	}

	// Also reject any pending inbound request from them (best-effort, no-op if none exists).
	if err := h.db.RejectFollowRequest(ctx, activity.Actor); err != nil {
		h.logger.Debug("inbox: no pending inbound follow request to reject", "actor", activity.Actor, "error", err)
	}

	activityJSON, err := json.Marshal(activity)
	if err != nil {
		h.logger.Error("inbox: failed to marshal Reject activity", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.storeActivity(ctx, activity.ID, ActivityReject, activity.Actor, activityJSON)

	h.logger.Info("inbox: follow rejected", "by", activity.Actor)
	w.WriteHeader(http.StatusAccepted)
}

func (h *InboxHandler) handleUndo(ctx context.Context, w http.ResponseWriter, activity *RawActivity) {
	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	undoType, _ := objectMap["type"].(string)
	if undoType != ActivityFollow {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if err := h.db.RemoveFollow(ctx, activity.Actor); err != nil {
		// Not-found is expected when the follow was never accepted or already undone.
		// AP convention: acknowledge the Undo regardless.
		h.logger.Debug("inbox: Undo(Follow) no-op", "actor", activity.Actor, "error", err)
	}

	activityJSON, err := json.Marshal(activity)
	if err != nil {
		h.logger.Error("inbox: failed to marshal Undo activity", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.storeActivity(ctx, activity.ID, ActivityUndo, activity.Actor, activityJSON)

	h.logger.Info("inbox: follow undone", "by", activity.Actor)
	w.WriteHeader(http.StatusAccepted)
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

	// Only fetch from domains that are allowed (if an allowlist is configured).
	if len(h.allowedDomains) > 0 {
		u, err := url.Parse(actorURL)
		if err != nil {
			return "", fmt.Errorf("invalid actor URL: %w", err)
		}
		host := u.Hostname()
		allowed := false
		for _, d := range h.allowedDomains {
			if host == d || strings.HasSuffix(host, "."+d) {
				allowed = true
				break
			}
		}
		if !allowed {
			return "", fmt.Errorf("actor domain %q not in allowed list", host)
		}
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

func (h *InboxHandler) handleCreate(ctx context.Context, w http.ResponseWriter, activity *RawActivity, rawBody []byte) {
	if !h.isFollowed(ctx, activity.Actor) {
		h.logger.Warn("inbox: Create from non-followed actor", "actor", activity.Actor)
		http.Error(w, "not following actor", http.StatusForbidden)
		return
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		http.Error(w, "invalid Create object", http.StatusBadRequest)
		return
	}

	rw := &statusRecorder{ResponseWriter: w}
	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCIManifest":
		h.ingestManifest(ctx, rw, objectMap, activity.Actor)
	default:
		h.logger.Debug("inbox: unhandled Create object type", "type", objType)
		rw.WriteHeader(http.StatusAccepted)
	}

	if rw.status == 0 || rw.status < 400 {
		h.storeActivity(ctx, activity.ID, ActivityCreate, activity.Actor, rawBody)
	}
}

func (h *InboxHandler) handleUpdate(ctx context.Context, w http.ResponseWriter, activity *RawActivity, rawBody []byte) {
	if !h.isFollowed(ctx, activity.Actor) {
		http.Error(w, "not following actor", http.StatusForbidden)
		return
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		http.Error(w, "invalid Update object", http.StatusBadRequest)
		return
	}

	rw := &statusRecorder{ResponseWriter: w}
	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCITag":
		h.ingestTag(ctx, rw, objectMap, activity.Actor)
	case "Actor", "Person", "Service", "Application":
		if h.actorCache != nil {
			h.actorCache.Invalidate(activity.Actor)
		}
		rw.WriteHeader(http.StatusAccepted)
	default:
		h.logger.Debug("inbox: unhandled Update object type", "type", objType)
		rw.WriteHeader(http.StatusAccepted)
	}

	// storeActivity is called unconditionally below for all non-error responses;
	// the Actor invalidation case calls rw.WriteHeader directly but still reaches it.
	if rw.status == 0 || rw.status < 400 {
		h.storeActivity(ctx, activity.ID, ActivityUpdate, activity.Actor, rawBody)
	}
}

func (h *InboxHandler) handleAnnounce(ctx context.Context, w http.ResponseWriter, activity *RawActivity, rawBody []byte) {
	if !h.isFollowed(ctx, activity.Actor) {
		http.Error(w, "not following actor", http.StatusForbidden)
		return
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	rw := &statusRecorder{ResponseWriter: w}
	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCIBlob":
		h.ingestBlobRef(ctx, rw, objectMap, activity.Actor)
	default:
		h.logger.Debug("inbox: unhandled Announce object type", "type", objType)
		rw.WriteHeader(http.StatusAccepted)
	}

	if rw.status == 0 || rw.status < 400 {
		h.storeActivity(ctx, activity.ID, ActivityAnnounce, activity.Actor, rawBody)
	}
}

func (h *InboxHandler) handleDelete(ctx context.Context, w http.ResponseWriter, activity *RawActivity) {
	if !h.isFollowed(ctx, activity.Actor) {
		http.Error(w, "not following actor", http.StatusForbidden)
		return
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		// Object might be just the ID string of the deleted object.
		h.logger.Info("inbox: received Delete (id-only)", "from", activity.Actor)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	rw := &statusRecorder{ResponseWriter: w}
	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCIManifest":
		h.deleteManifest(ctx, rw, objectMap, activity.Actor)
	case "OCITag":
		h.deleteTag(ctx, rw, objectMap, activity.Actor)
	default:
		h.logger.Debug("inbox: unhandled Delete object type", "type", objType)
		rw.WriteHeader(http.StatusAccepted)
	}

	if rw.status == 0 || rw.status < 400 {
		activityJSON, err := json.Marshal(activity)
		if err != nil {
			h.logger.Error("inbox: failed to marshal Delete activity", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		h.storeActivity(ctx, activity.ID, ActivityDelete, activity.Actor, activityJSON)
	}
}

func (h *InboxHandler) deleteManifest(ctx context.Context, w http.ResponseWriter, obj map[string]any, actorURL string) {
	repo, _ := obj["ociRepository"].(string)
	digest, _ := obj["ociDigest"].(string)

	if repo == "" || digest == "" {
		http.Error(w, "missing ociRepository or ociDigest", http.StatusBadRequest)
		return
	}

	repoObj, err := h.db.GetRepository(ctx, repo)
	if err != nil || repoObj == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	isOwner, err := h.db.IsRepositoryOwner(ctx, repoObj.ID, actorURL)
	if err != nil || !isOwner {
		h.logger.Warn("inbox: rejected Delete manifest from non-owner", "repo", repo, "actor", actorURL)
		http.Error(w, "not authorized for repository", http.StatusForbidden)
		return
	}

	if err := h.db.DeleteManifest(ctx, repoObj.ID, digest); err != nil {
		h.logger.Error("inbox: failed to delete manifest", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.logger.Info("inbox: deleted manifest", "repo", repo, "digest", digest, "from", actorURL)
	w.WriteHeader(http.StatusAccepted)
}

func (h *InboxHandler) deleteTag(ctx context.Context, w http.ResponseWriter, obj map[string]any, actorURL string) {
	repo, _ := obj["ociRepository"].(string)
	tag, _ := obj["ociTag"].(string)

	if repo == "" || tag == "" {
		http.Error(w, "missing ociRepository or ociTag", http.StatusBadRequest)
		return
	}

	repoObj, err := h.db.GetRepository(ctx, repo)
	if err != nil || repoObj == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	isOwner, err := h.db.IsRepositoryOwner(ctx, repoObj.ID, actorURL)
	if err != nil || !isOwner {
		h.logger.Warn("inbox: rejected Delete tag from non-owner", "repo", repo, "actor", actorURL)
		http.Error(w, "not authorized for repository", http.StatusForbidden)
		return
	}

	if err := h.db.DeleteTag(ctx, repoObj.ID, tag); err != nil {
		if errors.Is(err, database.ErrTagImmutable) {
			h.logger.Warn("inbox: rejected delete of immutable tag", "repo", repo, "tag", tag, "actor", actorURL)
			http.Error(w, "tag is immutable", http.StatusForbidden)
			return
		}
		h.logger.Error("inbox: failed to delete tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.logger.Info("inbox: deleted tag", "repo", repo, "tag", tag, "from", actorURL)
	w.WriteHeader(http.StatusAccepted)
}

func (h *InboxHandler) ingestManifest(ctx context.Context, w http.ResponseWriter, obj map[string]any, actorURL string) {
	repo, _ := obj["ociRepository"].(string)
	digest, _ := obj["ociDigest"].(string)
	mediaType, _ := obj["ociMediaType"].(string)
	size, _ := obj["ociSize"].(float64)

	if repo == "" || digest == "" {
		http.Error(w, "missing ociRepository or ociDigest", http.StatusBadRequest)
		return
	}

	if err := validate.RepoName(repo); err != nil {
		http.Error(w, "invalid repository name", http.StatusBadRequest)
		return
	}

	if err := validate.Digest(digest); err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	if mediaType == "" || !validate.MediaType(mediaType) {
		http.Error(w, "invalid or missing ociMediaType", http.StatusBadRequest)
		return
	}

	if size <= 0 || size > float64(h.maxManifestSize) {
		http.Error(w, "invalid manifest size", http.StatusBadRequest)
		return
	}

	// Enforce that the repository name is scoped to the sender's namespace.
	// The namespace comes from the actor's ociNamespace field (split-domain),
	// falling back to the actor URL hostname for older nodes.
	senderNS, err := h.fetchSenderNamespace(ctx, actorURL)
	if err != nil {
		h.logger.Warn("inbox: cannot derive namespace from actor", "actor", actorURL, "error", err)
		http.Error(w, "invalid actor URL", http.StatusBadRequest)
		return
	}
	if !repoOwnedBySender(repo, senderNS) {
		h.logger.Warn("inbox: repo name does not match sender namespace",
			"repo", repo, "sender_namespace", senderNS, "actor", actorURL)
		http.Error(w, "repository name must be scoped to sender's domain", http.StatusForbidden)
		return
	}

	repoObj, err := h.db.GetRepository(ctx, repo)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if repoObj != nil {
		isOwner, err := h.db.IsRepositoryOwner(ctx, repoObj.ID, actorURL)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !isOwner {
			h.logger.Warn("inbox: rejected manifest from non-owner",
				"repo", repo, "actor", actorURL, "owner", repoObj.OwnerID)
			http.Error(w, "not authorized for repository", http.StatusForbidden)
			return
		}
	} else {
		repoObj, err = h.db.GetOrCreateRepository(ctx, repo, actorURL)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	subjectDigest, _ := obj["ociSubjectDigest"].(string)
	var subjectPtr *string
	if subjectDigest != "" {
		subjectPtr = &subjectDigest
	}

	content := []byte{}
	if encoded, _ := obj["ociContent"].(string); encoded != "" {
		decoded, err := DecodeContent(encoded)
		if err != nil {
			h.logger.Warn("inbox: invalid manifest content encoding", "error", err)
		} else {
			if int64(len(decoded)) > h.maxManifestSize {
				http.Error(w, "manifest content exceeds size limit", http.StatusBadRequest)
				return
			}
			h256 := sha256.Sum256(decoded)
			computed := "sha256:" + hex.EncodeToString(h256[:])
			if computed != digest {
				h.logger.Warn("inbox: manifest content digest mismatch",
					"claimed", digest, "computed", computed, "actor", actorURL)
				http.Error(w, "manifest content digest mismatch", http.StatusBadRequest)
				return
			}
			content = decoded
		}
	}

	m := &database.Manifest{
		RepositoryID:  repoObj.ID,
		Digest:        digest,
		MediaType:     mediaType,
		SizeBytes:     int64(size),
		Content:       content,
		SourceActor:   &actorURL,
		SubjectDigest: subjectPtr,
	}

	if err := h.db.PutManifest(ctx, m); err != nil {
		h.logger.Error("inbox: failed to store manifest", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.logger.Info("inbox: ingested manifest", "repo", repo, "digest", digest, "from", actorURL)
	w.WriteHeader(http.StatusAccepted)
}

func senderDomainFromActorURL(actorURL string) (string, error) {
	u, err := url.Parse(actorURL)
	if err != nil {
		return "", fmt.Errorf("parsing actor URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("actor URL has no host")
	}
	return strings.ToLower(host), nil
}

func repoOwnedBySender(repo, senderDomain string) bool {
	parts := strings.SplitN(repo, "/", 2)
	return strings.ToLower(parts[0]) == senderDomain
}

// fetchSenderNamespace returns the OCI namespace for a remote actor.
// It checks the actor's ociNamespace field (supports split-domain), falling
// back to the actor URL hostname for older nodes that don't advertise it.
func (h *InboxHandler) fetchSenderNamespace(ctx context.Context, actorURL string) (string, error) {
	if item := h.nsCache.Get(actorURL); item != nil {
		return item.Value(), nil
	}

	actor, err := FetchActor(ctx, actorURL)
	if err != nil {
		return senderDomainFromActorURL(actorURL)
	}

	ns := actor.OCINamespace
	if ns == "" {
		ns, err = senderDomainFromActorURL(actorURL)
		if err != nil {
			return "", err
		}
	}
	h.nsCache.Set(actorURL, ns, ttlcache.DefaultTTL)
	return ns, nil
}

func (h *InboxHandler) ingestTag(ctx context.Context, w http.ResponseWriter, obj map[string]any, actorURL string) {
	repo, _ := obj["ociRepository"].(string)
	tag, _ := obj["ociTag"].(string)
	digest, _ := obj["ociDigest"].(string)

	if repo == "" || tag == "" || digest == "" {
		http.Error(w, "missing ociRepository, ociTag, or ociDigest", http.StatusBadRequest)
		return
	}

	if err := validate.RepoName(repo); err != nil {
		http.Error(w, "invalid repository name", http.StatusBadRequest)
		return
	}

	if err := validate.Tag(tag); err != nil {
		http.Error(w, "invalid tag name", http.StatusBadRequest)
		return
	}

	if err := validate.Digest(digest); err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	repoObj, err := h.db.GetRepository(ctx, repo)
	if err != nil || repoObj == nil {
		w.WriteHeader(http.StatusAccepted) // ignore tag for unknown repo
		return
	}

	isOwner, err := h.db.IsRepositoryOwner(ctx, repoObj.ID, actorURL)
	if err != nil || !isOwner {
		h.logger.Warn("inbox: rejected tag from non-owner", "repo", repo, "actor", actorURL)
		http.Error(w, "not authorized for repository", http.StatusForbidden)
		return
	}

	manifest, err := h.db.GetManifestByDigest(ctx, repoObj.ID, digest)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if manifest == nil {
		h.logger.Warn("inbox: rejected tag pointing to unknown manifest", "repo", repo, "digest", digest, "actor", actorURL)
		http.Error(w, "manifest not found", http.StatusBadRequest)
		return
	}

	if err := h.db.PutTag(ctx, repoObj.ID, tag, digest); err != nil {
		h.logger.Error("inbox: failed to store tag", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	h.logger.Info("inbox: ingested tag", "repo", repo, "tag", tag, "from", actorURL)
	w.WriteHeader(http.StatusAccepted)
}

func (h *InboxHandler) ingestBlobRef(ctx context.Context, w http.ResponseWriter, obj map[string]any, actorURL string) {
	digest, _ := obj["ociDigest"].(string)
	size, _ := obj["ociSize"].(float64)
	endpoint, _ := obj["ociEndpoint"].(string)

	if digest == "" || endpoint == "" {
		http.Error(w, "missing ociDigest or ociEndpoint", http.StatusBadRequest)
		return
	}

	if err := validate.Digest(digest); err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	if err := validate.PeerEndpoint(endpoint); err != nil {
		http.Error(w, "invalid peer endpoint", http.StatusBadRequest)
		return
	}

	// Endpoint must match the sender's domain.
	senderDomain, err := senderDomainFromActorURL(actorURL)
	if err != nil {
		http.Error(w, "invalid actor URL", http.StatusBadRequest)
		return
	}
	endpointURL, err := url.Parse(endpoint)
	if err != nil || strings.ToLower(endpointURL.Hostname()) != senderDomain {
		h.logger.Warn("inbox: blob endpoint does not match sender domain",
			"endpoint", endpoint, "sender_domain", senderDomain, "actor", actorURL)
		http.Error(w, "endpoint must match sender domain", http.StatusForbidden)
		return
	}

	if size < 0 || size > float64(h.maxBlobSize) {
		http.Error(w, "invalid blob size", http.StatusBadRequest)
		return
	}

	if err := h.db.PutPeerBlob(ctx, actorURL, digest, endpoint); err != nil {
		h.logger.Error("inbox: failed to store peer blob", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if err := h.db.PutBlob(ctx, digest, int64(size), nil, false); err != nil {
		h.logger.Error("inbox: failed to store blob metadata", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if h.blobReplicator != nil {
		h.blobReplicator.ReplicateBlob(ctx, endpoint, digest, int64(size))
	}

	h.logger.Debug("inbox: ingested blob ref", "digest", digest, "from", actorURL)
	w.WriteHeader(http.StatusAccepted)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
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
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
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

func (h *InboxHandler) shouldAutoAccept(ctx context.Context, actorURL string) bool {
	if h.autoAccept == AutoAcceptAll {
		return true
	}

	// "mutual" triggers when we have an outgoing follow in any non-rejected state
	// (pending or accepted). This covers the simultaneous-follow case where both
	// sides send Follow at the same time and neither has accepted yet.
	if h.autoAccept == AutoAcceptMutual {
		of, err := h.db.GetOutgoingFollow(ctx, actorURL)
		if err == nil && of != nil && (of.Status == "pending" || of.Status == "accepted") {
			return true
		}
	}

	if len(h.allowedDomains) > 0 {
		u, err := url.Parse(actorURL)
		if err == nil {
			host := u.Hostname()
			for _, allowed := range h.allowedDomains {
				if host == allowed || strings.HasSuffix(host, "."+allowed) {
					return true
				}
			}
		}
	}

	return false
}
