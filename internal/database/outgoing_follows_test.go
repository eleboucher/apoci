package database

import (
	"context"
	"testing"

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
