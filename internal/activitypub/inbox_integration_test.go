package activitypub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/apoci/apoci/internal/config"
	"github.com/apoci/apoci/internal/database"
)

// signedInboxPost creates and signs a POST request to /ap/inbox.
func signedInboxPost(t *testing.T, sender *Identity, activity any) *http.Request {
	t.Helper()
	body, err := json.Marshal(activity)
	require.NoError(t, err)
	req := httptest.NewRequest("POST", "/ap/inbox", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/activity+json")
	require.NoError(t, SignRequest(req, sender.KeyID(), sender.PrivateKey, body))
	return req
}

// setupInboxTest creates two identities (alice and bob), an inbox handler for bob,
// and an HTTP server that serves alice's actor document (so bob can fetch the public key).
func setupInboxTest(t *testing.T) (alice *Identity, bob *Identity, inbox *InboxHandler, db *database.DB) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, discardLogger())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	alice, err = LoadOrCreateIdentity("alice.test", "", "", discardLogger())
	require.NoError(t, err)

	bob, err = LoadOrCreateIdentity("bob.test", "", "", discardLogger())
	require.NoError(t, err)

	inbox = NewInboxHandler(bob, db, InboxConfig{
		MaxManifestSize: config.DefaultMaxManifestSize,
		MaxBlobSize:     config.DefaultMaxBlobSize,
		AutoAccept:      "none",
	}, discardLogger())

	// Start an HTTP server that serves alice's actor document
	alicePEM, _ := alice.PublicKeyPEM()
	aliceActor := Actor{
		Context: []any{"https://www.w3.org/ns/activitystreams", "https://w3id.org/security/v1"},
		Type:    "Person",
		ID:      alice.ActorURL,
		Inbox:   "https://alice.test/ap/inbox",
		Outbox:  "https://alice.test/ap/outbox",
		PublicKey: ActorPublicKey{
			ID:           alice.KeyID(),
			Owner:        alice.ActorURL,
			PublicKeyPEM: alicePEM,
		},
	}

	actorSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/activity+json")
		_ = json.NewEncoder(w).Encode(aliceActor)
	}))
	t.Cleanup(actorSrv.Close)

	// Override alice's actor URL to point to the test server
	alice.ActorURL = actorSrv.URL + "/ap/actor"
	alice.Domain = "alice.test"
	aliceActor.ID = alice.ActorURL
	aliceActor.PublicKey.ID = alice.ActorURL + "#main-key"
	aliceActor.PublicKey.Owner = alice.ActorURL

	return alice, bob, inbox, db
}

func TestInboxFollowAcceptFlow(t *testing.T) {
	alice, bob, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	// Alice sends a Follow to Bob
	follow := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#follow-1",
		"type":     "Follow",
		"actor":    alice.ActorURL,
		"object":   bob.ActorURL,
	}

	req := signedInboxPost(t, alice, follow)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Verify follow request was stored
	fr, err := db.GetFollowRequest(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, fr, "expected follow request to be stored")
}

func TestInboxRejectsActorMismatch(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	// Activity claims to be from someone else
	activity := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       "https://evil.com/fake",
		"type":     "Follow",
		"actor":    "https://evil.com/ap/actor",
		"object":   "https://bob.test/ap/actor",
	}

	req := signedInboxPost(t, alice, activity)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "expected 403 for actor mismatch")
}

func TestInboxAcceptMarksOutgoingFollowAccepted(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	// Pre-store a pending outgoing follow to alice (simulating we sent a Follow to alice).
	require.NoError(t, db.AddOutgoingFollow(ctx, alice.ActorURL))

	accept := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#accept-1",
		"type":     "Accept",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":   "Follow",
			"actor":  "https://bob.test/ap/actor",
			"object": alice.ActorURL,
		},
	}

	req := signedInboxPost(t, alice, accept)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Outgoing follow should be marked as accepted.
	of, err := db.GetOutgoingFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.NotNil(t, of)
	require.Equal(t, "accepted", of.Status, "expected outgoing follow to be marked accepted after Accept")

	// Accept should NOT auto-promote an incoming follow request -- that requires
	// explicit operator approval.
	f, err := db.GetFollow(ctx, alice.ActorURL)
	require.NoError(t, err)
	require.Nil(t, f, "Accept should not auto-promote incoming follow requests")
}

func TestInboxRejectCleansUp(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollowRequest(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	reject := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#reject-1",
		"type":     "Reject",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type": "Follow",
		},
	}

	req := signedInboxPost(t, alice, reject)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	fr, _ := db.GetFollowRequest(ctx, alice.ActorURL)
	require.Nil(t, fr, "expected follow request to be cleaned up after Reject")
}

func TestInboxUndoRemovesFollow(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	undo := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#undo-1",
		"type":     "Undo",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type": "Follow",
		},
	}

	req := signedInboxPost(t, alice, undo)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)

	f, _ := db.GetFollow(ctx, alice.ActorURL)
	require.Nil(t, f, "expected follow to be removed after Undo")
}

func TestInboxCreateManifest(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	// Alice must be a follower for content activities to be accepted
	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-1",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": "test/app",
			"ociDigest":     "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
			"ociMediaType":  "application/vnd.oci.image.manifest.v1+json",
			"ociSize":       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	// Verify repository was created with alice as owner
	repo, err := db.GetRepository(ctx, "test/app")
	require.NoError(t, err)
	require.NotNil(t, repo, "expected repository to be created")
	require.Equal(t, alice.ActorURL, repo.OwnerID)
}

func TestInboxCreateManifestRejectsNonFollower(t *testing.T) {
	alice, _, inbox, _ := setupInboxTest(t)

	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-1",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": "test/app",
			"ociDigest":     "sha256:abc123",
			"ociMediaType":  "application/vnd.oci.image.manifest.v1+json",
			"ociSize":       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "expected 403 for non-follower Create")
}

func TestInboxUpdateTag(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	// Create the repo and a manifest first
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	_, err := db.GetOrCreateRepository(ctx, "test/app", alice.ActorURL)
	require.NoError(t, err)
	repo, _ := db.GetRepository(ctx, "test/app")
	require.NoError(t, db.PutManifest(ctx, &database.Manifest{
		RepositoryID: repo.ID,
		Digest:       digest,
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    100,
		Content:      []byte(`{"schemaVersion":2}`),
	}))

	update := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#update-1",
		"type":     "Update",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCITag",
			"ociRepository": "test/app",
			"ociTag":        "latest",
			"ociDigest":     digest,
		},
	}

	req := signedInboxPost(t, alice, update)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	m, err := db.GetManifestByTag(ctx, repo.ID, "latest")
	require.NoError(t, err)
	require.NotNil(t, m, "expected tag to resolve to manifest")
	require.Equal(t, digest, m.Digest)
}

func TestInboxAnnounceBlobRef(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	// Alice must exist as a peer for blob lookup to work
	require.NoError(t, db.UpsertPeer(ctx, &database.Peer{
		ActorURL:          alice.ActorURL,
		Endpoint:          "https://alice.test",
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}))

	announce := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#announce-1",
		"type":     "Announce",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":        "OCIBlob",
			"ociDigest":   "sha256:b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2",
			"ociSize":     float64(4096),
			"ociEndpoint": "https://alice.test",
		},
	}

	req := signedInboxPost(t, alice, announce)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	pbs, err := db.FindPeersWithBlob(ctx, "sha256:b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2b3b4b5b6b7b8b9b0b1b2")
	require.NoError(t, err)
	require.Len(t, pbs, 1)
}

func TestInboxDeleteIsAccepted(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(context.Background(), alice.ActorURL, alicePEM, "https://alice.test"))

	del := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#delete-1",
		"type":     "Delete",
		"actor":    alice.ActorURL,
		"object":   fmt.Sprintf("%s/objects/manifest/sha256:abc", alice.ActorURL),
	}

	req := signedInboxPost(t, alice, del)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code)
}

func TestInboxOwnershipEnforcement(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	// Create repo owned by someone else
	_, err := db.GetOrCreateRepository(ctx, "bobs/repo", "https://bob.test/ap/actor")
	require.NoError(t, err)

	// Alice tries to push a manifest to bob's repo
	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-steal",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": "bobs/repo",
			"ociDigest":     "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
			"ociMediaType":  "application/vnd.oci.image.manifest.v1+json",
			"ociSize":       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}
