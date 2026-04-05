package blobstore

import (
	"bytes"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(dir, nopLog())
	require.NoError(t, err)
	return s
}

func TestPutAndOpen(t *testing.T) {
	s := testStore(t)

	data := []byte("hello world")
	digest, size, err := s.Put(bytes.NewReader(data), "")
	require.NoError(t, err)
	require.Equal(t, int64(len(data)), size)
	require.NotEmpty(t, digest, "expected non-empty digest")

	f, err := s.Open(digest)
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
	// First put to get the real digest
	digest, _, err := s.Put(bytes.NewReader(data), "")
	require.NoError(t, err)

	// Put again with correct expected digest
	digest2, _, err := s.Put(bytes.NewReader(data), digest)
	require.NoError(t, err)
	require.Equal(t, digest, digest2)
}

func TestPutWithWrongDigest(t *testing.T) {
	s := testStore(t)

	data := []byte("hello world")
	_, _, err := s.Put(bytes.NewReader(data), "sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.Error(t, err, "expected error for wrong digest")
}

func TestExists(t *testing.T) {
	s := testStore(t)

	require.False(t, s.Exists("sha256:0000000000000000000000000000000000000000000000000000000000000000"), "expected false for nonexistent blob")

	data := []byte("test data")
	digest, _, _ := s.Put(bytes.NewReader(data), "")

	require.True(t, s.Exists(digest), "expected true for existing blob")
}

func TestDelete(t *testing.T) {
	s := testStore(t)

	data := []byte("delete me")
	digest, _, _ := s.Put(bytes.NewReader(data), "")

	require.True(t, s.Exists(digest), "expected blob to exist before delete")

	require.NoError(t, s.Delete(digest))

	require.False(t, s.Exists(digest), "expected blob to not exist after delete")
}

func TestDeleteNonexistent(t *testing.T) {
	s := testStore(t)
	// Should not error
	require.NoError(t, s.Delete("sha256:0000000000000000000000000000000000000000000000000000000000000000"))
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
		require.False(t, s.Exists(d), "Exists should return false for malicious digest %q", d)
		_, err := s.Open(d)
		require.Error(t, err, "Open should error for malicious digest %q", d)
	}
}

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
