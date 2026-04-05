package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (db *DB) GetManifestByDigest(ctx context.Context, repoID int64, digest string) (*Manifest, error) {
	m := &Manifest{}
	err := db.bun.NewSelect().Model(m).
		Where("repository_id = ?", repoID).
		Where("digest = ?", digest).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying manifest by digest: %w", err)
	}
	return m, nil
}

func (db *DB) GetManifestByTag(ctx context.Context, repoID int64, tag string) (*Manifest, error) {
	m := &Manifest{}
	err := db.bun.NewRaw(
		`SELECT m.id, m.repository_id, m.digest, m.media_type, m.size_bytes, m.content,
		        m.source_actor, m.subject_digest, m.artifact_type, m.created_at
		 FROM manifests m
		 JOIN tags t ON t.repository_id = m.repository_id AND t.manifest_digest = m.digest
		 WHERE m.repository_id = ? AND t.name = ?`, repoID, tag).Scan(ctx, m)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying manifest by tag: %w", err)
	}
	return m, nil
}

func (db *DB) PutManifest(ctx context.Context, m *Manifest) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO manifests (repository_id, digest, media_type, size_bytes, content, source_actor, subject_digest, artifact_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(repository_id, digest) DO UPDATE SET
		   media_type = excluded.media_type,
		   size_bytes = excluded.size_bytes,
		   content = excluded.content,
		   source_actor = excluded.source_actor,
		   subject_digest = excluded.subject_digest,
		   artifact_type = excluded.artifact_type`,
		m.RepositoryID, m.Digest, m.MediaType, m.SizeBytes, m.Content, m.SourceActor, m.SubjectDigest, m.ArtifactType).Exec(ctx)
	if err != nil {
		return fmt.Errorf("putting manifest: %w", err)
	}
	return nil
}

func (db *DB) ListManifestsBySubject(ctx context.Context, repoID int64, subjectDigest string) ([]Manifest, error) {
	var manifests []Manifest
	err := db.bun.NewSelect().Model(&manifests).
		Where("repository_id = ?", repoID).
		Where("subject_digest = ?", subjectDigest).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing manifests by subject: %w", err)
	}
	return manifests, nil
}

func (db *DB) DeleteManifest(ctx context.Context, repoID int64, digest string) error {
	_, err := db.bun.NewRaw(
		"DELETE FROM manifests WHERE repository_id = ? AND digest = ?", repoID, digest).Exec(ctx)
	if err != nil {
		return fmt.Errorf("deleting manifest: %w", err)
	}
	return nil
}

func (db *DB) PutManifestLayers(ctx context.Context, manifestID int64, blobDigests []string) error {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, digest := range blobDigests {
		if _, err := tx.NewRaw(
			"INSERT INTO manifest_layers (manifest_id, blob_digest) VALUES (?, ?) ON CONFLICT (manifest_id, blob_digest) DO NOTHING",
			manifestID, digest).Exec(ctx); err != nil {
			return fmt.Errorf("inserting manifest layer: %w", err)
		}
	}

	return tx.Commit()
}
