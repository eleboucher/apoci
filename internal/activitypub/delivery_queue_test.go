package activitypub

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func openTestDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, discardLogger())
	require.NoError(t, err, "opening test database")
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testIdentity(t *testing.T) *Identity {
	t.Helper()
	id, err := LoadOrCreateIdentity("test.example.com", "", "", discardLogger())
	require.NoError(t, err, "creating test identity")
	return id
}

func TestDeliveryQueueStartStop(t *testing.T) {
	db := openTestDB(t)
	identity := testIdentity(t)

	q := NewDeliveryQueue(db, identity, discardLogger())

	ctx := t.Context()

	q.Start(ctx)

	// Stopping should return without hanging.
	done := make(chan struct{})
	go func() {
		q.Stop()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return within 5 seconds")
	}
}

func TestDeliveryQueueProcessesPendingDeliveries(t *testing.T) {
	db := openTestDB(t)
	identity := testIdentity(t)

	var received atomic.Int32
	var receivedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedBody = body
		received.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	activity := map[string]string{
		"type":   "Create",
		"object": "https://test.example.com/image:latest",
	}
	activityJSON, err := json.Marshal(activity)
	require.NoError(t, err, "marshaling activity")

	ctx := context.Background()
	require.NoError(t, db.EnqueueDelivery(ctx, "activity-1", srv.URL+"/inbox", activityJSON), "enqueuing delivery")

	// Directly invoke processBatch to avoid waiting for the ticker.
	q := NewDeliveryQueue(db, identity, discardLogger())
	q.processBatch(ctx)

	require.Equal(t, int32(1), received.Load())

	// Verify the body arrived intact.
	var got map[string]string
	require.NoError(t, json.Unmarshal(receivedBody, &got), "unmarshaling received body")
	assert.Equal(t, ActivityCreate, got["type"])

	// Verify it's marked as delivered (no longer pending).
	pending, err := db.PendingDeliveries(ctx, 100)
	require.NoError(t, err, "querying pending deliveries")
	assert.Len(t, pending, 0, "expected 0 pending deliveries after success")
}

func TestDeliveryQueueRetriesOnFailure(t *testing.T) {
	db := openTestDB(t)
	identity := testIdentity(t)

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx := context.Background()
	activityJSON := []byte(`{"type":"Create"}`)

	require.NoError(t, db.EnqueueDelivery(ctx, "activity-fail", srv.URL+"/inbox", activityJSON), "enqueuing delivery")

	q := NewDeliveryQueue(db, identity, discardLogger())

	// First attempt.
	q.processBatch(ctx)

	require.Equal(t, int32(1), attempts.Load())

	// After first failure the delivery should still exist but with incremented
	// attempts and a future next_attempt_at (so it won't appear in pending yet).
	rows, err := db.QueryContext(ctx,
		`SELECT attempts, status, next_attempt_at FROM delivery_queue WHERE activity_id = ?`,
		"activity-fail")
	require.NoError(t, err, "querying delivery")
	defer func() { _ = rows.Close() }()

	require.True(t, rows.Next(), "delivery row not found")
	var dbAttempts int
	var status string
	var nextAttemptAt time.Time
	require.NoError(t, rows.Scan(&dbAttempts, &status, &nextAttemptAt), "scanning delivery")
	_ = rows.Close()

	assert.Equal(t, 1, dbAttempts)
	assert.Equal(t, "pending", status)
	// The next attempt should be in the future (at least 1 second of backoff).
	assert.True(t, nextAttemptAt.After(time.Now().Add(-1*time.Second)), "expected next_attempt_at to be in the future, got %v", nextAttemptAt)

	// A second processBatch should not pick it up because next_attempt_at is in the future.
	q.processBatch(ctx)
	assert.Equal(t, int32(1), attempts.Load(), "expected delivery not to be retried yet")
}

func TestDeliveryQueueCleanup(t *testing.T) {
	db := openTestDB(t)
	identity := testIdentity(t)

	ctx := context.Background()

	// Enqueue and immediately mark as delivered.
	require.NoError(t, db.EnqueueDelivery(ctx, "old-activity", "https://peer.example.com/inbox", []byte(`{}`)), "enqueuing delivery")
	require.NoError(t, db.MarkDelivered(ctx, 1), "marking delivered")

	// Backdate the created_at to 8 days ago via a raw connection to the same DB file.
	rawConn := openRawConn(t, db)
	eightDaysAgo := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.DateTime)
	_, err := rawConn.ExecContext(ctx,
		`UPDATE delivery_queue SET created_at = ? WHERE activity_id = ?`,
		eightDaysAgo, "old-activity")
	require.NoError(t, err, "backdating created_at")

	// Also enqueue a fresh delivered entry that should NOT be cleaned up.
	require.NoError(t, db.EnqueueDelivery(ctx, "fresh-activity", "https://peer.example.com/inbox", []byte(`{}`)), "enqueuing fresh delivery")
	require.NoError(t, db.MarkDelivered(ctx, 2), "marking fresh delivered")

	q := NewDeliveryQueue(db, identity, discardLogger())
	q.cleanup(ctx)

	// The old delivery should be gone, the fresh one should remain.
	rows, err := db.QueryContext(ctx,
		`SELECT activity_id FROM delivery_queue ORDER BY id`)
	require.NoError(t, err, "querying deliveries")
	defer func() { _ = rows.Close() }()

	var remaining []string
	for rows.Next() {
		var actID string
		require.NoError(t, rows.Scan(&actID), "scanning")
		remaining = append(remaining, actID)
	}
	require.NoError(t, rows.Err(), "iterating rows")

	require.Len(t, remaining, 1)
	assert.Equal(t, "fresh-activity", remaining[0])
}

// openRawConn opens a second raw SQL connection to the same database file
// used by the given *database.DB. This allows test code to execute arbitrary
// SQL (e.g. backdating timestamps) that the database package doesn't expose.
func openRawConn(t *testing.T, db *database.DB) *sql.DB {
	t.Helper()

	// Discover the DB file path by querying PRAGMA database_list.
	rows, err := db.QueryContext(context.Background(), "PRAGMA database_list")
	require.NoError(t, err, "querying database_list")
	defer func() { _ = rows.Close() }()

	var dbPath string
	for rows.Next() {
		var seq int
		var name, file string
		require.NoError(t, rows.Scan(&seq, &name, &file), "scanning database_list")
		if name == "main" {
			dbPath = file
			break
		}
	}
	require.NotEmpty(t, dbPath, "could not determine database file path")

	conn, err := sql.Open("sqlite3", "file:"+dbPath+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err, "opening raw connection")
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}
