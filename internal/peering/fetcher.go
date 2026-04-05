package peering

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/apoci/apoci/internal/validate"
)

type Fetcher struct {
	client          *http.Client
	logger          *slog.Logger
	maxBlobSize     int64
	maxManifestSize int64
}

func NewFetcher(timeout time.Duration, maxBlobSize, maxManifestSize int64, logger *slog.Logger) *Fetcher {
	return &Fetcher{
		client: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				DialContext: validate.SafeDialContext,
			},
		},
		logger:          logger,
		maxBlobSize:     maxBlobSize,
		maxManifestSize: maxManifestSize,
	}
}

type FetchResult struct {
	Data   []byte
	Digest string
	Size   int64
}

func (f *Fetcher) FetchBlob(ctx context.Context, peerEndpoint, repo, digest string) (*FetchResult, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", strings.TrimRight(peerEndpoint, "/"), repo, digest)

	f.logger.Debug("fetching blob from peer",
		"url", url,
		"digest", digest,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching blob from %s: %w", peerEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s returned %d for blob %s", peerEndpoint, resp.StatusCode, digest)
	}

	// Size limit prevents OOM from malicious peers.
	h := sha256.New()
	limited := io.LimitReader(resp.Body, f.maxBlobSize+1)
	data, err := io.ReadAll(io.TeeReader(limited, h))
	if err != nil {
		return nil, fmt.Errorf("reading blob from %s: %w", peerEndpoint, err)
	}
	if int64(len(data)) > f.maxBlobSize {
		return nil, fmt.Errorf("blob from %s exceeds max size (%d bytes)", peerEndpoint, f.maxBlobSize)
	}

	computedDigest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if computedDigest != digest {
		return nil, fmt.Errorf("digest mismatch from peer %s: expected %s, got %s", peerEndpoint, digest, computedDigest)
	}

	f.logger.Debug("blob fetched and verified",
		"digest", digest,
		"size", int64(len(data)),
		"peer", peerEndpoint,
	)

	return &FetchResult{
		Data:   data,
		Digest: computedDigest,
		Size:   int64(len(data)),
	}, nil
}

// BlobStream provides a streaming reader for a blob being fetched from a peer.
// The caller must close Body when done.
type BlobStream struct {
	Body io.ReadCloser
}

type streamLimitedReader struct {
	io.Reader
	closer io.Closer
}

func (r *streamLimitedReader) Close() error {
	return r.closer.Close()
}

// FetchBlobStream initiates a blob fetch from a peer and returns a streaming reader.
// Unlike FetchBlob, the data is not buffered in memory — the caller should pipe it
// directly to persistent storage (e.g., blobstore.Put) which handles digest verification.
func (f *Fetcher) FetchBlobStream(ctx context.Context, peerEndpoint, repo, digest string) (*BlobStream, error) {
	url := fmt.Sprintf("%s/v2/%s/blobs/%s", strings.TrimRight(peerEndpoint, "/"), repo, digest)

	f.logger.Debug("streaming blob from peer", "url", url, "digest", digest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching blob from %s: %w", peerEndpoint, err)
	}

	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("peer %s returned %d for blob %s", peerEndpoint, resp.StatusCode, digest)
	}

	return &BlobStream{
		Body: &streamLimitedReader{
			Reader: io.LimitReader(resp.Body, f.maxBlobSize+1),
			closer: resp.Body,
		},
	}, nil
}

func (f *Fetcher) FetchManifest(ctx context.Context, peerEndpoint, repo, reference string) ([]byte, string, error) {
	url := fmt.Sprintf("%s/v2/%s/manifests/%s", strings.TrimRight(peerEndpoint, "/"), repo, reference)

	f.logger.Debug("fetching manifest from peer",
		"url", url,
		"reference", reference,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, */*")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetching manifest from %s: %w", peerEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("peer %s returned %d for manifest %s/%s", peerEndpoint, resp.StatusCode, repo, reference)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxManifestSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("reading manifest from %s: %w", peerEndpoint, err)
	}
	if int64(len(data)) > f.maxManifestSize {
		return nil, "", fmt.Errorf("manifest from %s exceeds max size (%d bytes)", peerEndpoint, f.maxManifestSize)
	}

	// Always verify manifest digest — compute from content and check against
	// the reference (if it's a digest) or the Docker-Content-Digest response header.
	h := sha256.New()
	h.Write(data)
	computedDigest := "sha256:" + hex.EncodeToString(h.Sum(nil))

	if strings.HasPrefix(reference, "sha256:") {
		if computedDigest != reference {
			return nil, "", fmt.Errorf("manifest digest mismatch from peer %s: expected %s, got %s", peerEndpoint, reference, computedDigest)
		}
	} else if dcd := resp.Header.Get("Docker-Content-Digest"); dcd != "" && dcd != computedDigest {
		return nil, "", fmt.Errorf("manifest digest mismatch from peer %s: header says %s, computed %s", peerEndpoint, dcd, computedDigest)
	}

	mediaType := resp.Header.Get("Content-Type")
	if mediaType == "" {
		mediaType = "application/vnd.oci.image.manifest.v1+json"
	}

	return data, mediaType, nil
}

func (f *Fetcher) CheckHealth(ctx context.Context, peerEndpoint string) error {
	url := fmt.Sprintf("%s/v2/", strings.TrimRight(peerEndpoint, "/"))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating health check request: %w", err)
	}

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed for %s: %w", peerEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check returned %d for %s", resp.StatusCode, peerEndpoint)
	}
	return nil
}
