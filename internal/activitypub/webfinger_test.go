package activitypub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWebFingerWithAcctResource(t *testing.T) {
	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	handler := NewWebFingerHandler(id)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:registry@test.example.com", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	ct := rec.Header().Get("Content-Type")
	require.Equal(t, "application/jrd+json", ct)

	var resp WebFingerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Len(t, resp.Links, 1)
	require.Equal(t, testActorURL, resp.Links[0].Href)
}

func TestWebFingerWithActorURL(t *testing.T) {
	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	handler := NewWebFingerHandler(id)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=https://test.example.com/ap/actor", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
}

func TestWebFingerMissingResource(t *testing.T) {
	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	handler := NewWebFingerHandler(id)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/.well-known/webfinger", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestWebFingerWrongDomain(t *testing.T) {
	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	handler := NewWebFingerHandler(id)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:user@other.example.com", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestWebFingerWithAccountDomain(t *testing.T) {
	id, _ := LoadOrCreateIdentity("https://registry.example.com", "registry.example.com", "example.com", "", discardLogger())
	handler := NewWebFingerHandler(id)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:registry@example.com", nil)
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)

	var resp WebFingerResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))

	require.Equal(t, "acct:registry@example.com", resp.Subject)
	require.Contains(t, resp.Aliases, "https://registry.example.com/ap/actor")
	require.Len(t, resp.Links, 1)
	require.Equal(t, "https://registry.example.com/ap/actor", resp.Links[0].Href)
}

func TestWebFingerSplitDomainBothWork(t *testing.T) {
	id, _ := LoadOrCreateIdentity("https://registry.example.com", "registry.example.com", "example.com", "", discardLogger())
	handler := NewWebFingerHandler(id)

	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:registry@example.com", nil)
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)

	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:registry@registry.example.com", nil)
	handler.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code)

	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest("GET", "/.well-known/webfinger?resource=acct:registry@other.com", nil)
	handler.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusNotFound, rec3.Code)
}
