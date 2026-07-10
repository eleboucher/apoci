package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
)

var ErrPackageOwnerMismatch = errors.New("package is owned by another actor")

// ErrRepositoryBusy is returned when a repo-level delete finds unexpired
// upload sessions — a push is mid-flight and the delete must not race it.
var ErrRepositoryBusy = errors.New("repository has open upload sessions")

// ErrRepositoryGone means the parent packages row vanished between the push
// path's transactions (a repo-level delete won the race). Failing loudly here
// is what prevents a silently-lost push: the client retries, the retry
// recreates the repo, and the push lands.
//
// PutManifestWithLayers and PutPackageTag guard against this with a
// SELECT EXISTS parent-exists check in the same transaction as the write. On
// SQLite that check is race-proof because BEGIN IMMEDIATE takes a global
// writer lock for the whole transaction. On Postgres, under the default READ
// COMMITTED isolation, that SELECT EXISTS takes no row lock and there is no
// FK from package_versions/package_tags to packages, so a concurrent repo
// delete can still commit between the check and the write there; Postgres-safe
// locking (e.g. SELECT ... FOR UPDATE or an FK) is a known follow-up, not a
// guarantee this code makes today.
var ErrRepositoryGone = errors.New("repository no longer exists")

const ociPackageType = "oci"

// isPostgresDB reports whether the given bun IDB speaks the Postgres dialect.
// Used to choose between RETURNING (Postgres) and a read-back (SQLite) for
// generated-column values like auto-increment ids and created_at.
func isPostgresDB(idb bun.IDB) bool {
	_, ok := idb.Dialect().(*pgdialect.Dialect)
	return ok
}

func (db *DB) GetPackage(ctx context.Context, pkgType, name string) (*Package, error) {
	p := &Package{}
	err := db.bun.NewSelect().Model(p).
		Where("type = ?", pkgType).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying package: %w", err)
	}
	return p, nil
}

func (db *DB) GetPackageByID(ctx context.Context, id int64) (*Package, error) {
	p := &Package{}
	err := db.bun.NewSelect().Model(p).Where("id = ?", id).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying package by id: %w", err)
	}
	return p, nil
}

func (db *DB) GetOrCreatePackage(ctx context.Context, pkgType, name, ownerID string) (*Package, error) {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning package transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existing Package
	err = tx.NewRaw(
		"SELECT id, type, name, owner_id, private, created_at FROM packages WHERE type = ? AND name = ?",
		pkgType, name,
	).Scan(ctx, &existing.ID, &existing.Type, &existing.Name, &existing.OwnerID, &existing.Private, &existing.CreatedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("querying package in transaction: %w", err)
	}
	if err == nil {
		if existing.OwnerID != ownerID {
			return nil, fmt.Errorf("%w: %s/%s owned by %s, not %s", ErrPackageOwnerMismatch, pkgType, name, existing.OwnerID, ownerID)
		}
		return &existing, nil
	}

	if _, err := tx.NewRaw(
		"INSERT INTO packages (type, name, owner_id) VALUES (?, ?, ?) ON CONFLICT (type, name) DO NOTHING",
		pkgType, name, ownerID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("creating package: %w", err)
	}

	var pkg Package
	if err := tx.NewRaw(
		"SELECT id, type, name, owner_id, private, created_at FROM packages WHERE type = ? AND name = ?",
		pkgType, name,
	).Scan(ctx, &pkg.ID, &pkg.Type, &pkg.Name, &pkg.OwnerID, &pkg.Private, &pkg.CreatedAt); err != nil {
		return nil, fmt.Errorf("reading package after create: %w", err)
	}
	if pkg.OwnerID != ownerID {
		return nil, fmt.Errorf("%w: %s/%s owned by %s, not %s", ErrPackageOwnerMismatch, pkgType, name, pkg.OwnerID, ownerID)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing package: %w", err)
	}
	return &pkg, nil
}

func (db *DB) ListPackages(ctx context.Context, pkgType, startAfter string, limit int) ([]Package, error) {
	var pkgs []Package
	err := db.bun.NewSelect().Model(&pkgs).
		Where("type = ?", pkgType).
		Where("name > ?", startAfter).
		OrderExpr("name").
		Limit(limit).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing packages: %w", err)
	}
	return pkgs, nil
}

func (db *DB) SetPackagePrivate(ctx context.Context, id int64, private bool) error {
	if _, err := db.bun.NewRaw(
		"UPDATE packages SET private = ? WHERE id = ?", private, id,
	).Exec(ctx); err != nil {
		return fmt.Errorf("setting package private: %w", err)
	}
	return nil
}

func (db *DB) IsPackageOwner(ctx context.Context, packageID int64, ownerID string) (bool, error) {
	var got string
	err := db.bun.NewRaw(
		"SELECT owner_id FROM packages WHERE id = ?", packageID,
	).Scan(ctx, &got)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("checking package owner: %w", err)
	}
	return got == ownerID, nil
}

func (db *DB) GetPackageVersion(ctx context.Context, packageID int64, version string) (*PackageVersion, error) {
	return getPackageVersion(ctx, db.bun, packageID, version)
}

func getPackageVersion(ctx context.Context, idb bun.IDB, packageID int64, version string) (*PackageVersion, error) {
	v := &PackageVersion{}
	err := idb.NewSelect().Model(v).
		Where("package_id = ?", packageID).
		Where("version = ?", version).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying package version: %w", err)
	}
	return v, nil
}

func (db *DB) GetPackageVersionByTag(ctx context.Context, packageID int64, tagName string) (*PackageVersion, error) {
	v := &PackageVersion{}
	err := db.bun.NewRaw(
		`SELECT pv.id, pv.package_id, pv.version, pv.metadata, pv.media_type, pv.size_bytes,
		        pv.source_actor, pv.subject_digest, pv.artifact_type, pv.created_at
		 FROM package_versions pv
		 JOIN package_tags t ON t.package_id = pv.package_id AND t.version = pv.version
		 WHERE pv.package_id = ? AND t.name = ?`,
		packageID, tagName,
	).Scan(ctx, v)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying package version by tag: %w", err)
	}
	return v, nil
}

func (db *DB) PutPackageVersion(ctx context.Context, v *PackageVersion) error {
	return putPackageVersion(ctx, db.bun, v)
}

// InsertPackageVersionIfNew inserts a version only if (package_id, version) does
// not already exist, reporting whether it was newly inserted. Publish paths use
// it to enforce immutability atomically: a racing duplicate publish gets
// inserted=false instead of silently overwriting via the upsert.
func (db *DB) InsertPackageVersionIfNew(ctx context.Context, v *PackageVersion) (bool, error) {
	res, err := db.bun.NewRaw(
		`INSERT INTO package_versions (package_id, version, metadata, media_type, size_bytes, source_actor, subject_digest, artifact_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(package_id, version) DO NOTHING`,
		v.PackageID, v.Version, v.Metadata, v.MediaType, v.SizeBytes,
		v.SourceActor, v.SubjectDigest, v.ArtifactType,
	).Exec(ctx)
	if err != nil {
		return false, fmt.Errorf("inserting package version: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("checking version insert: %w", err)
	}
	if n == 0 {
		return false, nil
	}
	got, err := getPackageVersion(ctx, db.bun, v.PackageID, v.Version)
	if err != nil {
		return false, fmt.Errorf("reading version after insert: %w", err)
	}
	if got != nil {
		*v = *got
	}
	return true, nil
}

func putPackageVersion(ctx context.Context, idb bun.IDB, v *PackageVersion) error {
	upsert := `INSERT INTO package_versions (package_id, version, metadata, media_type, size_bytes, source_actor, subject_digest, artifact_type)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(package_id, version) DO UPDATE SET
		   metadata = excluded.metadata,
		   media_type = excluded.media_type,
		   size_bytes = excluded.size_bytes,
		   source_actor = excluded.source_actor,
		   subject_digest = excluded.subject_digest,
		   artifact_type = excluded.artifact_type`
	if isPostgresDB(idb) {
		// Postgres can return the generated id + created_at in the same round trip,
		// so callers see the right ID immediately without a follow-up SELECT.
		if err := idb.NewRaw(upsert+` RETURNING id, created_at`,
			v.PackageID, v.Version, v.Metadata, v.MediaType, v.SizeBytes,
			v.SourceActor, v.SubjectDigest, v.ArtifactType,
		).Scan(ctx, &v.ID, &v.CreatedAt); err != nil {
			return fmt.Errorf("upserting package version: %w", err)
		}
		return nil
	}
	if _, err := idb.NewRaw(upsert,
		v.PackageID, v.Version, v.Metadata, v.MediaType, v.SizeBytes,
		v.SourceActor, v.SubjectDigest, v.ArtifactType,
	).Exec(ctx); err != nil {
		return fmt.Errorf("putting package version: %w", err)
	}
	got, err := getPackageVersion(ctx, idb, v.PackageID, v.Version)
	if err != nil {
		return fmt.Errorf("reading version after put: %w", err)
	}
	if got != nil {
		*v = *got
	}
	return nil
}

func (db *DB) ListPackageVersions(ctx context.Context, packageID int64) ([]PackageVersion, error) {
	var versions []PackageVersion
	err := db.bun.NewSelect().Model(&versions).
		Where("package_id = ?", packageID).
		OrderExpr("created_at DESC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing package versions: %w", err)
	}
	return versions, nil
}

func (db *DB) ListPackageVersionsBySubject(ctx context.Context, packageID int64, subjectDigest string) ([]PackageVersion, error) {
	var versions []PackageVersion
	err := db.bun.NewSelect().Model(&versions).
		Where("package_id = ?", packageID).
		Where("subject_digest = ?", subjectDigest).
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing package versions by subject: %w", err)
	}
	return versions, nil
}

// DeletePackageVersion removes a version, its tags, and its files. Orphaned
// blobs are left for the GC sweep.
func (db *DB) DeletePackageVersion(ctx context.Context, packageID int64, version string) error {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning delete version transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var versionID int64
	err = tx.NewRaw(
		"SELECT id FROM package_versions WHERE package_id = ? AND version = ?",
		packageID, version,
	).Scan(ctx, &versionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("looking up version: %w", err)
	}

	if _, err := tx.NewRaw(
		"DELETE FROM package_tags WHERE package_id = ? AND version = ?",
		packageID, version,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting tags for version: %w", err)
	}

	if _, err := tx.NewRaw(
		"DELETE FROM package_files WHERE version_id = ?", versionID,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting files for version: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM package_versions WHERE id = ?", versionID,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting version: %w", err)
	}
	return tx.Commit()
}

func (db *DB) PutPackageFile(ctx context.Context, f *PackageFile) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO package_files (version_id, filename, blob_digest, size_bytes, content_type)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(version_id, filename) DO UPDATE SET
		   blob_digest = excluded.blob_digest,
		   size_bytes = excluded.size_bytes,
		   content_type = excluded.content_type`,
		f.VersionID, f.Filename, f.BlobDigest, f.SizeBytes, f.ContentType,
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("putting package file: %w", err)
	}
	return nil
}

type BlobRef struct {
	Digest    string
	Size      int64
	MediaType *string
}

func (db *DB) PutBlobReferences(ctx context.Context, versionID int64, refs []BlobRef) error {
	if len(refs) == 0 {
		return nil
	}
	return db.bun.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		return putBlobReferences(ctx, tx, versionID, refs)
	})
}

func putBlobReferences(ctx context.Context, idb bun.IDB, versionID int64, refs []BlobRef) error {
	for _, r := range refs {
		if r.Digest == "" {
			continue
		}
		if _, err := idb.NewRaw(
			`INSERT INTO package_files (version_id, filename, blob_digest, size_bytes, content_type)
			 VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT (version_id, filename) DO UPDATE SET
			   size_bytes = CASE WHEN excluded.size_bytes > 0 THEN excluded.size_bytes ELSE package_files.size_bytes END,
			   content_type = COALESCE(excluded.content_type, package_files.content_type)`,
			versionID, r.Digest, r.Digest, r.Size, r.MediaType,
		).Exec(ctx); err != nil {
			return fmt.Errorf("inserting blob reference %s: %w", r.Digest, err)
		}
	}
	return nil
}

func (db *DB) GetPackageFile(ctx context.Context, versionID int64, filename string) (*PackageFile, error) {
	f := &PackageFile{}
	err := db.bun.NewSelect().Model(f).
		Where("version_id = ?", versionID).
		Where("filename = ?", filename).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying package file: %w", err)
	}
	return f, nil
}

func (db *DB) ListPackageFiles(ctx context.Context, versionID int64) ([]PackageFile, error) {
	var files []PackageFile
	err := db.bun.NewSelect().Model(&files).
		Where("version_id = ?", versionID).
		OrderExpr("filename").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing package files: %w", err)
	}
	return files, nil
}

func (db *DB) DeletePackageFile(ctx context.Context, versionID int64, filename string) error {
	if _, err := db.bun.NewRaw(
		"DELETE FROM package_files WHERE version_id = ? AND filename = ?",
		versionID, filename,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting package file: %w", err)
	}
	return nil
}

func (db *DB) GetPackageTag(ctx context.Context, packageID int64, name string) (*PackageTag, error) {
	t := &PackageTag{}
	err := db.bun.NewSelect().Model(t).
		Where("package_id = ?", packageID).
		Where("name = ?", name).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("querying package tag: %w", err)
	}
	return t, nil
}

// PutPackageTag upserts a tag, repointing it at version if it already exists.
// The parent-exists check runs in the same transaction so a tag can never be
// written for a package that a concurrent repo delete just removed.
func (db *DB) PutPackageTag(ctx context.Context, packageID int64, name, version string) error {
	return db.bun.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var exists bool
		if err := tx.NewRaw(
			"SELECT EXISTS(SELECT 1 FROM packages WHERE id = ?)", packageID,
		).Scan(ctx, &exists); err != nil {
			return fmt.Errorf("checking package exists: %w", err)
		}
		if !exists {
			return fmt.Errorf("putting tag for package %d: %w", packageID, ErrRepositoryGone)
		}
		if _, err := tx.NewRaw(
			`INSERT INTO package_tags (package_id, name, version, updated_at)
			 VALUES (?, ?, ?, CURRENT_TIMESTAMP)
			 ON CONFLICT(package_id, name) DO UPDATE SET
			   version = excluded.version,
			   updated_at = excluded.updated_at`,
			packageID, name, version,
		).Exec(ctx); err != nil {
			return fmt.Errorf("putting tag: %w", err)
		}
		return nil
	})
}

func (db *DB) DeletePackageTag(ctx context.Context, packageID int64, name string) error {
	if _, err := db.bun.NewRaw(
		"DELETE FROM package_tags WHERE package_id = ? AND name = ?",
		packageID, name,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting tag: %w", err)
	}
	return nil
}

func (db *DB) ListPackageTagsAfter(ctx context.Context, packageID int64, startAfter string, limit int) ([]string, error) {
	var names []string
	err := db.bun.NewRaw(
		"SELECT name FROM package_tags WHERE package_id = ? AND name > ? ORDER BY name LIMIT ?",
		packageID, startAfter, limit,
	).Scan(ctx, &names)
	if err != nil {
		return nil, fmt.Errorf("listing package tags: %w", err)
	}
	return names, nil
}

func (db *DB) ListPackageTags(ctx context.Context, packageID int64) ([]PackageTag, error) {
	var tags []PackageTag
	err := db.bun.NewSelect().Model(&tags).
		Where("package_id = ?", packageID).
		OrderExpr("name").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing package tags: %w", err)
	}
	return tags, nil
}

// IsVersionDeleted: pass packageName="" to look up by version alone (OCI's
// digest-based tombstone), or a non-empty name to scope by (type, name).
func (db *DB) IsVersionDeleted(ctx context.Context, pkgType, packageName, version string) (bool, error) {
	var exists bool
	var err error
	if packageName == "" {
		err = db.bun.NewRaw(
			"SELECT EXISTS(SELECT 1 FROM deleted_versions WHERE package_type = ? AND version = ?)",
			pkgType, version,
		).Scan(ctx, &exists)
	} else {
		err = db.bun.NewRaw(
			"SELECT EXISTS(SELECT 1 FROM deleted_versions WHERE package_type = ? AND package_name = ? AND version = ?)",
			pkgType, packageName, version,
		).Scan(ctx, &exists)
	}
	if err != nil {
		return false, fmt.Errorf("checking deleted version: %w", err)
	}
	return exists, nil
}

func (db *DB) RecordDeletedVersion(ctx context.Context, pkgType, packageName, version, sourceActor string) error {
	_, err := db.bun.NewRaw(
		`INSERT INTO deleted_versions (package_type, package_name, version, source_actor)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(package_type, package_name, version) DO NOTHING`,
		pkgType, packageName, version, sourceActor,
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("recording deleted version: %w", err)
	}
	return nil
}

func (db *DB) PruneDeletedVersions(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().Add(-olderThan)
	res, err := db.bun.NewRaw(
		"DELETE FROM deleted_versions WHERE deleted_at < ?", cutoff,
	).Exec(ctx)
	if err != nil {
		return 0, fmt.Errorf("pruning deleted versions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// Repository / Manifest / Tag DTOs below adapt the package tables to the
// pre-v6 Go API still used by the OCI handler and ActivityPub layer.

func (db *DB) GetRepository(ctx context.Context, name string) (*Repository, error) {
	pkg, err := db.GetPackage(ctx, ociPackageType, name)
	if err != nil || pkg == nil {
		return nil, err
	}
	return packageAsRepository(pkg), nil
}

func (db *DB) GetOrCreateRepository(ctx context.Context, name, ownerID string) (*Repository, error) {
	pkg, err := db.GetOrCreatePackage(ctx, ociPackageType, name, ownerID)
	if err != nil {
		return nil, err
	}
	return packageAsRepository(pkg), nil
}

func (db *DB) IsRepositoryOwner(ctx context.Context, repoID int64, did string) (bool, error) {
	return db.IsPackageOwner(ctx, repoID, did)
}

func (db *DB) ListRepositoriesAfter(ctx context.Context, startAfter string, limit int) ([]Repository, error) {
	pkgs, err := db.ListPackages(ctx, ociPackageType, startAfter, limit)
	if err != nil {
		return nil, err
	}
	repos := make([]Repository, len(pkgs))
	for i, p := range pkgs {
		repos[i] = *packageAsRepository(&p)
	}
	return repos, nil
}

func (db *DB) SetRepositoryPrivate(ctx context.Context, id int64, private bool) error {
	return db.SetPackagePrivate(ctx, id, private)
}

func (db *DB) GetManifestByDigest(ctx context.Context, repoID int64, digest string) (*Manifest, error) {
	v, err := db.GetPackageVersion(ctx, repoID, digest)
	if err != nil || v == nil {
		return nil, err
	}
	return versionAsManifest(v), nil
}

func (db *DB) GetManifestByTag(ctx context.Context, repoID int64, tag string) (*Manifest, error) {
	v, err := db.GetPackageVersionByTag(ctx, repoID, tag)
	if err != nil || v == nil {
		return nil, err
	}
	return versionAsManifest(v), nil
}

func (db *DB) PutManifest(ctx context.Context, m *Manifest) error {
	v := manifestAsVersion(m)
	if err := db.PutPackageVersion(ctx, v); err != nil {
		return err
	}
	m.ID = v.ID
	m.CreatedAt = v.CreatedAt
	return nil
}

// PutManifestWithLayers stores the manifest and its blob references in a single
// transaction, so a stored manifest never coexists with missing package_files
// rows. Without this atomicity, the GC orphan-blob sweep could reap a layer
// that a just-stored manifest references but whose package_files row hasn't
// been written yet (the manifest and refs were previously separate writes).
func (db *DB) PutManifestWithLayers(ctx context.Context, m *Manifest, refs []BlobRef) error {
	return db.bun.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		var exists bool
		if err := tx.NewRaw(
			"SELECT EXISTS(SELECT 1 FROM packages WHERE id = ?)", m.RepositoryID,
		).Scan(ctx, &exists); err != nil {
			return fmt.Errorf("checking repository exists: %w", err)
		}
		if !exists {
			return fmt.Errorf("storing manifest for repository %d: %w", m.RepositoryID, ErrRepositoryGone)
		}
		v := manifestAsVersion(m)
		if err := putPackageVersion(ctx, tx, v); err != nil {
			return err
		}
		if err := putBlobReferences(ctx, tx, v.ID, refs); err != nil {
			return err
		}
		m.ID = v.ID
		m.CreatedAt = v.CreatedAt
		return nil
	})
}

func (db *DB) ListManifestsBySubject(ctx context.Context, repoID int64, subjectDigest string) ([]Manifest, error) {
	versions, err := db.ListPackageVersionsBySubject(ctx, repoID, subjectDigest)
	if err != nil {
		return nil, err
	}
	manifests := make([]Manifest, len(versions))
	for i, v := range versions {
		manifests[i] = *versionAsManifest(&v)
	}
	return manifests, nil
}

func (db *DB) DeleteManifest(ctx context.Context, repoID int64, digest string) error {
	return db.DeletePackageVersion(ctx, repoID, digest)
}

func (db *DB) DeletePackage(ctx context.Context, packageID int64) error {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning delete package transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.NewRaw(
		`DELETE FROM package_files
		 WHERE version_id IN (SELECT id FROM package_versions WHERE package_id = ?)`,
		packageID,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting package files: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM package_tags WHERE package_id = ?", packageID,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting package tags: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM package_versions WHERE package_id = ?", packageID,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting package versions: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM packages WHERE id = ?", packageID,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting package: %w", err)
	}
	// Uploads are keyed by repository_id, so nothing else reaps them.
	if _, err := tx.NewRaw(
		"DELETE FROM upload_sessions WHERE repository_id = ?", packageID,
	).Exec(ctx); err != nil {
		return fmt.Errorf("deleting upload sessions: %w", err)
	}
	return tx.Commit()
}

func (db *DB) DeleteRepository(ctx context.Context, repoID int64) error {
	return db.DeletePackage(ctx, repoID)
}

func (db *DB) ListPackagesPendingWithdrawal(ctx context.Context, pkgType string) ([]Package, error) {
	var pkgs []Package
	err := db.bun.NewSelect().Model(&pkgs).
		Where("type = ?", pkgType).
		Where("federation_withdrawn_at IS NULL").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing packages pending withdrawal: %w", err)
	}
	return pkgs, nil
}

func (db *DB) MarkPackageWithdrawn(ctx context.Context, packageID int64) error {
	_, err := db.bun.NewRaw(
		"UPDATE packages SET federation_withdrawn_at = current_timestamp WHERE id = ?",
		packageID,
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("marking package withdrawn: %w", err)
	}
	return nil
}

// DeletePackageWithBlobs returns digests whose blob rows it deleted; the caller
// removes the bytes from storage. Unlike GC, peer_blobs does not gate purge.
func (db *DB) DeletePackageWithBlobs(ctx context.Context, packageID int64) ([]string, error) {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning delete package transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	candidates, err := deletePackageRowsTx(ctx, tx, packageID)
	if err != nil {
		return nil, err
	}
	purged, err := purgeUnreferencedBlobs(ctx, tx, candidates)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing delete package transaction: %w", err)
	}
	return purged, nil
}

// deletePackageRowsTx deletes a package's files, tags, versions, and its own
// row inside tx, returning the distinct blob digests those files referenced
// (the purge candidates).
func deletePackageRowsTx(ctx context.Context, tx bun.Tx, packageID int64) ([]string, error) {
	var candidates []string
	if err := tx.NewRaw(
		`SELECT DISTINCT pf.blob_digest
		 FROM package_files pf
		 JOIN package_versions pv ON pv.id = pf.version_id
		 JOIN blobs b ON b.digest = pf.blob_digest
		 WHERE pv.package_id = ?`, packageID,
	).Scan(ctx, &candidates); err != nil {
		return nil, fmt.Errorf("collecting blob digests for package: %w", err)
	}

	if _, err := tx.NewRaw(
		`DELETE FROM package_files
		 WHERE version_id IN (SELECT id FROM package_versions WHERE package_id = ?)`,
		packageID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting package files: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM package_tags WHERE package_id = ?", packageID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting package tags: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM package_versions WHERE package_id = ?", packageID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting package versions: %w", err)
	}
	// Uploads are keyed by repository_id, so nothing else reaps them.
	if _, err := tx.NewRaw(
		"DELETE FROM upload_sessions WHERE repository_id = ?", packageID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting upload sessions: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM packages WHERE id = ?", packageID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting package: %w", err)
	}
	return candidates, nil
}

// RepoDeletion reports what a repo-level delete removed.
type RepoDeletion struct {
	Name            string
	ManifestDigests []string
	TagNames        []string
	PurgedBlobs     []string
}

// DeleteOwnedRepositoryWithBlobs deletes an OCI repository owned by ownerID:
// manifests, tags, files, the packages row, and any blob rows left
// unreferenced. Per-manifest tombstones are recorded in the SAME transaction,
// so a federation peer can never resurrect the deleted content (getManifest
// re-stores peer fetches unless IsManifestDeleted says otherwise).
//
// The owner re-check, the open-upload-session check, and the delete all run
// in one immediate transaction: under BEGIN IMMEDIATE nothing can create a
// session or land a manifest between the checks and the delete. That
// guarantee is a SQLite property (the DSN sets _txlock=immediate, so BEGIN
// IMMEDIATE takes a single global writer lock for the transaction's
// lifetime). On Postgres, under the default READ COMMITTED isolation, these
// checks take no row locks, so a concurrent push can still interleave with
// this delete and land a manifest or tag the delete doesn't see —
// Postgres-safe locking (e.g. SELECT ... FOR UPDATE) is a known follow-up,
// not something this transaction enforces today.
//
// Returns (nil, nil) if the repository does not exist, ErrPackageOwnerMismatch
// if it is not owned by ownerID, ErrRepositoryBusy if an unexpired upload
// session exists (a push is mid-flight).
func (db *DB) DeleteOwnedRepositoryWithBlobs(ctx context.Context, repoID int64, ownerID string) (*RepoDeletion, error) {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning repo delete transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Re-check existence and ownership inside the transaction — the caller's
	// lookup ran in a separate transaction and may be stale.
	var row struct {
		Name    string `bun:"name"`
		OwnerID string `bun:"owner_id"`
	}
	err = tx.NewRaw(
		"SELECT name, owner_id FROM packages WHERE id = ? AND type = ?",
		repoID, ociPackageType,
	).Scan(ctx, &row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("looking up repository for delete: %w", err)
	}
	if row.OwnerID != ownerID {
		return nil, ErrPackageOwnerMismatch
	}

	// App clock, not CURRENT_TIMESTAMP — matches GetUploadSession.
	var busy bool
	if err := tx.NewRaw(
		"SELECT EXISTS(SELECT 1 FROM upload_sessions WHERE repository_id = ? AND expires_at > ?)",
		repoID, time.Now(),
	).Scan(ctx, &busy); err != nil {
		return nil, fmt.Errorf("checking upload sessions: %w", err)
	}
	if busy {
		return nil, ErrRepositoryBusy
	}

	var digests []string
	if err := tx.NewRaw(
		"SELECT version FROM package_versions WHERE package_id = ? ORDER BY id",
		repoID,
	).Scan(ctx, &digests); err != nil {
		return nil, fmt.Errorf("listing manifests for delete: %w", err)
	}
	var tags []string
	if err := tx.NewRaw(
		"SELECT name FROM package_tags WHERE package_id = ? ORDER BY name",
		repoID,
	).Scan(ctx, &tags); err != nil {
		return nil, fmt.Errorf("listing tags for delete: %w", err)
	}

	// Tombstone every manifest in the same transaction (same statement shape
	// as RecordDeletedVersion, set-based).
	if _, err := tx.NewRaw(
		`INSERT INTO deleted_versions (package_type, package_name, version, source_actor)
		 SELECT ?, ?, pv.version, ? FROM package_versions pv WHERE pv.package_id = ?
		 ON CONFLICT(package_type, package_name, version) DO NOTHING`,
		ociPackageType, row.Name, ownerID, repoID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("recording manifest tombstones: %w", err)
	}

	candidates, err := deletePackageRowsTx(ctx, tx, repoID)
	if err != nil {
		return nil, err
	}
	purged, err := purgeUnreferencedBlobs(ctx, tx, candidates)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing repo delete transaction: %w", err)
	}
	return &RepoDeletion{
		Name:            row.Name,
		ManifestDigests: digests,
		TagNames:        tags,
		PurgedBlobs:     purged,
	}, nil
}

func (db *DB) DeletePackageVersionWithBlobs(ctx context.Context, packageID int64, version string) ([]string, error) {
	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning delete version transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var versionID int64
	if err := tx.NewRaw(
		"SELECT id FROM package_versions WHERE package_id = ? AND version = ?",
		packageID, version,
	).Scan(ctx, &versionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("looking up version: %w", err)
	}

	var candidates []string
	if err := tx.NewRaw(
		`SELECT DISTINCT pf.blob_digest
		 FROM package_files pf
		 JOIN blobs b ON b.digest = pf.blob_digest
		 WHERE pf.version_id = ?`, versionID,
	).Scan(ctx, &candidates); err != nil {
		return nil, fmt.Errorf("collecting blob digests for version: %w", err)
	}

	if _, err := tx.NewRaw(
		"DELETE FROM package_tags WHERE package_id = ? AND version = ?",
		packageID, version,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting tags for version: %w", err)
	}

	if _, err := tx.NewRaw(
		"DELETE FROM package_files WHERE version_id = ?", versionID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting files for version: %w", err)
	}
	if _, err := tx.NewRaw(
		"DELETE FROM package_versions WHERE id = ?", versionID,
	).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting version: %w", err)
	}

	purged, err := purgeUnreferencedBlobs(ctx, tx, candidates)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing delete version transaction: %w", err)
	}
	return purged, nil
}

func purgeUnreferencedBlobs(ctx context.Context, tx bun.Tx, candidates []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	// Resolve which candidates have no remaining references and delete them in
	// two set-based statements rather than 2N round-trips, which would hold the
	// write lock (and block all other writers under SQLite) for the duration.
	var purged []string
	if err := tx.NewRaw(
		`SELECT b.digest FROM blobs b
		 WHERE b.digest IN (?)
		   AND NOT EXISTS (SELECT 1 FROM package_files pf WHERE pf.blob_digest = b.digest)`,
		bun.List(candidates),
	).Scan(ctx, &purged); err != nil {
		return nil, fmt.Errorf("finding unreferenced blobs: %w", err)
	}
	if len(purged) == 0 {
		return nil, nil
	}
	if _, err := tx.NewRaw("DELETE FROM blobs WHERE digest IN (?)", bun.List(purged)).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting unreferenced blobs: %w", err)
	}
	return purged, nil
}

func (db *DB) PutManifestLayers(ctx context.Context, manifestID int64, refs []BlobRef) error {
	return db.PutBlobReferences(ctx, manifestID, refs)
}

func (db *DB) GetTag(ctx context.Context, repoID int64, name string) (*Tag, error) {
	t, err := db.GetPackageTag(ctx, repoID, name)
	if err != nil || t == nil {
		return nil, err
	}
	return tagAsLegacyTag(t), nil
}

func (db *DB) PutTag(ctx context.Context, repoID int64, name, manifestDigest string) error {
	return db.PutPackageTag(ctx, repoID, name, manifestDigest)
}

func (db *DB) DeleteTag(ctx context.Context, repoID int64, name string) error {
	return db.DeletePackageTag(ctx, repoID, name)
}

func (db *DB) ListTagsAfter(ctx context.Context, repoID int64, startAfter string, limit int) ([]string, error) {
	return db.ListPackageTagsAfter(ctx, repoID, startAfter, limit)
}

func (db *DB) IsManifestDeleted(ctx context.Context, digest string) (bool, error) {
	return db.IsVersionDeleted(ctx, ociPackageType, "", digest)
}

func (db *DB) RecordDeletedManifest(ctx context.Context, digest, repoName, sourceActor string) error {
	return db.RecordDeletedVersion(ctx, ociPackageType, repoName, digest, sourceActor)
}

func (db *DB) PruneDeletedManifests(ctx context.Context, olderThan time.Duration) (int64, error) {
	return db.PruneDeletedVersions(ctx, olderThan)
}

type UntaggedManifest struct {
	PackageID   int64
	PackageName string
	OwnerID     string
	Digest      string
}

type PackageRetention struct {
	ID      int64
	Name    string
	OwnerID string
}

type TagForRetention struct {
	Name      string
	UpdatedAt time.Time
}

func (db *DB) ListOCIPackagesForRetention(ctx context.Context, startAfter string, limit int) ([]PackageRetention, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []struct {
		ID      int64  `bun:"id"`
		Name    string `bun:"name"`
		OwnerID string `bun:"owner_id"`
	}
	err := db.bun.NewRaw(
		"SELECT id, name, owner_id FROM packages WHERE type = ? AND name > ? ORDER BY name LIMIT ?",
		ociPackageType, startAfter, limit,
	).Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("listing oci packages for retention: %w", err)
	}
	out := make([]PackageRetention, len(rows))
	for i, r := range rows {
		out[i] = PackageRetention{ID: r.ID, Name: r.Name, OwnerID: r.OwnerID}
	}
	return out, nil
}

func (db *DB) ListTagsForRetention(ctx context.Context, packageID int64) ([]TagForRetention, error) {
	var rows []struct {
		Name      string    `bun:"name"`
		UpdatedAt time.Time `bun:"updated_at"`
	}
	err := db.bun.NewRaw(
		"SELECT name, updated_at FROM package_tags WHERE package_id = ? ORDER BY updated_at DESC",
		packageID,
	).Scan(ctx, &rows)
	if err != nil {
		return nil, fmt.Errorf("listing tags for retention: %w", err)
	}
	out := make([]TagForRetention, len(rows))
	for i, r := range rows {
		out[i] = TagForRetention{
			Name:      r.Name,
			UpdatedAt: r.UpdatedAt,
		}
	}
	return out, nil
}

// PruneUntaggedManifests deletes OCI manifests older than olderThan that have
// no tag, no OCI referrer (subject), and no parent manifest (index child).
// Cascades package_files.
func (db *DB) PruneUntaggedManifests(ctx context.Context, olderThan time.Duration, limit int) ([]UntaggedManifest, error) {
	if limit <= 0 {
		limit = 500
	}
	cutoff := time.Now().Add(-olderThan)

	tx, err := db.bun.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("beginning prune transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var rows []struct {
		ID          int64  `bun:"id"`
		PackageID   int64  `bun:"package_id"`
		PackageName string `bun:"package_name"`
		OwnerID     string `bun:"owner_id"`
		Digest      string `bun:"digest"`
	}
	if err := tx.NewRaw(`
		SELECT pv.id AS id, pv.package_id AS package_id, p.name AS package_name, p.owner_id AS owner_id, pv.version AS digest
		FROM package_versions pv
		JOIN packages p ON p.id = pv.package_id
		WHERE p.type = ?
		  AND pv.created_at < ?
		  AND NOT EXISTS (SELECT 1 FROM package_tags pt
		                  WHERE pt.package_id = pv.package_id AND pt.version = pv.version)
		  AND NOT EXISTS (SELECT 1 FROM package_versions ref
		                  WHERE ref.package_id = pv.package_id
		                    AND ref.subject_digest = pv.version)
		  AND NOT EXISTS (SELECT 1 FROM package_files pf
		                  JOIN package_versions parent ON parent.id = pf.version_id
		                  WHERE parent.package_id = pv.package_id
		                    AND pf.blob_digest = pv.version)
		ORDER BY pv.id
		LIMIT ?
	`, ociPackageType, cutoff, limit).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("finding untagged manifests: %w", err)
	}

	if len(rows) == 0 {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("committing prune transaction: %w", err)
		}
		return nil, nil
	}

	versionIDs := make([]int64, len(rows))
	for i, r := range rows {
		versionIDs[i] = r.ID
	}

	if _, err := tx.NewRaw("DELETE FROM package_files WHERE version_id IN (?)", bun.List(versionIDs)).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting package_files: %w", err)
	}
	if _, err := tx.NewRaw("DELETE FROM package_versions WHERE id IN (?)", bun.List(versionIDs)).Exec(ctx); err != nil {
		return nil, fmt.Errorf("deleting package_versions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("committing prune transaction: %w", err)
	}

	out := make([]UntaggedManifest, len(rows))
	for i, r := range rows {
		out[i] = UntaggedManifest{
			PackageID:   r.PackageID,
			PackageName: r.PackageName,
			OwnerID:     r.OwnerID,
			Digest:      r.Digest,
		}
	}
	return out, nil
}

func packageAsRepository(p *Package) *Repository {
	return &Repository{
		ID:        p.ID,
		Name:      p.Name,
		OwnerID:   p.OwnerID,
		Private:   p.Private,
		CreatedAt: p.CreatedAt,
	}
}

func versionAsManifest(v *PackageVersion) *Manifest {
	return &Manifest{
		ID:            v.ID,
		RepositoryID:  v.PackageID,
		Digest:        v.Version,
		MediaType:     v.MediaType,
		SizeBytes:     v.SizeBytes,
		Content:       v.Metadata,
		SourceActor:   v.SourceActor,
		SubjectDigest: v.SubjectDigest,
		ArtifactType:  v.ArtifactType,
		CreatedAt:     v.CreatedAt,
	}
}

func manifestAsVersion(m *Manifest) *PackageVersion {
	return &PackageVersion{
		ID:            m.ID,
		PackageID:     m.RepositoryID,
		Version:       m.Digest,
		Metadata:      m.Content,
		MediaType:     m.MediaType,
		SizeBytes:     m.SizeBytes,
		SourceActor:   m.SourceActor,
		SubjectDigest: m.SubjectDigest,
		ArtifactType:  m.ArtifactType,
		CreatedAt:     m.CreatedAt,
	}
}

func tagAsLegacyTag(t *PackageTag) *Tag {
	return &Tag{
		ID:             t.ID,
		RepositoryID:   t.PackageID,
		Name:           t.Name,
		ManifestDigest: t.Version,
		UpdatedAt:      t.UpdatedAt,
	}
}

type RepoWithStats struct {
	ID        int64
	Name      string
	OwnerID   string
	Tags      []string
	SizeBytes int64
	UpdatedAt time.Time
}

type ReposPage struct {
	Repos      []RepoWithStats
	TotalCount int
	Page       int
	PageSize   int
	TotalPages int
}

func (db *DB) ListReposWithStats(ctx context.Context, query string) ([]RepoWithStats, error) {
	rows, err := db.queryRepoStats(ctx, query, 0, 0, false)
	if err != nil {
		return nil, err
	}
	return rows.Repos, nil
}

func (db *DB) ListReposWithStatsPaginated(ctx context.Context, query string, page, pageSize int) (*ReposPage, error) {
	if pageSize <= 0 {
		pageSize = 20
	}
	if page <= 0 {
		page = 1
	}
	return db.queryRepoStats(ctx, query, page, pageSize, true)
}

func (db *DB) tagAggSQL() string {
	_, isPostgres := db.bun.Dialect().(*pgdialect.Dialect)
	if isPostgres {
		return `COALESCE(
			(SELECT STRING_AGG(t.name, ',') FROM (
				SELECT name FROM package_tags WHERE package_id = p.id ORDER BY updated_at DESC LIMIT 10
			) t),
			''
		)`
	}
	return `COALESCE(
			(SELECT GROUP_CONCAT(t.name, ',') FROM (
				SELECT name FROM package_tags WHERE package_id = p.id ORDER BY updated_at DESC LIMIT 10
			) t),
			''
		)`
}

func (db *DB) queryRepoStats(ctx context.Context, query string, page, pageSize int, paginated bool) (*ReposPage, error) {
	_, isPostgres := db.bun.Dialect().(*pgdialect.Dialect)

	tagAgg := db.tagAggSQL()

	totalCount := 0
	if paginated {
		countQuery := `SELECT COUNT(*) FROM packages p WHERE p.type = ? AND p.private = false`
		args := []any{ociPackageType}
		if query != "" {
			likePattern := "%" + escapeLike(query) + "%"
			if isPostgres {
				countQuery += ` AND p.name ILIKE ? ESCAPE '\'`
			} else {
				countQuery += ` AND p.name LIKE ? ESCAPE '\' COLLATE NOCASE`
			}
			args = append(args, likePattern)
		}
		if err := db.bun.NewRaw(countQuery, args...).Scan(ctx, &totalCount); err != nil {
			return nil, fmt.Errorf("counting repos: %w", err)
		}
	}

	baseQuery := `
		SELECT
			p.id,
			p.name,
			p.owner_id,
			` + tagAgg + ` as tags,
			COALESCE(
				(SELECT SUM(b.size_bytes) FROM blobs b
				 WHERE b.digest IN (
					SELECT DISTINCT pf.blob_digest
					FROM package_files pf
					JOIN package_versions pv ON pv.id = pf.version_id
					WHERE pv.package_id = p.id
				 )),
				0
			) as size_bytes,
			COALESCE(
				(SELECT MAX(updated_at) FROM package_tags WHERE package_id = p.id),
				p.created_at
			) as updated_at
		FROM packages p
		WHERE p.type = ? AND p.private = false
	`

	args := []any{ociPackageType}
	if query != "" {
		likePattern := "%" + escapeLike(query) + "%"
		if isPostgres {
			baseQuery += ` AND p.name ILIKE ? ESCAPE '\'`
		} else {
			baseQuery += ` AND p.name LIKE ? ESCAPE '\' COLLATE NOCASE`
		}
		args = append(args, likePattern)
	}
	baseQuery += " ORDER BY p.name"
	if paginated {
		baseQuery += " LIMIT ? OFFSET ?"
		offset := (page - 1) * pageSize
		args = append(args, pageSize, offset)
	}

	var rows []struct {
		ID        int64     `bun:"id"`
		Name      string    `bun:"name"`
		OwnerID   string    `bun:"owner_id"`
		Tags      string    `bun:"tags"`
		SizeBytes int64     `bun:"size_bytes"`
		UpdatedAt time.Time `bun:"updated_at"`
	}
	if err := db.bun.NewRaw(baseQuery, args...).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing repos with stats: %w", err)
	}

	repos := make([]RepoWithStats, len(rows))
	for i, row := range rows {
		var tags []string
		if row.Tags != "" {
			tags = strings.Split(row.Tags, ",")
		}
		repos[i] = RepoWithStats{
			ID:        row.ID,
			Name:      row.Name,
			OwnerID:   row.OwnerID,
			Tags:      tags,
			SizeBytes: row.SizeBytes,
			UpdatedAt: row.UpdatedAt,
		}
	}

	if !paginated {
		return &ReposPage{Repos: repos}, nil
	}
	totalPages := (totalCount + pageSize - 1) / pageSize
	if totalPages == 0 {
		totalPages = 1
	}
	return &ReposPage{
		Repos:      repos,
		TotalCount: totalCount,
		Page:       page,
		PageSize:   pageSize,
		TotalPages: totalPages,
	}, nil
}

type TagWithDetails struct {
	Name            string
	Digest          string
	MediaType       string
	ArtifactType    *string
	SizeBytes       int64
	UpdatedAt       time.Time
	ManifestContent []byte
}

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

	var totalCount int
	if err := db.bun.NewRaw(
		`SELECT COUNT(*) FROM package_tags WHERE package_id = ?`, repoID,
	).Scan(ctx, &totalCount); err != nil {
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
	if err := db.bun.NewRaw(`
		SELECT t.name, t.version as digest, pv.media_type, pv.artifact_type, pv.size_bytes, t.updated_at, pv.metadata as content
		FROM package_tags t
		JOIN package_versions pv ON pv.package_id = t.package_id AND pv.version = t.version
		WHERE t.package_id = ?
		ORDER BY t.updated_at DESC
		LIMIT ? OFFSET ?
	`, repoID, pageSize, offset).Scan(ctx, &rows); err != nil {
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

func (db *DB) ListLocallyHostedRepos(ctx context.Context) ([]RepoWithStats, error) {
	tagAgg := db.tagAggSQL()
	q := `
		SELECT
			p.id,
			p.name,
			p.owner_id,
			` + tagAgg + ` as tags,
			COALESCE(
				(SELECT SUM(b.size_bytes) FROM blobs b
				 WHERE b.stored_locally = true
				 AND b.digest IN (
					SELECT DISTINCT pf.blob_digest
					FROM package_files pf
					JOIN package_versions pv ON pv.id = pf.version_id
					WHERE pv.package_id = p.id
				 )),
				0
			) as size_bytes,
			COALESCE(
				(SELECT MAX(updated_at) FROM package_tags WHERE package_id = p.id),
				p.created_at
			) as updated_at
		FROM packages p
		WHERE p.type = ?
		AND EXISTS (
			SELECT 1 FROM blobs b
			JOIN package_files pf ON pf.blob_digest = b.digest
			JOIN package_versions pv ON pv.id = pf.version_id
			WHERE pv.package_id = p.id AND b.stored_locally = true
		)
		ORDER BY size_bytes DESC
	`
	var rows []struct {
		ID        int64     `bun:"id"`
		Name      string    `bun:"name"`
		OwnerID   string    `bun:"owner_id"`
		Tags      string    `bun:"tags"`
		SizeBytes int64     `bun:"size_bytes"`
		UpdatedAt time.Time `bun:"updated_at"`
	}
	if err := db.bun.NewRaw(q, ociPackageType).Scan(ctx, &rows); err != nil {
		return nil, fmt.Errorf("listing locally hosted repos: %w", err)
	}
	repos := make([]RepoWithStats, len(rows))
	for i, row := range rows {
		var tags []string
		if row.Tags != "" {
			tags = strings.Split(row.Tags, ",")
		}
		repos[i] = RepoWithStats{
			ID:        row.ID,
			Name:      row.Name,
			OwnerID:   row.OwnerID,
			Tags:      tags,
			SizeBytes: row.SizeBytes,
			UpdatedAt: row.UpdatedAt,
		}
	}
	return repos, nil
}
