package oci

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSpecV2EndpointReturns200(t *testing.T) {
	_, srv := testRegistry(t)

	resp, err := http.Get(srv.URL + "/v2/")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, 200, resp.StatusCode)
}

func TestSpecManifestPushReturnsDigestHeader(t *testing.T) {
	_, srv := testRegistry(t)

	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc","size":0,"mediaType":"application/vnd.oci.image.config.v1+json"},"layers":[]}`

	req, _ := http.NewRequest("PUT", srv.URL+"/v2/test.example.com/test/spec/manifests/v1", strings.NewReader(manifest))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)

	digest := resp.Header.Get("Docker-Content-Digest")
	require.NotEmpty(t, digest, "missing Docker-Content-Digest header on manifest push")
	require.True(t, strings.HasPrefix(digest, "sha256:"), "Docker-Content-Digest should start with sha256:, got %s", digest)

	location := resp.Header.Get("Location")
	require.NotEmpty(t, location, "missing Location header on manifest push")
}

func TestSpecBlobUploadReturnsLocationHeader(t *testing.T) {
	_, srv := testRegistry(t)

	req, _ := http.NewRequest("POST", srv.URL+"/v2/test.example.com/test/spec/blobs/uploads/", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusAccepted, resp.StatusCode)

	location := resp.Header.Get("Location")
	require.NotEmpty(t, location, "missing Location header on blob upload start")
}

func TestSpecManifestGetNotFoundFormat(t *testing.T) {
	_, srv := testRegistry(t)

	resp, err := http.Get(srv.URL + "/v2/nonexistent/repo/manifests/latest")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	var errResp struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &errResp), "expected OCI error JSON format, got: %s", body)
	require.NotEmpty(t, errResp.Errors, "expected at least one error in response, got: %s", body)
}

func TestSpecBlobGetNotFoundFormat(t *testing.T) {
	_, srv := testRegistry(t)

	resp, err := http.Get(srv.URL + "/v2/test/repo/blobs/sha256:0000000000000000000000000000000000000000000000000000000000000000")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	var errResp struct {
		Errors []struct {
			Code string `json:"code"`
		} `json:"errors"`
	}
	body, _ := io.ReadAll(resp.Body)
	require.NoError(t, json.Unmarshal(body, &errResp), "expected OCI error JSON format, got: %s", body)
}

func TestSpecManifestDeleteReturns202(t *testing.T) {
	_, srv := testRegistry(t)

	// Push a manifest first
	manifest := `{"schemaVersion":2}`
	req, _ := http.NewRequest("PUT", srv.URL+"/v2/test.example.com/test/del/manifests/v1", strings.NewReader(manifest))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()
	digest := resp.Header.Get("Docker-Content-Digest")

	// Delete by digest
	req, _ = http.NewRequest("DELETE", srv.URL+"/v2/test.example.com/test/del/manifests/"+digest, nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusAccepted, resp.StatusCode)
}

func TestSpecManifestHeadReturnsDescriptor(t *testing.T) {
	_, srv := testRegistry(t)

	manifest := `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:abc","size":0},"layers":[]}`
	req, _ := http.NewRequest("PUT", srv.URL+"/v2/test.example.com/test/head/manifests/v1", strings.NewReader(manifest))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, _ := http.DefaultClient.Do(req)
	_ = resp.Body.Close()

	// HEAD request
	req, _ = http.NewRequest("HEAD", srv.URL+"/v2/test.example.com/test/head/manifests/v1", nil)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NotEmpty(t, resp.Header.Get("Docker-Content-Digest"), "missing Docker-Content-Digest on HEAD")
	require.NotEmpty(t, resp.Header.Get("Content-Type"), "missing Content-Type on HEAD")
	require.NotEmpty(t, resp.Header.Get("Content-Length"), "missing Content-Length on HEAD")
}

func TestSpecCosignTagFormatAccepted(t *testing.T) {
	_, srv := testRegistry(t)

	manifest := `{"schemaVersion":2}`
	cosignTag := "sha256-abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890.sig"

	req, _ := http.NewRequest("PUT", srv.URL+"/v2/test.example.com/test/cosign/manifests/"+cosignTag, strings.NewReader(manifest))
	req.Header.Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestSpecTagsListPagination(t *testing.T) {
	reg, srv := testRegistry(t)
	ctx := context.Background()

	// Push 3 tags
	manifest := []byte(`{"schemaVersion":2}`)
	for _, tag := range []string{"a", "b", "c"} {
		_, err := reg.PushManifest(ctx, "test.example.com/test/pagination", tag, manifest, "application/vnd.oci.image.manifest.v1+json")
		require.NoError(t, err)
	}

	// List with limit
	resp, err := http.Get(srv.URL + "/v2/test.example.com/test/pagination/tags/list?n=2")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)

	var tagList struct {
		Tags []string `json:"tags"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&tagList))
	require.LessOrEqual(t, len(tagList.Tags), 2)
}
