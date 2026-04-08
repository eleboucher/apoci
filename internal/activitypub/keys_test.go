package activitypub

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadOrCreateIdentityEphemeral(t *testing.T) {
	id, err := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	require.NoError(t, err)

	require.Equal(t, testActorURL, id.ActorURL)
	require.Equal(t, "test.example.com", id.Domain)
	require.NotNil(t, id.PrivateKey, "expected private key to be generated")
}

func TestLoadOrCreateIdentityPersisted(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key.pem")

	// First call: generate and save
	id1, err := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", keyPath, discardLogger())
	require.NoError(t, err)

	// File should exist
	_, err = os.Stat(keyPath)
	require.NoError(t, err, "key file should exist")

	// Second call: load existing
	id2, err := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", keyPath, discardLogger())
	require.NoError(t, err)

	// Keys should match
	require.Equal(t, 0, id1.PrivateKey.D.Cmp(id2.PrivateKey.D), "loaded key should match generated key")
}

func TestPublicKeyPEM(t *testing.T) {
	id, err := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	require.NoError(t, err)

	pem, err := id.PublicKeyPEM()
	require.NoError(t, err)

	require.NotEmpty(t, pem, "expected non-empty PEM")
	require.Equal(t, "-----BEGIN PUBLIC KEY-----\n", pem[:27])
}

func TestKeyID(t *testing.T) {
	id, err := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	require.NoError(t, err)

	expected := "https://test.example.com/ap/actor#main-key"
	require.Equal(t, expected, id.KeyID())
}
