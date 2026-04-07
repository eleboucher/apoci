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

type BlobReplicator interface {
	ReplicateBlob(ctx context.Context, peerEndpoint, digest string, size int64)
}

type InboxHandler struct {
	identity       *Identity
	db             *database.DB
	blobReplicator BlobReplicator

	maxManifestSize int64
	maxBlobSize     int64

	autoAccept     string
	allowedDomains []string
	blockedDomains map[string]struct{}
	blockedActors  map[string]struct{}
	actorLimiters  *ttlcache.Cache[string, *rate.Limiter]

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

	limiters := ttlcache.New[string, *rate.Limiter](
		ttlcache.WithTTL[string, *rate.Limiter](10 * time.Minute),
	)
	go limiters.Start()

	return &InboxHandler{
		identity:        identity,
		db:              db,
		maxManifestSize: cfg.MaxManifestSize,
		maxBlobSize:     cfg.MaxBlobSize,
		autoAccept:      cfg.AutoAccept,
		allowedDomains:  cfg.AllowedDomains,
		blockedDomains:  blockedDomainSet,
		blockedActors:   blockedActorSet,
		actorLimiters:   limiters,
		logger:          logger,
	}
}

func (h *InboxHandler) SetBlobReplicator(r BlobReplicator) {
	h.blobReplicator = r
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

	if err := VerifyRequest(r, pubKeyPEM, body); err != nil {
		h.logger.Warn("inbox: invalid signature", "actor", actorURL, "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var activity RawActivity
	if err := json.Unmarshal(body, &activity); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	if normaliseActorURL(activity.Actor) != normaliseActorURL(actorURL) {
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

	// Deduplication: skip activities we've already processed (spec MUST).
	existing, err := h.db.GetActivity(r.Context(), activity.ID)
	if err != nil {
		h.logger.Warn("inbox: failed to check activity dedup", "id", activity.ID, "error", err)
	}
	if err == nil && existing != nil {
		metrics.InboxDedupHits.Add(1)
		h.logger.Debug("inbox: duplicate activity, skipping", "id", activity.ID)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	metrics.InboxActivities.Add(activity.Type, 1)

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
		if err := SendAccept(ctx, h.identity, h.db, activity.Actor); err != nil {
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

	// An Accept(Follow) means a remote peer accepted our outgoing Follow request.
	// It does NOT auto-accept any incoming follow request from them — that requires
	// explicit operator approval via "apoci follow accept".
	if err := h.db.AcceptOutgoingFollow(ctx, activity.Actor); err != nil {
		h.logger.Warn("inbox: failed to record outgoing follow acceptance", "by", activity.Actor, "error", err)
	} else {
		h.logger.Info("inbox: outgoing follow accepted", "by", activity.Actor)
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

	if err := h.db.RejectOutgoingFollow(ctx, activity.Actor); err != nil {
		h.logger.Warn("inbox: failed to record outgoing follow rejection", "by", activity.Actor, "error", err)
	}

	if err := h.db.RejectFollowRequest(ctx, activity.Actor); err != nil {
		h.logger.Error("inbox: failed to reject follow request", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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
		h.logger.Error("inbox: failed to remove follow", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
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

	actor, err := FetchActor(ctx, actorURL)
	if err != nil {
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
func normaliseActorURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.RawQuery = ""
	u.Host = strings.ToLower(u.Host)
	u.Scheme = strings.ToLower(u.Scheme)
	return strings.TrimRight(u.String(), "/")
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
	default:
		h.logger.Debug("inbox: unhandled Update object type", "type", objType)
		rw.WriteHeader(http.StatusAccepted)
	}

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

	if size <= 0 || size > float64(h.maxManifestSize) {
		http.Error(w, "invalid manifest size", http.StatusBadRequest)
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
			// Continue with empty content; manifest pull-through will fetch it on demand.
		} else {
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
	item := h.actorLimiters.Get(actorURL)
	if item != nil {
		return item.Value().Allow()
	}
	lim := rate.NewLimiter(5, 20)
	h.actorLimiters.Set(actorURL, lim, ttlcache.DefaultTTL)
	return lim.Allow()
}

func (h *InboxHandler) shouldAutoAccept(ctx context.Context, actorURL string) bool {
	if h.autoAccept == "all" {
		return true
	}

	if h.autoAccept == "mutual" {
		of, err := h.db.GetOutgoingFollow(ctx, actorURL)
		if err == nil && of != nil && of.Status == "accepted" {
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
