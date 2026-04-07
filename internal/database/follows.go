package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/uptrace/bun"
)

func (db *DB) AddFollow(ctx context.Context, actorURL, publicKeyPEM, endpoint string) error {
	return db.bun.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewRaw(
			`INSERT INTO follows (actor_url, public_key_pem, endpoint)
			 VALUES (?, ?, ?)
			 ON CONFLICT(actor_url) DO UPDATE SET
			   public_key_pem = excluded.public_key_pem,
			   endpoint = excluded.endpoint`,
			actorURL, publicKeyPEM, endpoint).Exec(ctx); err != nil {
			return fmt.Errorf("adding follow: %w", err)
		}
		if _, err := tx.NewRaw("DELETE FROM follow_requests WHERE actor_url = ?", actorURL).Exec(ctx); err != nil {
			return fmt.Errorf("cleaning up follow request: %w", err)
		}
		return nil
	})
}

func (db *DB) RemoveFollow(ctx context.Context, actorURL string) error {
	res, err := db.bun.NewRaw(
		"DELETE FROM follows WHERE actor_url = ?", actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("removing follow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no follow found for %q", actorURL)
	}
	return nil
}

func (db *DB) GetFollow(ctx context.Context, actorURL string) (*Follow, error) {
	f := &Follow{}
	err := db.bun.NewSelect().Model(f).Where("actor_url = ?", actorURL).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying follow: %w", err)
	}
	return f, nil
}

func (db *DB) ListFollows(ctx context.Context) ([]Follow, error) {
	var follows []Follow
	err := db.bun.NewSelect().Model(&follows).OrderExpr("approved_at DESC").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing follows: %w", err)
	}
	return follows, nil
}

func (db *DB) AddFollowRequest(ctx context.Context, actorURL, publicKeyPEM, endpoint string) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO follow_requests (actor_url, public_key_pem, endpoint)
		 VALUES (?, ?, ?)
		 ON CONFLICT(actor_url) DO UPDATE SET
		   public_key_pem = excluded.public_key_pem,
		   endpoint = excluded.endpoint`,
		actorURL, publicKeyPEM, endpoint).Exec(ctx)
	if err != nil {
		return fmt.Errorf("adding follow request: %w", err)
	}
	return nil
}

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

func (db *DB) ListFollowRequests(ctx context.Context) ([]FollowRequest, error) {
	var requests []FollowRequest
	err := db.bun.NewSelect().Model(&requests).OrderExpr("requested_at DESC").Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing follow requests: %w", err)
	}
	return requests, nil
}

func (db *DB) CountFollows(ctx context.Context) (int, error) {
	var count int
	if err := db.bun.NewRaw("SELECT COUNT(*) FROM follows").Scan(ctx, &count); err != nil {
		return 0, fmt.Errorf("counting follows: %w", err)
	}
	return count, nil
}

func (db *DB) ListFollowsPage(ctx context.Context, offset, limit int) ([]Follow, error) {
	var follows []Follow
	err := db.bun.NewSelect().Model(&follows).
		OrderExpr("approved_at DESC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing follows page: %w", err)
	}
	return follows, nil
}

// ListFollowsBatch returns followers in batches using cursor-based pagination.
func (db *DB) ListFollowsBatch(ctx context.Context, afterID int64, limit int) ([]Follow, error) {
	var follows []Follow
	err := db.bun.NewSelect().Model(&follows).
		Where("id > ?", afterID).
		OrderExpr("id ASC").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing follows batch: %w", err)
	}
	return follows, nil
}

func (db *DB) AcceptFollowRequest(ctx context.Context, actorURL string) error {
	fr, err := db.GetFollowRequest(ctx, actorURL)
	if err != nil {
		return err
	}
	if fr == nil {
		return fmt.Errorf("no pending follow request from %s", actorURL)
	}
	return db.AddFollow(ctx, fr.ActorURL, fr.PublicKeyPEM, fr.Endpoint)
}

func (db *DB) RejectFollowRequest(ctx context.Context, actorURL string) error {
	res, err := db.bun.NewRaw(
		"DELETE FROM follow_requests WHERE actor_url = ?", actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("rejecting follow request: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no pending follow request from %s", actorURL)
	}
	return nil
}
