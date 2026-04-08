package activitypub

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func TestPublishManifestCreatesActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(context.Background(), id, db, "https://test.example.com", discardLogger())

	ctx := context.Background()
	manifest := []byte(`{"schemaVersion":2}`)
	err = pub.PublishManifest(ctx, "test/repo", "sha256:abc123", "application/vnd.oci.image.manifest.v1+json", int64(len(manifest)), manifest, nil)
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Create", activities[0].Type)

	var activity map[string]any
	require.NoError(t, json.Unmarshal(activities[0].ObjectJSON, &activity))
	require.Equal(t, "Create", activity["type"])

	obj, ok := activity["object"].(map[string]any)
	require.True(t, ok, "expected object to be a map")
	require.Equal(t, "test/repo", obj["ociRepository"])
	require.Equal(t, "sha256:abc123", obj["ociDigest"])
}

func TestPublishTagCreatesUpdateActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(context.Background(), id, db, "https://test.example.com", discardLogger())

	ctx := context.Background()
	err = pub.PublishTag(ctx, "test/repo", "v1.0", "sha256:abc123")
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Update", activities[0].Type)
}

func TestPublishBlobRefCreatesAnnounceActivity(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	id, _ := LoadOrCreateIdentity("https://test.example.com", "test.example.com", "", "", discardLogger())
	pub := NewAPPublisher(context.Background(), id, db, "https://test.example.com", discardLogger())

	ctx := context.Background()
	err = pub.PublishBlobRef(ctx, "sha256:blob123", 4096)
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, id.ActorURL, 0, 10)
	require.NoError(t, err)
	require.Len(t, activities, 1)
	require.Equal(t, "Announce", activities[0].Type)
}
