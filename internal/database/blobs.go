package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (db *DB) GetBlob(ctx context.Context, digest string) (*Blob, error) {
	b := &Blob{}
	err := db.bun.NewSelect().Model(b).Where("digest = ?", digest).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying blob: %w", err)
	}
	return b, nil
}

func (db *DB) PutBlob(ctx context.Context, digest string, sizeBytes int64, mediaType *string, storedLocally bool) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO blobs (digest, size_bytes, media_type, stored_locally)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(digest) DO UPDATE SET
		   size_bytes = CASE WHEN excluded.stored_locally THEN excluded.size_bytes ELSE blobs.size_bytes END,
		   media_type = COALESCE(excluded.media_type, blobs.media_type),
		   stored_locally = blobs.stored_locally OR excluded.stored_locally`,
		digest, sizeBytes, mediaType, storedLocally).Exec(ctx)
	if err != nil {
		return fmt.Errorf("putting blob: %w", err)
	}
	return nil
}

// FindRepoForBlob returns the name of a repository that references this blob via manifest layers.
func (db *DB) FindRepoForBlob(ctx context.Context, digest string) (string, error) {
	var name string
	err := db.bun.NewRaw(
		`SELECT r.name FROM repositories r
		 JOIN manifests m ON m.repository_id = r.id
		 JOIN manifest_layers ml ON ml.manifest_id = m.id
		 WHERE ml.blob_digest = ?
		 LIMIT 1`, digest).Scan(ctx, &name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("finding repo for blob: %w", err)
	}
	return name, nil
}

func (db *DB) DeleteBlob(ctx context.Context, digest string) error {
	_, err := db.bun.NewRaw("DELETE FROM blobs WHERE digest = ?", digest).Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting blob: %w", err)
	}
	return nil
}

// OrphanedBlobs returns blob digests that are not stored locally, not referenced by
// any manifest layer, and have no peer blob references.
func (db *DB) OrphanedBlobs(ctx context.Context, limit int) ([]string, error) {
	var digests []string
	err := db.bun.NewRaw(
		`SELECT b.digest FROM blobs b
		 WHERE b.stored_locally = false
		   AND NOT EXISTS (SELECT 1 FROM manifest_layers ml WHERE ml.blob_digest = b.digest)
		   AND NOT EXISTS (SELECT 1 FROM peer_blobs pb WHERE pb.blob_digest = b.digest)
		 LIMIT ?`, limit).Scan(ctx, &digests)
	if err != nil {
		return nil, fmt.Errorf("finding orphaned blobs: %w", err)
	}
	return digests, nil
}

// AllBlobDigests returns all blob digests known in the database, paging in batches of pageSize.
func (db *DB) AllBlobDigests(ctx context.Context, pageSize int) (map[string]bool, error) {
	digests := make(map[string]bool)
	var afterDigest string
	for {
		var batch []string
		err := db.bun.NewRaw(
			"SELECT digest FROM blobs WHERE digest > ? ORDER BY digest LIMIT ?",
			afterDigest, pageSize).Scan(ctx, &batch)
		if err != nil {
			return nil, fmt.Errorf("listing blob digests: %w", err)
		}
		for _, d := range batch {
			digests[d] = true
		}
		if len(batch) < pageSize {
			break
		}
		afterDigest = batch[len(batch)-1]
	}
	return digests, nil
}
