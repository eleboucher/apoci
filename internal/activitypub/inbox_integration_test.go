package activitypub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
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
// It returns the sender domain (hostname of alice's resolved actor URL) for use in repo names.
func setupInboxTest(t *testing.T) (alice *Identity, bob *Identity, inbox *InboxHandler, db *database.DB) {
	t.Helper()
	dir := t.TempDir()

	db, err := database.OpenSQLite(dir, 0, 0, discardLogger())
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

	alice.ActorURL = actorSrv.URL + "/ap/actor"
	alice.Domain = "alice.test"
	aliceActor.ID = alice.ActorURL
	aliceActor.PublicKey.ID = alice.ActorURL + "#main-key"
	aliceActor.PublicKey.Owner = alice.ActorURL

	return alice, bob, inbox, db
}

func aliceRepoName(alice *Identity, suffix string) string {
	domain, _ := senderDomainFromActorURL(alice.ActorURL)
	if suffix == "" {
		return domain + "/app"
	}
	return domain + "/" + suffix
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

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	repo := aliceRepoName(alice, "app")
	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-1",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": repo,
			"ociDigest":     "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
			"ociMediaType":  "application/vnd.oci.image.manifest.v1+json",
			"ociSize":       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	repoObj, err := db.GetRepository(ctx, repo)
	require.NoError(t, err)
	require.NotNil(t, repoObj)
	require.Equal(t, alice.ActorURL, repoObj.OwnerID)
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

	repoName := aliceRepoName(alice, "app")
	digest := "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd"
	_, err := db.GetOrCreateRepository(ctx, repoName, alice.ActorURL)
	require.NoError(t, err)
	repoObj, _ := db.GetRepository(ctx, repoName)
	require.NoError(t, db.PutManifest(ctx, &database.Manifest{
		RepositoryID: repoObj.ID,
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
			"ociRepository": repoName,
			"ociTag":        "latest",
			"ociDigest":     digest,
		},
	}

	req := signedInboxPost(t, alice, update)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	m, err := db.GetManifestByTag(ctx, repoObj.ID, "latest")
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, digest, m.Digest)
}

func TestInboxAnnounceBlobRef(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	aliceEndpoint := EndpointFromActorURL(alice.ActorURL)
	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, aliceEndpoint))

	require.NoError(t, db.UpsertPeer(ctx, &database.Peer{
		ActorURL:          alice.ActorURL,
		Endpoint:          aliceEndpoint,
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
			"ociEndpoint": aliceEndpoint,
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

	repoName := aliceRepoName(alice, "repo")
	otherActor := "https://" + aliceRepoName(alice, "ap/other-actor")
	_, err := db.GetOrCreateRepository(ctx, repoName, otherActor)
	require.NoError(t, err)

	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-steal",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": repoName,
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

// manifestDigestAndContent returns content bytes and their sha256 digest string.
func manifestDigestAndContent(content []byte) (digest string, encoded string) {
	h := sha256.Sum256(content)
	digest = "sha256:" + hex.EncodeToString(h[:])
	encoded = base64.StdEncoding.EncodeToString(content)
	return
}

func TestInboxCreateManifestWithContent_DigestMatch(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	content := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	digest, encoded := manifestDigestAndContent(content)
	repoName := aliceRepoName(alice, "app")

	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-content",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": repoName,
			"ociDigest":     digest,
			"ociMediaType":  "application/vnd.oci.image.manifest.v1+json",
			"ociSize":       float64(len(content)),
			"ociContent":    encoded,
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

	repoObj, err := db.GetRepository(ctx, repoName)
	require.NoError(t, err)
	require.NotNil(t, repoObj)
	m, err := db.GetManifestByDigest(ctx, repoObj.ID, digest)
	require.NoError(t, err)
	require.NotNil(t, m)
	require.Equal(t, content, m.Content)
}

func TestInboxCreateManifestWithContent_DigestMismatch(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	legitimateContent := []byte(`{"schemaVersion":2}`)
	digest, _ := manifestDigestAndContent(legitimateContent)
	malwareContent := []byte(`<malware>not what you asked for</malware>`)
	tamperedEncoded := base64.StdEncoding.EncodeToString(malwareContent)

	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-tampered",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": aliceRepoName(alice, "app"),
			"ociDigest":     digest,
			"ociMediaType":  "application/vnd.oci.image.manifest.v1+json",
			"ociSize":       float64(len(legitimateContent)),
			"ociContent":    tamperedEncoded,
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "tampered content must be rejected")
}

func TestInboxCreateManifestWrongDomain(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	create := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#create-squatter",
		"type":     "Create",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCIManifest",
			"ociRepository": "someotherdomain.example/app",
			"ociDigest":     "sha256:abc123def456abc123def456abc123def456abc123def456abc123def456abcd",
			"ociMediaType":  "application/vnd.oci.image.manifest.v1+json",
			"ociSize":       float64(256),
		},
	}

	req := signedInboxPost(t, alice, create)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, "pushing to another domain must be rejected")
}

func TestInboxUpdateTagUnknownManifest(t *testing.T) {
	alice, _, inbox, db := setupInboxTest(t)
	ctx := context.Background()

	alicePEM, _ := alice.PublicKeyPEM()
	require.NoError(t, db.AddFollow(ctx, alice.ActorURL, alicePEM, "https://alice.test"))

	repoName := aliceRepoName(alice, "app")
	_, err := db.GetOrCreateRepository(ctx, repoName, alice.ActorURL)
	require.NoError(t, err)

	update := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       alice.ActorURL + "#update-ghost",
		"type":     "Update",
		"actor":    alice.ActorURL,
		"object": map[string]any{
			"type":          "OCITag",
			"ociRepository": repoName,
			"ociTag":        "latest",
			"ociDigest":     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}

	req := signedInboxPost(t, alice, update)
	rec := httptest.NewRecorder()
	inbox.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, "tag pointing to non-existent manifest must be rejected")
}
