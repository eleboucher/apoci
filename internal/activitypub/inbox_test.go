package activitypub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/apoci/apoci/internal/config"
	"github.com/apoci/apoci/internal/database"
)

func testInboxSetup(t *testing.T) *InboxHandler {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, discardLogger())
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
	db, err := database.OpenSQLite(dir, discardLogger())
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
	db, err := database.OpenSQLite(dir, discardLogger())
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
	db, err := database.OpenSQLite(dir, discardLogger())
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
	db, err := database.OpenSQLite(dir, discardLogger())
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
