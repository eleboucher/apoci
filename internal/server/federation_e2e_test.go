package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/activitypub"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/config"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

// proxyHandler forwards to a handler set after the httptest server starts.
type proxyHandler struct {
	h atomic.Pointer[http.Handler]
}

func (p *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := p.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
	} else {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}
}

func (p *proxyHandler) Set(h http.Handler) {
	p.h.Store(&h)
}

// TestTwoNodeFederation exercises: Follow → push manifest on A → delivery to B's inbox → pull-through from B.
func TestTwoNodeFederation(t *testing.T) {
	activitypub.SetAllowInsecureHTTP(true)

	// Create httptest servers first so we know the URLs.
	aliceProxy := &proxyHandler{}
	bobProxy := &proxyHandler{}
	aliceHTTP := httptest.NewServer(aliceProxy)
	defer aliceHTTP.Close()
	bobHTTP := httptest.NewServer(bobProxy)
	defer bobHTTP.Close()

	aliceDir := t.TempDir()
	bobDir := t.TempDir()

	aliceDB, err := database.OpenSQLite(aliceDir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = aliceDB.Close() })

	bobDB, err := database.OpenSQLite(bobDir, 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = bobDB.Close() })

	aliceBlobs, err := blobstore.New(aliceDir, nopLog())
	require.NoError(t, err)
	bobBlobs, err := blobstore.New(bobDir, nopLog())
	require.NoError(t, err)

	aliceIdentity, err := activitypub.LoadOrCreateIdentity("alice.test", "", "", nopLog())
	require.NoError(t, err)
	bobIdentity, err := activitypub.LoadOrCreateIdentity("bob.test", "", "", nopLog())
	require.NoError(t, err)

	// Patch identities to use httptest URLs so actor documents serve correct inbox URLs.
	aliceURL, _ := url.Parse(aliceHTTP.URL)
	bobURL, _ := url.Parse(bobHTTP.URL)
	aliceIdentity.ActorURL = aliceHTTP.URL + "/ap/actor"
	aliceIdentity.Domain = aliceURL.Host
	bobIdentity.ActorURL = bobHTTP.URL + "/ap/actor"
	bobIdentity.Domain = bobURL.Host

	// Use the httptest host (e.g. "127.0.0.1") as the domain so that namespace
	// enforcement and sender-domain checks align with the httptest URL.
	aliceHost := aliceURL.Hostname()
	bobHost := bobURL.Hostname()

	makeCfg := func(host, endpoint string) *config.Config {
		return &config.Config{
			Name:          host,
			Endpoint:      endpoint,
			Domain:        host,
			AccountDomain: host,
			Listen:        ":0",
			RegistryToken: testRegistryToken,
			ImmutableTags: `^v[0-9]`,
			Federation:    config.Federation{AutoAccept: "all"},
			Peering: config.Peering{
				HealthCheckInterval: 30 * time.Second,
				FetchTimeout:        10 * time.Second,
			},
			Limits: config.Limits{
				MaxManifestSize: config.DefaultMaxManifestSize,
				MaxBlobSize:     config.DefaultMaxBlobSize,
			},
		}
	}

	aliceServer, err := New(makeCfg(aliceHost, aliceHTTP.URL), aliceDB, aliceBlobs, aliceIdentity, "test", nopLog())
	require.NoError(t, err)
	bobServer, err := New(makeCfg(bobHost, bobHTTP.URL), bobDB, bobBlobs, bobIdentity, "test", nopLog())
	require.NoError(t, err)

	// Wire the proxy handlers to the real routers.
	aliceProxy.Set(aliceServer.routes())
	bobProxy.Set(bobServer.routes())

	ctx := context.Background()

	// Start delivery queues so activities get delivered.
	aliceServer.deliveryQueue.Start(ctx)
	t.Cleanup(func() { aliceServer.deliveryQueue.Stop() })
	bobServer.deliveryQueue.Start(ctx)
	t.Cleanup(func() { bobServer.deliveryQueue.Stop() })

	// ---------------------------------------------------------------
	// Step 1: Bob sends a Follow to Alice.
	// ---------------------------------------------------------------
	bobPEM, err := bobIdentity.PublicKeyPEM()
	require.NoError(t, err)
	alicePEM, err := aliceIdentity.PublicKeyPEM()
	require.NoError(t, err)

	// Seed Bob's key in Alice's DB so signature verification works.
	require.NoError(t, aliceDB.AddFollowRequest(ctx, bobIdentity.ActorURL, bobPEM, bobHTTP.URL))

	followActivity := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       bobIdentity.ActorURL + "#follow-alice",
		"type":     "Follow",
		"actor":    bobIdentity.ActorURL,
		"object":   aliceIdentity.ActorURL,
	}
	followJSON, err := json.Marshal(followActivity)
	require.NoError(t, err)

	followReq, err := http.NewRequestWithContext(ctx, "POST", aliceHTTP.URL+"/ap/inbox", bytes.NewReader(followJSON))
	require.NoError(t, err)
	followReq.Header.Set("Content-Type", "application/activity+json")
	require.NoError(t, activitypub.SignRequest(followReq, bobIdentity.KeyID(), bobIdentity.PrivateKey, followJSON))

	resp, err := http.DefaultClient.Do(followReq)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "follow")

	// autoAccept=all → Bob should be in Alice's followers.
	follows, err := aliceDB.ListFollows(ctx)
	require.NoError(t, err)
	require.Len(t, follows, 1, "alice should have 1 follower")

	// Set up reverse direction: Bob trusts Alice (simulating mutual follow / Accept).
	require.NoError(t, bobDB.AddFollow(ctx, aliceIdentity.ActorURL, alicePEM, aliceHTTP.URL))
	require.NoError(t, bobDB.UpsertPeer(ctx, &database.Peer{
		ActorURL:          aliceIdentity.ActorURL,
		Endpoint:          aliceHTTP.URL,
		ReplicationPolicy: "lazy",
		IsHealthy:         true,
	}))

	// ---------------------------------------------------------------
	// Step 2: Alice pushes a blob + manifest.
	// ---------------------------------------------------------------
	repoName := aliceHost + "/myapp"

	blobData := []byte("federation e2e blob content")
	blobHash := sha256.Sum256(blobData)
	blobDigest := "sha256:" + hex.EncodeToString(blobHash[:])

	pushBlobReq := authReq(mustNewRequest(t, "POST",
		aliceHTTP.URL+"/v2/"+repoName+"/blobs/uploads/?digest="+blobDigest,
		bytes.NewReader(blobData)))
	pushBlobReq.Header.Set("Content-Type", "application/octet-stream")
	resp, err = http.DefaultClient.Do(pushBlobReq)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.True(t, resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted,
		"blob push: got %d", resp.StatusCode)

	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`,
		blobDigest, len(blobData))

	pushManifestReq := authReq(mustNewRequest(t, "PUT",
		aliceHTTP.URL+"/v2/"+repoName+"/manifests/latest",
		bytes.NewReader([]byte(manifest))))
	pushManifestReq.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(pushManifestReq)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "manifest push")

	// ---------------------------------------------------------------
	// Step 3: Wait for delivery to Bob's inbox.
	// ---------------------------------------------------------------
	// Poll until Bob receives the manifest via federation.
	deadline := time.Now().Add(15 * time.Second)
	var bobRepo *database.Repository
	for time.Now().Before(deadline) {
		aliceServer.deliveryQueue.Notify()
		time.Sleep(300 * time.Millisecond)

		bobRepo, _ = bobDB.GetRepository(ctx, repoName)
		if bobRepo != nil {
			// Check for any manifest in the repo.
			rows, _ := bobDB.QueryContext(ctx, "SELECT COUNT(*) FROM manifests WHERE repository_id = ?", bobRepo.ID)
			var count int
			if rows != nil && rows.Next() {
				_ = rows.Scan(&count)
				_ = rows.Close()
			}
			if count > 0 {
				break
			}
		}
	}
	require.NotNil(t, bobRepo, "bob should have the federated repo")

	// ---------------------------------------------------------------
	// Step 4: Bob pulls manifest by digest (pull-through from Alice).
	// ---------------------------------------------------------------
	// Get the manifest digest from Alice's push response.
	manifestHash := sha256.Sum256([]byte(manifest))
	manifestDigest := "sha256:" + hex.EncodeToString(manifestHash[:])

	pullReq := mustNewRequest(t, "GET", bobHTTP.URL+"/v2/"+repoName+"/manifests/"+manifestDigest, nil)
	resp, err = http.DefaultClient.Do(pullReq)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "bob manifest pull by digest")
	require.Equal(t, manifest, string(body))

	// ---------------------------------------------------------------
	// Step 5: Bob pulls blob via pull-through from Alice.
	// ---------------------------------------------------------------
	pullBlobReq := mustNewRequest(t, "GET", bobHTTP.URL+"/v2/"+repoName+"/blobs/"+blobDigest, nil)
	resp, err = http.DefaultClient.Do(pullBlobReq)
	require.NoError(t, err)
	blobBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "bob blob pull-through")
	require.Equal(t, string(blobData), string(blobBody))
}
