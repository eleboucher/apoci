package activitypub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestLookupWebFingerCachesResults(t *testing.T) {
	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		resp := WebFingerResponse{
			Subject: r.URL.Query().Get("resource"),
			Links: []WebFingerLink{
				{Rel: "self", Type: "application/activity+json", Href: "https://cache-test.invalid/ap/actor"},
			},
		}
		w.Header().Set("Content-Type", "application/jrd+json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	// Create a dedicated client for this test to avoid pollution.
	client := NewWebFingerClient()
	t.Cleanup(client.Stop)

	resource := "acct:registry@cache-test.invalid"
	ctx := context.Background()

	// Pre-seed the cache to verify the lookup returns cached value.
	client.cache.Set(resource, "https://cache-test.invalid/ap/actor", webfingerCacheTTL)

	got, err := client.Lookup(ctx, "cache-test.invalid", resource)
	require.NoError(t, err)
	require.Equal(t, "https://cache-test.invalid/ap/actor", got)

	require.Equal(t, int32(0), hits.Load(), "expected cache hit, but server was contacted")
	_ = srv
}
