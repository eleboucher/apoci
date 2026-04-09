package activitypub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

func testWorkerSetup(t *testing.T) (*InboxWorker, *InboxHandler, *database.DB) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	identity, err := LoadOrCreateIdentity("https://bob.test", "bob.test", "", "", discardLogger())
	require.NoError(t, err)

	handler := NewInboxHandler(identity, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      "none",
	}, discardLogger())
	t.Cleanup(handler.Stop)

	worker := NewInboxWorker(handler, discardLogger())
	handler.SetWorker(worker)

	return worker, handler, db
}

func TestInboxWorkerProcessesTask(t *testing.T) {
	worker, handler, db := testWorkerSetup(t)
	ctx := context.Background()

	bob, err := LoadOrCreateIdentity("https://bob.test", "bob.test", "", "", discardLogger())
	require.NoError(t, err)

	alice, err := LoadOrCreateIdentity("https://alice.test", "alice.test", "", "", discardLogger())
	require.NoError(t, err)

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test", nil))
	handler.SetNamespaceForActor(alice.ActorURL, "alice.test")

	worker.Start(ctx)
	defer worker.Stop()

	task := InboxTask{
		Activity: RawActivity{
			ID:     alice.ActorURL + "#follow-1",
			Type:   ActivityFollow,
			Actor:  alice.ActorURL,
			Object: bob.ActorURL,
		},
		PubKeyPEM: alicePEM,
	}
	require.True(t, worker.Enqueue(task))

	require.Eventually(t, func() bool {
		fr, _ := db.GetFollowRequest(ctx, alice.ActorURL)
		return fr != nil
	}, 2*time.Second, 10*time.Millisecond, "worker should process the follow request")
}

func TestInboxWorkerDrainsOnStop(t *testing.T) {
	worker, handler, db := testWorkerSetup(t)

	bob, err := LoadOrCreateIdentity("https://bob.test", "bob.test", "", "", discardLogger())
	require.NoError(t, err)

	alice, err := LoadOrCreateIdentity("https://alice.test", "alice.test", "", "", discardLogger())
	require.NoError(t, err)

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(context.Background(), alice.ActorURL, alicePEM, "https://alice.test", nil))
	handler.SetNamespaceForActor(alice.ActorURL, "alice.test")

	// Enqueue a task before starting so it's processed during drain.
	worker.Enqueue(InboxTask{
		Activity: RawActivity{
			ID:     alice.ActorURL + "#follow-drain",
			Type:   ActivityFollow,
			Actor:  alice.ActorURL,
			Object: bob.ActorURL,
		},
		PubKeyPEM: alicePEM,
	})

	ctx, cancel := context.WithCancel(context.Background())
	worker.Start(ctx)
	cancel()
	worker.Stop()

	// The task should have been drained and processed.
	fr, err := db.GetFollowRequest(context.Background(), alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, fr, "drained task should have been processed")
}

func TestInboxWorkerEnqueueFullQueue(t *testing.T) {
	worker, _, _ := testWorkerSetup(t)

	// Fill the queue without starting consumers.
	var enqueued int
	for range inboxQueueSize + 10 {
		if worker.Enqueue(InboxTask{}) {
			enqueued++
		}
	}

	assert.Equal(t, inboxQueueSize, enqueued, "should enqueue exactly queue capacity")
}

func TestInboxWorkerConcurrency(t *testing.T) {
	worker, _, _ := testWorkerSetup(t)

	ctx := context.Background()
	worker.Start(ctx)
	defer worker.Stop()

	for range 100 {
		worker.Enqueue(InboxTask{
			Activity: RawActivity{
				ID:   "test-concurrent",
				Type: "Unknown",
			},
		})
	}

	require.Eventually(t, func() bool {
		return worker.queue.Len() == 0
	}, 2*time.Second, 10*time.Millisecond)
}
