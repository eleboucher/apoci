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
	"regexp"
	"strings"
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

type Publisher interface {
	PublishManifest(ctx context.Context, repo, digest, mediaType string, size int64, content []byte, subjectDigest *string) error
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
	FetchBlob(ctx context.Context, peerEndpoint, repo, digest string) (*peering.FetchResult, error)
	FetchBlobStream(ctx context.Context, peerEndpoint, repo, digest string) (*peering.BlobStream, error)
	FetchManifest(ctx context.Context, peerEndpoint, repo, reference string) ([]byte, string, error)
}

type Registry struct {
	*ociregistry.Funcs
	db              *database.DB
	blobs           *blobstore.Store
	logger          *slog.Logger
	localID         string
	namespace       string
	immutableTagRe  *regexp.Regexp
	publisher       Publisher
	resolver        ContentResolver
	fetcher         BlobFetcher
	maxManifestSize int64
	maxBlobSize     int64
}

func NewRegistry(db *database.DB, blobs *blobstore.Store, localID, namespace, immutableTagPattern string, maxManifestSize, maxBlobSize int64, logger *slog.Logger) *Registry {
	var immutableRe *regexp.Regexp
	if immutableTagPattern != "" {
		immutableRe = regexp.MustCompile(immutableTagPattern)
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
	return r
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

func (r *Registry) checkNamespace(repo string) error {
	if r.namespace == "" {
		return nil
	}
	prefix := r.namespace + "/"
	if !strings.HasPrefix(repo, prefix) {
		return fmt.Errorf("%w: repository must start with %q", ociregistry.ErrDenied, prefix)
	}
	return nil
}

const defaultMediaType = "application/octet-stream"

func (r *Registry) getBlob(ctx context.Context, repo string, digest ociregistry.Digest) (ociregistry.BlobReader, error) {
	metrics.RegistryBlobPulls.Add(1)
	f, err := r.blobs.Open(string(digest))
	if err != nil && !errors.Is(err, blobstore.ErrBlobNotFound) {
		return nil, fmt.Errorf("opening blob: %w", err)
	}
	if f != nil {
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("statting blob: %w", err)
		}
		desc := ociregistry.Descriptor{
			MediaType: defaultMediaType,
			Digest:    digest,
			Size:      info.Size(),
		}
		return newBlobReader(f, desc), nil
	}

	if r.resolver != nil && r.fetcher != nil {
		reader, err := r.fetchBlobFromPeers(ctx, repo, digest)
		if err == nil && reader != nil {
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
		stream, err := r.fetcher.FetchBlobStream(ctx, peer.PeerEndpoint, repo, string(digest))
		if err != nil {
			r.logger.Warn("failed to fetch blob from peer",
				"peer", peer.PeerEndpoint,
				"digest", string(digest),
				"error", err,
			)
			continue
		}

		storedDigest, size, err := r.blobs.Put(io.LimitReader(stream.Body, r.maxBlobSize+1), string(digest))
		if closeErr := stream.Body.Close(); closeErr != nil {
			r.logger.Warn("failed to close blob stream", "peer", peer.PeerEndpoint, "error", closeErr)
		}
		if err != nil {
			r.logger.Warn("failed to store fetched blob", "error", err)
			continue
		}
		if size > r.maxBlobSize {
			_ = r.blobs.Delete(storedDigest)
			r.logger.Warn("fetched blob exceeds max size", "digest", storedDigest, "size", size, "max", r.maxBlobSize)
			continue
		}

		mt := defaultMediaType
		if err := r.db.PutBlob(ctx, storedDigest, size, &mt, true); err != nil {
			r.logger.Warn("failed to record pulled blob metadata", "digest", storedDigest, "error", err)
		}

		if r.publisher != nil {
			if err := r.publisher.PublishBlobRef(ctx, storedDigest, size); err != nil {
				r.logger.Warn("failed to publish pulled blob ref", "digest", storedDigest, "error", err)
			}
		}

		r.logger.Info("fetched blob from federation peer",
			"digest", storedDigest,
			"peer", peer.PeerEndpoint,
			"size", size,
		)

		f, err := r.blobs.Open(storedDigest)
		if err != nil {
			if errors.Is(err, blobstore.ErrBlobNotFound) {
				r.logger.Warn("cached blob disappeared after fetch", "digest", storedDigest)
			} else {
				r.logger.Warn("failed to open cached blob after fetch", "error", err)
			}
			continue
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			continue
		}
		desc := ociregistry.Descriptor{
			MediaType: defaultMediaType,
			Digest:    digest,
			Size:      info.Size(),
		}
		return newBlobReader(f, desc), nil
	}

	return nil, fmt.Errorf("no peers have blob %s", string(digest))
}

func (r *Registry) getBlobRange(ctx context.Context, repo string, digest ociregistry.Digest, offset0, offset1 int64) (ociregistry.BlobReader, error) {
	f, err := r.blobs.Open(string(digest))
	if err != nil && !errors.Is(err, blobstore.ErrBlobNotFound) {
		return nil, fmt.Errorf("opening blob: %w", err)
	}
	if errors.Is(err, blobstore.ErrBlobNotFound) {
		if r.resolver != nil && r.fetcher != nil {
			reader, fetchErr := r.fetchBlobFromPeers(ctx, repo, digest)
			if fetchErr == nil && reader != nil {
				_ = reader.Close()
				metrics.RegistryBlobPullThru.Add(1)
				f, err = r.blobs.Open(string(digest))
				if err != nil {
					return nil, ociregistry.ErrBlobUnknown
				}
			}
		}
		if f == nil {
			return nil, ociregistry.ErrBlobUnknown
		}
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("statting blob: %w", err)
	}

	totalSize := info.Size()
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

	return nil, ociregistry.ErrManifestUnknown
}

func (r *Registry) getTag(ctx context.Context, repo string, tagName string) (ociregistry.BlobReader, error) {
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

	// Cache the content locally so we don't have to pull-through again.
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
	blob, err := r.db.GetBlob(ctx, string(digest))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("resolving blob: %w", err)
	}
	if blob == nil || !blob.StoredLocally {
		return ociregistry.Descriptor{}, ociregistry.ErrBlobUnknown
	}

	mediaType := "application/octet-stream"
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
	if err := r.checkNamespace(repo); err != nil {
		return ociregistry.Descriptor{}, err
	}
	if _, err := r.db.GetOrCreateRepository(ctx, repo, r.localID); err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("getting repository: %w", err)
	}

	limited := io.LimitReader(rd, r.maxBlobSize+1)
	digest, size, err := r.blobs.Put(limited, string(desc.Digest))
	if err != nil {
		return ociregistry.Descriptor{}, fmt.Errorf("storing blob: %w", err)
	}
	if size > r.maxBlobSize {
		_ = r.blobs.Delete(digest)
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

func (r *Registry) pushBlobChunked(ctx context.Context, repo string, chunkSize int) (ociregistry.BlobWriter, error) {
	if err := r.checkNamespace(repo); err != nil {
		return nil, err
	}
	if _, err := r.db.GetOrCreateRepository(ctx, repo, r.localID); err != nil {
		return nil, fmt.Errorf("getting repository: %w", err)
	}

	// The commit callback fires after the HTTP handler returns, so use a non-cancellable
	// context with a timeout to prevent indefinite blocking on shutdown.
	commitCtx, commitCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Minute)
	buf := ocimem.NewBuffer(func(b *ocimem.Buffer) error {
		defer commitCancel()
		desc, data, err := b.GetBlob()
		if err != nil {
			return fmt.Errorf("getting blob from buffer: %w", err)
		}
		if int64(len(data)) > r.maxBlobSize {
			return fmt.Errorf("%w: blob exceeds maximum size (%d bytes)", ociregistry.ErrBlobUploadInvalid, r.maxBlobSize)
		}
		_, _, err = r.blobs.Put(bytes.NewReader(data), string(desc.Digest))
		if err != nil {
			return fmt.Errorf("storing chunked blob: %w", err)
		}
		mt := desc.MediaType
		if err := r.db.PutBlob(commitCtx, string(desc.Digest), desc.Size, &mt, true); err != nil {
			return fmt.Errorf("recording chunked blob: %w", err)
		}
		r.logger.Info("chunked blob committed",
			"repo", repo,
			"digest", string(desc.Digest),
			"size", desc.Size)
		return nil
	}, "")

	return buf, nil
}

func (r *Registry) pushBlobChunkedResume(ctx context.Context, repo, id string, offset int64, chunkSize int) (ociregistry.BlobWriter, error) {
	return nil, ociregistry.ErrUnsupported
}

func (r *Registry) pushManifest(ctx context.Context, repo string, tag string, contents []byte, mediaType string) (ociregistry.Descriptor, error) {
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

	meta := parseManifestMeta(contents)

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
		if err := r.publisher.PublishManifest(ctx, repo, digest, mediaType, int64(len(contents)), contents, meta.subjectDigest); err != nil {
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

func (r *Registry) deleteBlob(ctx context.Context, repo string, digest ociregistry.Digest) error {
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("getting repository: %w", err)
	}
	if repoObj == nil {
		return ociregistry.ErrNameUnknown
	}
	if repoObj.OwnerID != r.localID {
		return ociregistry.ErrDenied
	}
	// Delete from DB first so that any concurrent reader that opens the file
	// via the DB index will get a not-found error rather than a dangling open.
	if err := r.db.DeleteBlob(ctx, string(digest)); err != nil {
		return fmt.Errorf("deleting blob record: %w", err)
	}
	if err := r.blobs.Delete(string(digest)); err != nil {
		return fmt.Errorf("deleting blob file: %w", err)
	}
	return nil
}

func (r *Registry) deleteManifest(ctx context.Context, repo string, digest ociregistry.Digest) error {
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("getting repository: %w", err)
	}
	if repoObj == nil {
		return ociregistry.ErrNameUnknown
	}
	if repoObj.OwnerID != r.localID {
		return ociregistry.ErrDenied
	}
	return r.db.DeleteManifest(ctx, repoObj.ID, string(digest))
}

func (r *Registry) deleteTag(ctx context.Context, repo string, name string) error {
	repoObj, err := r.db.GetRepository(ctx, repo)
	if err != nil {
		return fmt.Errorf("getting repository: %w", err)
	}
	if repoObj == nil {
		return ociregistry.ErrNameUnknown
	}
	if repoObj.OwnerID != r.localID {
		return ociregistry.ErrDenied
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

func parseManifestMeta(content []byte) manifestMeta {
	var parsed struct {
		Config       ocispec.Descriptor   `json:"config"`
		Layers       []ocispec.Descriptor `json:"layers"`
		Subject      *ocispec.Descriptor  `json:"subject,omitempty"`
		ArtifactType string               `json:"artifactType,omitempty"`
	}
	if err := json.Unmarshal(content, &parsed); err != nil {
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
