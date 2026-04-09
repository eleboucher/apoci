package activitypub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/jellydator/ttlcache/v3"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

func (h *InboxHandler) processCreate(ctx context.Context, activity *RawActivity) error {
	if !h.isFollowed(ctx, activity.Actor) {
		return &FedError{Kind: ErrNotRelevant, Message: fmt.Sprintf("not following actor %s", activity.Actor)}
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
		return &FedError{Kind: ErrNotRelevant, Message: fmt.Sprintf("not following actor %s", activity.Actor)}
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
		return &FedError{Kind: ErrNotRelevant, Message: fmt.Sprintf("not following actor %s", activity.Actor)}
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
		return &FedError{Kind: ErrNotRelevant, Message: fmt.Sprintf("not following actor %s", activity.Actor)}
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

	if err := h.db.RecordDeletedManifest(ctx, digest, repo, actorURL); err != nil {
		h.logger.Warn("inbox: failed to record tombstone", "digest", digest, "error", err)
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

	h.recordLayersAndReplicate(ctx, content, repoObj.ID, digest)

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

// recordLayersAndReplicate records manifest-layer associations and triggers
// eager replication for blobs that haven't been fetched yet.
func (h *InboxHandler) recordLayersAndReplicate(ctx context.Context, content []byte, repoID int64, digest string) {
	if content == nil {
		return
	}
	layerDigests := extractLayerDigests(content)
	if len(layerDigests) == 0 {
		return
	}

	man, err := h.db.GetManifestByDigest(ctx, repoID, digest)
	if err != nil {
		h.logger.Warn("inbox: could not fetch manifest to record layers", "digest", digest, "error", err)
		return
	}
	if man == nil {
		return
	}
	if err := h.db.PutManifestLayers(ctx, man.ID, layerDigests); err != nil {
		h.logger.Warn("inbox: failed to record manifest layers", "digest", digest, "error", err)
	}

	if h.blobReplicator == nil {
		return
	}
	for _, ld := range layerDigests {
		peers, err := h.db.FindPeersWithBlob(ctx, ld)
		if err != nil || len(peers) == 0 {
			continue
		}
		blob, err := h.db.GetBlob(ctx, ld)
		if err != nil || blob == nil || blob.StoredLocally {
			continue
		}
		h.blobReplicator.ReplicateBlob(ctx, peers[0].PeerEndpoint, ld, blob.SizeBytes)
	}
}

// extractLayerDigests parses an OCI manifest and returns all referenced digests
// (config + layers) for recording in manifest_layers.
func extractLayerDigests(content []byte) []string {
	var parsed struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		return nil
	}
	var digests []string
	if parsed.Config.Digest != "" {
		digests = append(digests, parsed.Config.Digest)
	}
	for _, l := range parsed.Layers {
		if l.Digest != "" {
			digests = append(digests, l.Digest)
		}
	}
	return digests
}
