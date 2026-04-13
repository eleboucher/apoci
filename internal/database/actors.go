package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/uptrace/bun"
)

// UpsertActor inserts or updates an actor.
func (db *DB) UpsertActor(ctx context.Context, a *Actor) error {
	_, err := db.bun.NewRaw(`
		INSERT INTO actors (actor_url, name, alias, endpoint, public_key_pem,
			they_follow_us, they_follow_us_at, we_follow_them, we_follow_status, we_follow_accept_at,
			is_healthy, replication_policy, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(actor_url) DO UPDATE SET
			name = COALESCE(excluded.name, actors.name),
			alias = COALESCE(excluded.alias, actors.alias),
			endpoint = excluded.endpoint,
			public_key_pem = COALESCE(excluded.public_key_pem, actors.public_key_pem),
			they_follow_us = CASE WHEN excluded.they_follow_us THEN TRUE ELSE actors.they_follow_us END,
			they_follow_us_at = COALESCE(excluded.they_follow_us_at, actors.they_follow_us_at),
			we_follow_them = CASE WHEN excluded.we_follow_them THEN TRUE ELSE actors.we_follow_them END,
			we_follow_status = COALESCE(excluded.we_follow_status, actors.we_follow_status),
			we_follow_accept_at = COALESCE(excluded.we_follow_accept_at, actors.we_follow_accept_at),
			is_healthy = excluded.is_healthy,
			replication_policy = excluded.replication_policy,
			last_seen_at = COALESCE(excluded.last_seen_at, actors.last_seen_at)`,
		a.ActorURL, a.Name, a.Alias, a.Endpoint, a.PublicKeyPEM,
		a.TheyFollowUs, a.TheyFollowUsAt, a.WeFollowThem, a.WeFollowStatus, a.WeFollowAcceptAt,
		a.IsHealthy, a.ReplicationPolicy, a.LastSeenAt).Exec(ctx)
	if err != nil {
		return fmt.Errorf("upserting actor: %w", err)
	}
	return nil
}

// GetActor retrieves an actor by URL.
func (db *DB) GetActor(ctx context.Context, actorURL string) (*Actor, error) {
	a := &Actor{}
	err := db.bun.NewSelect().Model(a).Where("actor_url = ?", actorURL).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying actor: %w", err)
	}
	return a, nil
}

// DeleteActor permanently removes the actor row for the given actorURL.
func (db *DB) DeleteActor(ctx context.Context, actorURL string) error {
	db.logger.Debug("DeleteActor", "actorURL", actorURL)
	res, err := db.bun.NewDelete().Model((*Actor)(nil)).
		Where("actor_url = ?", actorURL).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting actor: %w", err)
	}
	n, _ := res.RowsAffected()
	db.logger.Debug("DeleteActor done", "actorURL", actorURL, "rowsAffected", n)
	return nil
}

// endpointDomainConds returns WHERE args matching an actor endpoint by domain:
// https://domain, https://domain/*, and https://domain:* (with port).
func endpointDomainConds(domain string) (exact, withPath, withPort string) {
	base := "https://" + domain
	return base, base + "/%", base + ":%"
}

// FindActorByInput looks up an actor by: exact actor_url, alias, name, or endpoint domain.
func (db *DB) FindActorByInput(ctx context.Context, input string) (*Actor, error) {
	db.logger.Debug("FindActorByInput", "input", input)

	// Exact actor_url match
	a := &Actor{}
	if err := db.bun.NewSelect().Model(a).Where("actor_url = ?", input).Scan(ctx); err == nil {
		db.logger.Debug("FindActorByInput matched by actor_url", "actorURL", a.ActorURL)
		return a, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("querying actor by url: %w", err)
	}

	// Alias match
	a = &Actor{}
	if err := db.bun.NewSelect().Model(a).Where("alias = ?", input).Limit(1).Scan(ctx); err == nil {
		db.logger.Debug("FindActorByInput matched by alias", "actorURL", a.ActorURL, "alias", input)
		return a, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("querying actor by alias: %w", err)
	}

	// Name match
	a = &Actor{}
	if err := db.bun.NewSelect().Model(a).Where("name = ?", input).Limit(1).Scan(ctx); err == nil {
		db.logger.Debug("FindActorByInput matched by name", "actorURL", a.ActorURL, "name", input)
		return a, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("querying actor by name: %w", err)
	}

	// Endpoint domain match
	a = &Actor{}
	exact, withPath, withPort := endpointDomainConds(input)
	if err := db.bun.NewSelect().Model(a).
		Where("endpoint = ? OR endpoint LIKE ? OR endpoint LIKE ?", exact, withPath, withPort).
		Limit(1).Scan(ctx); err == nil {
		db.logger.Debug("FindActorByInput matched by endpoint", "actorURL", a.ActorURL, "endpoint", a.Endpoint)
		return a, nil
	} else if errors.Is(err, sql.ErrNoRows) {
		db.logger.Debug("FindActorByInput no match", "input", input)
		return nil, nil
	} else {
		return nil, fmt.Errorf("querying actor by endpoint: %w", err)
	}
}

// ListActors returns all actors.
func (db *DB) ListActors(ctx context.Context) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).OrderExpr("created_at DESC").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing actors: %w", err)
	}
	return actors, nil
}

// ListAllPeers returns all actors (alias for health checking compatibility).
func (db *DB) ListAllPeers(ctx context.Context) ([]Actor, error) {
	return db.ListActors(ctx)
}

// CountPeers returns the count of actors known to the system.
func (db *DB) CountPeers(ctx context.Context) (int, error) {
	count, err := db.bun.NewSelect().Model((*Actor)(nil)).Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting actors: %w", err)
	}
	return count, nil
}

// AddFollowRequest creates or updates a pending follow request.
func (db *DB) AddFollowRequest(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error {
	fr := &FollowRequest{
		ActorURL:     actorURL,
		PublicKeyPEM: publicKeyPEM,
		Endpoint:     endpoint,
		Alias:        alias,
	}
	_, err := db.bun.NewInsert().Model(fr).
		On("CONFLICT (actor_url) DO UPDATE").
		Set("public_key_pem = EXCLUDED.public_key_pem").
		Set("endpoint = EXCLUDED.endpoint").
		Set("alias = EXCLUDED.alias").
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("adding follow request: %w", err)
	}
	return nil
}

// GetFollowRequest retrieves a pending follow request.
func (db *DB) GetFollowRequest(ctx context.Context, actorURL string) (*FollowRequest, error) {
	fr := &FollowRequest{}
	err := db.bun.NewSelect().Model(fr).Where("actor_url = ?", actorURL).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying follow request: %w", err)
	}
	return fr, nil
}

// ListFollowRequests returns all pending follow requests.
func (db *DB) ListFollowRequests(ctx context.Context) ([]FollowRequest, error) {
	var requests []FollowRequest
	err := db.bun.NewSelect().Model(&requests).OrderExpr("requested_at DESC").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing follow requests: %w", err)
	}
	return requests, nil
}

// RejectFollowRequest deletes a pending follow request.
func (db *DB) RejectFollowRequest(ctx context.Context, actorURL string) error {
	res, err := db.bun.NewDelete().Model((*FollowRequest)(nil)).
		Where("actor_url = ?", actorURL).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("rejecting follow request: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no pending follow request from %s", actorURL)
	}
	return nil
}

// RefreshFollowRequest updates actor-derived fields for a pending follow request.
func (db *DB) RefreshFollowRequest(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error {
	res, err := db.bun.NewUpdate().Model((*FollowRequest)(nil)).
		Set("public_key_pem = ?", publicKeyPEM).
		Set("endpoint = ?", endpoint).
		Set("alias = ?", alias).
		Where("actor_url = ?", actorURL).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("refreshing follow request: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no follow request found for %q", actorURL)
	}
	return nil
}

// ListFollowers returns all actors that follow us.
func (db *DB) ListFollowers(ctx context.Context) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("they_follow_us = TRUE").
		OrderExpr("they_follow_us_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing followers: %w", err)
	}
	return actors, nil
}

// ListFollows returns followers (alias for compatibility).
func (db *DB) ListFollows(ctx context.Context) ([]Actor, error) {
	return db.ListFollowers(ctx)
}

// GetFollow returns an actor that follows us.
func (db *DB) GetFollow(ctx context.Context, actorURL string) (*Actor, error) {
	a := &Actor{}
	err := db.bun.NewSelect().Model(a).
		Where("actor_url = ? AND they_follow_us = TRUE", actorURL).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying follow: %w", err)
	}
	return a, nil
}

// AddFollow adds or updates a follower (promotes from follow request).
func (db *DB) AddFollow(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error {
	return db.bun.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		now := time.Now()
		if _, err := tx.NewRaw(`
			INSERT INTO actors (actor_url, public_key_pem, endpoint, alias, they_follow_us, they_follow_us_at, is_healthy, replication_policy)
			VALUES (?, ?, ?, ?, TRUE, ?, TRUE, 'lazy')
			ON CONFLICT(actor_url) DO UPDATE SET
				public_key_pem = excluded.public_key_pem,
				endpoint = excluded.endpoint,
				alias = COALESCE(excluded.alias, actors.alias),
				they_follow_us = TRUE,
				they_follow_us_at = excluded.they_follow_us_at`,
			actorURL, publicKeyPEM, endpoint, alias, now).Exec(ctx); err != nil {
			return fmt.Errorf("adding follow: %w", err)
		}
		if _, err := tx.NewRaw("DELETE FROM follow_requests WHERE actor_url = ?", actorURL).Exec(ctx); err != nil {
			return fmt.Errorf("cleaning up follow request: %w", err)
		}
		return nil
	})
}

// RemoveFollow removes an inbound follower.
func (db *DB) RemoveFollow(ctx context.Context, actorURL string) error {
	db.logger.Debug("RemoveFollow", "actorURL", actorURL)
	res, err := db.bun.NewUpdate().Model((*Actor)(nil)).
		Set("they_follow_us = FALSE").
		Set("they_follow_us_at = NULL").
		Where("actor_url = ? AND they_follow_us = TRUE", actorURL).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("removing follow: %w", err)
	}
	n, _ := res.RowsAffected()
	db.logger.Debug("RemoveFollow done", "actorURL", actorURL, "rowsAffected", n)
	if n == 0 {
		return fmt.Errorf("no follow found for %q", actorURL)
	}
	return nil
}

// CountFollows returns the count of actors that follow us.
func (db *DB) CountFollows(ctx context.Context) (int, error) {
	count, err := db.bun.NewSelect().Model((*Actor)(nil)).
		Where("they_follow_us = TRUE").
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting follows: %w", err)
	}
	return count, nil
}

// ListFollowsPage returns followers with pagination.
func (db *DB) ListFollowsPage(ctx context.Context, offset, limit int) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("they_follow_us = TRUE").
		OrderExpr("they_follow_us_at DESC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing follows page: %w", err)
	}
	return actors, nil
}

// ListFollowsBatch returns followers using cursor-based pagination.
func (db *DB) ListFollowsBatch(ctx context.Context, afterID int64, limit int) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("they_follow_us = TRUE AND id > ?", afterID).
		OrderExpr("id ASC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing follows batch: %w", err)
	}
	return actors, nil
}

// RefreshFollow updates actor-derived fields for an existing follower.
func (db *DB) RefreshFollow(ctx context.Context, actorURL, publicKeyPEM, endpoint string, alias *string) error {
	res, err := db.bun.NewUpdate().Model((*Actor)(nil)).
		Set("public_key_pem = ?", publicKeyPEM).
		Set("endpoint = ?", endpoint).
		Set("alias = ?", alias).
		Where("actor_url = ? AND they_follow_us = TRUE", actorURL).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("refreshing follow: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no follow found for %q", actorURL)
	}
	return nil
}

// AcceptFollowRequest promotes a follow request to a follower.
func (db *DB) AcceptFollowRequest(ctx context.Context, actorURL string) error {
	fr, err := db.GetFollowRequest(ctx, actorURL)
	if err != nil {
		return err
	}
	if fr == nil {
		return fmt.Errorf("no pending follow request from %s", actorURL)
	}
	return db.AddFollow(ctx, fr.ActorURL, fr.PublicKeyPEM, fr.Endpoint, fr.Alias)
}

// ListFollowing returns all actors we follow (accepted).
func (db *DB) ListFollowing(ctx context.Context) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("we_follow_them = TRUE AND we_follow_status = 'accepted'").
		OrderExpr("we_follow_accept_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing following: %w", err)
	}
	return actors, nil
}

// GetOutgoingFollow returns an actor we're following or have requested to follow.
func (db *DB) GetOutgoingFollow(ctx context.Context, actorURL string) (*Actor, error) {
	a := &Actor{}
	err := db.bun.NewSelect().Model(a).
		Where("actor_url = ? AND we_follow_them = TRUE", actorURL).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying outgoing follow: %w", err)
	}
	return a, nil
}

func (db *DB) AddOutgoingFollow(ctx context.Context, actorURL string) error {
	_, err := db.bun.NewRaw(`
		INSERT INTO actors (actor_url, endpoint, we_follow_them, we_follow_status, is_healthy, replication_policy)
		VALUES (?, '', TRUE, 'pending', TRUE, 'lazy')
		ON CONFLICT(actor_url) DO UPDATE SET
			we_follow_them = TRUE,
			we_follow_status = CASE
				WHEN actors.we_follow_status = 'accepted' THEN 'accepted'
				ELSE 'pending'
			END,
			we_follow_accept_at = CASE
				WHEN actors.we_follow_status = 'accepted' THEN actors.we_follow_accept_at
				ELSE NULL
			END`,
		actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("adding outgoing follow: %w", err)
	}
	return nil
}

// AcceptOutgoingFollow marks an outgoing follow as accepted.
func (db *DB) AcceptOutgoingFollow(ctx context.Context, actorURL string) error {
	res, err := db.bun.NewUpdate().Model((*Actor)(nil)).
		Set("we_follow_status = ?", FollowStatusAccepted).
		Set("we_follow_accept_at = CURRENT_TIMESTAMP").
		Where("actor_url = ? AND we_follow_them = TRUE AND we_follow_status = ?", actorURL, FollowStatusPending).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("accepting outgoing follow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no pending outgoing follow for %s", actorURL)
	}
	return nil
}

// RejectOutgoingFollow marks an outgoing follow as rejected.
func (db *DB) RejectOutgoingFollow(ctx context.Context, actorURL string) error {
	_, err := db.bun.NewUpdate().Model((*Actor)(nil)).
		Set("we_follow_status = ?", FollowStatusRejected).
		Where("actor_url = ? AND we_follow_them = TRUE AND we_follow_status = ?", actorURL, FollowStatusPending).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("rejecting outgoing follow: %w", err)
	}
	return nil
}

// RemoveOutgoingFollow removes an outgoing follow.
func (db *DB) RemoveOutgoingFollow(ctx context.Context, actorURL string) error {
	db.logger.Debug("RemoveOutgoingFollow", "actorURL", actorURL)
	res, err := db.bun.NewUpdate().Model((*Actor)(nil)).
		Set("we_follow_them = FALSE").
		Set("we_follow_status = NULL").
		Set("we_follow_accept_at = NULL").
		Where("actor_url = ? AND we_follow_them = TRUE", actorURL).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("removing outgoing follow: %w", err)
	}
	n, _ := res.RowsAffected()
	db.logger.Debug("RemoveOutgoingFollow done", "actorURL", actorURL, "rowsAffected", n)
	if n == 0 {
		return fmt.Errorf("no outgoing follow found for %q", actorURL)
	}
	return nil
}

// ListOutgoingFollows returns all actors we follow with the given status.
func (db *DB) ListOutgoingFollows(ctx context.Context, status string) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("we_follow_them = TRUE AND we_follow_status = ?", status).
		OrderExpr("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing outgoing follows: %w", err)
	}
	return actors, nil
}

// ListAllOutgoingFollows returns all actors we follow regardless of status.
func (db *DB) ListAllOutgoingFollows(ctx context.Context) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("we_follow_them = TRUE").
		OrderExpr("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing all outgoing follows: %w", err)
	}
	return actors, nil
}

// CountOutgoingFollows returns the count of actors we follow with the given status.
func (db *DB) CountOutgoingFollows(ctx context.Context, status string) (int, error) {
	count, err := db.bun.NewSelect().Model((*Actor)(nil)).
		Where("we_follow_them = TRUE AND we_follow_status = ?", status).
		Count(ctx)
	if err != nil {
		return 0, fmt.Errorf("counting outgoing follows: %w", err)
	}
	return count, nil
}

// ListOutgoingFollowsPage returns actors we follow with pagination.
func (db *DB) ListOutgoingFollowsPage(ctx context.Context, status string, limit, offset int) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("we_follow_them = TRUE AND we_follow_status = ?", status).
		OrderExpr("created_at DESC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing outgoing follows page: %w", err)
	}
	return actors, nil
}

// DeleteStaleOutgoingFollows removes stale pending and rejected outgoing follows.
func (db *DB) DeleteStaleOutgoingFollows(ctx context.Context, pendingTTL, rejectedTTL time.Duration) (int64, error) {
	now := time.Now()
	pendingCutoff := now.Add(-pendingTTL)
	rejectedCutoff := now.Add(-rejectedTTL)

	res, err := db.bun.NewRaw(`
		UPDATE actors SET we_follow_them = FALSE, we_follow_status = NULL, we_follow_accept_at = NULL
		WHERE we_follow_them = TRUE AND (
			(we_follow_status = 'pending' AND created_at < ?)
			OR (we_follow_status = 'rejected' AND created_at < ?)
		)`,
		pendingCutoff, rejectedCutoff).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("deleting stale outgoing follows: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UpsertPeer inserts or updates an actor as a peer (for blob replication).
func (db *DB) UpsertPeer(ctx context.Context, actorURL, endpoint string, name *string, replicationPolicy string, isHealthy bool) error {
	_, err := db.bun.NewRaw(`
		INSERT INTO actors (actor_url, name, endpoint, is_healthy, replication_policy, last_seen_at)
		VALUES (?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(actor_url) DO UPDATE SET
			name = COALESCE(excluded.name, actors.name),
			endpoint = excluded.endpoint,
			is_healthy = excluded.is_healthy,
			replication_policy = excluded.replication_policy,
			last_seen_at = excluded.last_seen_at`,
		actorURL, name, endpoint, isHealthy, replicationPolicy).Exec(ctx)
	if err != nil {
		return fmt.Errorf("upserting peer: %w", err)
	}
	return nil
}

// GetPeer retrieves an actor as a peer.
func (db *DB) GetPeer(ctx context.Context, actorURL string) (*Actor, error) {
	return db.GetActor(ctx, actorURL)
}

// SetActorHealth updates the health status of an actor.
func (db *DB) SetActorHealth(ctx context.Context, actorURL string, healthy bool) error {
	_, err := db.bun.NewUpdate().Model((*Actor)(nil)).
		Set("is_healthy = ?", healthy).
		Set("last_seen_at = CURRENT_TIMESTAMP").
		Where("actor_url = ?", actorURL).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting actor health: %w", err)
	}
	return nil
}

// SetPeerHealth updates the health status of an actor (alias for compatibility).
func (db *DB) SetPeerHealth(ctx context.Context, actorURL string, healthy bool) error {
	return db.SetActorHealth(ctx, actorURL, healthy)
}

// SetPeerHealthByDomain updates is_healthy for all actors whose endpoint hostname matches.
// Matches endpoints like https://domain/..., https://domain:port/..., and https://domain (no path).
func (db *DB) SetPeerHealthByDomain(ctx context.Context, domain string, healthy bool) error {
	exact, withPath, withPort := endpointDomainConds(domain)
	_, err := db.bun.NewRaw(
		`UPDATE actors SET is_healthy = ?
		 WHERE endpoint = ?
		    OR endpoint LIKE ?
		    OR endpoint LIKE ?`,
		healthy, exact, withPath, withPort).Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting actor health by domain: %w", err)
	}
	return nil
}

// UnhealthyPeerDomains returns the hostnames of all unhealthy actors.
func (db *DB) UnhealthyPeerDomains(ctx context.Context) ([]string, error) {
	var actors []Actor
	if err := db.bun.NewSelect().Model(&actors).
		Where("is_healthy = false").
		Column("endpoint").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("querying unhealthy actors: %w", err)
	}
	domains := make([]string, 0, len(actors))
	for _, a := range actors {
		if u, err := parseEndpointURL(a.Endpoint); err == nil && u != "" {
			domains = append(domains, u)
		}
	}
	return domains, nil
}

func parseEndpointURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("cannot extract hostname from %q", endpoint)
	}
	return u.Hostname(), nil
}

// ListHealthyActors returns all actors marked as healthy.
func (db *DB) ListHealthyActors(ctx context.Context) ([]Actor, error) {
	var actors []Actor
	err := db.bun.NewSelect().Model(&actors).
		Where("is_healthy = TRUE").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing healthy actors: %w", err)
	}
	return actors, nil
}

// =============================================================================
// PeerBlob methods (unchanged from peers.go)
// =============================================================================

// PutPeerBlob records that a peer has a blob.
func (db *DB) PutPeerBlob(ctx context.Context, peerActor, blobDigest, peerEndpoint string) error {
	_, err := db.bun.NewRaw(`
		INSERT INTO peer_blobs (peer_actor, blob_digest, peer_endpoint, last_verified_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(peer_actor, blob_digest) DO UPDATE SET
			peer_endpoint = excluded.peer_endpoint,
			last_verified_at = excluded.last_verified_at`,
		peerActor, blobDigest, peerEndpoint).Exec(ctx)
	if err != nil {
		return fmt.Errorf("putting peer blob: %w", err)
	}
	return nil
}

// CleanupStalePeerBlobs removes peer blob references not verified within the given duration.
func (db *DB) CleanupStalePeerBlobs(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := db.bun.NewDelete().Model((*PeerBlob)(nil)).
		Where("last_verified_at < ?", cutoff).
		Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("cleaning up stale peer blobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// FindPeersWithBlob returns all peer blobs for a given digest from healthy actors.
func (db *DB) FindPeersWithBlob(ctx context.Context, blobDigest string) ([]PeerBlob, error) {
	var blobs []PeerBlob
	err := db.bun.NewRaw(`
		SELECT pb.id, pb.peer_actor, pb.blob_digest, pb.peer_endpoint, pb.last_verified_at
		FROM peer_blobs pb
		JOIN actors a ON a.actor_url = pb.peer_actor
		WHERE pb.blob_digest = ? AND a.is_healthy = true
		ORDER BY pb.last_verified_at DESC`, blobDigest).Scan(ctx, &blobs)
	if err != nil {
		return nil, fmt.Errorf("finding peers with blob: %w", err)
	}
	return blobs, nil
}
