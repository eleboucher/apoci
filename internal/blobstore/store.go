package blobstore

import (
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

// ErrBlobNotFound is returned by Open when the requested blob does not exist on disk.
var ErrBlobNotFound = errors.New("blob not found")

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

// Put writes content to a temp file, hashes it, and atomically renames to the final path.
// If expectedDigest is non-empty, the computed hash must match.
func (s *Store) Put(r io.Reader, expectedDigest string) (digest string, size int64, err error) {
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

	finalPath, _ := s.pathForDigest(computed) // computed digest is always valid
	if err = os.MkdirAll(filepath.Dir(finalPath), 0o750); err != nil {
		return "", 0, fmt.Errorf("creating blob subdirectory: %w", err)
	}

	if err = os.Rename(tmpPath, finalPath); err != nil {
		return "", 0, fmt.Errorf("moving blob to final path: %w", err)
	}

	s.logger.Debug("blob stored", "digest", computed, "size", size)
	return computed, size, nil
}

func (s *Store) Open(digest string) (*os.File, error) {
	path, err := s.pathForDigest(digest)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path) //nolint:gosec // path is constructed from content-addressable digest
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrBlobNotFound
		}
		return nil, fmt.Errorf("opening blob: %w", err)
	}
	return f, nil
}

func (s *Store) Exists(digest string) bool {
	path, err := s.pathForDigest(digest)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (s *Store) Delete(digest string) error {
	path, err := s.pathForDigest(digest)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting blob: %w", err)
	}
	return nil
}

var hexDigestRe = regexp.MustCompile(`^[a-f0-9]{64}$`)

// ListDigests enumerates all blob digests stored on disk.
func (s *Store) ListDigests() ([]string, error) {
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
		return digests, errors.Join(errs...)
	}
	return digests, nil
}

// pathForDigest returns the filesystem path for a validated digest.
// Rejects any digest that doesn't match sha256:[a-f0-9]{64} to prevent path traversal.
func (s *Store) pathForDigest(digest string) (string, error) {
	if err := validate.Digest(digest); err != nil {
		return "", err
	}
	hash := strings.TrimPrefix(digest, "sha256:")
	return filepath.Join(s.root, hash[:2], hash), nil
}
