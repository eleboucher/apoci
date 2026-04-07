package activitypub

import (
	"context"
	"time"

	"github.com/jellydator/ttlcache/v3"
	"golang.org/x/sync/singleflight"
)

const actorCacheTTL = 1 * time.Hour

// ActorCache provides a TTL-based cache for remote actor profiles.
type ActorCache struct {
	identity *Identity
	cache    *ttlcache.Cache[string, *Actor]
	group    singleflight.Group
}

func NewActorCache(ctx context.Context, identity *Identity) *ActorCache {
	cache := ttlcache.New[string, *Actor](
		ttlcache.WithTTL[string, *Actor](actorCacheTTL),
	)
	go cache.Start() // starts automatic expired-item eviction
	context.AfterFunc(ctx, func() {
		cache.Stop()
	})

	return &ActorCache{
		identity: identity,
		cache:    cache,
	}
}

// Get returns a cached actor or fetches it (with a signed request) if missing/expired.
func (c *ActorCache) Get(ctx context.Context, actorURL string) (*Actor, error) {
	item := c.cache.Get(actorURL)
	if item != nil {
		return item.Value(), nil
	}

	// Use singleflight to deduplicate concurrent fetches for the same URL.
	v, err, _ := c.group.Do(actorURL, func() (any, error) {
		// Double-check: another goroutine in this flight may have populated the cache.
		if item := c.cache.Get(actorURL); item != nil {
			return item.Value(), nil
		}
		actor, err := FetchActorSigned(ctx, actorURL, c.identity)
		if err != nil {
			return nil, err
		}
		c.cache.Set(actorURL, actor, ttlcache.DefaultTTL)
		return actor, nil
	})
	if err != nil {
		return nil, err
	}

	return v.(*Actor), nil
}

func (c *ActorCache) Invalidate(actorURL string) {
	c.cache.Delete(actorURL)
}

func (c *ActorCache) Stop() {
	c.cache.Stop()
}
