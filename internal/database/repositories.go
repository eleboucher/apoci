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
	r, err := db.GetRepository(ctx, name)
	if err != nil {
		return nil, err
	}
	if r != nil {
		if r.OwnerID != ownerDID {
			return nil, fmt.Errorf("repository %q owned by %s, not %s", name, r.OwnerID, ownerDID)
		}
		return r, nil
	}

	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning repository transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// ON CONFLICT DO NOTHING avoids a constraint error when two concurrent requests race.
	_, err = tx.NewRaw(
		"INSERT INTO repositories (name, owner_id) VALUES (?, ?) ON CONFLICT (name) DO NOTHING", name, ownerDID).Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating repository: %w", err)
	}

	// Re-read to get the ID (whether we just inserted or a concurrent writer did).
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
