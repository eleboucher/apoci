package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"time"
)

func (db *DB) UpsertPeer(ctx context.Context, p *Peer) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO peers (actor_url, name, endpoint, replication_policy, last_seen_at, is_healthy)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(actor_url) DO UPDATE SET
		   name = COALESCE(excluded.name, peers.name),
		   endpoint = excluded.endpoint,
		   replication_policy = excluded.replication_policy,
		   last_seen_at = excluded.last_seen_at,
		   is_healthy = excluded.is_healthy`,
		p.ActorURL, p.Name, p.Endpoint, p.ReplicationPolicy, p.LastSeenAt, p.IsHealthy).Exec(ctx)
	if err != nil {
		return fmt.Errorf("upserting peer: %w", err)
	}
	return nil
}

func (db *DB) GetPeer(ctx context.Context, actorURL string) (*Peer, error) {
	p := &Peer{}
	err := db.bun.NewSelect().Model(p).Where("actor_url = ?", actorURL).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying peer: %w", err)
	}
	return p, nil
}

func (db *DB) ListAllPeers(ctx context.Context) ([]Peer, error) {
	var peers []Peer
	err := db.bun.NewSelect().Model(&peers).OrderExpr("last_seen_at DESC").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing all peers: %w", err)
	}
	return peers, nil
}

func (db *DB) SetPeerHealth(ctx context.Context, actorURL string, healthy bool) error {
	_, err := db.bun.NewRaw(
		"UPDATE peers SET is_healthy = ?, last_seen_at = CURRENT_TIMESTAMP WHERE actor_url = ?",
		healthy, actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting peer health: %w", err)
	}
	return nil
}

// SetPeerHealthByDomain updates is_healthy for all peers whose endpoint
// hostname is exactly domain. The LIKE patterns match:
//   - https://domain/...   (no explicit port)
//   - https://domain:port/...
//
// Used by the circuit breaker to persist health state.
func (db *DB) SetPeerHealthByDomain(ctx context.Context, domain string, healthy bool) error {
	_, err := db.bun.NewRaw(
		`UPDATE peers SET is_healthy = ?
		 WHERE endpoint LIKE ? OR endpoint LIKE ?`,
		healthy,
		"https://"+domain+"/%",
		"https://"+domain+":_%/%").Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting peer health by domain: %w", err)
	}
	return nil
}

// UnhealthyPeerDomains returns the hostnames (extracted from endpoint) of all
// peers currently marked is_healthy = false. Used to pre-warm the circuit breaker
// on startup so a restart during an outage doesn't immediately retry dead peers.
func (db *DB) UnhealthyPeerDomains(ctx context.Context) ([]string, error) {
	var peers []Peer
	if err := db.bun.NewSelect().Model(&peers).
		Where("is_healthy = false").
		Column("endpoint").
		Scan(ctx); err != nil {
		return nil, fmt.Errorf("querying unhealthy peers: %w", err)
	}
	domains := make([]string, 0, len(peers))
	for _, p := range peers {
		if u, err := parseEndpointURL(p.Endpoint); err == nil && u != "" {
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

func (db *DB) PutPeerBlob(ctx context.Context, peerActor, blobDigest, peerEndpoint string) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO peer_blobs (peer_actor, blob_digest, peer_endpoint, last_verified_at)
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
	res, err := db.bun.NewRaw(
		`DELETE FROM peer_blobs WHERE last_verified_at < ?`, cutoff).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("cleaning up stale peer blobs: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func (db *DB) CountPeers(ctx context.Context) (int, error) {
	var count int
	if err := db.bun.NewRaw("SELECT COUNT(*) FROM peers").Scan(ctx, &count); err != nil {
		return 0, fmt.Errorf("counting peers: %w", err)
	}
	return count, nil
}

func (db *DB) FindPeersWithBlob(ctx context.Context, blobDigest string) ([]PeerBlob, error) {
	var blobs []PeerBlob
	err := db.bun.NewRaw(
		`SELECT pb.id, pb.peer_actor, pb.blob_digest, pb.peer_endpoint, pb.last_verified_at
		 FROM peer_blobs pb
		 JOIN peers p ON p.actor_url = pb.peer_actor
		 WHERE pb.blob_digest = ? AND p.is_healthy = true
		 ORDER BY pb.last_verified_at DESC`, blobDigest).Scan(ctx, &blobs)
	if err != nil {
		return nil, fmt.Errorf("finding peers with blob: %w", err)
	}
	return blobs, nil
}
