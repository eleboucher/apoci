package activitypub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestActorHandler(t *testing.T) {
	id, _ := LoadOrCreateIdentity("test.example.com", "", "", discardLogger())
	handler := NewActorHandler(id, "Test Registry", "https://test.example.com")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/ap/actor", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	ct := rec.Header().Get("Content-Type")
	require.Equal(t, "application/activity+json", ct)

	var actor Actor
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&actor))

	require.Equal(t, "Application", actor.Type)
	require.Equal(t, testActorURL, actor.ID)
	require.Equal(t, "https://test.example.com/ap/inbox", actor.Inbox)
	require.NotEmpty(t, actor.PublicKey.PublicKeyPEM, "expected public key PEM to be set")
	require.Equal(t, "https://test.example.com/ap/actor#main-key", actor.PublicKey.ID)
}

func TestActorHandlerRejectsPost(t *testing.T) {
	id, _ := LoadOrCreateIdentity("test.example.com", "", "", discardLogger())
	handler := NewActorHandler(id, "Test", "https://test.example.com")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ap/actor", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
