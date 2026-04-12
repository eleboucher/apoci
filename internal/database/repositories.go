package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun/dialect/pgdialect"
)

func (db *DB) GetRepository(ctx context.Context, name string) (*Repository, error) {
	r := &Repository{}
	err := db.bun.NewSelect().Model(r).Where("name = ?", name).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying repository: %w", err)
	}
	return r, nil
}

func (db *DB) GetOrCreateRepository(ctx context.Context, name, ownerDID string) (*Repository, error) {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning repository transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing Repository
	err = tx.NewRaw(
		"SELECT id, name, owner_id, private, created_at FROM repositories WHERE name = ?", name).
		Scan(ctx, &existing.ID, &existing.Name, &existing.OwnerID, &existing.Private, &existing.CreatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("querying repository in transaction: %w", err)
	}
	if err == nil {
		if existing.OwnerID != ownerDID {
			return nil, fmt.Errorf("repository %q owned by %s, not %s", name, existing.OwnerID, ownerDID)
		}
		return &existing, nil
	}

	_, err = tx.NewRaw(
		"INSERT INTO repositories (name, owner_id) VALUES (?, ?) ON CONFLICT (name) DO NOTHING", name, ownerDID).Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating repository: %w", err)
	}

	var repo Repository
	err = tx.NewRaw(
		"SELECT id, name, owner_id, private, created_at FROM repositories WHERE name = ?", name).Scan(ctx, &repo.ID, &repo.Name, &repo.OwnerID, &repo.Private, &repo.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("reading repository after create: %w", err)
	}

	if repo.OwnerID != ownerDID {
		return nil, fmt.Errorf("repository %q owned by %s, not %s", name, repo.OwnerID, ownerDID)
	}

	_, err = tx.NewRaw(
		"INSERT INTO repository_owners (repository_id, owner_id) VALUES (?, ?) ON CONFLICT (repository_id, owner_id) DO NOTHING", repo.ID, ownerDID).Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("recording repository owner: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing repository: %w", err)
	}

	return &repo, nil
}

func (db *DB) IsRepositoryOwner(ctx context.Context, repoID int64, did string) (bool, error) {
	var count int
	err := db.bun.NewRaw(
		"SELECT COUNT(*) FROM repository_owners WHERE repository_id = ? AND owner_id = ?",
		repoID, did).Scan(ctx, &count)
	if err != nil {
		return false, fmt.Errorf("checking repository ownership: %w", err)
	}
	return count > 0, nil
}

func (db *DB) ListRepositoriesAfter(ctx context.Context, startAfter string, limit int) ([]Repository, error) {
	var repos []Repository
	err := db.bun.NewSelect().Model(&repos).
		Where("name > ?", startAfter).
		OrderExpr("name").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	return repos, nil
}

// SetRepositoryPrivate marks a repository as private or public.
func (db *DB) SetRepositoryPrivate(ctx context.Context, id int64, private bool) error {
	_, err := db.bun.NewRaw(
		"UPDATE repositories SET private = ? WHERE id = ?", private, id).Exec(ctx)
	if err != nil {
		return fmt.Errorf("setting repository private: %w", err)
	}
	return nil
}

// RepoWithStats holds repository data with computed stats for UI display.
type RepoWithStats struct {
	ID        int64
	Name      string
	OwnerID   string
	Tags      []string
	SizeBytes int64
	UpdatedAt time.Time
}

// ListReposWithStats returns all public repositories with their tags, total size, and last update time.
// If query is non-empty, filters by name (case-insensitive substring match).
// Private repositories are excluded from the results.
func (db *DB) ListReposWithStats(ctx context.Context, query string) ([]RepoWithStats, error) {
	_, isPostgres := db.bun.Dialect().(*pgdialect.Dialect)

	// Use dialect-specific tag aggregation
	var tagAgg string
	if isPostgres {
		tagAgg = `COALESCE(
			(SELECT STRING_AGG(t.name, ',') FROM (
				SELECT name FROM tags WHERE repository_id = r.id ORDER BY updated_at DESC LIMIT 10
			) t),
			''
		)`
	} else {
		tagAgg = `COALESCE(
			(SELECT GROUP_CONCAT(t.name, ',') FROM (
				SELECT name FROM tags WHERE repository_id = r.id ORDER BY updated_at DESC LIMIT 10
			) t),
			''
		)`
	}

	baseQuery := `
		SELECT
			r.id,
			r.name,
			r.owner_id,
			` + tagAgg + ` as tags,
			COALESCE(
				(SELECT SUM(b.size_bytes) FROM blobs b
				 JOIN manifest_layers ml ON ml.blob_digest = b.digest
				 JOIN manifests m ON m.id = ml.manifest_id
				 WHERE m.repository_id = r.id),
				0
			) as size_bytes,
			COALESCE(
				(SELECT MAX(updated_at) FROM tags WHERE repository_id = r.id),
				r.created_at
			) as updated_at
		FROM repositories r
		WHERE r.private = false
	`

	var rows []struct {
		ID        int64     `bun:"id"`
		Name      string    `bun:"name"`
		OwnerID   string    `bun:"owner_id"`
		Tags      string    `bun:"tags"`
		SizeBytes int64     `bun:"size_bytes"`
		UpdatedAt time.Time `bun:"updated_at"`
	}

	var err error
	if query != "" {
		likePattern := "%" + query + "%"
		if isPostgres {
			baseQuery += " AND r.name ILIKE ? ORDER BY r.name"
		} else {
			baseQuery += " AND r.name LIKE ? COLLATE NOCASE ORDER BY r.name"
		}
		err = db.bun.NewRaw(baseQuery, likePattern).Scan(ctx, &rows)
	} else {
		baseQuery += " ORDER BY r.name"
		err = db.bun.NewRaw(baseQuery).Scan(ctx, &rows)
	}

	if err != nil {
		return nil, fmt.Errorf("listing repos with stats: %w", err)
	}

	result := make([]RepoWithStats, len(rows))
	for i, row := range rows {
		var tags []string
		if row.Tags != "" {
			tags = strings.Split(row.Tags, ",")
		}
		result[i] = RepoWithStats{
			ID:        row.ID,
			Name:      row.Name,
			OwnerID:   row.OwnerID,
			Tags:      tags,
			SizeBytes: row.SizeBytes,
			UpdatedAt: row.UpdatedAt,
		}
	}

	return result, nil
}
