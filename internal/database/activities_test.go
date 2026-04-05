package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

const testAliceActor = "https://alice.example.com/ap/actor"

func TestActivityCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	err := db.PutActivity(ctx, "https://example.com/ap/activities/1", "Create", testAliceActor, []byte(`{"type":"Create"}`))
	require.NoError(t, err)

	got, err := db.GetActivity(ctx, "https://example.com/ap/activities/1")
	require.NoError(t, err)
	require.NotNil(t, got, "expected activity")
	require.Equal(t, "Create", got.Type)
	require.Equal(t, testAliceActor, got.ActorURL)

	// Duplicate insert is a no-op
	err = db.PutActivity(ctx, "https://example.com/ap/activities/1", "Update", testAliceActor, []byte(`{"type":"Update"}`))
	require.NoError(t, err)
	got, _ = db.GetActivity(ctx, "https://example.com/ap/activities/1")
	require.Equal(t, "Create", got.Type, "expected original Create to be preserved")

	// Get nonexistent
	missing, err := db.GetActivity(ctx, "https://example.com/ap/activities/nonexistent")
	require.NoError(t, err)
	require.Nil(t, missing, "expected nil for nonexistent activity")
}
