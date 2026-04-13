package peering

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/notify"
)

// dbHealthRepo adapts *database.DB to HealthRepository for tests.
type dbHealthRepo struct {
	db *database.DB
}

func (r *dbHealthRepo) ListAllPeers(ctx context.Context) ([]PeerRecord, error) {
	peers, err := r.db.ListAllPeers(ctx)
	if err != nil {
		return nil, err
	}
	records := make([]PeerRecord, len(peers))
	for i, p := range peers {
		records[i] = PeerRecord{
			ActorURL:  p.ActorURL,
			Endpoint:  p.Endpoint,
			IsHealthy: p.IsHealthy,
		}
	}
	return records, nil
}

func (r *dbHealthRepo) SetPeerHealth(ctx context.Context, actorURL string, healthy bool) error {
	return r.db.SetPeerHealth(ctx, actorURL, healthy)
}

func TestHealthCheckerStartStop(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	// Set up a mock peer endpoint so checkAll has something to hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// Insert a peer so checkAll exercises the full path
	ctx := context.Background()
	now := time.Now()
	name := "test-peer"
	require.NoError(t, db.UpsertActor(ctx, &database.Actor{
		ActorURL:          "https://test.example.com/ap/actor",
		Name:              &name,
		Endpoint:          srv.URL,
		ReplicationPolicy: "lazy",
		LastSeenAt:        &now,
		IsHealthy:         false,
	}))

	fetcher := NewFetcher(5*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	hc := NewHealthChecker(&dbHealthRepo{db: db}, fetcher, 100*time.Millisecond, notify.New("test", nil, nil, nopLog()), nopLog())

	hc.Start(ctx)

	// Wait long enough for at least one check cycle (immediate + one tick)
	time.Sleep(250 * time.Millisecond)

	hc.Stop()

	// Verify the peer was marked healthy after the check
	peer, err := db.GetPeer(ctx, "https://test.example.com/ap/actor")
	require.NoError(t, err)
	require.NotNil(t, peer, "expected peer to exist")
	require.True(t, peer.IsHealthy, "expected peer to be marked healthy after successful check")
}

func TestHealthCheckerDoubleStartIsNoop(t *testing.T) {
	dir := t.TempDir()
	db, err := database.OpenSQLite(dir, 0, 0, nopLog())
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	fetcher := NewFetcher(5*time.Second, config.DefaultMaxBlobSize, config.DefaultMaxManifestSize, nopLog())
	hc := NewHealthChecker(&dbHealthRepo{db: db}, fetcher, 100*time.Millisecond, notify.New("test", nil, nil, nopLog()), nopLog())

	ctx := context.Background()
	hc.Start(ctx)
	hc.Start(ctx) // should not panic or start a second goroutine
	hc.Stop()
}
