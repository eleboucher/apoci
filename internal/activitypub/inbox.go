package activitypub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

type ActorInvalidator interface {
	Invalidate(actorURL string)
}

// ActivityEnqueuer pushes validated activities for async processing.
type ActivityEnqueuer interface {
	Enqueue(task InboxTask) bool
}

type InboxHandler struct {
	identity       *Identity
	db             *database.DB
	blobReplicator BlobReplicator
	actorCache     ActorInvalidator
	enqueue        EnqueueFunc
	worker         ActivityEnqueuer

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

func (h *InboxHandler) SetWorker(w ActivityEnqueuer) {
	h.worker = w
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
	if err := h.db.AddFollowRequest(ctx, activity.Actor, pubKeyPEM, actorEndpoint); err != nil {
		return fmt.Errorf("storing follow request: %w", err)
	}

	if h.shouldAutoAccept(ctx, activity.Actor) {
		if err := SendAccept(ctx, h.identity, h.db, activity.Actor, h.enqueue); err != nil {
			h.logger.Warn("inbox: auto-accept failed, request remains pending", "from", activity.Actor, "error", err)
		} else {
			h.logger.Info("inbox: auto-accepted follow request", "from", activity.Actor)
			return nil
		}
	}

	h.logger.Info("inbox: received follow request (pending operator approval)", "from", activity.Actor)
	return nil
}

func (h *InboxHandler) processAccept(ctx context.Context, activity *RawActivity) error {
	followType, ok := extractObjectType(activity.Object)
	if !ok {
		return fmt.Errorf("invalid Accept object")
	}

	if followType != ActivityFollow {
		return nil
	}

	outgoing, err := h.db.GetOutgoingFollow(ctx, activity.Actor)
	if err != nil || outgoing == nil || outgoing.Status != "pending" {
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
			if err := SendAccept(ctx, h.identity, h.db, activity.Actor, h.enqueue); err != nil {
				h.logger.Warn("inbox: mutual auto-accept of pending inbound follow failed", "from", activity.Actor, "error", err)
			} else {
				h.logger.Info("inbox: mutual auto-accepted pending inbound follow", "from", activity.Actor)
			}
		}
	}

	return nil
}

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
			if matchesDomain(host, d) {
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

func (h *InboxHandler) processCreate(ctx context.Context, activity *RawActivity) error {
	if !h.isFollowed(ctx, activity.Actor) {
		return fmt.Errorf("not following actor %s", activity.Actor)
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid Create object")
	}

	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCIManifest":
		return h.ingestManifest(ctx, objectMap, activity.Actor)
	default:
		h.logger.Debug("inbox: unhandled Create object type", "type", objType)
		return nil
	}
}

func (h *InboxHandler) processUpdate(ctx context.Context, activity *RawActivity) error {
	if !h.isFollowed(ctx, activity.Actor) {
		return fmt.Errorf("not following actor %s", activity.Actor)
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid Update object")
	}

	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCITag":
		return h.ingestTag(ctx, objectMap, activity.Actor)
	case "Actor", "Person", "Service", "Application":
		if h.actorCache != nil {
			h.actorCache.Invalidate(activity.Actor)
		}
		return nil
	default:
		h.logger.Debug("inbox: unhandled Update object type", "type", objType)
		return nil
	}
}

func (h *InboxHandler) processAnnounce(ctx context.Context, activity *RawActivity) error {
	if !h.isFollowed(ctx, activity.Actor) {
		return fmt.Errorf("not following actor %s", activity.Actor)
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		return nil
	}

	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCIBlob":
		return h.ingestBlobRef(ctx, objectMap, activity.Actor)
	default:
		h.logger.Debug("inbox: unhandled Announce object type", "type", objType)
		return nil
	}
}

func (h *InboxHandler) processDelete(ctx context.Context, activity *RawActivity) error {
	if !h.isFollowed(ctx, activity.Actor) {
		return fmt.Errorf("not following actor %s", activity.Actor)
	}

	objectMap, ok := activity.Object.(map[string]any)
	if !ok {
		h.logger.Info("inbox: received Delete (id-only)", "from", activity.Actor)
		return nil
	}

	objType, _ := objectMap["type"].(string)
	switch objType {
	case "OCIManifest":
		return h.deleteManifest(ctx, objectMap, activity.Actor)
	case "OCITag":
		return h.deleteTag(ctx, objectMap, activity.Actor)
	default:
		h.logger.Debug("inbox: unhandled Delete object type", "type", objType)
		return nil
	}
}

func (h *InboxHandler) requireRepoOwner(ctx context.Context, repo, actorURL string) (*database.Repository, error) {
	repoObj, err := h.db.GetRepository(ctx, repo)
	if err != nil || repoObj == nil {
		return nil, nil // repo doesn't exist yet, not an error
	}
	isOwner, err := h.db.IsRepositoryOwner(ctx, repoObj.ID, actorURL)
	if err != nil || !isOwner {
		return nil, fmt.Errorf("not authorized for repository %s", repo)
	}
	return repoObj, nil
}

func (h *InboxHandler) deleteManifest(ctx context.Context, obj map[string]any, actorURL string) error {
	repo, _ := obj["ociRepository"].(string)
	digest, _ := obj["ociDigest"].(string)

	if repo == "" || digest == "" {
		return fmt.Errorf("missing ociRepository or ociDigest")
	}

	repoObj, err := h.requireRepoOwner(ctx, repo, actorURL)
	if err != nil {
		return err
	}
	if repoObj == nil {
		return nil
	}

	if err := h.db.DeleteManifest(ctx, repoObj.ID, digest); err != nil {
		return fmt.Errorf("deleting manifest: %w", err)
	}

	h.logger.Info("inbox: deleted manifest", "repo", repo, "digest", digest, "from", actorURL)
	return nil
}

func (h *InboxHandler) deleteTag(ctx context.Context, obj map[string]any, actorURL string) error {
	repo, _ := obj["ociRepository"].(string)
	tag, _ := obj["ociTag"].(string)

	if repo == "" || tag == "" {
		return fmt.Errorf("missing ociRepository or ociTag")
	}

	repoObj, err := h.requireRepoOwner(ctx, repo, actorURL)
	if err != nil {
		return err
	}
	if repoObj == nil {
		return nil
	}

	if err := h.db.DeleteTag(ctx, repoObj.ID, tag); err != nil {
		return fmt.Errorf("deleting tag: %w", err)
	}

	h.logger.Info("inbox: deleted tag", "repo", repo, "tag", tag, "from", actorURL)
	return nil
}

func (h *InboxHandler) ingestManifest(ctx context.Context, obj map[string]any, actorURL string) error {
	repo, _ := obj["ociRepository"].(string)
	digest, _ := obj["ociDigest"].(string)
	mediaType, _ := obj["ociMediaType"].(string)
	size, _ := obj["ociSize"].(float64)
	tag, _ := obj["ociTag"].(string)

	if repo == "" || digest == "" {
		return fmt.Errorf("missing ociRepository or ociDigest")
	}
	if err := validate.RepoName(repo); err != nil {
		return fmt.Errorf("invalid repository name: %w", err)
	}
	if err := validate.Digest(digest); err != nil {
		return fmt.Errorf("invalid digest: %w", err)
	}
	if mediaType == "" || !validate.MediaType(mediaType) {
		return fmt.Errorf("invalid or missing ociMediaType")
	}
	if size <= 0 || size > float64(h.maxManifestSize) {
		return fmt.Errorf("invalid manifest size")
	}
	if tag != "" {
		if err := validate.Tag(tag); err != nil {
			return fmt.Errorf("invalid tag: %w", err)
		}
	}

	senderNS, err := h.fetchSenderNamespace(ctx, actorURL)
	if err != nil {
		return fmt.Errorf("cannot derive namespace from actor: %w", err)
	}
	if !repoOwnedBySender(repo, senderNS) {
		return fmt.Errorf("repository %s not scoped to sender namespace %s", repo, senderNS)
	}

	repoObj, err := h.db.GetRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("looking up repository: %w", err)
	}

	if repoObj != nil {
		isOwner, err := h.db.IsRepositoryOwner(ctx, repoObj.ID, actorURL)
		if err != nil {
			return fmt.Errorf("checking repo ownership: %w", err)
		}
		if !isOwner {
			return fmt.Errorf("not authorized for repository %s", repo)
		}
	} else {
		repoObj, err = h.db.GetOrCreateRepository(ctx, repo, actorURL)
		if err != nil {
			return fmt.Errorf("creating repository: %w", err)
		}
	}

	subjectDigest, _ := obj["ociSubjectDigest"].(string)
	var subjectPtr *string
	if subjectDigest != "" {
		subjectPtr = &subjectDigest
	}

	var content []byte
	if encoded, _ := obj["ociContent"].(string); encoded != "" {
		decoded, err := DecodeContent(encoded)
		if err != nil {
			h.logger.Warn("inbox: invalid manifest content encoding", "error", err)
		} else {
			if int64(len(decoded)) > h.maxManifestSize {
				return fmt.Errorf("manifest content exceeds size limit")
			}
			h256 := sha256.Sum256(decoded)
			computed := "sha256:" + hex.EncodeToString(h256[:])
			if computed != digest {
				return fmt.Errorf("manifest content digest mismatch: claimed %s, computed %s", digest, computed)
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
		return fmt.Errorf("storing manifest: %w", err)
	}

	// Store the tag atomically with the manifest when the sender includes it in
	// the Create activity. This avoids the race where Update(OCITag) arrives and
	// is processed before Create(OCIManifest) has committed.
	if tag != "" {
		if err := h.db.PutTag(ctx, repoObj.ID, tag, digest); err != nil {
			return fmt.Errorf("storing tag: %w", err)
		}
		h.logger.Info("inbox: ingested manifest", "repo", repo, "tag", tag, "digest", digest, "from", actorURL)
		return nil
	}

	h.logger.Info("inbox: ingested manifest", "repo", repo, "digest", digest, "from", actorURL)
	return nil
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
// The claimed namespace is validated: it must be the actor's hostname or a
// parent domain of it (e.g. registry.example.com may claim example.com).
func (h *InboxHandler) fetchSenderNamespace(ctx context.Context, actorURL string) (string, error) {
	if item := h.nsCache.Get(actorURL); item != nil {
		return item.Value(), nil
	}

	actorHost, err := senderDomainFromActorURL(actorURL)
	if err != nil {
		return "", err
	}

	actor, fetchErr := FetchActor(ctx, actorURL)
	if fetchErr != nil {
		h.nsCache.Set(actorURL, actorHost, ttlcache.DefaultTTL)
		return actorHost, nil
	}

	ns := actor.OCINamespace
	if ns == "" || !validNamespaceForHost(ns, actorHost) {
		ns = actorHost
	}
	h.nsCache.Set(actorURL, ns, ttlcache.DefaultTTL)
	return ns, nil
}

// validNamespaceForHost checks that ns is the host itself or a parent domain.
// e.g. host "registry.example.com" may claim "example.com" or "registry.example.com",
// but not "evil.com".
func validNamespaceForHost(ns, actorHost string) bool {
	return matchesDomain(actorHost, ns)
}

func (h *InboxHandler) ingestTag(ctx context.Context, obj map[string]any, actorURL string) error {
	repo, _ := obj["ociRepository"].(string)
	tag, _ := obj["ociTag"].(string)
	digest, _ := obj["ociDigest"].(string)

	if repo == "" || tag == "" || digest == "" {
		return fmt.Errorf("missing ociRepository, ociTag, or ociDigest")
	}
	if err := validate.RepoName(repo); err != nil {
		return fmt.Errorf("invalid repository name: %w", err)
	}
	if err := validate.Tag(tag); err != nil {
		return fmt.Errorf("invalid tag name: %w", err)
	}
	if err := validate.Digest(digest); err != nil {
		return fmt.Errorf("invalid digest: %w", err)
	}

	repoObj, err := h.requireRepoOwner(ctx, repo, actorURL)
	if err != nil {
		return err
	}
	if repoObj == nil {
		return nil
	}

	manifest, err := h.db.GetManifestByDigest(ctx, repoObj.ID, digest)
	if err != nil {
		return fmt.Errorf("looking up manifest: %w", err)
	}
	if manifest == nil {
		return fmt.Errorf("manifest %s not found in repo %s", digest, repo)
	}

	if err := h.db.PutTag(ctx, repoObj.ID, tag, digest); err != nil {
		return fmt.Errorf("storing tag: %w", err)
	}

	h.logger.Info("inbox: ingested tag", "repo", repo, "tag", tag, "from", actorURL)
	return nil
}

func (h *InboxHandler) ingestBlobRef(ctx context.Context, obj map[string]any, actorURL string) error {
	digest, _ := obj["ociDigest"].(string)
	size, _ := obj["ociSize"].(float64)
	endpoint, _ := obj["ociEndpoint"].(string)

	if digest == "" || endpoint == "" {
		return fmt.Errorf("missing ociDigest or ociEndpoint")
	}
	if err := validate.Digest(digest); err != nil {
		return fmt.Errorf("invalid digest: %w", err)
	}
	if err := validate.PeerEndpoint(endpoint); err != nil {
		return fmt.Errorf("invalid peer endpoint: %w", err)
	}

	senderDomain, err := senderDomainFromActorURL(actorURL)
	if err != nil {
		return fmt.Errorf("invalid actor URL: %w", err)
	}
	endpointURL, err := url.Parse(endpoint)
	if err != nil || strings.ToLower(endpointURL.Hostname()) != senderDomain {
		return fmt.Errorf("endpoint %s does not match sender domain %s", endpoint, senderDomain)
	}

	if size < 0 || size > float64(h.maxBlobSize) {
		return fmt.Errorf("invalid blob size")
	}

	if err := h.db.PutPeerBlob(ctx, actorURL, digest, endpoint); err != nil {
		return fmt.Errorf("storing peer blob: %w", err)
	}

	if err := h.db.PutBlob(ctx, digest, int64(size), nil, false); err != nil {
		return fmt.Errorf("storing blob metadata: %w", err)
	}

	if h.blobReplicator != nil {
		h.blobReplicator.ReplicateBlob(ctx, endpoint, digest, int64(size))
	}

	h.logger.Debug("inbox: ingested blob ref", "digest", digest, "from", actorURL)
	return nil
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
				if matchesDomain(host, allowed) {
					return true
				}
			}
		}
	}

	return false
}
