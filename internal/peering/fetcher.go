package peering

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/version"
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

// BlobStream provides a streaming reader for a blob being fetched from a peer.
// The caller must close Body when done. Size is the advertised Content-Length,
// or -1 when the source did not declare one.
type BlobStream struct {
	Body io.ReadCloser
	Size int64
}

// ErrBlobTooLarge is returned by a blob stream when the peer sends more than
// the configured maximum, instead of silently truncating to the limit.
var ErrBlobTooLarge = errors.New("peer blob exceeds maximum size")

type streamLimitedReader struct {
	reader io.Reader
	closer io.Closer
	limit  int64
	read   int64
}

func (r *streamLimitedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.read += int64(n)
	if r.read > r.limit {
		return n, ErrBlobTooLarge
	}
	return n, err
}

func (r *streamLimitedReader) Close() error {
	return r.closer.Close()
}

func peerURL(endpoint, path string) string {
	return strings.TrimRight(endpoint, "/") + path
}

// FetchStream is the URL-agnostic counterpart to FetchBlobStream. Backends
// whose download URLs aren't OCI-shaped (npm, cargo, pypi) build the URL
// themselves and pass it here.
func (f *Fetcher) FetchStream(ctx context.Context, sourceURL string) (*BlobStream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", sourceURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("source %s returned %d", sourceURL, resp.StatusCode)
	}
	return &BlobStream{
		Body: &streamLimitedReader{
			reader: resp.Body,
			closer: resp.Body,
			limit:  f.maxBlobSize,
		},
		Size: resp.ContentLength,
	}, nil
}

// FetchBlobStream initiates a blob fetch from a peer and returns a streaming reader.
// The data is not buffered in memory — the caller should pipe it directly to
// persistent storage (e.g., blobstore.Put) which handles digest verification.
func (f *Fetcher) FetchBlobStream(ctx context.Context, peerEndpoint, repo, digest string) (*BlobStream, error) {
	url := peerURL(peerEndpoint, fmt.Sprintf("/v2/%s/blobs/%s", repo, digest))

	f.logger.Debug("streaming blob from peer", "url", url, "digest", digest)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent)

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
			reader: resp.Body,
			closer: resp.Body,
			limit:  f.maxBlobSize,
		},
		Size: resp.ContentLength,
	}, nil
}

func (f *Fetcher) FetchManifest(ctx context.Context, peerEndpoint, repo, reference string) ([]byte, string, error) {
	url := peerURL(peerEndpoint, fmt.Sprintf("/v2/%s/manifests/%s", repo, reference))

	f.logger.Debug("fetching manifest from peer",
		"url", url,
		"reference", reference,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json, */*")
	req.Header.Set("User-Agent", version.UserAgent)

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
	url := peerURL(peerEndpoint, "/v2/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("creating health check request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return fmt.Errorf("health check failed for %s: %w", peerEndpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Two responses mean the peer's registry is alive:
	//   - 200: a peer not yet on the bearer-challenge build (unconditional /v2/ ping).
	//   - 401 + WWW-Authenticate: Bearer: a peer on this build, which challenges an
	//     anonymous /v2/ ping to arm OCI clients' token flow — exactly how Docker
	//     Hub/ghcr.io answer an anonymous ping. The registry is up and speaking the
	//     OCI token protocol, so it is healthy.
	// Anything else (including a bare 401 with no bearer challenge) is a failure.
	healthy := resp.StatusCode == http.StatusOK ||
		(resp.StatusCode == http.StatusUnauthorized && strings.Contains(resp.Header.Get("WWW-Authenticate"), "Bearer"))
	if !healthy {
		return fmt.Errorf("health check returned %d for %s", resp.StatusCode, peerEndpoint)
	}
	return nil
}
