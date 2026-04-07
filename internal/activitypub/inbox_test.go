package activitypub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func testInboxSetup(t *testing.T) *InboxHandler {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	id, err := LoadOrCreateIdentity("bob.example.com", "", "", discardLogger())
	require.NoError(t, err)

	handler := NewInboxHandler(id, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      "none",
	}, discardLogger())
	return handler
}

func TestInboxRejectsUnsigned(t *testing.T) {
	handler := testInboxSetup(t)

	body := []byte(`{"type":"Follow","actor":"https://alice.example.com/ap/actor","object":"https://bob.example.com/ap/actor"}`)
	req := httptest.NewRequest("POST", "/ap/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/activity+json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestInboxRejectsGet(t *testing.T) {
	handler := testInboxSetup(t)

	req := httptest.NewRequest("GET", "/ap/inbox", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestKeyIDToActorURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://example.com/ap/actor#main-key", "https://example.com/ap/actor"},
		{"https://example.com/ap/actor", "https://example.com/ap/actor"},
	}

	for _, tt := range tests {
		got := keyIDToActorURL(tt.input)
		assert.Equal(t, tt.expected, got, "keyIDToActorURL(%q)", tt.input)
	}
}

func TestEndpointFromActorURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"https://example.com/ap/actor", "https://example.com"},
		{"https://example.com/other", "https://example.com"},
		{"https://node.example.org:8443/ap/actor", "https://node.example.org:8443"},
		{"%%invalid", "%%invalid"},
	}

	for _, tt := range tests {
		got := EndpointFromActorURL(tt.input)
		assert.Equal(t, tt.expected, got, "EndpointFromActorURL(%q)", tt.input)
	}
}

const testOrderedCollection = "OrderedCollection"

func TestOutboxHandler(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("test.example.com", "", "", discardLogger())
	handler := NewOutboxHandler(id, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ap/outbox", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var collection map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&collection))
	require.Equal(t, testOrderedCollection, collection["type"])
}

func TestFollowingHandler(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("test.example.com", "", "", discardLogger())
	handler := NewFollowingHandler(id, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ap/following", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var collection map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&collection))
	require.Equal(t, testOrderedCollection, collection["type"])
	require.Equal(t, float64(0), collection["totalItems"])
	require.Equal(t, "https://test.example.com/ap/following", collection["id"])
}

func TestFollowingHandlerRejectsPost(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("test.example.com", "", "", discardLogger())
	handler := NewFollowingHandler(id, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ap/following", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestFollowersHandler(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("test.example.com", "", "", discardLogger())
	handler := NewFollowersHandler(id, db)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ap/followers", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var collection map[string]any
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&collection))
	require.Equal(t, testOrderedCollection, collection["type"])
	require.Equal(t, float64(0), collection["totalItems"])
}

// TestShouldAutoAcceptMutualPending verifies that mutual mode auto-accepts
// when our outgoing follow is still pending (simultaneous-follow scenario).
func TestShouldAutoAcceptMutualPending(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	peerActor := "https://peer.example.com/ap/actor"

	// Add outgoing follow in pending state (we sent Follow, they haven't accepted yet).
	require.NoError(t, db.AddOutgoingFollow(ctx, peerActor))

	id, _ := LoadOrCreateIdentity("bob.example.com", "", "", discardLogger())
	handler := NewInboxHandler(id, db, InboxConfig{
		MaxManifestSize: 1 << 20,
		MaxBlobSize:     1 << 20,
		AutoAccept:      "mutual",
	}, discardLogger())

	// Should auto-accept even though our outgoing follow is only pending.
	require.True(t, handler.shouldAutoAccept(ctx, peerActor),
		"mutual mode must auto-accept when outgoing follow is pending")
}

// TestShouldAutoAcceptMutualAccepted verifies that mutual mode still accepts
// when the outgoing follow has already been accepted.
func TestShouldAutoAcceptMutualAccepted(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	peerActor := "https://peer2.example.com/ap/actor"

	require.NoError(t, db.AddOutgoingFollow(ctx, peerActor))
	require.NoError(t, db.AcceptOutgoingFollow(ctx, peerActor))

	id, _ := LoadOrCreateIdentity("bob.example.com", "", "", discardLogger())
	handler := NewInboxHandler(id, db, InboxConfig{
		MaxManifestSize: 1 << 20,
		MaxBlobSize:     1 << 20,
		AutoAccept:      "mutual",
	}, discardLogger())

	require.True(t, handler.shouldAutoAccept(ctx, peerActor),
		"mutual mode must auto-accept when outgoing follow is accepted")
}

// TestShouldAutoAcceptMutualNone verifies that mutual mode does NOT auto-accept
// when we have no outgoing follow at all.
func TestShouldAutoAcceptMutualNone(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	ctx := context.Background()

	id, _ := LoadOrCreateIdentity("bob.example.com", "", "", discardLogger())
	handler := NewInboxHandler(id, db, InboxConfig{
		MaxManifestSize: 1 << 20,
		MaxBlobSize:     1 << 20,
		AutoAccept:      "mutual",
	}, discardLogger())

	require.False(t, handler.shouldAutoAccept(ctx, "https://stranger.example.com/ap/actor"),
		"mutual mode must not auto-accept with no outgoing follow")
}
