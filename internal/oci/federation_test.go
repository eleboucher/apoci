package oci_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/oci"
)

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testRegistryWithFederation(t *testing.T) (*oci.Registry, *database.DB) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	blobs, err := blobstore.New(dir, nopLog())
	require.NoError(t, err)

	identity, err := activitypub.LoadOrCreateIdentity("test.example.com", "", "", nopLog())
	require.NoError(t, err)

	reg := oci.NewRegistry(db, blobs, identity.ActorURL, "", "", config.DefaultMaxManifestSize, config.DefaultMaxBlobSize, nopLog())
	pub := activitypub.NewAPPublisher(context.Background(), identity, db, "https://test.example.com", nopLog())
	reg.SetPublisher(pub)

	return reg, db
}

func TestPushManifestCreatesAPActivity(t *testing.T) {
	reg, db := testRegistryWithFederation(t)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:cfg"},"layers":[]}`)
	mediaType := "application/vnd.oci.image.manifest.v1+json"

	_, err := reg.PushManifest(ctx, "test.example.com/test/federated", "v1.0", manifest, mediaType)
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, "https://test.example.com/ap/actor", 0, 10)
	require.NoError(t, err)

	var createCount, updateCount int
	for _, a := range activities {
		switch a.Type {
		case "Create":
			createCount++
			var activity map[string]any
			require.NoError(t, json.Unmarshal(a.ObjectJSON, &activity))
			obj, _ := activity["object"].(map[string]any)
			require.Equal(t, "test.example.com/test/federated", obj["ociRepository"])
		case "Update":
			updateCount++
			var activity map[string]any
			require.NoError(t, json.Unmarshal(a.ObjectJSON, &activity))
			obj, _ := activity["object"].(map[string]any)
			require.Equal(t, "v1.0", obj["ociTag"])
		}
	}

	require.GreaterOrEqual(t, createCount, 1, "expected at least 1 Create activity")
	require.GreaterOrEqual(t, updateCount, 1, "expected at least 1 Update activity")
}

func TestPushWithoutTagDoesNotCreateUpdateActivity(t *testing.T) {
	reg, db := testRegistryWithFederation(t)
	ctx := context.Background()

	manifest := []byte(`{"schemaVersion":2}`)
	_, err := reg.PushManifest(ctx, "test.example.com/test/notag", "", manifest, "application/vnd.oci.image.manifest.v1+json")
	require.NoError(t, err)

	activities, err := db.ListActivitiesPage(ctx, "https://test.example.com/ap/actor", 0, 10)
	require.NoError(t, err)

	for _, a := range activities {
		if a.Type == "Update" {
			var activity map[string]any
			require.NoError(t, json.Unmarshal(a.ObjectJSON, &activity))
			obj, _ := activity["object"].(map[string]any)
			require.NotEqual(t, "OCITag", obj["type"], "expected no Update/OCITag activity for tagless push")
		}
	}
}
