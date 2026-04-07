package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
		"SELECT id, name, owner_id, created_at FROM repositories WHERE name = ?", name).
		Scan(ctx, &existing.ID, &existing.Name, &existing.OwnerID, &existing.CreatedAt)
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
		"SELECT id, name, owner_id, created_at FROM repositories WHERE name = ?", name).Scan(ctx, &repo.ID, &repo.Name, &repo.OwnerID, &repo.CreatedAt)
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
