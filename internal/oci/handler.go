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
	"regexp"
	"strings"
	"sync"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"cuelabs.dev/go/oci/ociregistry"
	"cuelabs.dev/go/oci/ociregistry/ocimem"
	"cuelabs.dev/go/oci/ociregistry/ociserver"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/metrics"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/peering"
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

	GetManifestByDigest(ctx context.Context, repoID int64, digest string) (*database.Manifest, error)
	GetManifestByTag(ctx context.Context, repoID int64, tag string) (*database.Manifest, error)
	PutManifest(ctx context.Context, m *database.Manifest) error
	DeleteManifest(ctx context.Context, repoID int64, digest string) error
	IsManifestDeleted(ctx context.Context, digest string) (bool, error)
	ListManifestsBySubject(ctx context.Context, repoID int64, subjectDigest string) ([]database.Manifest, error)
	PutManifestLayers(ctx context.Context, manifestID int64, layerDigests []string) error

	PutTagWithImmutable(ctx context.Context, repoID int64, tag, digest string, immutable bool) error
	DeleteTag(ctx context.Context, repoID int64, tag string) error
	ListTagsAfter(ctx context.Context, repoID int64, startAfter string, limit int) ([]string, error)

	GetFollow(ctx context.Context, actorURL string) (*database.Follow, error)

	CreateUploadSession(ctx context.Context, uuid string, repoID int64, ttl time.Duration) (*database.UploadSession, error)
	GetUploadSession(ctx context.Context, uuid string) (*database.UploadSession, error)
	DeleteUploadSession(ctx context.Context, uuid string) error
	ListExpiredUploadSessions(ctx context.Context, limit int) ([]string, error)
}

type Publisher interface {
	PublishManifest(ctx context.Context, repo, tag, digest, mediaType string, size int64, content []byte, subjectDigest *string) error
	PublishTag(ctx context.Context, repo, tag, digest string) error
	PublishBlobRef(ctx context.Context, digest string, size int64) error
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

type Registry struct {
	*ociregistry.Funcs
	db              RegistryRepository
	blobs           blobstore.BlobStore
	logger          *slog.Logger
	localID         string
	namespace       string
	immutableTagRe  *regexp.Regexp
	publisher       Publisher
	resolver        ContentResolver
	fetcher         BlobFetcher
	maxManifestSize int64
	maxBlobSize     int64

	uploadsMu sync.Mutex
	uploads   map[string]*ocimem.Buffer
}

func NewRegistry(db *database.DB, blobs blobstore.BlobStore, localID, namespace, immutableTagPattern string, maxManifestSize, maxBlobSize int64, logger *slog.Logger) (*Registry, error) {
	var immutableRe *regexp.Regexp
	if immutableTagPattern != "" {
		var err error
		immutableRe, err = regexp.Compile(immutableTagPattern)
		if err != nil {
			return nil, fmt.Errorf("invalid immutable tag pattern %q: %w", immutableTagPattern, err)
		}
	}

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
		immutableTagRe:  immutableRe,
		maxManifestSize: maxManifestSize,
		maxBlobSize:     maxBlobSize,
		uploads:         make(map[string]*ocimem.Buffer),
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

func (r *Registry) SetPublisher(p Publisher) {
	r.publisher = p
}

func (r *Registry) SetFederation(resolver ContentResolver, fetcher BlobFetcher) {
	r.resolver = resolver
	r.fetcher = fetcher
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
		delete(r.uploads, uuid)
		r.uploadsMu.Unlock()

		if err := r.db.DeleteUploadSession(ctx, uuid); err != nil {
			r.logger.Warn("failed to delete expired upload session", "uuid", uuid, "error", err)
		}
	}

	if len(expired) > 0 {
		r.logger.Info("cleaned expired upload sessions", "count", len(expired))
	}
	return len(expired), nil
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

const defaultMediaType = "application/octet-stream"

func (r *Registry) getBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	repo = r.normalizeRepo(repo)
	metrics.RegistryBlobPulls.Add(1)

	exists, err := r.db.BlobExistsInRepo(ctx, repo, string(digest))
	if err != nil {
		return nil, fmt.Errorf("checking blob repo scope: %w", err)
	}
	if !exists {
		return nil, ociregistry.ErrBlobUnknown
	}

	f, blobSize, openErr := r.blobs.Open(ctx, string(digest))
	switch {
	case openErr == nil:
		return newBlobReader(f, ociregistry.Descriptor{
			MediaType: defaultMediaType,
			Digest:    digest,
			Size:      blobSize,
		}), nil
	case !errors.Is(openErr, blobstore.ErrBlobNotFound):
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
	repo = r.normalizeRepo(repo)
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
					if err := r.db.PutManifest(ctx, m); err != nil {
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
	repo = r.normalizeRepo(repo)
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}
	if repoObj == nil {
		return nil, ociregistry.ErrNameUnknown
	}

	m, err := r.db.GetManifestByTag(ctx, repoObj.ID, tagName)
	if err != nil {
		return nil, fmt.Errorf("getting manifest by tag: %w", err)
	}
	if m == nil {
		return nil, ociregistry.ErrManifestUnknown
	}

	return r.serveManifest(ctx, repo, m)
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
	repo = r.normalizeRepo(repo)
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving manifest: %w", err)
	}
	if repoObj == nil {
		return ociregistry.Descriptor{}, ociregistry.ErrNameUnknown
	}

	m, err := r.db.GetManifestByDigest(ctx, repoObj.ID, string(digest))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving manifest: %w", err)
	}
	if m == nil {
		return ociregistry.Descriptor{}, ociregistry.ErrManifestUnknown
	}

	return ociregistry.Descriptor{
		MediaType: m.MediaType,
		Digest:    ociregistry.Digest(m.Digest),
		Size:      m.SizeBytes,
	}, nil
}

func (r *Registry) resolveTag(ctx context.Context, repo string, tagName string) (ociregistry.Descriptor, error) {
	repo = r.normalizeRepo(repo)
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving tag: %w", err)
	}
	if repoObj == nil {
		return ociregistry.Descriptor{}, ociregistry.ErrNameUnknown
	}

	m, err := r.db.GetManifestByTag(ctx, repoObj.ID, tagName)
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving tag: %w", err)
	}
	if m == nil {
		return ociregistry.Descriptor{}, ociregistry.ErrManifestUnknown
	}

	return ociregistry.Descriptor{
		MediaType: m.MediaType,
		Digest:    ociregistry.Digest(m.Digest),
		Size:      m.SizeBytes,
	}, nil
}

func (r *Registry) pushBlob(ctx context.Context, repo string, desc ociregistry.Descriptor, rd io.Reader) (ociregistry.Descriptor, error) {
	repo = r.normalizeRepo(repo)
	if err := r.checkNamespace(repo); err != nil {
		return ociregistry.Descriptor{}, err
	}

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

const uploadSessionTTL = 1 * time.Hour

func (r *Registry) pushBlobChunked(ctx context.Context, repo string, chunkSize int) (ociregistry.BlobWriter, error) {
	repo = r.normalizeRepo(repo)
	if err := r.checkNamespace(repo); err != nil {
		return nil, err
	}
	repoObj, err := r.db.GetOrCreateRepository(ctx, repo, r.localID)
	if err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}

	buf := r.newUploadBuffer(repo, "")

	if _, err := r.db.CreateUploadSession(ctx, buf.ID(), repoObj.ID, uploadSessionTTL); err != nil {
		r.logger.Warn("failed to persist upload session", "uuid", buf.ID(), "error", err)
	}

	return &limitedBlobWriter{BlobWriter: buf, maxSize: r.maxBlobSize}, nil
}

type limitedBlobWriter struct {
	ociregistry.BlobWriter
	maxSize int64
}

func (l *limitedBlobWriter) Write(p []byte) (int, error) {
	if l.Size()+int64(len(p)) > l.maxSize {
		return 0, fmt.Errorf("%w: blob exceeds maximum size (%d bytes)", ociregistry.ErrBlobUploadInvalid, l.maxSize)
	}
	return l.BlobWriter.Write(p)
}

func (r *Registry) pushBlobChunkedResume(ctx context.Context, repo, id string, offset int64, chunkSize int) (ociregistry.BlobWriter, error) {
	repo = r.normalizeRepo(repo)
	if err := r.checkNamespace(repo); err != nil {
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
	buf, ok := r.uploads[id]
	r.uploadsMu.Unlock()

	if !ok {
		// Session exists in DB but buffer was lost (e.g. server restart).
		// Clean up the stale DB record.
		_ = r.db.DeleteUploadSession(ctx, id)
		return nil, fmt.Errorf("%w: upload %q session expired (server restarted)", ociregistry.ErrBlobUploadUnknown, id)
	}

	return &limitedBlobWriter{BlobWriter: buf, maxSize: r.maxBlobSize}, nil
}

func (r *Registry) newUploadBuffer(repo, uuid string) *ocimem.Buffer {
	buf := ocimem.NewBuffer(func(b *ocimem.Buffer) error {
		r.uploadsMu.Lock()
		delete(r.uploads, b.ID())
		r.uploadsMu.Unlock()

		desc, data, err := b.GetBlob()
		if err != nil {
			return fmt.Errorf("getting blob from buffer: %w", err)
		}
		if int64(len(data)) > r.maxBlobSize {
			return fmt.Errorf("%w: blob exceeds maximum size (%d bytes)", ociregistry.ErrBlobUploadInvalid, r.maxBlobSize)
		}
		// This callback runs after the HTTP request context has been released,
		// so we use a fresh timeout context for storage and DB operations.
		commitCtx, commitCancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer commitCancel()

		_, _, err = r.blobs.Put(commitCtx, bytes.NewReader(data), string(desc.Digest))
		if err != nil {
			return fmt.Errorf("storing chunked blob: %w", err)
		}

		mt := desc.MediaType
		if err := r.db.PutBlob(commitCtx, string(desc.Digest), desc.Size, &mt, true); err != nil {
			return fmt.Errorf("recording chunked blob: %w", err)
		}

		// Clean up the upload session from DB.
		if err := r.db.DeleteUploadSession(commitCtx, b.ID()); err != nil {
			r.logger.Warn("failed to delete upload session", "uuid", b.ID(), "error", err)
		}

		metrics.RegistryBlobPushes.Add(1)
		r.logger.Info("chunked blob committed",
			"repo", repo,
			"digest", string(desc.Digest),
			"size", desc.Size)

		if r.publisher != nil {
			if err := r.publisher.PublishBlobRef(commitCtx, string(desc.Digest), desc.Size); err != nil {
				r.logger.Warn("failed to publish chunked blob ref to federation", "error", err)
			}
		}

		return nil
	}, uuid)

	r.uploadsMu.Lock()
	r.uploads[buf.ID()] = buf
	r.uploadsMu.Unlock()

	return buf
}

func (r *Registry) pushManifest(ctx context.Context, repo string, tag string, contents []byte, mediaType string) (ociregistry.Descriptor, error) {
	repo = r.normalizeRepo(repo)
	if err := r.checkNamespace(repo); err != nil {
		return ociregistry.Descriptor{}, err
	}
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
	for _, blobDigest := range meta.layerDigests {
		if validate.Digest(blobDigest) != nil {
			continue
		}
		blob, err := r.db.GetBlob(ctx, blobDigest)
		if err != nil {
			return ociregistry.Descriptor{}, fmt.Errorf("checking blob %s: %w", blobDigest, err)
		}
		if blob == nil || !blob.StoredLocally {
			return ociregistry.Descriptor{}, fmt.Errorf("%w: referenced blob %s not found", ociregistry.ErrBlobUnknown, blobDigest)
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

	if err := r.db.PutManifest(ctx, m); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("storing manifest: %w", err)
	}

	if tag != "" {
		immutable := r.immutableTagRe != nil && r.immutableTagRe.MatchString(tag)
		if err := r.db.PutTagWithImmutable(ctx, repoObj.ID, tag, digest, immutable); err != nil {
			if errors.Is(err, database.ErrTagImmutable) {
				return ociregistry.Descriptor{}, fmt.Errorf("%w: tag %q is immutable", ociregistry.ErrDenied, tag)
			}
			return ociregistry.Descriptor{}, fmt.Errorf("storing tag: %w", err)
		}
	}

	if len(meta.layerDigests) > 0 {
		man, err := r.db.GetManifestByDigest(ctx, repoObj.ID, digest)
		if err == nil && man != nil {
			if err := r.db.PutManifestLayers(ctx, man.ID, meta.layerDigests); err != nil {
				r.logger.Warn("failed to record manifest layers", "digest", digest, "error", err)
			}
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

	return ociregistry.Descriptor{
		MediaType: mediaType,
		Digest:    ociregistry.Digest(digest),
		Size:      int64(len(contents)),
	}, nil
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
	repo = r.normalizeRepo(repo)
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
	repo = r.normalizeRepo(repo)
	repoObj, err := r.getOwnedRepo(ctx, repo)
	if err != nil {
		return err
	}
	return r.db.DeleteManifest(ctx, repoObj.ID, string(digest))
}

func (r *Registry) deleteTag(ctx context.Context, repo string, name string) error {
	repo = r.normalizeRepo(repo)
	repoObj, err := r.getOwnedRepo(ctx, repo)
	if err != nil {
		return err
	}
	if err := r.db.DeleteTag(ctx, repoObj.ID, name); err != nil {
		if errors.Is(err, database.ErrTagImmutable) {
			return fmt.Errorf("%w: tag %q is immutable", ociregistry.ErrDenied, name)
		}
		return fmt.Errorf("deleting tag: %w", err)
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
	layerDigests  []string
	subjectDigest *string
	artifactType  *string
}

func parseManifestMeta(content []byte, logger *slog.Logger) manifestMeta {
	var parsed struct {
		Config       ocispec.Descriptor   `json:"config"`
		Layers       []ocispec.Descriptor `json:"layers"`
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

	if parsed.Config.Digest != "" {
		meta.layerDigests = append(meta.layerDigests, string(parsed.Config.Digest))
	}
	for _, layer := range parsed.Layers {
		if layer.Digest != "" {
			meta.layerDigests = append(meta.layerDigests, string(layer.Digest))
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
