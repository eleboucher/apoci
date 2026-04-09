package blobstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/validate"
)

// ErrBlobNotFound is returned by Open when the requested blob does not exist.
var ErrBlobNotFound = errors.New("blob not found")

// BlobStore is the interface for content-addressable blob storage.
type BlobStore interface {
	Put(ctx context.Context, r io.Reader, expectedDigest string) (digest string, size int64, err error)
	// Open returns a seekable reader for the blob and its size. Returns
	// ErrBlobNotFound if the blob does not exist. The returned reader is not safe
	// for concurrent use.
	Open(ctx context.Context, digest string) (io.ReadSeekCloser, int64, error)
	// Exists reports whether the blob is present in the store.
	// Returns an error on storage failure; callers must not treat an error as "not found".
	Exists(ctx context.Context, digest string) (bool, error)
	// Delete removes the blob. It is not an error if the blob does not exist.
	Delete(ctx context.Context, digest string) error
	ListDigests(ctx context.Context) ([]string, error)
}

type Store struct {
	root   string
	logger *slog.Logger
}

func New(dataDir string, logger *slog.Logger) (*Store, error) {
	root := filepath.Join(dataDir, "blobs", "sha256")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("creating blob directory: %w", err)
	}
	return &Store{root: root, logger: logger}, nil
}

func (s *Store) Put(_ context.Context, r io.Reader, expectedDigest string) (digest string, size int64, err error) {
	if expectedDigest != "" {
		if err := validate.Digest(expectedDigest); err != nil {
			return "", 0, fmt.Errorf("invalid expected digest: %w", err)
		}
	}

	tmp, err := os.CreateTemp(s.root, ".upload-*")
	if err != nil {
		return "", 0, fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	w := io.MultiWriter(tmp, h)

	size, err = io.Copy(w, r)
	if err != nil {
		return "", 0, fmt.Errorf("writing blob: %w", err)
	}

	if err = tmp.Sync(); err != nil {
		return "", 0, fmt.Errorf("syncing blob: %w", err)
	}

	computed := "sha256:" + hex.EncodeToString(h.Sum(nil))

	if expectedDigest != "" && computed != expectedDigest {
		return "", 0, fmt.Errorf("digest mismatch: expected %s, got %s", expectedDigest, computed)
	}

	finalPath, err := s.pathForDigest(computed) // computed digest is always valid
	if err != nil {
		return "", 0, fmt.Errorf("unexpected invalid computed digest %s: %w", computed, err)
	}
	if err = os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		return "", 0, fmt.Errorf("creating blob subdirectory: %w", err)
	}

	if err = os.Rename(tmpPath, finalPath); err != nil {
		return "", 0, fmt.Errorf("moving blob to final path: %w", err)
	}

	s.logger.Debug("blob stored", "digest", computed, "size", size)
	return computed, size, nil
}

func (s *Store) Open(_ context.Context, digest string) (io.ReadSeekCloser, int64, error) {
	path, err := s.pathForDigest(digest)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(path) //nolint:gosec // path is constructed from content-addressable digest
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, ErrBlobNotFound
		}
		return nil, 0, fmt.Errorf("opening blob: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, fmt.Errorf("statting blob: %w", err)
	}
	return f, fi.Size(), nil
}

func (s *Store) Exists(_ context.Context, digest string) (bool, error) {
	path, err := s.pathForDigest(digest)
	if err != nil {
		return false, nil // invalid digest is simply not found
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("checking blob existence: %w", err)
}

func (s *Store) Delete(_ context.Context, digest string) error {
	path, err := s.pathForDigest(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("deleting blob: %w", err)
	}
	return nil
}

var hexDigestRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (s *Store) ListDigests(_ context.Context) ([]string, error) {
	var digests []string
	var errs []error

	subdirs, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("reading blob root: %w", err)
	}
	for _, subdir := range subdirs {
		if !subdir.IsDir() || len(subdir.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(s.root, subdir.Name()))
		if err != nil {
			errs = append(errs, fmt.Errorf("reading blob subdir %s: %w", subdir.Name(), err))
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if !hexDigestRe.MatchString(entry.Name()) {
				continue
			}
			digests = append(digests, "sha256:"+entry.Name())
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return digests, nil
}

func (s *Store) pathForDigest(digest string) (string, error) {
	if err := validate.Digest(digest); err != nil {
		return "", err
	}
	hash := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(s.root, hash[:2], hash), nil
}
