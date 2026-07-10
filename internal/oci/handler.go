package oci

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	godigest "github.com/opencontainers/go-digest"
	specs "github.com/opencontainers/image-spec/specs-go"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ocimem"
	"cuelabs.dev/go/oci/ociregistry/ociserver"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/upstream"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

type RegistryRepository interface {
	BlobExistsInRepo(ctx context.Context, repoName, digest string) (bool, error)
	GetBlob(ctx context.Context, digest string) (*database.Blob, error)
	PutBlob(ctx context.Context, digest string, sizeBytes int64, mediaType *string, storedLocally bool) error
	DeleteBlob(ctx context.Context, digest string) error

	GetRepository(ctx context.Context, name string) (*database.Repository, error)
	GetOrCreateRepository(ctx context.Context, name, ownerID string) (*database.Repository, error)
	ListRepositoriesAfter(ctx context.Context, startAfter string, limit int) ([]database.Repository, error)
	SetRepositoryPrivate(ctx context.Context, id int64, private bool) error

	GetManifestByDigest(ctx context.Context, repoID int64, digest string) (*database.Manifest, error)
	GetManifestByTag(ctx context.Context, repoID int64, tag string) (*database.Manifest, error)
	PutManifest(ctx context.Context, m *database.Manifest) error
	PutManifestWithLayers(ctx context.Context, m *database.Manifest, refs []database.BlobRef) error
	DeleteManifest(ctx context.Context, repoID int64, digest string) error
	IsManifestDeleted(ctx context.Context, digest string) (bool, error)
	RecordDeletedManifest(ctx context.Context, digest, repoName, sourceActor string) error
	ListManifestsBySubject(ctx context.Context, repoID int64, subjectDigest string) ([]database.Manifest, error)
	PutManifestLayers(ctx context.Context, manifestID int64, refs []database.BlobRef) error

	PutTag(ctx context.Context, repoID int64, tag, digest string) error
	DeleteTag(ctx context.Context, repoID int64, tag string) error
	ListTagsAfter(ctx context.Context, repoID int64, startAfter string, limit int) ([]string, error)

	GetFollow(ctx context.Context, actorURL string) (*database.Actor, error)

	CreateUploadSession(ctx context.Context, uuid string, repoID int64, ttl time.Duration) (*database.UploadSession, error)
	GetUploadSession(ctx context.Context, uuid string) (*database.UploadSession, error)
	DeleteUploadSession(ctx context.Context, uuid string) error
	ListExpiredUploadSessions(ctx context.Context, limit int) ([]string, error)
	UploadSessionUUIDsByRepo(ctx context.Context, repoID int64) ([]string, error)
	AllUploadSessionUUIDs(ctx context.Context) ([]string, error)
}

type Publisher interface {
	PublishManifest(ctx context.Context, repo, tag, digest, mediaType string, size int64, content []byte, subjectDigest *string) error
	PublishTag(ctx context.Context, repo, tag, digest string) error
	PublishBlobRef(ctx context.Context, digest string, size int64) error
	PublishManifestDelete(ctx context.Context, repo, digest string) error
	PublishTagDelete(ctx context.Context, repo, tag string) error
}

// ManifestObserver is notified inline after a manifest is pushed, so
// implementations must return quickly. tag is empty for digest-only pushes;
// subjectDigest is non-nil for referrers.
type ManifestObserver interface {
	OnManifestPushed(repo, tag, digest, mediaType string, subjectDigest *string)
}

type BlobPeer struct {
	PeerEndpoint string
}

type ContentResolver interface {
	FindBlobPeers(ctx context.Context, digest string) ([]BlobPeer, error)
}

type BlobFetcher interface {
	FetchBlobStream(ctx context.Context, peerEndpoint, repo, digest string) (*peering.BlobStream, error)
	FetchManifest(ctx context.Context, peerEndpoint, repo, reference string) ([]byte, string, error)
}

type UpstreamFetcher interface {
	HasRegistry(name string) bool
	FetchBlobStream(ctx context.Context, registry, repo, digest string) (*peering.BlobStream, error)
	FetchManifest(ctx context.Context, registry, repo, reference string) ([]byte, string, error)
	IsRepoPrivate(registry, repo string) bool
}

type Registry struct {
	*ociregistry.Funcs
	db              RegistryRepository
	blobs           blobstore.BlobStore
	logger          *slog.Logger
	localID         string
	namespace       string
	publisher       Publisher
	observers       []ManifestObserver
	resolver        ContentResolver
	fetcher         BlobFetcher
	upstreamFetcher UpstreamFetcher
	maxManifestSize int64
	maxBlobSize     int64

	uploadDir string // staging directory for in-progress chunked uploads
	uploadsMu sync.Mutex
	uploads   map[string]*diskBlobWriter
}

func NewRegistry(db *database.DB, blobs blobstore.BlobStore, localID, namespace string, maxManifestSize, maxBlobSize int64, logger *slog.Logger) (*Registry, error) {
	// When no explicit namespace is given, derive it from the localID (actor URL)
	// so that writes are always namespace-enforced.
	if namespace == "" && localID != "" {
		if u, err := url.Parse(localID); err == nil && u.Host != "" {
			namespace = u.Hostname()
		}
	}

	r := &Registry{
		db:              db,
		blobs:           blobs,
		logger:          logger,
		localID:         localID,
		namespace:       namespace,
		maxManifestSize: maxManifestSize,
		maxBlobSize:     maxBlobSize,
		// Default staging directory for chunked uploads. The operator should
		// override this via SetUploadDir to keep large uploads on the data
		// volume rather than the OS temp dir.
		uploadDir: os.TempDir(),
		uploads:   make(map[string]*diskBlobWriter),
	}
	r.Funcs = &ociregistry.Funcs{
		GetBlob_:               r.getBlob,
		GetBlobRange_:          r.getBlobRange,
		GetManifest_:           r.getManifest,
		GetTag_:                r.getTag,
		ResolveBlob_:           r.resolveBlob,
		ResolveManifest_:       r.resolveManifest,
		ResolveTag_:            r.resolveTag,
		PushBlob_:              r.pushBlob,
		PushBlobChunked_:       r.pushBlobChunked,
		PushBlobChunkedResume_: r.pushBlobChunkedResume,
		MountBlob_:             r.mountBlob,
		PushManifest_:          r.pushManifest,
		DeleteBlob_:            r.deleteBlob,
		DeleteManifest_:        r.deleteManifest,
		DeleteTag_:             r.deleteTag,
		Repositories_:          r.repositories,
		Tags_:                  r.tags,
		Referrers_:             r.referrers,
	}
	return r, nil
}

func (r *Registry) Repo() RegistryRepository {
	return r.db
}

// Namespace returns the resolved local namespace normalizeRepo prepends to
// non-domain-scoped repo names (empty when none is enforced). The UI reads it to
// strip that prefix from locally-owned repos, so display matches storage.
func (r *Registry) Namespace() string {
	return r.namespace
}

func (r *Registry) SetPublisher(p Publisher) {
	r.publisher = p
}

func (r *Registry) SetFederation(resolver ContentResolver, fetcher BlobFetcher) {
	r.resolver = resolver
	r.fetcher = fetcher
}

func (r *Registry) SetUpstreamFetcher(f UpstreamFetcher) {
	r.upstreamFetcher = f
}

// SetUploadDir sets the staging directory for in-progress chunked uploads and
// removes any staging files left over from a previous run.
func (r *Registry) SetUploadDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating upload staging dir: %w", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "upload-*"))
	for _, m := range matches {
		if err := os.Remove(m); err != nil {
			r.logger.Warn("removing leftover upload staging file failed", "path", m, "error", err)
		}
	}
	r.uploadDir = dir
	return nil
}

func (r *Registry) AddManifestObserver(o ManifestObserver) {
	r.observers = append(r.observers, o)
}

func (r *Registry) Handler() http.Handler {
	return ociserver.New(r, nil)
}

// CleanExpiredUploads removes upload sessions that have expired from both
// the in-memory buffer map and the database.
func (r *Registry) CleanExpiredUploads(ctx context.Context) (int, error) {
	expired, err := r.db.ListExpiredUploadSessions(ctx, 100)
	if err != nil {
		return 0, fmt.Errorf("listing expired upload sessions: %w", err)
	}

	for _, uuid := range expired {
		r.uploadsMu.Lock()
		w := r.uploads[uuid]
		delete(r.uploads, uuid)
		r.uploadsMu.Unlock()

		if w != nil {
			w.cleanup()
		}

		if err := r.db.DeleteUploadSession(ctx, uuid); err != nil {
			r.logger.Warn("failed to delete expired upload session", "uuid", uuid, "error", err)
		}
	}

	if len(expired) > 0 {
		r.logger.Info("cleaned expired upload sessions", "count", len(expired))
	}

	// The reaper above only removes a staging file when its writer is still in
	// r.uploads; sweep the dir directly to catch files orphaned otherwise.
	r.sweepStaleStagingFiles(uploadSessionTTL)

	return len(expired), nil
}

// PurgeUploadSessions force-clears upload sessions regardless of expiry to
// recover a repository wedged by a partial push, removing the in-memory writer,
// staging file, and DB row for each. With repo == "" it clears every session.
// Unlike CleanExpiredUploads it ignores the TTL, so it is a manual recovery action.
func (r *Registry) PurgeUploadSessions(ctx context.Context, repo string) (int, error) {
	var uuids []string
	var err error
	if repo == "" {
		uuids, err = r.db.AllUploadSessionUUIDs(ctx)
	} else {
		repoObj, gerr := r.db.GetRepository(ctx, repo)
		if gerr != nil {
			return 0, fmt.Errorf("looking up repository: %w", gerr)
		}
		if repoObj == nil {
			return 0, nil
		}
		uuids, err = r.db.UploadSessionUUIDsByRepo(ctx, repoObj.ID)
	}
	if err != nil {
		return 0, fmt.Errorf("listing upload sessions to purge: %w", err)
	}

	for _, uuid := range uuids {
		r.uploadsMu.Lock()
		w := r.uploads[uuid]
		delete(r.uploads, uuid)
		r.uploadsMu.Unlock()

		if w != nil {
			w.cleanup()
		}

		if err := r.db.DeleteUploadSession(ctx, uuid); err != nil {
			r.logger.Warn("failed to delete purged upload session", "uuid", uuid, "error", err)
		}
	}

	// Staging files are named by upload ID, not repo, so only a purge-all can
	// safely sweep orphans; a scoped purge relies on the per-session cleanup above.
	if repo == "" {
		r.sweepStaleStagingFiles(0)
	}

	if len(uuids) > 0 {
		r.logger.Info("purged upload sessions", "count", len(uuids), "repo", repo)
	}
	return len(uuids), nil
}

// sweepStaleStagingFiles removes "upload-*" files older than maxAge that no
// in-flight writer owns.
func (r *Registry) sweepStaleStagingFiles(maxAge time.Duration) {
	if r.uploadDir == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(r.uploadDir, "upload-*"))
	if err != nil {
		r.logger.Warn("globbing upload staging files failed", "error", err)
		return
	}

	// Files owned by a live writer must survive even if idle.
	active := make(map[string]struct{})
	r.uploadsMu.Lock()
	for _, w := range r.uploads {
		if p := w.Path(); p != "" {
			active[p] = struct{}{}
		}
	}
	r.uploadsMu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	removed := 0
	for _, m := range matches {
		if _, ok := active[m]; ok {
			continue
		}
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(m); err != nil {
			r.logger.Warn("removing stale upload staging file failed", "path", m, "error", err)
			continue
		}
		removed++
	}
	if removed > 0 {
		r.logger.Info("swept stale upload staging files", "count", removed)
	}
}

// normalizeRepo auto-prepends the namespace prefix when the repo is not
// domain-scoped. Domain-scoped repos (first component contains a dot, e.g.
// "mortecouille.dev/user/app") pass through unchanged so federated repos
// remain addressable.
func (r *Registry) normalizeRepo(repo string) string {
	if r.namespace == "" {
		return repo
	}
	prefix := r.namespace + "/"
	if strings.HasPrefix(repo, prefix) {
		return repo
	}
	if first, _, _ := strings.Cut(repo, "/"); strings.Contains(first, ".") {
		return repo
	}
	return prefix + repo
}

func (r *Registry) checkNamespace(repo string) error {
	if r.namespace == "" {
		return nil
	}
	if !strings.HasPrefix(repo, r.namespace+"/") {
		return fmt.Errorf("%w: repository %q is not in local namespace %q", ociregistry.ErrDenied, repo, r.namespace)
	}
	return nil
}

// looksLikeNamespaceTypo flags a first segment that is a DNS-label prefix of
// namespace, e.g. "erwanleboucher" for "erwanleboucher.eu". Without this guard such a
// push silently nests under "<ns>/<partial>/...".
func looksLikeNamespaceTypo(repo, namespace string) bool {
	if namespace == "" {
		return false
	}
	first, _, _ := strings.Cut(repo, "/")
	if first == "" || strings.Contains(first, ".") {
		return false
	}
	return strings.HasPrefix(namespace, first+".")
}

func (r *Registry) normalizeRepoForWrite(repo string) (string, error) {
	if looksLikeNamespaceTypo(repo, r.namespace) {
		first, rest, hasRest := strings.Cut(repo, "/")
		hint := fmt.Sprintf("the local namespace is %q", r.namespace)
		if hasRest {
			hint = fmt.Sprintf("push to %q instead", r.namespace+"/"+rest)
		}
		return "", fmt.Errorf("%w: repository %q first segment %q is a partial match of namespace %q; %s", ociregistry.ErrDenied, repo, first, r.namespace, hint)
	}
	repo = r.normalizeRepo(repo)
	if err := r.checkNamespace(repo); err != nil {
		return "", err
	}
	return repo, nil
}

const defaultMediaType = "application/octet-stream"

func (r *Registry) getBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	originalRepo := repo
	repo = r.normalizeRepo(repo)
	r.logger.Debug("oci: getBlob", "repo", repo, "originalRepo", originalRepo, "digest", string(digest))
	metrics.RegistryBlobPulls.Add(1)

	exists, err := r.db.BlobExistsInRepo(ctx, repo, string(digest))
	if err != nil {
		return nil, fmt.Errorf("checking blob repo scope: %w", err)
	}
	if !exists {
		if r.upstreamFetcher != nil {
			reader, err := r.fetchBlobFromUpstream(ctx, originalRepo, digest)
			if err != nil {
				r.logger.Debug("blob not found on upstream", "repo", originalRepo, "digest", string(digest), "error", err)
			} else if reader != nil {
				return reader, nil
			}
		}
		return nil, ociregistry.ErrBlobUnknown
	}

	f, blobSize, openErr := r.blobs.Open(ctx, string(digest))
	if openErr == nil {
		return newBlobReader(f, ociregistry.Descriptor{
			MediaType: defaultMediaType,
			Digest:    digest,
			Size:      blobSize,
		}), nil
	}
	if !errors.Is(openErr, blobstore.ErrBlobNotFound) {
		return nil, fmt.Errorf("opening blob: %w", openErr)
	}

	if r.resolver != nil && r.fetcher != nil {
		reader, err := r.fetchBlobFromPeers(ctx, repo, digest)
		if err != nil {
			r.logger.Debug("blob not found on any peer", "digest", string(digest), "error", err)
		} else if reader != nil {
			metrics.RegistryBlobPullThru.Add(1)
			return reader, nil
		}
	}

	return nil, ociregistry.ErrBlobUnknown
}

func (r *Registry) fetchBlobFromPeers(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	peers, err := r.resolver.FindBlobPeers(ctx, string(digest))
	if err != nil {
		return nil, fmt.Errorf("finding blob peers: %w", err)
	}

	for _, peer := range peers {
		peerFetchStart := time.Now()
		stream, err := r.fetcher.FetchBlobStream(ctx, peer.PeerEndpoint, repo, string(digest))
		if err != nil {
			r.logger.Warn("failed to fetch blob from peer",
				"peer", peer.PeerEndpoint,
				"digest", string(digest),
				"error", err,
			)
			continue
		}

		if stream.Size > r.maxBlobSize {
			_ = stream.Body.Close()
			r.logger.Warn("peer advertises oversize blob", "peer", peer.PeerEndpoint, "digest", string(digest), "size", stream.Size, "max", r.maxBlobSize)
			continue
		}

		storedDigest, size, err := r.blobs.Put(ctx, io.LimitReader(stream.Body, r.maxBlobSize+1), string(digest))
		if closeErr := stream.Body.Close(); closeErr != nil {
			r.logger.Warn("failed to close blob stream", "peer", peer.PeerEndpoint, "error", closeErr)
		}
		if err != nil {
			r.logger.Warn("failed to store fetched blob", "error", err)
			continue
		}
		if size > r.maxBlobSize {
			_ = r.blobs.Delete(ctx, storedDigest)
			r.logger.Warn("fetched blob exceeds max size", "digest", storedDigest, "size", size, "max", r.maxBlobSize)
			continue
		}

		mt := defaultMediaType
		if err := r.db.PutBlob(ctx, storedDigest, size, &mt, true); err != nil {
			r.logger.Warn("failed to record pulled blob metadata", "digest", storedDigest, "error", err)
		}

		if r.publisher != nil {
			if pubErr := r.publisher.PublishBlobRef(ctx, storedDigest, size); pubErr != nil {
				r.logger.Warn("failed to publish pulled blob ref", "digest", storedDigest, "error", pubErr)
			}
		}

		metrics.PeerFetchDuration.Observe(time.Since(peerFetchStart).Seconds())
		r.logger.Info("fetched blob from federation peer",
			"digest", storedDigest,
			"peer", peer.PeerEndpoint,
			"size", size,
		)

		f, fetchedSize, err := r.blobs.Open(ctx, storedDigest)
		if err != nil {
			if errors.Is(err, blobstore.ErrBlobNotFound) {
				r.logger.Warn("cached blob disappeared after fetch", "digest", storedDigest)
			} else {
				r.logger.Warn("failed to open cached blob after fetch", "error", err)
			}
			continue
		}
		desc := ociregistry.Descriptor{
			MediaType: defaultMediaType,
			Digest:    digest,
			Size:      fetchedSize,
		}
		return newBlobReader(f, desc), nil
	}

	return nil, fmt.Errorf("no peers have blob %s", string(digest))
}

func (r *Registry) fetchBlobFromUpstream(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	registry, upstreamRepo, ok := upstream.ParseUpstreamRepo(repo)
	if !ok || !r.upstreamFetcher.HasRegistry(registry) {
		return nil, nil // not an upstream repo
	}

	normalizedRepo := r.normalizeRepo(repo)

	fetchStart := time.Now()
	stream, err := r.upstreamFetcher.FetchBlobStream(ctx, registry, upstreamRepo, string(digest))
	if err != nil {
		return nil, fmt.Errorf("fetching from upstream %s: %w", registry, err)
	}

	if stream.Size > r.maxBlobSize {
		_ = stream.Body.Close()
		return nil, fmt.Errorf("upstream blob exceeds max size (%d > %d)", stream.Size, r.maxBlobSize)
	}

	storedDigest, size, err := r.blobs.Put(ctx, io.LimitReader(stream.Body, r.maxBlobSize+1), string(digest))
	if closeErr := stream.Body.Close(); closeErr != nil {
		r.logger.Warn("failed to close upstream blob stream", "error", closeErr)
	}
	if err != nil {
		return nil, fmt.Errorf("storing upstream blob: %w", err)
	}
	if size > r.maxBlobSize {
		if delErr := r.blobs.Delete(ctx, storedDigest); delErr != nil {
			r.logger.Warn("failed to delete oversized upstream blob", "digest", storedDigest, "error", delErr)
		}
		return nil, fmt.Errorf("upstream blob exceeds max size (%d > %d)", size, r.maxBlobSize)
	}

	mt := defaultMediaType
	if err := r.db.PutBlob(ctx, storedDigest, size, &mt, true); err != nil {
		r.logger.Warn("failed to record upstream blob", "error", err)
	}

	if repoObj, err := r.db.GetOrCreateRepository(ctx, normalizedRepo, "upstream:"+registry); err != nil {
		r.logger.Warn("failed to create upstream repo", "error", err)
	} else {
		private := r.upstreamFetcher.IsRepoPrivate(registry, upstreamRepo)
		if err := r.db.SetRepositoryPrivate(ctx, repoObj.ID, private); err != nil {
			r.logger.Warn("failed to update upstream repo privacy", "error", err)
		}
	}

	metrics.UpstreamBlobPullThru.WithLabelValues(registry).Inc()
	metrics.PeerFetchDuration.Observe(time.Since(fetchStart).Seconds())
	r.logger.Info("fetched blob from upstream",
		"registry", registry,
		"repo", upstreamRepo,
		"digest", storedDigest,
		"size", size,
	)

	f, cachedSize, err := r.blobs.Open(ctx, storedDigest)
	if err != nil {
		return nil, fmt.Errorf("opening cached upstream blob: %w", err)
	}
	return newBlobReader(f, ociregistry.Descriptor{
		MediaType: defaultMediaType,
		Digest:    digest,
		Size:      cachedSize,
	}), nil
}

func (r *Registry) fetchManifestFromUpstream(ctx context.Context, repo, reference string) (ociregistry.BlobReader, error) {
	registry, upstreamRepo, ok := upstream.ParseUpstreamRepo(repo)
	if !ok || !r.upstreamFetcher.HasRegistry(registry) {
		return nil, nil // not an upstream repo
	}

	normalizedRepo := r.normalizeRepo(repo)

	fetchStart := time.Now()
	data, mediaType, err := r.upstreamFetcher.FetchManifest(ctx, registry, upstreamRepo, reference)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest from upstream %s: %w", registry, err)
	}

	computed := string(godigest.FromBytes(data))

	refDigest, parseErr := godigest.Parse(reference)
	refIsDigest := parseErr == nil
	if refIsDigest {
		if computed != reference && string(refDigest) != computed {
			return nil, fmt.Errorf("manifest digest mismatch: expected %s, got %s", reference, computed)
		}
	}

	repoObj, err := r.db.GetOrCreateRepository(ctx, normalizedRepo, "upstream:"+registry)
	if err != nil {
		return nil, fmt.Errorf("creating upstream repo: %w", err)
	}

	private := r.upstreamFetcher.IsRepoPrivate(registry, upstreamRepo)
	if err := r.db.SetRepositoryPrivate(ctx, repoObj.ID, private); err != nil {
		r.logger.Warn("failed to update upstream repo privacy", "error", err)
	}

	meta := parseManifestMeta(data, r.logger)

	m := &database.Manifest{
		RepositoryID:  repoObj.ID,
		Digest:        computed,
		MediaType:     mediaType,
		SizeBytes:     int64(len(data)),
		Content:       data,
		SubjectDigest: meta.subjectDigest,
		ArtifactType:  meta.artifactType,
	}
	upstreamRefs := append([]database.BlobRef(nil), meta.layers...)
	upstreamRefs = append(upstreamRefs, meta.children...)
	if err := r.db.PutManifestWithLayers(ctx, m, upstreamRefs); err != nil {
		return nil, fmt.Errorf("caching upstream manifest: %w", err)
	}

	if !refIsDigest {
		if err := r.db.PutTag(ctx, repoObj.ID, reference, computed); err != nil {
			r.logger.Warn("failed to cache upstream tag", "tag", reference, "error", err)
		}
	}

	metrics.UpstreamManifestPullThru.WithLabelValues(registry).Inc()
	metrics.PeerFetchDuration.Observe(time.Since(fetchStart).Seconds())
	r.logger.Info("fetched manifest from upstream",
		"registry", registry,
		"repo", upstreamRepo,
		"reference", reference,
		"digest", computed,
		"size", len(data),
	)

	desc := ociregistry.Descriptor{
		MediaType: mediaType,
		Digest:    ociregistry.Digest(computed),
		Size:      int64(len(data)),
	}
	return ocimem.NewBytesReader(data, desc), nil
}

func (r *Registry) getBlobRange(ctx context.Context, repo string, digest ociregistry.Digest, offset0, offset1 int64) (ociregistry.BlobReader, error) {
	repo = r.normalizeRepo(repo)

	exists, err := r.db.BlobExistsInRepo(ctx, repo, string(digest))
	if err != nil {
		return nil, fmt.Errorf("checking blob repo scope: %w", err)
	}
	if !exists {
		return nil, ociregistry.ErrBlobUnknown
	}

	f, totalSize, err := r.blobs.Open(ctx, string(digest))
	if err != nil && !errors.Is(err, blobstore.ErrBlobNotFound) {
		return nil, fmt.Errorf("opening blob: %w", err)
	}
	if errors.Is(err, blobstore.ErrBlobNotFound) {
		if r.resolver != nil && r.fetcher != nil {
			reader, fetchErr := r.fetchBlobFromPeers(ctx, repo, digest)
			if fetchErr == nil && reader != nil {
				if closeErr := reader.Close(); closeErr != nil {
					r.logger.Warn("failed to close peer blob reader", "digest", string(digest), "error", closeErr)
				}
				metrics.RegistryBlobPullThru.Add(1)
				f, totalSize, err = r.blobs.Open(ctx, string(digest))
				if err != nil {
					r.logger.Warn("failed to open blob after peer fetch", "digest", string(digest), "error", err)
					return nil, ociregistry.ErrBlobUnknown
				}
			}
		}
		if f == nil {
			return nil, ociregistry.ErrBlobUnknown
		}
	}

	if offset0 >= totalSize {
		_ = f.Close()
		return nil, ociregistry.ErrRangeInvalid
	}

	if _, err := f.Seek(offset0, io.SeekStart); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("seeking blob: %w", err)
	}

	end := offset1
	if end < 0 || end > totalSize {
		end = totalSize
	}
	rangeSize := end - offset0

	desc := ociregistry.Descriptor{
		MediaType: defaultMediaType,
		Digest:    digest,
		Size:      rangeSize,
	}

	limited := io.LimitReader(f, rangeSize)
	return newBlobReader(&limitedReadCloser{Reader: limited, Closer: f}, desc), nil
}

type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func (r *Registry) getManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	originalRepo := repo
	repo = r.normalizeRepo(repo)
	r.logger.Debug("oci: getManifest", "repo", repo, "originalRepo", originalRepo, "digest", string(digest))
	metrics.RegistryManifestPulls.Add(1)
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}
	if repoObj != nil {
		m, err := r.db.GetManifestByDigest(ctx, repoObj.ID, string(digest))
		if err != nil {
			return nil, fmt.Errorf("getting manifest: %w", err)
		}
		if m != nil {
			return r.serveManifest(ctx, repo, m)
		}
	}

	// Manifest not found locally. Try fetching from federation peers that own this repo's namespace.
	if r.fetcher != nil && repoObj != nil && repoObj.OwnerID != r.localID {
		follow, err := r.db.GetFollow(ctx, repoObj.OwnerID)
		if err == nil && follow != nil {
			data, mediaType, fetchErr := r.fetcher.FetchManifest(ctx, follow.Endpoint, repo, string(digest))
			if fetchErr == nil {
				computed := string(godigest.FromBytes(data))
				if computed == string(digest) {
					m := &database.Manifest{
						RepositoryID: repoObj.ID,
						Digest:       computed,
						MediaType:    mediaType,
						SizeBytes:    int64(len(data)),
						Content:      data,
						SourceActor:  &repoObj.OwnerID,
					}
					peerMeta := parseManifestMeta(data, r.logger)
					peerRefs := append([]database.BlobRef(nil), peerMeta.layers...)
					peerRefs = append(peerRefs, peerMeta.children...)
					if err := r.db.PutManifestWithLayers(ctx, m, peerRefs); err != nil {
						r.logger.Warn("failed to cache fetched manifest", "error", err)
					}
					metrics.RegistryManifestPullThru.Add(1)
					desc := ociregistry.Descriptor{
						MediaType: mediaType,
						Digest:    digest,
						Size:      int64(len(data)),
					}
					return ocimem.NewBytesReader(data, desc), nil
				}
				r.logger.Warn("manifest digest mismatch from federation peer",
					"expected", string(digest), "got", computed)
			}
		}
	}

	if r.upstreamFetcher != nil {
		reader, err := r.fetchManifestFromUpstream(ctx, originalRepo, string(digest))
		if err != nil {
			r.logger.Debug("manifest not found on upstream", "repo", originalRepo, "digest", string(digest), "error", err)
		} else if reader != nil {
			return reader, nil
		}
	}

	// Check if the manifest was deleted — return 410 Gone so clients know it's intentionally absent.
	// Use a plain error (no OCI code) so ociserver falls back to HTTPError.StatusCode() = 410
	// rather than overriding with the MANIFEST_UNKNOWN → 404 mapping.
	deleted, tombErr := r.db.IsManifestDeleted(ctx, string(digest))
	if tombErr != nil {
		r.logger.Warn("checking manifest tombstone failed", "digest", digest, "error", tombErr)
	} else if deleted {
		return nil, ociregistry.NewHTTPError(errors.New("manifest deleted"), http.StatusGone, nil, nil)
	}

	return nil, ociregistry.ErrManifestUnknown
}

func (r *Registry) getTag(ctx context.Context, repo string, tagName string) (ociregistry.BlobReader, error) {
	originalRepo := repo
	repo = r.normalizeRepo(repo)
	r.logger.Debug("oci: getTag", "repo", repo, "originalRepo", originalRepo, "tag", tagName)

	// Try local first
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}
	if repoObj != nil {
		m, err := r.db.GetManifestByTag(ctx, repoObj.ID, tagName)
		if err != nil {
			return nil, fmt.Errorf("getting manifest by tag: %w", err)
		}
		if m != nil {
			return r.serveManifest(ctx, repo, m)
		}
	}

	// Try upstream registry (use original repo to detect upstream prefix like "docker.io/")
	if r.upstreamFetcher != nil {
		reader, err := r.fetchManifestFromUpstream(ctx, originalRepo, tagName)
		if err != nil {
			r.logger.Debug("tag not found on upstream", "repo", originalRepo, "tag", tagName, "error", err)
		} else if reader != nil {
			return reader, nil
		}
	}

	if repoObj == nil {
		return nil, ociregistry.ErrNameUnknown
	}
	return nil, ociregistry.ErrManifestUnknown
}

// serveManifest returns the manifest content, fetching from the source peer if content is empty.
func (r *Registry) serveManifest(ctx context.Context, repo string, m *database.Manifest) (ociregistry.BlobReader, error) {
	if len(m.Content) > 0 {
		desc := ociregistry.Descriptor{
			MediaType: m.MediaType,
			Digest:    ociregistry.Digest(m.Digest),
			Size:      m.SizeBytes,
		}
		return ocimem.NewBytesReader(m.Content, desc), nil
	}

	// Content is empty (federated manifest without inline content). Pull-through from source peer.
	if r.fetcher != nil && m.SourceActor != nil {
		result, err := r.fetchManifestFromSource(ctx, repo, m)
		if err == nil && result != nil {
			desc := ociregistry.Descriptor{
				MediaType: m.MediaType,
				Digest:    ociregistry.Digest(m.Digest),
				Size:      int64(len(result)),
			}
			return ocimem.NewBytesReader(result, desc), nil
		}
		r.logger.Warn("failed to pull-through manifest from source",
			"repo", repo,
			"digest", m.Digest,
			"source", *m.SourceActor,
			"error", err,
		)
	}

	return nil, ociregistry.ErrManifestUnknown
}

// fetchManifestFromSource fetches manifest content from the originating peer and caches it locally.
func (r *Registry) fetchManifestFromSource(ctx context.Context, repo string, m *database.Manifest) ([]byte, error) {
	follow, err := r.db.GetFollow(ctx, *m.SourceActor)
	if err != nil || follow == nil {
		return nil, fmt.Errorf("source actor %s not in follows", *m.SourceActor)
	}

	data, mediaType, err := r.fetcher.FetchManifest(ctx, follow.Endpoint, repo, m.Digest)
	if err != nil {
		return nil, fmt.Errorf("fetching manifest from peer %s: %w", follow.Endpoint, err)
	}

	computed := string(godigest.FromBytes(data))
	if computed != m.Digest {
		return nil, fmt.Errorf("manifest digest mismatch: expected %s, got %s", m.Digest, computed)
	}

	if mediaType != "" {
		m.MediaType = mediaType
	}
	m.Content = data
	m.SizeBytes = int64(len(data))
	if err := r.db.PutManifest(ctx, m); err != nil {
		r.logger.Warn("failed to cache pulled manifest", "error", err)
	}

	metrics.RegistryManifestPullThru.Add(1)
	r.logger.Info("fetched manifest from federation peer",
		"repo", repo,
		"digest", m.Digest,
		"peer", follow.Endpoint,
		"size", len(data),
	)

	return data, nil
}

func (r *Registry) resolveBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	repo = r.normalizeRepo(repo)
	exists, err := r.db.BlobExistsInRepo(ctx, repo, string(digest))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("checking blob repo scope: %w", err)
	}
	if !exists {
		return ociregistry.Descriptor{}, ociregistry.ErrBlobUnknown
	}

	blob, err := r.db.GetBlob(ctx, string(digest))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving blob: %w", err)
	}
	if blob == nil || !blob.StoredLocally {
		return ociregistry.Descriptor{}, ociregistry.ErrBlobUnknown
	}

	mediaType := defaultMediaType
	if blob.MediaType != nil {
		mediaType = *blob.MediaType
	}

	return ociregistry.Descriptor{
		MediaType: mediaType,
		Digest:    digest,
		Size:      blob.SizeBytes,
	}, nil
}

func (r *Registry) resolveManifest(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	originalRepo := repo
	repo = r.normalizeRepo(repo)
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving manifest: %w", err)
	}
	if repoObj != nil {
		m, err := r.db.GetManifestByDigest(ctx, repoObj.ID, string(digest))
		if err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("resolving manifest: %w", err)
		}
		if m != nil {
			return ociregistry.Descriptor{
				MediaType: m.MediaType,
				Digest:    ociregistry.Digest(m.Digest),
				Size:      m.SizeBytes,
			}, nil
		}
	}

	if r.upstreamFetcher != nil {
		reader, err := r.fetchManifestFromUpstream(ctx, originalRepo, string(digest))
		if err != nil {
			r.logger.Debug("resolve manifest not found on upstream", "repo", originalRepo, "digest", string(digest), "error", err)
		} else if reader != nil {
			desc := reader.Descriptor()
			_ = reader.Close()
			return desc, nil
		}
	}

	if repoObj == nil {
		return ociregistry.Descriptor{}, ociregistry.ErrNameUnknown
	}
	return ociregistry.Descriptor{}, ociregistry.ErrManifestUnknown
}

func (r *Registry) resolveTag(ctx context.Context, repo string, tagName string) (ociregistry.Descriptor, error) {
	originalRepo := repo
	repo = r.normalizeRepo(repo)
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving tag: %w", err)
	}
	if repoObj != nil {
		m, err := r.db.GetManifestByTag(ctx, repoObj.ID, tagName)
		if err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("resolving tag: %w", err)
		}
		if m != nil {
			return ociregistry.Descriptor{
				MediaType: m.MediaType,
				Digest:    ociregistry.Digest(m.Digest),
				Size:      m.SizeBytes,
			}, nil
		}
	}

	if r.upstreamFetcher != nil {
		reader, err := r.fetchManifestFromUpstream(ctx, originalRepo, tagName)
		if err != nil {
			r.logger.Debug("resolve tag not found on upstream", "repo", originalRepo, "tag", tagName, "error", err)
		} else if reader != nil {
			desc := reader.Descriptor()
			_ = reader.Close()
			return desc, nil
		}
	}

	if repoObj == nil {
		return ociregistry.Descriptor{}, ociregistry.ErrNameUnknown
	}
	return ociregistry.Descriptor{}, ociregistry.ErrManifestUnknown
}

func (r *Registry) pushBlob(ctx context.Context, repo string, desc ociregistry.Descriptor, rd io.Reader) (ociregistry.Descriptor, error) {
	repo, err := r.normalizeRepoForWrite(repo)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	r.logger.Debug("oci: pushBlob", "repo", repo, "digest", string(desc.Digest), "size", desc.Size)

	if desc.Size > r.maxBlobSize {
		return ociregistry.Descriptor{}, fmt.Errorf("%w: blob exceeds maximum size (%d bytes)", ociregistry.ErrBlobUploadInvalid, r.maxBlobSize)
	}

	if _, err := r.db.GetOrCreateRepository(ctx, repo, r.localID); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("getting repository: %w", err)
	}

	limited := io.LimitReader(rd, r.maxBlobSize+1)
	digest, size, err := r.blobs.Put(ctx, limited, string(desc.Digest))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("storing blob: %w", err)
	}
	if size > r.maxBlobSize {
		_ = r.blobs.Delete(ctx, digest)
		return ociregistry.Descriptor{}, fmt.Errorf("%w: blob exceeds maximum size (%d bytes)", ociregistry.ErrBlobUploadInvalid, r.maxBlobSize)
	}

	mt := desc.MediaType
	if err := r.db.PutBlob(ctx, digest, size, &mt, true); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("recording blob: %w", err)
	}

	metrics.RegistryBlobPushes.Add(1)
	r.logger.Info("blob pushed",
		"repo", repo,
		"digest", digest,
		"size", size)

	if r.publisher != nil {
		if err := r.publisher.PublishBlobRef(ctx, digest, size); err != nil {
			r.logger.Warn("failed to publish blob ref to federation", "error", err)
		}
	}

	return ociregistry.Descriptor{
		MediaType: desc.MediaType,
		Digest:    ociregistry.Digest(digest),
		Size:      size,
	}, nil
}

func (r *Registry) mountBlob(ctx context.Context, fromRepo, toRepo string, digest ociregistry.Digest) (ociregistry.Descriptor, error) {
	fromRepo = r.normalizeRepo(fromRepo)
	toRepo, err := r.normalizeRepoForWrite(toRepo)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	r.logger.Debug("oci: mountBlob", "fromRepo", fromRepo, "toRepo", toRepo, "digest", string(digest))
	if _, err := r.db.GetOrCreateRepository(ctx, toRepo, r.localID); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("getting repository: %w", err)
	}

	blob, err := r.db.GetBlob(ctx, string(digest))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("looking up blob: %w", err)
	}
	if blob == nil || !blob.StoredLocally {
		return ociregistry.Descriptor{}, ociregistry.ErrBlobUnknown
	}

	mediaType := defaultMediaType
	if blob.MediaType != nil {
		mediaType = *blob.MediaType
	}

	metrics.RegistryBlobMounts.Add(1)
	r.logger.Info("blob mounted",
		"fromRepo", fromRepo,
		"toRepo", toRepo,
		"digest", string(digest),
		"size", blob.SizeBytes)

	return ociregistry.Descriptor{
		MediaType: mediaType,
		Digest:    digest,
		Size:      blob.SizeBytes,
	}, nil
}

const uploadSessionTTL = 1 * time.Hour

func (r *Registry) pushBlobChunked(ctx context.Context, repo string, chunkSize int) (ociregistry.BlobWriter, error) {
	repo, err := r.normalizeRepoForWrite(repo)
	if err != nil {
		return nil, err
	}
	repoObj, err := r.db.GetOrCreateRepository(ctx, repo, r.localID)
	if err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}

	w, err := r.newUploadWriter(repo, "")
	if err != nil {
		return nil, err
	}

	if _, err := r.db.CreateUploadSession(ctx, w.ID(), repoObj.ID, uploadSessionTTL); err != nil {
		// Without a persisted session the follow-up PATCH/PUT can't resume, so
		// fail the POST and discard the staged writer rather than leak it.
		r.uploadsMu.Lock()
		delete(r.uploads, w.ID())
		r.uploadsMu.Unlock()
		_ = w.Cancel()
		return nil, fmt.Errorf("persisting upload session: %w", err)
	}

	return w, nil
}

func (r *Registry) pushBlobChunkedResume(ctx context.Context, repo, id string, offset int64, chunkSize int) (ociregistry.BlobWriter, error) {
	if _, err := r.normalizeRepoForWrite(repo); err != nil {
		return nil, err
	}

	// Validate the session exists and hasn't expired.
	session, err := r.db.GetUploadSession(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("checking upload session: %w", err)
	}
	if session == nil {
		return nil, fmt.Errorf("%w: upload %q not found or expired", ociregistry.ErrBlobUploadUnknown, id)
	}

	r.uploadsMu.Lock()
	w, ok := r.uploads[id]
	r.uploadsMu.Unlock()

	if !ok {
		// Session exists in DB but the staged upload was lost (e.g. server
		// restart). Clean up the stale DB record.
		_ = r.db.DeleteUploadSession(ctx, id)
		return nil, fmt.Errorf("%w: upload %q session expired (server restarted)", ociregistry.ErrBlobUploadUnknown, id)
	}

	return w, nil
}

// newUploadWriter creates a disk-backed blob writer for a chunked upload and
// registers it so subsequent PATCH/PUT requests can resume it by ID.
func (r *Registry) newUploadWriter(repo, uuid string) (*diskBlobWriter, error) {
	w, err := newDiskBlobWriter(r.uploadDir, uuid, r.maxBlobSize, nil)
	if err != nil {
		return nil, err
	}
	id := w.ID()

	// onDone runs on every terminal outcome (commit success, digest mismatch,
	// or cancel), so a failed upload doesn't leak its r.uploads entry until the
	// TTL reaper runs. DeleteUploadSession is idempotent with the commit path.
	w.onDone = func() {
		r.uploadsMu.Lock()
		delete(r.uploads, id)
		r.uploadsMu.Unlock()
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = r.db.DeleteUploadSession(delCtx, id)
	}

	w.commit = func(dig ociregistry.Digest, data io.Reader) (ociregistry.Descriptor, error) {
		r.uploadsMu.Lock()
		delete(r.uploads, id)
		r.uploadsMu.Unlock()

		// This callback runs after the HTTP request context has been released,
		// so we use a fresh timeout context for storage and DB operations.
		commitCtx, commitCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer commitCancel()

		digestStr, size, err := r.blobs.Put(commitCtx, data, string(dig))
		if err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("storing chunked blob: %w", err)
		}

		mt := "application/octet-stream"
		if err := r.db.PutBlob(commitCtx, digestStr, size, &mt, true); err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("recording chunked blob: %w", err)
		}

		if err := r.db.DeleteUploadSession(commitCtx, id); err != nil {
			r.logger.Warn("failed to delete upload session", "uuid", id, "error", err)
		}

		metrics.RegistryBlobPushes.Add(1)
		r.logger.Info("chunked blob committed", "repo", repo, "digest", digestStr, "size", size)

		if r.publisher != nil {
			if err := r.publisher.PublishBlobRef(commitCtx, digestStr, size); err != nil {
				r.logger.Warn("failed to publish chunked blob ref to federation", "error", err)
			}
		}

		return ociregistry.Descriptor{MediaType: mt, Digest: ociregistry.Digest(digestStr), Size: size}, nil
	}

	r.uploadsMu.Lock()
	r.uploads[id] = w
	r.uploadsMu.Unlock()

	return w, nil
}

func (r *Registry) pushManifest(ctx context.Context, repo string, tag string, contents []byte, mediaType string) (ociregistry.Descriptor, error) {
	repo, err := r.normalizeRepoForWrite(repo)
	if err != nil {
		return ociregistry.Descriptor{}, err
	}
	r.logger.Debug("oci: pushManifest", "repo", repo, "tag", tag, "mediaType", mediaType, "size", len(contents))
	if err := validate.ManifestContent(contents, r.maxManifestSize); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("%w: %w", ociregistry.ErrManifestInvalid, err)
	}
	if err := validate.Tag(tag); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("%w: %w", ociregistry.ErrManifestInvalid, err)
	}

	repoObj, err := r.db.GetOrCreateRepository(ctx, repo, r.localID)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("getting repository: %w", err)
	}

	dgst := godigest.FromBytes(contents)
	digest := string(dgst)

	meta := parseManifestMeta(contents, r.logger)

	// Verify all referenced blobs exist locally before accepting the manifest.
	// Only check well-formed digests to avoid rejecting manifests with placeholder references.
	for _, ref := range meta.layers {
		if validate.Digest(ref.Digest) != nil {
			continue
		}
		blob, err := r.db.GetBlob(ctx, ref.Digest)
		if err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("checking blob %s: %w", ref.Digest, err)
		}
		if blob == nil || !blob.StoredLocally {
			return ociregistry.Descriptor{}, fmt.Errorf("%w: referenced blob %s not found", ociregistry.ErrBlobUnknown, ref.Digest)
		}
	}

	// Children must already exist: accepting a dangling index leaves pulls broken.
	for _, ref := range meta.children {
		if validate.Digest(ref.Digest) != nil {
			continue
		}
		child, err := r.db.GetManifestByDigest(ctx, repoObj.ID, ref.Digest)
		if err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("checking child manifest %s: %w", ref.Digest, err)
		}
		if child == nil {
			return ociregistry.Descriptor{}, fmt.Errorf("%w: referenced manifest %s not found", ociregistry.ErrManifestUnknown, ref.Digest)
		}
	}

	m := &database.Manifest{
		RepositoryID:  repoObj.ID,
		Digest:        digest,
		MediaType:     mediaType,
		SizeBytes:     int64(len(contents)),
		Content:       contents,
		SubjectDigest: meta.subjectDigest,
		ArtifactType:  meta.artifactType,
	}

	// Write the manifest and its blob references atomically so the GC orphan
	// sweep can never observe a stored manifest whose layer references are still
	// missing.
	refs := append([]database.BlobRef(nil), meta.layers...)
	refs = append(refs, meta.children...)
	if err := r.db.PutManifestWithLayers(ctx, m, refs); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("storing manifest: %w", err)
	}

	if tag != "" {
		if err := r.db.PutTag(ctx, repoObj.ID, tag, digest); err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("storing tag: %w", err)
		}
	}

	metrics.RegistryManifestPushes.Add(1)
	r.logger.Info("manifest pushed",
		"repo", repo,
		"tag", tag,
		"digest", digest,
		"size", int64(len(contents)))

	if r.publisher != nil {
		if err := r.publisher.PublishManifest(ctx, repo, tag, digest, mediaType, int64(len(contents)), contents, meta.subjectDigest); err != nil {
			r.logger.Warn("failed to publish manifest to federation", "error", err)
		}
		if tag != "" {
			if err := r.publisher.PublishTag(ctx, repo, tag, digest); err != nil {
				r.logger.Warn("failed to publish tag to federation", "error", err)
			}
		}
	}

	for _, o := range r.observers {
		o.OnManifestPushed(repo, tag, digest, mediaType, meta.subjectDigest)
	}

	return ociregistry.Descriptor{
		MediaType: mediaType,
		Digest:    ociregistry.Digest(digest),
		Size:      int64(len(contents)),
	}, nil
}

var emptyConfig = []byte("{}")

const emptyConfigMediaType = "application/vnd.oci.empty.v1+json"

// AttachReferrer stores payload as a layer and writes an OCI artifact manifest
// with the given subject, so it federates like any other manifest. Returns the
// referrer digest.
func (r *Registry) AttachReferrer(ctx context.Context, repo, subjectDigest, artifactType string, annotations map[string]string, payload []byte, payloadMediaType string) (string, error) {
	repoW, err := r.normalizeRepoForWrite(repo)
	if err != nil {
		return "", err
	}
	repoObj, err := r.db.GetRepository(ctx, repoW)
	if err != nil {
		return "", fmt.Errorf("getting repository: %w", err)
	}
	if repoObj == nil {
		return "", fmt.Errorf("%w: %s", ociregistry.ErrNameUnknown, repoW)
	}
	subject, err := r.db.GetManifestByDigest(ctx, repoObj.ID, subjectDigest)
	if err != nil {
		return "", fmt.Errorf("loading subject manifest: %w", err)
	}
	if subject == nil {
		return "", fmt.Errorf("%w: subject %s", ociregistry.ErrManifestUnknown, subjectDigest)
	}

	cfgDigest := godigest.FromBytes(emptyConfig)
	if _, err := r.pushBlob(ctx, repoW, ociregistry.Descriptor{
		MediaType: emptyConfigMediaType,
		Digest:    cfgDigest,
		Size:      int64(len(emptyConfig)),
	}, bytes.NewReader(emptyConfig)); err != nil {
		return "", fmt.Errorf("storing referrer config: %w", err)
	}

	payloadDigest := godigest.FromBytes(payload)
	if _, err := r.pushBlob(ctx, repoW, ociregistry.Descriptor{
		MediaType: payloadMediaType,
		Digest:    payloadDigest,
		Size:      int64(len(payload)),
	}, bytes.NewReader(payload)); err != nil {
		return "", fmt.Errorf("storing referrer payload: %w", err)
	}

	manifest := ocispec.Manifest{
		Versioned:    specs.Versioned{SchemaVersion: 2},
		MediaType:    ocispec.MediaTypeImageManifest,
		ArtifactType: artifactType,
		Config: ocispec.Descriptor{
			MediaType: emptyConfigMediaType,
			Digest:    cfgDigest,
			Size:      int64(len(emptyConfig)),
		},
		Layers: []ocispec.Descriptor{{
			MediaType: payloadMediaType,
			Digest:    payloadDigest,
			Size:      int64(len(payload)),
		}},
		Subject: &ocispec.Descriptor{
			MediaType: subject.MediaType,
			Digest:    godigest.Digest(subjectDigest),
			Size:      subject.SizeBytes,
		},
		Annotations: annotations,
	}
	content, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("marshaling referrer manifest: %w", err)
	}

	desc, err := r.pushManifest(ctx, repoW, "", content, ocispec.MediaTypeImageManifest)
	if err != nil {
		return "", err
	}
	return string(desc.Digest), nil
}

// HasReferrer reports whether a referrer of artifactType already points at subjectDigest.
func (r *Registry) HasReferrer(ctx context.Context, repo, subjectDigest, artifactType string) (bool, error) {
	repoObj, err := r.db.GetRepository(ctx, r.normalizeRepo(repo))
	if err != nil {
		return false, fmt.Errorf("getting repository: %w", err)
	}
	if repoObj == nil {
		return false, nil
	}
	manifests, err := r.db.ListManifestsBySubject(ctx, repoObj.ID, subjectDigest)
	if err != nil {
		return false, fmt.Errorf("listing referrers: %w", err)
	}
	for _, m := range manifests {
		if m.ArtifactType != nil && *m.ArtifactType == artifactType {
			return true, nil
		}
	}
	return false, nil
}

// getOwnedRepo looks up the repository and verifies the caller owns it.
func (r *Registry) getOwnedRepo(ctx context.Context, repo string) (*database.Repository, error) {
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}
	if repoObj == nil {
		return nil, ociregistry.ErrNameUnknown
	}
	if repoObj.OwnerID != r.localID {
		return nil, ociregistry.ErrDenied
	}
	return repoObj, nil
}

func (r *Registry) deleteBlob(ctx context.Context, repo string, digest ociregistry.Digest) error {
	repo, err := r.normalizeRepoForWrite(repo)
	if err != nil {
		return err
	}
	r.logger.Debug("oci: deleteBlob", "repo", repo, "digest", string(digest))
	if _, err := r.getOwnedRepo(ctx, repo); err != nil {
		return err
	}
	// Delete from DB first so that any concurrent reader that opens the file
	// via the DB index will get a not-found error rather than a dangling open.
	if err := r.db.DeleteBlob(ctx, string(digest)); err != nil {
		return fmt.Errorf("deleting blob record: %w", err)
	}
	if err := r.blobs.Delete(ctx, string(digest)); err != nil {
		return fmt.Errorf("deleting blob file: %w", err)
	}
	return nil
}

func (r *Registry) deleteManifest(ctx context.Context, repo string, digest ociregistry.Digest) error {
	repo, err := r.normalizeRepoForWrite(repo)
	if err != nil {
		return err
	}
	r.logger.Debug("oci: deleteManifest", "repo", repo, "digest", string(digest))
	repoObj, err := r.getOwnedRepo(ctx, repo)
	if err != nil {
		return err
	}
	// Record the 410-Gone tombstone before removing the manifest, so a failure
	// can never leave the manifest gone without its tombstone (which would make
	// getManifest return 404 instead of the intended 410).
	if err := r.db.RecordDeletedManifest(ctx, string(digest), repo, r.localID); err != nil {
		return fmt.Errorf("recording manifest tombstone: %w", err)
	}
	if err := r.db.DeleteManifest(ctx, repoObj.ID, string(digest)); err != nil {
		return err
	}
	if r.publisher != nil {
		if err := r.publisher.PublishManifestDelete(ctx, repo, string(digest)); err != nil {
			r.logger.Warn("failed to publish manifest delete to federation", "error", err)
		}
	}
	return nil
}

func (r *Registry) deleteTag(ctx context.Context, repo string, name string) error {
	repo, err := r.normalizeRepoForWrite(repo)
	if err != nil {
		return err
	}
	r.logger.Debug("oci: deleteTag", "repo", repo, "tag", name)
	repoObj, err := r.getOwnedRepo(ctx, repo)
	if err != nil {
		return err
	}
	if err := r.db.DeleteTag(ctx, repoObj.ID, name); err != nil {
		return fmt.Errorf("deleting tag: %w", err)
	}
	if r.publisher != nil {
		if err := r.publisher.PublishTagDelete(ctx, repo, name); err != nil {
			r.logger.Warn("failed to publish tag delete to federation", "error", err)
		}
	}
	return nil
}

const listPageSize = 1000

func (r *Registry) repositories(ctx context.Context, startAfter string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		cursor := startAfter
		for {
			repos, err := r.db.ListRepositoriesAfter(ctx, cursor, listPageSize)
			if err != nil {
				yield("", err)
				return
			}
			for _, repo := range repos {
				if !yield(repo.Name, nil) {
					return
				}
				cursor = repo.Name
			}
			if len(repos) < listPageSize {
				return
			}
		}
	}
}

func (r *Registry) tags(ctx context.Context, repo string, startAfter string) iter.Seq2[string, error] {
	repo = r.normalizeRepo(repo)
	return func(yield func(string, error) bool) {
		repoObj, err := r.db.GetRepository(ctx, repo)
		if err != nil {
			yield("", err)
			return
		}
		if repoObj == nil {
			yield("", ociregistry.ErrNameUnknown)
			return
		}

		cursor := startAfter
		for {
			tagList, err := r.db.ListTagsAfter(ctx, repoObj.ID, cursor, listPageSize)
			if err != nil {
				yield("", err)
				return
			}
			for _, t := range tagList {
				if !yield(t, nil) {
					return
				}
				cursor = t
			}
			if len(tagList) < listPageSize {
				return
			}
		}
	}
}

func (r *Registry) referrers(ctx context.Context, repo string, digest ociregistry.Digest, artifactType string) iter.Seq2[ociregistry.Descriptor, error] {
	repo = r.normalizeRepo(repo)
	return func(yield func(ociregistry.Descriptor, error) bool) {
		repoObj, err := r.db.GetRepository(ctx, repo)
		if err != nil {
			yield(ociregistry.Descriptor{}, err)
			return
		}
		if repoObj == nil {
			return
		}

		manifests, err := r.db.ListManifestsBySubject(ctx, repoObj.ID, string(digest))
		if err != nil {
			yield(ociregistry.Descriptor{}, err)
			return
		}

		for _, m := range manifests {
			at := ""
			if m.ArtifactType != nil {
				at = *m.ArtifactType
			}
			if artifactType != "" && at != artifactType {
				continue
			}
			desc := ociregistry.Descriptor{
				MediaType:    m.MediaType,
				Digest:       ociregistry.Digest(m.Digest),
				Size:         m.SizeBytes,
				ArtifactType: at,
			}
			if !yield(desc, nil) {
				return
			}
		}
	}
}

type manifestMeta struct {
	layers        []database.BlobRef
	children      []database.BlobRef
	subjectDigest *string
	artifactType  *string
}

func parseManifestMeta(content []byte, logger *slog.Logger) manifestMeta {
	var parsed struct {
		Config       ocispec.Descriptor   `json:"config"`
		Layers       []ocispec.Descriptor `json:"layers"`
		Manifests    []ocispec.Descriptor `json:"manifests"`
		Subject      *ocispec.Descriptor  `json:"subject,omitempty"`
		ArtifactType string               `json:"artifactType,omitempty"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
		if logger != nil {
			logger.Warn("failed to parse manifest metadata", "error", err)
		}
		return manifestMeta{}
	}

	var meta manifestMeta

	toRef := func(d ocispec.Descriptor) (database.BlobRef, bool) {
		if d.Digest == "" {
			return database.BlobRef{}, false
		}
		var mt *string
		if d.MediaType != "" {
			s := d.MediaType
			mt = &s
		}
		return database.BlobRef{
			Digest:    string(d.Digest),
			Size:      d.Size,
			MediaType: mt,
		}, true
	}
	if r, ok := toRef(parsed.Config); ok {
		meta.layers = append(meta.layers, r)
	}
	for _, layer := range parsed.Layers {
		if r, ok := toRef(layer); ok {
			meta.layers = append(meta.layers, r)
		}
	}
	for _, child := range parsed.Manifests {
		if r, ok := toRef(child); ok {
			meta.children = append(meta.children, r)
		}
	}

	if parsed.Subject != nil && parsed.Subject.Digest != "" {
		d := string(parsed.Subject.Digest)
		meta.subjectDigest = &d
	}

	if parsed.ArtifactType != "" {
		meta.artifactType = &parsed.ArtifactType
	}

	return meta
}
