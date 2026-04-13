package federation

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

type mockFed struct {
	resolveFollowTargetFn func(ctx context.Context, input string) (string, error)
	fetchActorFn          func(ctx context.Context, actorURL string) (*activitypub.Actor, error)
	deliverActivityFn     func(ctx context.Context, inboxURL string, activityJSON []byte) error
	sendAcceptFn          func(ctx context.Context, followerActorURL string) error
	sendRejectFn          func(ctx context.Context, followerActorURL string) error
	sendUndoFn            func(ctx context.Context, peerActorURL string) error
	sendFollowFn          func(ctx context.Context, targetActorURL string) (string, error)
}

func (m *mockFed) ResolveFollowTarget(ctx context.Context, input string) (string, error) {
	if m.resolveFollowTargetFn != nil {
		return m.resolveFollowTargetFn(ctx, input)
	}
	return input, nil
}

func (m *mockFed) FetchActor(ctx context.Context, actorURL string) (*activitypub.Actor, error) {
	if m.fetchActorFn != nil {
		return m.fetchActorFn(ctx, actorURL)
	}
	return &activitypub.Actor{
		ID:    actorURL,
		Inbox: actorURL + "/inbox",
	}, nil
}

func (m *mockFed) DeliverActivity(ctx context.Context, inboxURL string, activityJSON []byte) error {
	if m.deliverActivityFn != nil {
		return m.deliverActivityFn(ctx, inboxURL, activityJSON)
	}
	return nil
}

func (m *mockFed) SendAccept(ctx context.Context, followerActorURL string) error {
	if m.sendAcceptFn != nil {
		return m.sendAcceptFn(ctx, followerActorURL)
	}
	return nil
}

func (m *mockFed) SendReject(ctx context.Context, followerActorURL string) error {
	if m.sendRejectFn != nil {
		return m.sendRejectFn(ctx, followerActorURL)
	}
	return nil
}

func (m *mockFed) SendUndo(ctx context.Context, peerActorURL string) error {
	if m.sendUndoFn != nil {
		return m.sendUndoFn(ctx, peerActorURL)
	}
	return nil
}

func (m *mockFed) SendFollow(ctx context.Context, targetActorURL string) (string, error) {
	if m.sendFollowFn != nil {
		return m.sendFollowFn(ctx, targetActorURL)
	}
	return targetActorURL, nil
}

func nopLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testService(t *testing.T, fed *mockFed) (*Service, *database.DB) {
	t.Helper()
	db := testDB(t)
	return testServiceWithDB(t, fed, db), db
}

func testServiceWithDB(t *testing.T, fed *mockFed, db *database.DB) *Service {
	t.Helper()
	return &Service{
		Fed:      fed,
		DB:       db,
		ActorURL: "https://local.test/ap/actor",
		Logger:   nopLogger(),
	}
}

const (
	peerActorURL = "https://peer.example.com/ap/actor"
	peerInboxURL = "https://peer.example.com/ap/inbox"
)

func peerActor() *activitypub.Actor {
	return &activitypub.Actor{
		ID:    peerActorURL,
		Inbox: peerInboxURL,
	}
}

func TestAddFollowSuccess(t *testing.T) {
	var deliveredInbox string
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return peerActor(), nil
		},
		deliverActivityFn: func(_ context.Context, inbox string, _ []byte) error {
			deliveredInbox = inbox
			return nil
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	result, err := svc.AddFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Equal(t, peerActorURL, result.ActorID)
	require.Equal(t, peerInboxURL, deliveredInbox)

	// Outgoing follow must be persisted.
	of, err := svc.DB.GetOutgoingFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.NotNil(t, of.WeFollowStatus)
	require.Equal(t, "pending", *of.WeFollowStatus)

	// Peer record must be created.
	peer, err := db.GetPeer(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, peer)
	require.Equal(t, "lazy", peer.ReplicationPolicy)
}

func TestAddFollowStoresOutgoingBeforeDelivery(t *testing.T) {
	// Verify the outgoing follow is stored before delivery is attempted, so
	// that an immediate Accept from the peer can be matched.
	db := testDB(t)
	var storedBeforeDelivery bool
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return peerActor(), nil
		},
		deliverActivityFn: func(_ context.Context, _ string, _ []byte) error {
			of, _ := db.GetOutgoingFollow(context.Background(), peerActorURL)
			storedBeforeDelivery = of != nil
			return nil
		},
	}
	svc := &Service{
		Fed:      fed,
		DB:       db,
		ActorURL: "https://local.test/ap/actor",
		Logger:   nopLogger(),
	}
	ctx := context.Background()

	_, err := svc.AddFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.True(t, storedBeforeDelivery, "outgoing follow must be stored before delivery")
}

func TestAddFollowResolveError(t *testing.T) {
	fed := &mockFed{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("resolve failed")
		},
	}
	svc, _ := testService(t, fed)

	_, err := svc.AddFollow(context.Background(), "bad-input")
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolving target")
}

func TestAddFollowFetchActorError(t *testing.T) {
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return nil, errors.New("unreachable")
		},
	}
	svc, _ := testService(t, fed)

	_, err := svc.AddFollow(context.Background(), peerActorURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "fetching actor")
}

func TestAddFollowDeliveryError(t *testing.T) {
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return peerActor(), nil
		},
		deliverActivityFn: func(_ context.Context, _ string, _ []byte) error {
			return errors.New("delivery failed")
		},
	}
	svc, _ := testService(t, fed)

	_, err := svc.AddFollow(context.Background(), peerActorURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "delivering follow")
}

func TestRemoveFollowWithInboundFollow(t *testing.T) {
	fed := &mockFed{}
	svc, db := testService(t, fed)
	ctx := context.Background()

	// Simulate an accepted inbound follow.
	require.NoError(t, db.AddFollow(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	actorURL, err := svc.RemoveFollow(ctx, peerActorURL, false)
	require.NoError(t, err)
	require.Equal(t, peerActorURL, actorURL)

	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, f, "inbound follow should be removed")
}

func TestRemoveFollowWithOutgoingFollow(t *testing.T) {
	fed := &mockFed{}
	svc, _ := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, svc.DB.AddOutgoingFollow(ctx, peerActorURL))

	actorURL, err := svc.RemoveFollow(ctx, peerActorURL, false)
	require.NoError(t, err)
	require.Equal(t, peerActorURL, actorURL)

	of, err := svc.DB.GetOutgoingFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, of, "outgoing follow should be removed")
}

func TestRemoveFollowBothTables(t *testing.T) {
	fed := &mockFed{}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollow(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))
	require.NoError(t, svc.DB.AddOutgoingFollow(ctx, peerActorURL))

	_, err := svc.RemoveFollow(ctx, peerActorURL, false)
	require.NoError(t, err)

	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, f)

	of, err := svc.DB.GetOutgoingFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, of)
}

func TestRemoveFollowNeitherTableReturnsError(t *testing.T) {
	fed := &mockFed{}
	svc, _ := testService(t, fed)

	_, err := svc.RemoveFollow(context.Background(), peerActorURL, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "removing follow")
}

func TestRemoveFollowUndoFailureDoesNotBlock(t *testing.T) {
	fed := &mockFed{
		sendUndoFn: func(_ context.Context, _ string) error {
			return errors.New("peer unreachable")
		},
	}
	svc, _ := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, svc.DB.AddOutgoingFollow(ctx, peerActorURL))

	actorURL, err := svc.RemoveFollow(ctx, peerActorURL, false)
	require.NoError(t, err, "Undo failure should not block local removal")
	require.Equal(t, peerActorURL, actorURL)
}

func TestRemoveFollowResolveError(t *testing.T) {
	fed := &mockFed{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("resolve failed")
		},
	}
	svc, _ := testService(t, fed)

	_, err := svc.RemoveFollow(context.Background(), "bad", false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolving target")
}

func TestRemoveFollowForceSkipsResolveError(t *testing.T) {
	fed := &mockFed{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("resolve failed")
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollow(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	// With force=true, a resolve error must not prevent local record removal.
	_, err := svc.RemoveFollow(ctx, peerActorURL, true)
	require.NoError(t, err)

	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, f, "follow should be removed despite resolve failure")
}

func TestRemoveFollowForceSkipsUndoError(t *testing.T) {
	fed := &mockFed{
		sendUndoFn: func(_ context.Context, _ string) error {
			return errors.New("peer unreachable")
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollow(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	// With force=true, an Undo delivery failure must not prevent local record removal.
	actorURL, err := svc.RemoveFollow(ctx, peerActorURL, true)
	require.NoError(t, err)
	require.Equal(t, peerActorURL, actorURL)

	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, f, "follow should be removed despite Undo failure")
}

func TestAcceptFollowSuccess(t *testing.T) {
	var acceptedActor string
	fed := &mockFed{
		sendAcceptFn: func(_ context.Context, actor string) error {
			acceptedActor = actor
			return nil
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	result, err := svc.AcceptFollow(ctx, peerActorURL, "")
	require.NoError(t, err)
	require.Equal(t, peerActorURL, result.ActorURL)
	require.False(t, result.FollowedBack)
	require.Equal(t, peerActorURL, acceptedActor)

	// Follow request should be promoted to accepted follow.
	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, f, "follow should exist after accept")

	// Follow request should be consumed.
	fr, err := svc.DB.GetFollowRequest(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "follow request should be consumed")
}

func TestAcceptFollowNoPendingRequest(t *testing.T) {
	fed := &mockFed{}
	svc, _ := testService(t, fed)

	_, err := svc.AcceptFollow(context.Background(), peerActorURL, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending follow request")
}

func TestAcceptFollowDeliveryError(t *testing.T) {
	fed := &mockFed{
		sendAcceptFn: func(_ context.Context, _ string) error {
			return errors.New("delivery failed")
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	_, err := svc.AcceptFollow(ctx, peerActorURL, "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "delivering accept")

	// Even on delivery failure, the follow should be promoted locally.
	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, f, "follow should be accepted locally even if delivery fails")
}

func TestAcceptFollowMutualFollowBack(t *testing.T) {
	var followTarget string
	fed := &mockFed{
		sendAcceptFn: func(_ context.Context, _ string) error { return nil },
		sendFollowFn: func(_ context.Context, target string) (string, error) {
			followTarget = target
			return target, nil
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	result, err := svc.AcceptFollow(ctx, peerActorURL, activitypub.AutoAcceptMutual)
	require.NoError(t, err)
	require.True(t, result.FollowedBack)
	require.Equal(t, peerActorURL, followTarget)

	// Outgoing follow should be recorded.
	of, err := svc.DB.GetOutgoingFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, of, "outgoing follow should be recorded after follow-back")

	// Peer should be recorded.
	peer, err := db.GetPeer(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, peer)
}

func TestAcceptFollowMutualSkipsExistingOutgoing(t *testing.T) {
	followCalled := false
	fed := &mockFed{
		sendAcceptFn: func(_ context.Context, _ string) error { return nil },
		sendFollowFn: func(_ context.Context, _ string) (string, error) {
			followCalled = true
			return "", nil
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))
	require.NoError(t, svc.DB.AddOutgoingFollow(ctx, peerActorURL))

	result, err := svc.AcceptFollow(ctx, peerActorURL, activitypub.AutoAcceptMutual)
	require.NoError(t, err)
	require.False(t, result.FollowedBack, "should not follow back when already following")
	require.False(t, followCalled, "SendFollow should not be called when outgoing follow exists")
}

func TestAcceptFollowMutualFollowBackError(t *testing.T) {
	fed := &mockFed{
		sendAcceptFn: func(_ context.Context, _ string) error { return nil },
		sendFollowFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("follow-back failed")
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	result, err := svc.AcceptFollow(ctx, peerActorURL, activitypub.AutoAcceptMutual)
	require.NoError(t, err, "follow-back failure should not fail the accept")
	require.False(t, result.FollowedBack)
	require.Equal(t, peerActorURL, result.ActorURL)
}

func TestAcceptFollowResolveError(t *testing.T) {
	fed := &mockFed{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("resolve failed")
		},
	}
	svc, _ := testService(t, fed)

	_, err := svc.AcceptFollow(context.Background(), "bad", "")
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolving target")
}

func TestRejectFollowSuccess(t *testing.T) {
	var rejectedActor string
	fed := &mockFed{
		sendRejectFn: func(_ context.Context, actor string) error {
			rejectedActor = actor
			return nil
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	actorURL, err := svc.RejectFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.Equal(t, peerActorURL, actorURL)
	require.Equal(t, peerActorURL, rejectedActor)

	// Follow request should be removed.
	fr, err := svc.DB.GetFollowRequest(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "follow request should be removed after reject")
}

func TestRejectFollowNoPendingRequest(t *testing.T) {
	fed := &mockFed{}
	svc, _ := testService(t, fed)

	_, err := svc.RejectFollow(context.Background(), peerActorURL)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no pending follow request")
}

func TestRejectFollowDeliveryErrorDoesNotFail(t *testing.T) {
	fed := &mockFed{
		sendRejectFn: func(_ context.Context, _ string) error {
			return errors.New("delivery failed")
		},
	}
	svc, db := testService(t, fed)
	ctx := context.Background()

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "pubkey", "https://peer.example.com", nil))

	actorURL, err := svc.RejectFollow(ctx, peerActorURL)
	require.NoError(t, err, "delivery failure should not fail the reject")
	require.Equal(t, peerActorURL, actorURL)

	// Should still be rejected locally.
	fr, err := svc.DB.GetFollowRequest(ctx, peerActorURL)
	require.NoError(t, err)
	require.Nil(t, fr, "follow request should be rejected locally even if delivery fails")
}

func TestRejectFollowResolveError(t *testing.T) {
	fed := &mockFed{
		resolveFollowTargetFn: func(_ context.Context, _ string) (string, error) {
			return "", errors.New("resolve failed")
		},
	}
	svc, _ := testService(t, fed)

	_, err := svc.RejectFollow(context.Background(), "bad")
	require.Error(t, err)
	require.Contains(t, err.Error(), "resolving target")
}

func TestRefreshActorsUpdatesFollows(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return &activitypub.Actor{
				ID:           peerActorURL,
				Name:         "Peer Node",
				OCINamespace: "example.com", // split domain: account domain differs from registry domain
				PublicKey:    activitypub.ActorPublicKey{PublicKeyPEM: "new-key"},
			}, nil
		},
	}
	svc := testServiceWithDB(t, fed, db)

	require.NoError(t, db.AddFollow(ctx, peerActorURL, "old-key", "https://old.example.com", nil))
	svc.RefreshActors(ctx)

	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, f.PublicKeyPEM)
	require.Equal(t, "new-key", *f.PublicKeyPEM)
	require.NotNil(t, f.Alias)
	require.Equal(t, "example.com", *f.Alias)
}

func TestRefreshActorsUpdatesFollowRequests(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return &activitypub.Actor{
				ID:           peerActorURL,
				Name:         "Peer Node",
				OCINamespace: "example.com",
				PublicKey:    activitypub.ActorPublicKey{PublicKeyPEM: "new-key"},
			}, nil
		},
	}
	svc := testServiceWithDB(t, fed, db)

	require.NoError(t, db.AddFollowRequest(ctx, peerActorURL, "old-key", "https://old.example.com", nil))
	svc.RefreshActors(ctx)

	fr, err := db.GetFollowRequest(ctx, peerActorURL)
	require.NoError(t, err)
	require.Equal(t, "new-key", fr.PublicKeyPEM)
	require.NotNil(t, fr.Alias)
	require.Equal(t, "example.com", *fr.Alias)
}

func TestRefreshActorsSkipsOnFetchError(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return nil, errors.New("unreachable")
		},
	}
	svc := testServiceWithDB(t, fed, db)

	require.NoError(t, db.AddFollow(ctx, peerActorURL, "original-key", "https://peer.example.com", nil))
	svc.RefreshActors(ctx)

	// Original data must be untouched.
	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, f.PublicKeyPEM)
	require.Equal(t, "original-key", *f.PublicKeyPEM)
}

func TestRefreshActorsFallsBackToHostname(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	fed := &mockFed{
		fetchActorFn: func(_ context.Context, _ string) (*activitypub.Actor, error) {
			return &activitypub.Actor{
				ID:        peerActorURL,
				PublicKey: activitypub.ActorPublicKey{PublicKeyPEM: "new-key"},
				// No OCINamespace set, so alias falls back to actor URL hostname
			}, nil
		},
	}
	svc := testServiceWithDB(t, fed, db)

	require.NoError(t, db.AddFollow(ctx, peerActorURL, "old-key", "https://old.example.com", nil))
	svc.RefreshActors(ctx)

	f, err := db.GetFollow(ctx, peerActorURL)
	require.NoError(t, err)
	require.NotNil(t, f.PublicKeyPEM)
	require.Equal(t, "new-key", *f.PublicKeyPEM)
	require.NotNil(t, f.Alias)
	require.Equal(t, "peer.example.com", *f.Alias) // falls back to actor URL hostname
}
