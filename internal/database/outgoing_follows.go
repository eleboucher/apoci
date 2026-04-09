package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (db *DB) AddOutgoingFollow(ctx context.Context, actorURL string) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO outgoing_follows (actor_url, status)
		 VALUES (?, 'pending')
		 ON CONFLICT(actor_url) DO NOTHING`,
		actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("adding outgoing follow: %w", err)
	}
	return nil
}

func (db *DB) AcceptOutgoingFollow(ctx context.Context, actorURL string) error {
	res, err := db.bun.NewRaw(
		`UPDATE outgoing_follows
		 SET status = 'accepted', accepted_at = CURRENT_TIMESTAMP
		 WHERE actor_url = ? AND status = 'pending'`,
		actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("accepting outgoing follow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no pending outgoing follow for %s", actorURL)
	}
	return nil
}

func (db *DB) RejectOutgoingFollow(ctx context.Context, actorURL string) error {
	_, err := db.bun.NewRaw(
		`UPDATE outgoing_follows SET status = 'rejected'
		 WHERE actor_url = ? AND status = 'pending'`,
		actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("rejecting outgoing follow: %w", err)
	}
	return nil
}

func (db *DB) RemoveOutgoingFollow(ctx context.Context, actorURL string) error {
	res, err := db.bun.NewRaw(
		"DELETE FROM outgoing_follows WHERE actor_url = ?", actorURL).Exec(ctx)
	if err != nil {
		return fmt.Errorf("removing outgoing follow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("no outgoing follow found for %q", actorURL)
	}
	return nil
}

func (db *DB) GetOutgoingFollow(ctx context.Context, actorURL string) (*OutgoingFollow, error) {
	f := &OutgoingFollow{}
	err := db.bun.NewSelect().Model(f).Where("actor_url = ?", actorURL).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying outgoing follow: %w", err)
	}
	return f, nil
}

func (db *DB) ListOutgoingFollows(ctx context.Context, status string) ([]OutgoingFollow, error) {
	var follows []OutgoingFollow
	err := db.bun.NewSelect().Model(&follows).
		Where("status = ?", status).
		OrderExpr("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing outgoing follows: %w", err)
	}
	return follows, nil
}

func (db *DB) CountOutgoingFollows(ctx context.Context, status string) (int, error) {
	var n int
	err := db.bun.NewRaw(
		"SELECT COUNT(*) FROM outgoing_follows WHERE status = ?", status).Scan(ctx, &n)
	if err != nil {
		return 0, fmt.Errorf("counting outgoing follows: %w", err)
	}
	return n, nil
}

func (db *DB) ListOutgoingFollowsPage(ctx context.Context, status string, limit, offset int) ([]OutgoingFollow, error) {
	var follows []OutgoingFollow
	err := db.bun.NewSelect().Model(&follows).
		Where("status = ?", status).
		OrderExpr("created_at DESC").
		Limit(limit).
		Offset(offset).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing outgoing follows page: %w", err)
	}
	return follows, nil
}
