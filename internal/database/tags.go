package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrTagImmutable = errors.New("tag is immutable and cannot be overwritten")

func (db *DB) GetTag(ctx context.Context, repoID int64, name string) (*Tag, error) {
	t := &Tag{}
	err := db.bun.NewSelect().Model(t).
		Where("repository_id = ?", repoID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying tag: %w", err)
	}
	return t, nil
}

func (db *DB) PutTag(ctx context.Context, repoID int64, name, manifestDigest string) error {
	return db.PutTagWithImmutable(ctx, repoID, name, manifestDigest, false)
}

// PutTagWithImmutable atomically creates/updates a tag and sets its immutable flag.
// Uses a transaction to prevent a race between the immutable check and the upsert.
func (db *DB) PutTagWithImmutable(ctx context.Context, repoID int64, name, manifestDigest string, immutable bool) error {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning tag transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var isImmutable bool
	err = tx.NewRaw(
		"SELECT immutable FROM tags WHERE repository_id = ? AND name = ?",
		repoID, name).Scan(ctx, &isImmutable)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("checking tag immutability: %w", err)
	}
	if err == nil && isImmutable {
		return ErrTagImmutable
	}

	_, err = tx.NewRaw(
		`INSERT INTO tags (repository_id, name, manifest_digest, updated_at, immutable)
		 VALUES (?, ?, ?, CURRENT_TIMESTAMP, ?)
		 ON CONFLICT(repository_id, name) DO UPDATE SET
		   manifest_digest = excluded.manifest_digest,
		   updated_at = excluded.updated_at,
		   immutable = CASE WHEN tags.immutable THEN true ELSE excluded.immutable END`,
		repoID, name, manifestDigest, immutable).Exec(ctx)
	if err != nil {
		return fmt.Errorf("putting tag: %w", err)
	}
	return tx.Commit()
}

func (db *DB) DeleteTag(ctx context.Context, repoID int64, name string) error {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning delete tag transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var immutable bool
	err = tx.NewRaw(
		"SELECT immutable FROM tags WHERE repository_id = ? AND name = ?",
		repoID, name).Scan(ctx, &immutable)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // tag doesn't exist, nothing to delete
	}
	if err != nil {
		return fmt.Errorf("checking tag before delete: %w", err)
	}
	if immutable {
		return ErrTagImmutable
	}

	_, err = tx.NewRaw(
		"DELETE FROM tags WHERE repository_id = ? AND name = ?", repoID, name).Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting tag: %w", err)
	}
	return tx.Commit()
}

func (db *DB) ListTagsAfter(ctx context.Context, repoID int64, startAfter string, limit int) ([]string, error) {
	var tags []string
	err := db.bun.NewRaw(
		"SELECT name FROM tags WHERE repository_id = ? AND name > ? ORDER BY name LIMIT ?",
		repoID, startAfter, limit).Scan(ctx, &tags)
	if err != nil {
		return nil, fmt.Errorf("listing tags: %w", err)
	}
	return tags, nil
}
