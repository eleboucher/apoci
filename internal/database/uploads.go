package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (db *DB) CreateUploadSession(ctx context.Context, uuid string, repoID int64, ttl time.Duration) (*UploadSession, error) {
	s := &UploadSession{
		UUID:         uuid,
		RepositoryID: repoID,
		ExpiresAt:    time.Now().Add(ttl),
		CreatedAt:    time.Now(),
	}
	_, err := db.bun.NewInsert().Model(s).Returning("id").Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating upload session: %w", err)
	}
	return s, nil
}

func (db *DB) GetUploadSession(ctx context.Context, uuid string) (*UploadSession, error) {
	s := &UploadSession{}
	err := db.bun.NewSelect().Model(s).
		Where("uuid = ?", uuid).
		Where("expires_at > CURRENT_TIMESTAMP").
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying upload session: %w", err)
	}
	return s, nil
}

func (db *DB) UpdateUploadProgress(ctx context.Context, uuid string, bytesReceived int64) error {
	_, err := db.bun.NewRaw(
		"UPDATE upload_sessions SET bytes_received = ? WHERE uuid = ?",
		bytesReceived, uuid).Exec(ctx)
	if err != nil {
		return fmt.Errorf("updating upload progress: %w", err)
	}
	return nil
}

func (db *DB) DeleteUploadSession(ctx context.Context, uuid string) error {
	_, err := db.bun.NewRaw(
		"DELETE FROM upload_sessions WHERE uuid = ?", uuid).Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting upload session: %w", err)
	}
	return nil
}
