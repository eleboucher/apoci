package database

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOutgoingFollowLifecycle(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	url := "https://peer.example.com/ap/actor"

	// Add
	require.NoError(t, db.AddOutgoingFollow(ctx, url))

	// Get (pending)
	f, err := db.GetOutgoingFollow(ctx, url)
	require.NoError(t, err)
	require.NotNil(t, f, "expected outgoing follow, got nil")
	require.Equal(t, "pending", f.Status)
	require.Nil(t, f.AcceptedAt, "expected accepted_at to be nil for pending follow")

	// Accept
	require.NoError(t, db.AcceptOutgoingFollow(ctx, url))

	// Get (accepted)
	f, err = db.GetOutgoingFollow(ctx, url)
	require.NoError(t, err)
	require.NotNil(t, f, "expected outgoing follow after accept, got nil")
	require.Equal(t, "accepted", f.Status)
	require.NotNil(t, f.AcceptedAt, "expected accepted_at to be set")

	// Remove
	require.NoError(t, db.RemoveOutgoingFollow(ctx, url))
	f, err = db.GetOutgoingFollow(ctx, url)
	require.NoError(t, err)
	require.Nil(t, f, "expected nil after remove")
}

func TestOutgoingFollowReject(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	url := "https://reject.example.com/ap/actor"

	require.NoError(t, db.AddOutgoingFollow(ctx, url))
	require.NoError(t, db.RejectOutgoingFollow(ctx, url))

	f, err := db.GetOutgoingFollow(ctx, url)
	require.NoError(t, err)
	require.NotNil(t, f, "expected outgoing follow after reject, got nil")
	require.Equal(t, "rejected", f.Status)
}

func TestOutgoingFollowListByStatus(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	urls := []string{
		"https://a.example.com/ap/actor",
		"https://b.example.com/ap/actor",
		"https://c.example.com/ap/actor",
	}

	for _, u := range urls {
		require.NoError(t, db.AddOutgoingFollow(ctx, u))
	}

	// Accept one
	require.NoError(t, db.AcceptOutgoingFollow(ctx, urls[0]))

	// List pending (should be 2)
	pending, err := db.ListOutgoingFollows(ctx, "pending")
	require.NoError(t, err)
	require.Len(t, pending, 2)

	// List accepted (should be 1)
	accepted, err := db.ListOutgoingFollows(ctx, "accepted")
	require.NoError(t, err)
	require.Len(t, accepted, 1)
	require.Equal(t, urls[0], accepted[0].ActorURL)
}

func TestOutgoingFollowDuplicateAdd(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	url := "https://dup.example.com/ap/actor"

	require.NoError(t, db.AddOutgoingFollow(ctx, url))

	// Adding the same URL again should not error (ON CONFLICT).
	require.NoError(t, db.AddOutgoingFollow(ctx, url), "expected no error on duplicate add")

	// Should still be exactly one row.
	pending, err := db.ListOutgoingFollows(ctx, "pending")
	require.NoError(t, err)
	require.Len(t, pending, 1)
}

func TestOutgoingFollowDuplicateAddPreservesAccepted(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	url := "https://accepted-dup.example.com/ap/actor"

	require.NoError(t, db.AddOutgoingFollow(ctx, url))
	require.NoError(t, db.AcceptOutgoingFollow(ctx, url))

	// Adding again after acceptance must NOT reset status to pending.
	require.NoError(t, db.AddOutgoingFollow(ctx, url))

	f, err := db.GetOutgoingFollow(ctx, url)
	require.NoError(t, err)
	require.NotNil(t, f)
	require.Equal(t, "accepted", f.Status,
		"AddOutgoingFollow must not reset an already-accepted follow back to pending")
}

func TestRemoveOutgoingFollowNotFound(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	err := db.RemoveOutgoingFollow(ctx, "https://nonexistent.example.com/ap/actor")
	require.Error(t, err, "removing a non-existent outgoing follow should return an error")
}

func TestOutgoingFollowRetryAfterRejection(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	url := "https://retry-rejected.example.com/ap/actor"

	// Add and reject
	require.NoError(t, db.AddOutgoingFollow(ctx, url))
	require.NoError(t, db.RejectOutgoingFollow(ctx, url))

	f, err := db.GetOutgoingFollow(ctx, url)
	require.NoError(t, err)
	require.Equal(t, "rejected", f.Status)

	// Retry: adding again should reset to pending
	require.NoError(t, db.AddOutgoingFollow(ctx, url))

	f, err = db.GetOutgoingFollow(ctx, url)
	require.NoError(t, err)
	require.Equal(t, "pending", f.Status,
		"AddOutgoingFollow should reset rejected follows back to pending")
}

func TestListAllOutgoingFollows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	urls := []string{
		"https://all-a.example.com/ap/actor",
		"https://all-b.example.com/ap/actor",
		"https://all-c.example.com/ap/actor",
	}

	for _, u := range urls {
		require.NoError(t, db.AddOutgoingFollow(ctx, u))
	}

	// Accept one, reject another
	require.NoError(t, db.AcceptOutgoingFollow(ctx, urls[0]))
	require.NoError(t, db.RejectOutgoingFollow(ctx, urls[1]))

	// ListAllOutgoingFollows should return all 3
	all, err := db.ListAllOutgoingFollows(ctx)
	require.NoError(t, err)
	require.Len(t, all, 3)

	// Verify we have all three statuses
	statuses := make(map[string]int)
	for _, f := range all {
		statuses[f.Status]++
	}
	require.Equal(t, 1, statuses["accepted"])
	require.Equal(t, 1, statuses["rejected"])
	require.Equal(t, 1, statuses["pending"])
}

func TestDeleteStaleOutgoingFollows(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	// Create follows with different statuses
	pendingURL := "https://stale-pending.example.com/ap/actor"
	rejectedURL := "https://stale-rejected.example.com/ap/actor"
	acceptedURL := "https://stale-accepted.example.com/ap/actor"

	require.NoError(t, db.AddOutgoingFollow(ctx, pendingURL))
	require.NoError(t, db.AddOutgoingFollow(ctx, rejectedURL))
	require.NoError(t, db.AddOutgoingFollow(ctx, acceptedURL))

	require.NoError(t, db.RejectOutgoingFollow(ctx, rejectedURL))
	require.NoError(t, db.AcceptOutgoingFollow(ctx, acceptedURL))

	// Backdate the created_at timestamps to simulate old records
	_, err := db.bun.NewRaw(
		`UPDATE outgoing_follows SET created_at = ? WHERE actor_url IN (?, ?, ?)`,
		time.Now().Add(-48*time.Hour), pendingURL, rejectedURL, acceptedURL).Exec(ctx)
	require.NoError(t, err)

	// Delete with TTLs: pending 7 days, rejected 24 hours
	// Our records are 48h old, so rejected should be deleted but not pending
	n, err := db.DeleteStaleOutgoingFollows(ctx, 7*24*time.Hour, 24*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "should delete only the rejected follow")

	// Verify rejected is gone
	f, err := db.GetOutgoingFollow(ctx, rejectedURL)
	require.NoError(t, err)
	require.Nil(t, f, "rejected follow should be deleted")

	// Verify pending and accepted remain
	f, err = db.GetOutgoingFollow(ctx, pendingURL)
	require.NoError(t, err)
	require.NotNil(t, f, "pending follow should remain (not old enough)")

	f, err = db.GetOutgoingFollow(ctx, acceptedURL)
	require.NoError(t, err)
	require.NotNil(t, f, "accepted follow should never be deleted")
}

func TestDeleteStaleOutgoingFollowsKeepsAccepted(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	acceptedURL := "https://old-accepted.example.com/ap/actor"
	require.NoError(t, db.AddOutgoingFollow(ctx, acceptedURL))
	require.NoError(t, db.AcceptOutgoingFollow(ctx, acceptedURL))

	// Backdate to very old
	_, err := db.bun.NewRaw(
		`UPDATE outgoing_follows SET created_at = ? WHERE actor_url = ?`,
		time.Now().Add(-365*24*time.Hour), acceptedURL).Exec(ctx)
	require.NoError(t, err)

	// Delete with short TTLs
	n, err := db.DeleteStaleOutgoingFollows(ctx, 1*time.Hour, 1*time.Hour)
	require.NoError(t, err)
	require.Equal(t, int64(0), n, "accepted follows should never be deleted regardless of age")

	f, err := db.GetOutgoingFollow(ctx, acceptedURL)
	require.NoError(t, err)
	require.NotNil(t, f)
	require.Equal(t, "accepted", f.Status)
}
