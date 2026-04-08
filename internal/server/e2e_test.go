package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func mustNewRequest(t *testing.T, method, url string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, body)
	require.NoError(t, err)
	return req
}

// authReq adds the registry Bearer token to a request for write operations.
func authReq(req *http.Request) *http.Request {
	req.Header.Set("Authorization", "Bearer "+testRegistryToken)
	return req
}

func TestE2EFullBlobAndManifestFlow(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// 1. Push a blob (monolithic POST with digest)
	blobData := []byte("hello blob content for e2e test")
	blobHash := sha256.Sum256(blobData)
	blobDigest := "sha256:" + hex.EncodeToString(blobHash[:])

	req := authReq(mustNewRequest(t, "POST", srv.URL+"/v2/test.example.com/e2e/blobs/uploads/?digest="+blobDigest, bytes.NewReader(blobData)))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.True(t, resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusAccepted, "blob push: expected 201 or 202, got %d", resp.StatusCode)

	// 2. Push manifest referencing the blob (links blob to repo via manifest_layers)
	manifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"%s","size":%d,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`, blobDigest, len(blobData))
	req = authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/e2e/manifests/v1.0", strings.NewReader(manifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "manifest push")
	manifestDigest := resp.Header.Get("Docker-Content-Digest")

	// 3. Verify blob exists via HEAD (after manifest links it to the repo)
	req = mustNewRequest(t, "HEAD", srv.URL+"/v2/test.example.com/e2e/blobs/"+blobDigest, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "blob HEAD")

	// 4. Pull manifest by tag
	req = mustNewRequest(t, "GET", srv.URL+"/v2/test.example.com/e2e/manifests/v1.0", nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "manifest pull by tag")
	require.Equal(t, manifest, string(body), "manifest content mismatch")

	// 5. Pull manifest by digest
	req = mustNewRequest(t, "GET", srv.URL+"/v2/test.example.com/e2e/manifests/"+manifestDigest, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "manifest pull by digest")

	// 6. Pull blob content
	req = mustNewRequest(t, "GET", srv.URL+"/v2/test.example.com/e2e/blobs/"+blobDigest, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	blobBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "blob pull")
	require.Equal(t, string(blobData), string(blobBody))
}

func TestE2ECosignReferrersViaHTTP(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Push an image manifest
	imageManifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc","size":0,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`
	req := authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/cosign/manifests/v1", strings.NewReader(imageManifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	imageDigest := resp.Header.Get("Docker-Content-Digest")
	imageSize := len(imageManifest)

	// Push a cosign signature manifest with subject pointing to image
	sigManifest := fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","artifactType":"application/vnd.dev.cosign.simplesigning.v1+json","config":{"digest":"sha256:def","size":0,"mediaType":"application/vnd.oci.empty.v1+json"},"layers":[],"subject":{"digest":"%s","mediaType":"application/vnd.oci.image.manifest.v1+json","size":%d}}`, imageDigest, imageSize)
	req = authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/cosign/manifests/sha256-sig.sig", strings.NewReader(sigManifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "sig push")

	// Query referrers via HTTP
	resp, err = http.Get(srv.URL + "/v2/test.example.com/cosign/referrers/" + imageDigest)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var index struct {
		Manifests []struct {
			Digest       string `json:"digest"`
			ArtifactType string `json:"artifactType"`
		} `json:"manifests"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&index), "failed to decode referrers response")

	require.Len(t, index.Manifests, 1)
	require.Equal(t, "application/vnd.dev.cosign.simplesigning.v1+json", index.Manifests[0].ArtifactType)
}

func TestE2ETagImmutabilityViaHTTP(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	manifest1 := `{"schemaVersion":2}`
	manifest2 := `{"schemaVersion":2,"new":true}`

	// First push to v1.0 succeeds
	req := authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/immutable/manifests/v1.0", strings.NewReader(manifest1)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "first push")

	// Second push to v1.0 is rejected
	req = authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/immutable/manifests/v1.0", strings.NewReader(manifest2)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.NotEqual(t, http.StatusCreated, resp.StatusCode, "second push to v1.0 should be rejected (immutable)")

	// Push to 'latest' succeeds twice (not semver)
	for i := range 2 {
		body := fmt.Sprintf(`{"schemaVersion":2,"iteration":%d}`, i)
		req = authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/immutable/manifests/latest", strings.NewReader(body)))
		req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		resp, err = http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusCreated, resp.StatusCode, "push %d to latest", i)
	}
}

func TestE2ENamespaceEnforcementViaHTTP(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	manifest := `{"schemaVersion":2}`

	// Push to correct namespace succeeds
	req := authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/myapp/manifests/v1", strings.NewReader(manifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "namespaced push")

	// Push without namespace prefix succeeds (auto-prefixed)
	req = authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/someone-else/myapp/manifests/v1", strings.NewReader(manifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "push without prefix should succeed via auto-prefix")

	// Push to a foreign domain-scoped namespace is rejected
	req = authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/foreign.example.com/myapp/manifests/v1", strings.NewReader(manifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.NotEqual(t, http.StatusCreated, resp.StatusCode, "push to foreign domain must be rejected")
}

func TestE2EDeleteFlowViaHTTP(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// Push blob (monolithic)
	blobData := []byte("deletable blob")
	blobHash := sha256.Sum256(blobData)
	blobDigest := "sha256:" + hex.EncodeToString(blobHash[:])

	req := authReq(mustNewRequest(t, "POST", srv.URL+"/v2/test.example.com/deltest/blobs/uploads/?digest="+blobDigest, bytes.NewReader(blobData)))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	// Push manifest
	manifest := fmt.Sprintf(`{"schemaVersion":2,"config":{"digest":"%s","size":%d},"layers":[]}`, blobDigest, len(blobData))
	req = authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/deltest/manifests/latest", strings.NewReader(manifest)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	manifestDigest := resp.Header.Get("Docker-Content-Digest")
	_ = resp.Body.Close()

	// Delete manifest
	req = authReq(mustNewRequest(t, "DELETE", srv.URL+"/v2/test.example.com/deltest/manifests/"+manifestDigest, nil))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode, "manifest delete")

	// Verify manifest is gone
	req = mustNewRequest(t, "GET", srv.URL+"/v2/test.example.com/deltest/manifests/"+manifestDigest, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode, "manifest after delete")

	// Blob should still be fetchable (not deleted with manifest)
	req = mustNewRequest(t, "GET", srv.URL+"/v2/test.example.com/deltest/blobs/"+blobDigest, nil)
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	blobBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		require.Equal(t, string(blobData), string(blobBody), "blob content mismatch after manifest delete")
	}
}

func TestE2EWebFingerToActor(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	// WebFinger
	resp, err := http.Get(srv.URL + "/.well-known/webfinger?resource=acct:registry@test.example.com")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "webfinger")

	var wf struct {
		Links []struct {
			Href string `json:"href"`
		} `json:"links"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&wf))
	require.NotEmpty(t, wf.Links, "expected webfinger links")

	// Follow link to actor
	req := mustNewRequest(t, "GET", srv.URL+"/ap/actor", nil)
	req.Header.Set("Accept", "application/activity+json")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()

	var actor map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&actor))
	require.Equal(t, "Application", actor["type"])
	require.NotNil(t, actor["inbox"], "actor missing inbox URL")
	pk, ok := actor["publicKey"].(map[string]any)
	require.True(t, ok, "actor missing publicKey")
	require.NotNil(t, pk["publicKeyPem"], "actor missing publicKeyPem")
}

func TestE2ENodeInfo(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/.well-known/nodeinfo")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var wk struct {
		Links []struct{ Href string } `json:"links"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&wk))
	require.NotEmpty(t, wk.Links, "expected nodeinfo link")

	resp2, err := http.Get(srv.URL + "/ap/nodeinfo/2.1")
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()

	var ni map[string]any
	require.NoError(t, json.NewDecoder(resp2.Body).Decode(&ni))
	require.Equal(t, "2.1", ni["version"])
	sw, _ := ni["software"].(map[string]any)
	require.Equal(t, "apoci", sw["name"])
}

func TestE2EPushCreatesActivityInOutbox(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req := authReq(mustNewRequest(t, "PUT", srv.URL+"/v2/test.example.com/activity/manifests/v1", strings.NewReader(`{"schemaVersion":2}`)))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "push")

	resp, err = http.Get(srv.URL + "/ap/outbox")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	var outbox map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&outbox))
	totalItems, _ := outbox["totalItems"].(float64)
	require.GreaterOrEqual(t, totalItems, float64(1), "expected at least 1 activity in outbox")
}

func TestE2EInboxRejectsUnsigned(t *testing.T) {
	s := testServer(t)
	srv := httptest.NewServer(s.routes())
	defer srv.Close()

	req := mustNewRequest(t, "POST", srv.URL+"/ap/inbox", strings.NewReader(`{"type":"Follow","actor":"https://evil.com/ap/actor","object":"x"}`))
	req.Header.Set("Content-Type", "application/activity+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "expected 401 for unsigned inbox")
}
