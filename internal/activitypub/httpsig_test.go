package activitypub

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSignAndVerifyRequest(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	body := []byte(`{"type": "Follow"}`)
	req := httptest.NewRequest("POST", "https://example.com/ap/inbox", nil)

	err = SignRequest(req, "https://me.example.com/ap/actor#main-key", privKey, body)
	require.NoError(t, err)

	// Verify the Signature header was set
	require.NotEmpty(t, req.Header.Get("Signature"), "expected Signature header")

	// Verify the Date header was set
	require.NotEmpty(t, req.Header.Get("Date"), "expected Date header")

	// Verify the Digest header was set
	require.NotEmpty(t, req.Header.Get("Digest"), "expected Digest header for body")

	// Build PEM from public key
	id := &Identity{PrivateKey: privKey}
	pubPEM, _ := id.PublicKeyPEM()

	// Verify
	err = VerifyRequest(req, pubPEM, body, nil)
	require.NoError(t, err, "verification failed")
}

func TestSignAndVerifyRequestNoBody(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	req := httptest.NewRequest("GET", "https://example.com/ap/actor", nil)

	err = SignRequest(req, "https://me.example.com/ap/actor#main-key", privKey, nil)
	require.NoError(t, err)

	// No Digest header for GET
	require.Empty(t, req.Header.Get("Digest"), "should not have Digest header for empty body")

	id := &Identity{PrivateKey: privKey}
	pubPEM, _ := id.PublicKeyPEM()

	err = VerifyRequest(req, pubPEM, nil, nil)
	require.NoError(t, err, "verification failed")
}

func TestVerifyRequestWrongKey(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	req := httptest.NewRequest("POST", "https://example.com/ap/inbox", nil)
	require.NoError(t, SignRequest(req, "key1", privKey, []byte("hello")))

	otherID := &Identity{PrivateKey: otherKey}
	otherPEM, _ := otherID.PublicKeyPEM()

	err := VerifyRequest(req, otherPEM, []byte("hello"), nil)
	require.Error(t, err, "expected verification to fail with wrong key")
}

func TestExtractKeyID(t *testing.T) {
	privKey, _ := rsa.GenerateKey(rand.Reader, 2048)

	req := httptest.NewRequest("POST", "https://example.com/ap/inbox", nil)
	require.NoError(t, SignRequest(req, "https://alice.example.com/ap/actor#main-key", privKey, []byte("test")))

	keyID, err := ExtractKeyID(req)
	require.NoError(t, err)
	require.Equal(t, "https://alice.example.com/ap/actor#main-key", keyID)
}

func TestReplayDetection(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	body := []byte(`{"type": "Follow"}`)
	req := httptest.NewRequest("POST", "https://example.com/ap/inbox", nil)
	require.NoError(t, SignRequest(req, "https://me.example.com/ap/actor#main-key", privKey, body))

	id := &Identity{PrivateKey: privKey}
	pubPEM, _ := id.PublicKeyPEM()

	cache := NewSignatureCache()
	defer cache.Stop()

	// First verification should succeed.
	err = VerifyRequest(req, pubPEM, body, cache)
	require.NoError(t, err, "first verification should succeed")

	// Second verification with the same signature should be rejected as a replay.
	err = VerifyRequest(req, pubPEM, body, cache)
	require.Error(t, err, "expected replay to be rejected")
	require.Contains(t, err.Error(), "replayed signature")
}
