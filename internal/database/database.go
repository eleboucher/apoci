package database

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/pgdialect"
	"github.com/uptrace/bun/dialect/sqlitedialect"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	bun    *bun.DB
	logger *slog.Logger
}

// OpenSQLite opens a SQLite database in the given data directory.
func OpenSQLite(dataDir string, maxOpen, maxIdle int, logger *slog.Logger) (*DB, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}

	dbPath := filepath.Join(dataDir, "apoci.db")
	dsn := fmt.Sprintf("file:%s?_journal_mode=WAL&_foreign_keys=ON&_busy_timeout=5000&_synchronous=NORMAL", dbPath)

	sqldb, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite database: %w", err)
	}

	if maxOpen <= 0 {
		maxOpen = 4
	}
	if maxIdle <= 0 {
		maxIdle = maxOpen
	}
	sqldb.SetMaxOpenConns(maxOpen)
	sqldb.SetMaxIdleConns(maxIdle)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	bunDB := bun.NewDB(sqldb, sqlitedialect.New())

	db := &DB{bun: bunDB, logger: logger}
	if err := db.migrate(context.Background()); err != nil {
		_ = bunDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	logger.Info("database opened", "path", dbPath)
	return db, nil
}

// OpenPostgres opens a PostgreSQL database with the given DSN.
func OpenPostgres(dsn string, maxOpen, maxIdle int, logger *slog.Logger) (*DB, error) {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres database: %w", err)
	}

	if maxOpen <= 0 {
		maxOpen = 25
	}
	if maxIdle <= 0 {
		maxIdle = 10
	}
	sqldb.SetMaxOpenConns(maxOpen)
	sqldb.SetMaxIdleConns(maxIdle)

	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("pinging database: %w", err)
	}

	bunDB := bun.NewDB(sqldb, pgdialect.New())

	db := &DB{bun: bunDB, logger: logger}
	if err := db.migrate(context.Background()); err != nil {
		_ = bunDB.Close()
		return nil, fmt.Errorf("running migrations: %w", err)
	}

	logger.Info("database opened", "driver", "postgres")
	return db, nil
}

func (db *DB) Ping() error {
	return db.bun.Ping()
}

// QueryContext exposes raw SQL — used by tests for assertions.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.bun.QueryContext(ctx, query, args...)
}

func (db *DB) Close() error {
	return db.bun.Close()
}

// tableExists checks if a table exists in the database.
func (db *DB) tableExists(ctx context.Context, tableName string) bool {
	var count int
	err := db.bun.NewRaw(
		"SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", tableName).Scan(ctx, &count)
	if err != nil {
		return false
	}
	return count > 0
}

func (db *DB) migrate(ctx context.Context) error {
	if _, err := db.bun.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return fmt.Errorf("creating schema_version table: %w", err)
	}

	version := 0
	row := db.bun.QueryRowContext(ctx, `SELECT version FROM schema_version LIMIT 1`)
	if err := row.Scan(&version); err != nil {
		// No row yet.
		if _, err := db.bun.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (0)`); err != nil {
			return fmt.Errorf("initializing schema version: %w", err)
		}
	}

	if version < 1 {
		if err := db.migrateV1(ctx); err != nil {
			return fmt.Errorf("migration v1: %w", err)
		}
		if _, err := db.bun.ExecContext(ctx, `UPDATE schema_version SET version = 1`); err != nil {
			return fmt.Errorf("updating schema version to 1: %w", err)
		}
		version = 1
	}

	if version < 2 {
		if err := db.migrateV2(ctx); err != nil {
			return fmt.Errorf("migration v2: %w", err)
		}
		if _, err := db.bun.ExecContext(ctx, `UPDATE schema_version SET version = 2`); err != nil {
			return fmt.Errorf("updating schema version to 2: %w", err)
		}
		version = 2
	}

	if version < 3 {
		if err := db.migrateV3(ctx); err != nil {
			return fmt.Errorf("migration v3: %w", err)
		}
		if _, err := db.bun.ExecContext(ctx, `UPDATE schema_version SET version = 3`); err != nil {
			return fmt.Errorf("updating schema version to 3: %w", err)
		}
		version = 3
	}

	if version < 4 {
		if err := db.migrateV4(ctx); err != nil {
			return fmt.Errorf("migration v4: %w", err)
		}
		if _, err := db.bun.ExecContext(ctx, `UPDATE schema_version SET version = 4`); err != nil {
			return fmt.Errorf("updating schema version to 4: %w", err)
		}
		version = 4
	}

	if version < 5 {
		if err := db.migrateV5(ctx); err != nil {
			return fmt.Errorf("migration v5: %w", err)
		}
		if _, err := db.bun.ExecContext(ctx, `UPDATE schema_version SET version = 5`); err != nil {
			return fmt.Errorf("updating schema version to 5: %w", err)
		}
		version = 5
	}
	_ = version // used by future migrations

	return nil
}

func (db *DB) migrateV4(ctx context.Context) error {
	if _, ok := db.bun.Dialect().(*pgdialect.Dialect); ok {
		_, err := db.bun.ExecContext(ctx,
			"ALTER TABLE repositories ADD COLUMN IF NOT EXISTS private BOOLEAN NOT NULL DEFAULT FALSE")
		if err != nil {
			return fmt.Errorf("adding private column: %w", err)
		}
		return nil
	}
	var count int
	if err := db.bun.NewRaw(
		"SELECT COUNT(*) FROM pragma_table_info('repositories') WHERE name = 'private'").
		Scan(ctx, &count); err != nil {
		return fmt.Errorf("checking repositories.private column: %w", err)
	}
	if count > 0 {
		return nil // column already exists (fresh database)
	}
	if _, err := db.bun.ExecContext(ctx,
		"ALTER TABLE repositories ADD COLUMN private BOOLEAN NOT NULL DEFAULT FALSE"); err != nil {
		return fmt.Errorf("adding private column: %w", err)
	}
	return nil
}

// migrateV1 creates base tables, constraints, and indexes.
func (db *DB) migrateV1(ctx context.Context) error {
	models := []any{
		(*Repository)(nil),
		(*Manifest)(nil),
		(*Tag)(nil),
		(*Blob)(nil),
		(*RepositoryOwner)(nil),
		(*ManifestLayer)(nil),
		(*PeerBlob)(nil),
		(*Actor)(nil),
		(*FollowRequest)(nil),
		(*Activity)(nil),
		(*UploadSession)(nil),
		(*Delivery)(nil),
	}

	for _, model := range models {
		if _, err := db.bun.NewCreateTable().Model(model).IfNotExists().Exec(ctx); err != nil {
			return fmt.Errorf("creating table for %T: %w", model, err)
		}
	}

	compositeConstraints := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_manifests_repo_digest ON manifests (repository_id, digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_tags_repo_name ON tags (repository_id, name)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_peer_blobs_actor_digest ON peer_blobs (peer_actor, blob_digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_repo_owners_pk ON repository_owners (repository_id, owner_id)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_manifest_layers_pk ON manifest_layers (manifest_id, blob_digest)",
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_delivery_queue_activity_inbox ON delivery_queue (activity_id, inbox_url)",
	}
	for _, ddl := range compositeConstraints {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating constraint: %w", err)
		}
	}

	indexes := []string{
		"CREATE INDEX IF NOT EXISTS idx_manifests_digest ON manifests (digest)",
		"CREATE INDEX IF NOT EXISTS idx_manifests_repo ON manifests (repository_id)",
		"CREATE INDEX IF NOT EXISTS idx_blobs_stored ON blobs (stored_locally)",
		"CREATE INDEX IF NOT EXISTS idx_peer_blobs_digest ON peer_blobs (blob_digest)",
		"CREATE INDEX IF NOT EXISTS idx_peer_blobs_peer ON peer_blobs (peer_actor)",
		"CREATE INDEX IF NOT EXISTS idx_actors_is_healthy ON actors (is_healthy)",
		"CREATE INDEX IF NOT EXISTS idx_upload_sessions_expires ON upload_sessions (expires_at)",
		"CREATE INDEX IF NOT EXISTS idx_repositories_owner ON repositories (owner_id)",
		"CREATE INDEX IF NOT EXISTS idx_actors_actor_url ON actors (actor_url)",
		"CREATE INDEX IF NOT EXISTS idx_actors_they_follow_us ON actors (they_follow_us)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_them ON actors (we_follow_them)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_status ON actors (we_follow_status)",
		"CREATE INDEX IF NOT EXISTS idx_actors_created_at ON actors (created_at)",
		"CREATE INDEX IF NOT EXISTS idx_follow_requests_actor ON follow_requests (actor_url)",
		"CREATE INDEX IF NOT EXISTS idx_activities_type ON activities (type)",
		"CREATE INDEX IF NOT EXISTS idx_activities_actor ON activities (actor_url)",
		"CREATE INDEX IF NOT EXISTS idx_activities_published ON activities (published_at)",
		"CREATE INDEX IF NOT EXISTS idx_delivery_queue_pending ON delivery_queue (status, next_attempt_at)",
		"CREATE INDEX IF NOT EXISTS idx_delivery_queue_activity ON delivery_queue (activity_id)",
		"CREATE INDEX IF NOT EXISTS idx_tags_manifest_digest ON tags (manifest_digest)",
		"CREATE INDEX IF NOT EXISTS idx_manifest_layers_blob_digest ON manifest_layers (blob_digest)",
	}
	for _, ddl := range indexes {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating index: %w", err)
		}
	}

	return nil
}

// migrateV3 creates the deleted_manifests table for tombstone tracking.
func (db *DB) migrateV3(ctx context.Context) error {
	if _, err := db.bun.NewCreateTable().Model((*DeletedManifest)(nil)).IfNotExists().Exec(ctx); err != nil {
		return fmt.Errorf("creating deleted_manifests table: %w", err)
	}
	if _, err := db.bun.ExecContext(ctx,
		"CREATE INDEX IF NOT EXISTS idx_deleted_manifests_deleted_at ON deleted_manifests (deleted_at)"); err != nil {
		return fmt.Errorf("creating deleted_manifests index: %w", err)
	}
	return nil
}

// migrateV2 adds the alias column to follows and follow_requests.
// Note: follows table was removed in v5, so we skip if it doesn't exist.
func (db *DB) migrateV2(ctx context.Context) error {
	stmts := []string{
		"ALTER TABLE follows ADD COLUMN alias TEXT",
		"ALTER TABLE follow_requests ADD COLUMN alias TEXT",
	}
	for _, ddl := range stmts {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			errMsg := err.Error()
			if strings.Contains(errMsg, "duplicate column") || strings.Contains(errMsg, "already exists") ||
				strings.Contains(errMsg, "no such table") {
				continue
			}
			return fmt.Errorf("migrateV2: %w", err)
		}
	}
	return nil
}

// migrateV5 consolidates peers, follows, and outgoing_follows into a single actors table.
func (db *DB) migrateV5(ctx context.Context) error {
	// Create the actors table
	if _, err := db.bun.NewCreateTable().Model((*Actor)(nil)).IfNotExists().Exec(ctx); err != nil {
		return fmt.Errorf("creating actors table: %w", err)
	}

	// Create indexes for actors table
	actorIndexes := []string{
		"CREATE UNIQUE INDEX IF NOT EXISTS idx_actors_actor_url ON actors (actor_url)",
		"CREATE INDEX IF NOT EXISTS idx_actors_they_follow_us ON actors (they_follow_us)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_them ON actors (we_follow_them)",
		"CREATE INDEX IF NOT EXISTS idx_actors_is_healthy ON actors (is_healthy)",
		"CREATE INDEX IF NOT EXISTS idx_actors_we_follow_status ON actors (we_follow_status)",
		"CREATE INDEX IF NOT EXISTS idx_actors_created_at ON actors (created_at)",
	}
	for _, ddl := range actorIndexes {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("creating actors index: %w", err)
		}
	}

	// Migrate data from peers (discovery/health info) - only if table exists
	// This is the base layer with health and replication info
	if db.tableExists(ctx, "peers") {
		if _, err := db.bun.ExecContext(ctx, `
			INSERT INTO actors (actor_url, name, endpoint, is_healthy, replication_policy, last_seen_at, created_at)
			SELECT actor_url, name, endpoint, is_healthy, replication_policy, last_seen_at, created_at
			FROM peers
			ON CONFLICT(actor_url) DO UPDATE SET
				name = COALESCE(excluded.name, actors.name),
				endpoint = COALESCE(NULLIF(excluded.endpoint, ''), actors.endpoint),
				is_healthy = excluded.is_healthy,
				replication_policy = COALESCE(excluded.replication_policy, actors.replication_policy),
				last_seen_at = COALESCE(excluded.last_seen_at, actors.last_seen_at)
		`); err != nil {
			return fmt.Errorf("migrating peers to actors: %w", err)
		}
	}

	// Migrate data from follows (inbound: they follow us) - only if table exists
	// Merges with existing actor data, preserving peer info
	if db.tableExists(ctx, "follows") {
		if _, err := db.bun.ExecContext(ctx, `
			INSERT INTO actors (actor_url, endpoint, public_key_pem, alias, they_follow_us, they_follow_us_at)
			SELECT actor_url, endpoint, public_key_pem, alias, 1, approved_at
			FROM follows
			ON CONFLICT(actor_url) DO UPDATE SET
				endpoint = COALESCE(NULLIF(excluded.endpoint, ''), actors.endpoint),
				public_key_pem = COALESCE(excluded.public_key_pem, actors.public_key_pem),
				alias = COALESCE(excluded.alias, actors.alias),
				they_follow_us = 1,
				they_follow_us_at = COALESCE(excluded.they_follow_us_at, actors.they_follow_us_at)
		`); err != nil {
			return fmt.Errorf("migrating follows to actors: %w", err)
		}
	}

	// Migrate data from outgoing_follows (outbound: we follow them) - only if table exists
	// Merges with existing actor data, preserving peer and follow info
	if db.tableExists(ctx, "outgoing_follows") {
		if _, err := db.bun.ExecContext(ctx, `
			INSERT INTO actors (actor_url, endpoint, we_follow_them, we_follow_status, we_follow_accept_at)
			SELECT actor_url, '', 1, status, accepted_at
			FROM outgoing_follows
			ON CONFLICT(actor_url) DO UPDATE SET
				we_follow_them = 1,
				we_follow_status = COALESCE(excluded.we_follow_status, actors.we_follow_status),
				we_follow_accept_at = COALESCE(excluded.we_follow_accept_at, actors.we_follow_accept_at)
		`); err != nil {
			return fmt.Errorf("migrating outgoing_follows to actors: %w", err)
		}
	}

	// Drop old tables
	dropTables := []string{
		"DROP TABLE IF EXISTS peers",
		"DROP TABLE IF EXISTS follows",
		"DROP TABLE IF EXISTS outgoing_follows",
	}
	for _, ddl := range dropTables {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			return fmt.Errorf("dropping old table: %w", err)
		}
	}

	// Drop old indexes that reference dropped tables
	dropIndexes := []string{
		"DROP INDEX IF EXISTS idx_peers_healthy",
		"DROP INDEX IF EXISTS idx_follows_actor",
		"DROP INDEX IF EXISTS idx_outgoing_follows_status",
	}
	for _, ddl := range dropIndexes {
		if _, err := db.bun.ExecContext(ctx, ddl); err != nil {
			// Ignore errors for non-existent indexes
			continue
		}
	}

	return nil
}
