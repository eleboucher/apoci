package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (db *DB) PutActivity(ctx context.Context, activityID, activityType, actorURL string, objectJSON []byte) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO activities (activity_id, type, actor_url, object_json)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(activity_id) DO NOTHING`,
		activityID, activityType, actorURL, objectJSON).Exec(ctx)
	if err != nil {
		return fmt.Errorf("storing activity: %w", err)
	}
	return nil
}

func (db *DB) GetActivity(ctx context.Context, activityID string) (*Activity, error) {
	a := &Activity{}
	err := db.bun.NewSelect().Model(a).Where("activity_id = ?", activityID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying activity: %w", err)
	}
	return a, nil
}

// ListActivitiesPage returns a page of activities with cursor-based pagination.
func (db *DB) ListActivitiesPage(ctx context.Context, actorURL string, beforeID int64, limit int) ([]Activity, error) {
	var activities []Activity
	q := db.bun.NewSelect().Model(&activities).Where("1=1")

	if actorURL != "" {
		q = q.Where("actor_url = ?", actorURL)
	}
	if beforeID > 0 {
		q = q.Where("id < ?", beforeID)
	}

	q = q.OrderExpr("id DESC").Limit(limit)

	err := q.Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing activities page: %w", err)
	}
	return activities, nil
}

// CountActivities returns the total number of activities for the given actor.
func (db *DB) CountActivities(ctx context.Context, actorURL string) (int, error) {
	q := db.bun.NewSelect().TableExpr("activities").ColumnExpr("COUNT(*)")
	if actorURL != "" {
		q = q.Where("actor_url = ?", actorURL)
	}
	var count int
	if err := q.Scan(ctx, &count); err != nil {
		return 0, fmt.Errorf("counting activities: %w", err)
	}
	return count, nil
}
