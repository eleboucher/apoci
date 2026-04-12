package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
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

// TagWithDetails holds tag data with manifest info for UI display.
type TagWithDetails struct {
	Name            string
	Digest          string
	MediaType       string
	ArtifactType    *string
	SizeBytes       int64
	UpdatedAt       time.Time
	ManifestContent []byte // Raw manifest JSON for platform extraction
}

// TagsPage holds paginated tag results.
type TagsPage struct {
	Tags       []TagWithDetails
	TotalCount int
	Page       int
	PageSize   int
	TotalPages int
}

func (db *DB) ListTagsWithDetails(ctx context.Context, repoID int64, page, pageSize int) (*TagsPage, error) {
	if pageSize <= 0 {
		pageSize = 20
	}
	if page <= 0 {
		page = 1
	}

	// Get total count
	var totalCount int
	err := db.bun.NewRaw(`SELECT COUNT(*) FROM tags WHERE repository_id = ?`, repoID).Scan(ctx, &totalCount)
	if err != nil {
		return nil, fmt.Errorf("counting tags: %w", err)
	}

	totalPages := (totalCount + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}

	offset := (page - 1) * pageSize

	var rows []struct {
		Name            string    `bun:"name"`
		Digest          string    `bun:"digest"`
		MediaType       string    `bun:"media_type"`
		ArtifactType    *string   `bun:"artifact_type"`
		SizeBytes       int64     `bun:"size_bytes"`
		UpdatedAt       time.Time `bun:"updated_at"`
		ManifestContent []byte    `bun:"content"`
	}

	err = db.bun.NewRaw(`
		SELECT t.name, t.manifest_digest as digest, m.media_type, m.artifact_type, m.size_bytes, t.updated_at, m.content
		FROM tags t
		JOIN manifests m ON m.digest = t.manifest_digest AND m.repository_id = t.repository_id
		WHERE t.repository_id = ?
		ORDER BY t.updated_at DESC
		LIMIT ? OFFSET ?
	`, repoID, pageSize, offset).Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("listing tags with details: %w", err)
	}

	tags := make([]TagWithDetails, len(rows))
	for i, row := range rows {
		tags[i] = TagWithDetails{
			Name:            row.Name,
			Digest:          row.Digest,
			MediaType:       row.MediaType,
			ArtifactType:    row.ArtifactType,
			SizeBytes:       row.SizeBytes,
			UpdatedAt:       row.UpdatedAt,
			ManifestContent: row.ManifestContent,
		}
	}

	return &TagsPage{
		Tags:       tags,
		TotalCount: totalCount,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
	}, nil
}
