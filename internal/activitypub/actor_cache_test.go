package activitypub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestActorServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var fetchCount atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetchCount.Add(1)

		actor := Actor{
			Context:           []any{ContextActivityStreams},
			Type:              "Person",
			ID:                "http://" + r.Host + "/ap/actor",
			PreferredUsername: "testnode",
			Inbox:             "http://" + r.Host + "/ap/inbox",
			Outbox:            "http://" + r.Host + "/ap/outbox",
			PublicKey: ActorPublicKey{
				ID:           "http://" + r.Host + "/ap/actor#main-key",
				Owner:        "http://" + r.Host + "/ap/actor",
				PublicKeyPEM: "-----BEGIN PUBLIC KEY-----\nMIIBIjANBg==\n-----END PUBLIC KEY-----",
			},
		}

		w.Header().Set("Content-Type", "application/activity+json")
		if err := json.NewEncoder(w).Encode(actor); err != nil {
			t.Errorf("encoding actor: %v", err)
		}
	}))

	t.Cleanup(srv.Close)
	return srv, &fetchCount
}

func newTestIdentity(t *testing.T) *Identity {
	t.Helper()

	identity, err := LoadOrCreateIdentity("test.local", "", "", discardLogger())
	require.NoError(t, err, "creating identity")
	return identity
}

func TestActorCacheHit(t *testing.T) {
	srv, fetchCount := newTestActorServer(t)
	identity := newTestIdentity(t)
	cache := NewActorCache(context.Background(), identity)

	actorURL := srv.URL + "/ap/actor"
	ctx := context.Background()

	actor1, err := cache.Get(ctx, actorURL)
	require.NoError(t, err, "first Get")
	assert.Equal(t, actorURL, actor1.ID)

	actor2, err := cache.Get(ctx, actorURL)
	require.NoError(t, err, "second Get")
	assert.Equal(t, actorURL, actor2.ID)

	assert.Equal(t, int64(1), fetchCount.Load(), "server fetched times, want 1 (cache miss)")
}

func TestActorCacheInvalidate(t *testing.T) {
	srv, fetchCount := newTestActorServer(t)
	identity := newTestIdentity(t)
	cache := NewActorCache(context.Background(), identity)

	actorURL := srv.URL + "/ap/actor"
	ctx := context.Background()

	_, err := cache.Get(ctx, actorURL)
	require.NoError(t, err, "first Get")
	require.Equal(t, int64(1), fetchCount.Load(), "after first Get")

	cache.Invalidate(actorURL)

	_, err = cache.Get(ctx, actorURL)
	require.NoError(t, err, "Get after Invalidate")
	assert.Equal(t, int64(2), fetchCount.Load(), "after Invalidate+Get")
}

func TestActorCacheConcurrentAccess(t *testing.T) {
	srv, _ := newTestActorServer(t)
	identity := newTestIdentity(t)
	cache := NewActorCache(context.Background(), identity)

	actorURL := srv.URL + "/ap/actor"
	ctx := context.Background()

	const goroutines = 10
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			actor, err := cache.Get(ctx, actorURL)
			if err != nil {
				errs <- err
				return
			}
			if actor.ID != actorURL {
				errs <- fmt.Errorf("actor.ID mismatch: got %q, want %q", actor.ID, actorURL)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent Get failed: %v", err)
	}
}

func TestActorCacheEvictsExpiredEntries(t *testing.T) {
	srv, fetchCount := newTestActorServer(t)
	identity := newTestIdentity(t)

	// Build cache with a very short TTL so entries expire quickly in the test.
	shortTTL := 50 * time.Millisecond
	c := ttlcache.New[string, *Actor](
		ttlcache.WithTTL[string, *Actor](shortTTL),
	)
	go c.Start()

	cache := &ActorCache{
		identity: identity,
		cache:    c,
	}

	actorURL := srv.URL + "/ap/actor"
	ctx := context.Background()

	_, err := cache.Get(ctx, actorURL)
	require.NoError(t, err, "first Get")
	require.Equal(t, int64(1), fetchCount.Load())

	// Wait for the short TTL to expire the entry.
	time.Sleep(100 * time.Millisecond)

	// Confirm the entry is gone (Get on the underlying cache returns nil for expired items).
	require.Nil(t, cache.cache.Get(actorURL), "expired entry should have been evicted")

	// A new Get should trigger a second fetch.
	_, err = cache.Get(ctx, actorURL)
	require.NoError(t, err, "Get after eviction")
	require.Equal(t, int64(2), fetchCount.Load(), "expected 2 fetches after eviction")
}
