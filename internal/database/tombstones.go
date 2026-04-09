package database

import (
	"context"
	"fmt"
	"time"
)

func (db *DB) RecordDeletedManifest(ctx context.Context, digest, repoName, sourceActor string) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO deleted_manifests (digest, repo_name, source_actor)
		 VALUES (?, ?, ?)
		 ON CONFLICT(digest) DO NOTHING`,
		digest, repoName, sourceActor).Exec(ctx)
	if err != nil {
		return fmt.Errorf("recording deleted manifest: %w", err)
	}
	return nil
}

func (db *DB) IsManifestDeleted(ctx context.Context, digest string) (bool, error) {
	var exists bool
	err := db.bun.NewRaw(
		"SELECT EXISTS(SELECT 1 FROM deleted_manifests WHERE digest = ?)", digest).Scan(ctx, &exists)
	if err != nil {
		return false, fmt.Errorf("checking deleted manifest: %w", err)
	}
	return exists, nil
}

func (db *DB) PruneDeletedManifests(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := db.bun.NewRaw(
		"DELETE FROM deleted_manifests WHERE deleted_at < ?", cutoff).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("pruning deleted manifests: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
