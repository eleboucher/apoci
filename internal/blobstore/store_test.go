package blobstore

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

var ctx = context.Background()

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir, nopLog())
	require.NoError(t, err)
	return s
}

func mustExist(t *testing.T, s BlobStore, digest string) {
	t.Helper()
	ok, err := s.Exists(ctx, digest)
	require.NoError(t, err)
	require.True(t, ok)
}

func mustNotExist(t *testing.T, s BlobStore, digest string) {
	t.Helper()
	ok, err := s.Exists(ctx, digest)
	require.NoError(t, err)
	require.False(t, ok)
}

func TestPutAndOpen(t *testing.T) {
	s := testStore(t)

	data := []byte("hello world")
	digest, size, err := s.Put(ctx, bytes.NewReader(data), "")
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), size)
	require.NotEmpty(t, digest, "expected non-empty digest")

	f, _, err := s.Open(ctx, digest)
	require.NoError(t, err)
	require.NotNil(t, f)
	defer func() { _ = f.Close() }()

	got, err := io.ReadAll(f)
	require.NoError(t, err)
	require.True(t, bytes.Equal(got, data))
}

func TestPutWithExpectedDigest(t *testing.T) {
	s := testStore(t)

	data := []byte("hello world")
	digest, _, err := s.Put(ctx, bytes.NewReader(data), "")
	require.NoError(t, err)

	digest2, _, err := s.Put(ctx, bytes.NewReader(data), digest)
	require.NoError(t, err)
	require.Equal(t, digest, digest2)
}

func TestPutWithWrongDigest(t *testing.T) {
	s := testStore(t)

	data := []byte("hello world")
	_, _, err := s.Put(ctx, bytes.NewReader(data), "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err, "expected error for wrong digest")
}

func TestExists(t *testing.T) {
	s := testStore(t)

	mustNotExist(t, s, "sha256:0000000000000000000000000000000000000000000000000000000000000000")

	data := []byte("test data")
	digest, _, _ := s.Put(ctx, bytes.NewReader(data), "")

	mustExist(t, s, digest)
}

func TestDelete(t *testing.T) {
	s := testStore(t)

	data := []byte("delete me")
	digest, _, _ := s.Put(ctx, bytes.NewReader(data), "")

	mustExist(t, s, digest)
	require.NoError(t, s.Delete(ctx, digest))
	mustNotExist(t, s, digest)
}

func TestDeleteNonexistent(t *testing.T) {
	s := testStore(t)
	require.NoError(t, s.Delete(ctx, "sha256:0000000000000000000000000000000000000000000000000000000000000000"))
}

func TestPathTraversalRejected(t *testing.T) {
	s := testStore(t)

	malicious := []string{
		"sha256:../../../etc/passwd",
		"sha256:..%2F..%2Fetc%2Fpasswd",
		"notsha256:abcd",
		"sha256:short",
		"sha256:ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789", // uppercase
		"",
	}

	for _, d := range malicious {
		mustNotExist(t, s, d)
		_, _, err := s.Open(ctx, d)
		require.Error(t, err, "Open should error for malicious digest %q", d)
	}
}

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
