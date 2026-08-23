package npm

import (
	"bytes"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gtnpm "gitea.dev/modules/packages/npm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	testToken     = "test-token"
	testOwnerURL  = "https://alice.example.com/ap/actor"
	testVersion   = "1.0.0"
	testPkgLodash = "lodash"
)

func nopLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)

	b := New(Config{
		DB:       db,
		Blobs:    blobs,
		Endpoint: srv.URL,
		Token:    testToken,
		Owner:    testOwnerURL,
		Logger:   nopLog(),
	})
	srv.Config.Handler = b.Handler()
	return srv
}

// tarball bytes don't need to be a real gzip; only the SHA-512 integrity is checked.
func publishBody(t *testing.T, name, version string, tarball []byte, distTag string) []byte {
	t.Helper()
	bare := name
	if _, after, ok := strings.Cut(name, "/"); ok {
		bare = after
	}
	filename := fmt.Sprintf("%s-%s.tgz", bare, version)

	sum := sha512.Sum512(tarball)
	integrity := "sha512-" + base64.StdEncoding.EncodeToString(sum[:])

	upload := struct {
		gtnpm.PackageMetadata
		Attachments map[string]*gtnpm.PackageAttachment `json:"_attachments"`
	}{
		PackageMetadata: gtnpm.PackageMetadata{
			ID:       name,
			Name:     name,
			DistTags: map[string]string{},
			Versions: map[string]*gtnpm.PackageMetadataVersion{
				version: {
					ID:           name + "@" + version,
					Name:         name,
					Version:      version,
					Description:  "test pkg",
					Dist:         gtnpm.PackageDistribution{Integrity: integrity},
					Dependencies: map[string]string{"left-pad": testVersion},
				},
			},
		},
		Attachments: map[string]*gtnpm.PackageAttachment{
			filename: {
				ContentType: "application/octet-stream",
				Data:        base64.StdEncoding.EncodeToString(tarball),
				Length:      len(tarball),
			},
		},
	}
	if distTag != "" {
		upload.DistTags[distTag] = version
	}
	body, err := json.Marshal(upload)
	require.NoError(t, err)
	return body
}

func doRequest(t *testing.T, srv *httptest.Server, method, path string, body []byte, withAuth bool) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, r)
	require.NoError(t, err)
	if withAuth {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	return resp
}

func TestPublishAndRead(t *testing.T) {
	srv := newTestServer(t)

	tarball := []byte("fake-tarball-bytes")
	body := publishBody(t, testPkgLodash, testVersion, tarball, "latest")

	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode, "publish should succeed")

	resp = doRequest(t, srv, http.MethodGet, "/npm/lodash", nil, false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var pm gtnpm.PackageMetadata
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pm))
	assert.Equal(t, testPkgLodash, pm.Name)
	assert.Equal(t, testVersion, pm.DistTags["latest"])
	require.Contains(t, pm.Versions, testVersion)
	v := pm.Versions[testVersion]
	assert.Equal(t, "test pkg", v.Description)
	assert.Equal(t, testVersion, v.Dependencies["left-pad"])
	assert.NotEmpty(t, v.Dist.Integrity)
	assert.NotEmpty(t, v.Dist.Shasum)
	assert.Contains(t, v.Dist.Tarball, "/npm/lodash/-/lodash-1.0.0.tgz")

	resp = doRequest(t, srv, http.MethodGet, "/npm/lodash/-/lodash-1.0.0.tgz", nil, false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	got, _ := io.ReadAll(resp.Body)
	assert.Equal(t, tarball, got)
	assert.NotEmpty(t, resp.Header.Get("ETag"))
}

func TestPublishScopedPackage(t *testing.T) {
	srv := newTestServer(t)

	tarball := []byte("scoped-tarball")
	body := publishBody(t, "@scope/foo", "2.1.3", tarball, "latest")

	resp := doRequest(t, srv, http.MethodPut, "/npm/@scope%2Ffoo", body, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	resp = doRequest(t, srv, http.MethodGet, "/npm/@scope/foo", nil, false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var pm gtnpm.PackageMetadata
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&pm))
	assert.Equal(t, "@scope/foo", pm.Name)
	require.Contains(t, pm.Versions, "2.1.3")
	assert.Equal(t, "2.1.3", pm.DistTags["latest"])

	resp = doRequest(t, srv, http.MethodGet, "/npm/@scope/foo/-/foo-2.1.3.tgz", nil, false)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestPublishUnauthorized(t *testing.T) {
	srv := newTestServer(t)
	body := publishBody(t, testPkgLodash, testVersion, []byte("x"), "latest")

	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("WWW-Authenticate"), "Bearer")
}

func TestPublishBadIntegrity(t *testing.T) {
	srv := newTestServer(t)

	upload := struct {
		gtnpm.PackageMetadata
		Attachments map[string]*gtnpm.PackageAttachment `json:"_attachments"`
	}{
		PackageMetadata: gtnpm.PackageMetadata{
			ID: testPkgLodash, Name: testPkgLodash,
			Versions: map[string]*gtnpm.PackageMetadataVersion{
				testVersion: {
					Name: testPkgLodash, Version: testVersion,
					Dist: gtnpm.PackageDistribution{Integrity: "sha512-deadbeef=="},
				},
			},
		},
		Attachments: map[string]*gtnpm.PackageAttachment{
			"lodash-1.0.0.tgz": {Data: base64.StdEncoding.EncodeToString([]byte("payload"))},
		},
	}
	body, _ := json.Marshal(upload)
	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPublishNameMismatch(t *testing.T) {
	srv := newTestServer(t)
	body := publishBody(t, testPkgLodash, testVersion, []byte("x"), "")
	resp := doRequest(t, srv, http.MethodPut, "/npm/elsewhere", body, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestPackumentNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := doRequest(t, srv, http.MethodGet, "/npm/missing", nil, false)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDistTagsLifecycle(t *testing.T) {
	srv := newTestServer(t)

	for _, v := range []string{testVersion, "2.0.0", "3.0.0-beta"} {
		body := publishBody(t, "react", v, []byte("react-"+v), "")
		resp := doRequest(t, srv, http.MethodPut, "/npm/react", body, true)
		require.Equal(t, http.StatusCreated, resp.StatusCode, "publish %s", v)
		_ = resp.Body.Close()
	}

	versionJSON, _ := json.Marshal("2.0.0")
	resp := doRequest(t, srv, http.MethodPut, "/npm/-/package/react/dist-tags/latest", versionJSON, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode, "set latest=2.0.0")

	betaJSON, _ := json.Marshal("3.0.0-beta")
	resp2 := doRequest(t, srv, http.MethodPut, "/npm/-/package/react/dist-tags/beta", betaJSON, true)
	defer func() { _ = resp2.Body.Close() }()
	require.Equal(t, http.StatusOK, resp2.StatusCode)

	resp3 := doRequest(t, srv, http.MethodGet, "/npm/-/package/react/dist-tags", nil, false)
	defer func() { _ = resp3.Body.Close() }()
	require.Equal(t, http.StatusOK, resp3.StatusCode)
	var tags map[string]string
	require.NoError(t, json.NewDecoder(resp3.Body).Decode(&tags))
	assert.Equal(t, "2.0.0", tags["latest"])
	assert.Equal(t, "3.0.0-beta", tags["beta"])

	resp4 := doRequest(t, srv, http.MethodDelete, "/npm/-/package/react/dist-tags/beta", nil, true)
	defer func() { _ = resp4.Body.Close() }()
	require.Equal(t, http.StatusOK, resp4.StatusCode)

	resp5 := doRequest(t, srv, http.MethodGet, "/npm/-/package/react/dist-tags", nil, false)
	defer func() { _ = resp5.Body.Close() }()
	tags = nil
	require.NoError(t, json.NewDecoder(resp5.Body).Decode(&tags))
	assert.NotContains(t, tags, "beta")
	assert.Equal(t, "2.0.0", tags["latest"])
}

func TestDistTagPutMissingVersion(t *testing.T) {
	srv := newTestServer(t)

	body := publishBody(t, testPkgLodash, testVersion, []byte("data"), "")
	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	versionJSON, _ := json.Marshal("9.9.9")
	resp = doRequest(t, srv, http.MethodPut, "/npm/-/package/lodash/dist-tags/latest", versionJSON, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestRePublishConflicts(t *testing.T) {
	srv := newTestServer(t)

	tarball1 := []byte("v1-tarball")
	body1 := publishBody(t, testPkgLodash, testVersion, tarball1, "latest")
	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body1, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	tarball2 := []byte("v1-tarball-rebuilt")
	body2 := publishBody(t, testPkgLodash, testVersion, tarball2, "latest")
	resp = doRequest(t, srv, http.MethodPut, "/npm/lodash", body2, true)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusConflict, resp.StatusCode)

	resp2 := doRequest(t, srv, http.MethodGet, "/npm/lodash/-/lodash-1.0.0.tgz", nil, false)
	defer func() { _ = resp2.Body.Close() }()
	got, _ := io.ReadAll(resp2.Body)
	assert.Equal(t, tarball1, got, "first publish remains, immutable")
}

func TestUnconfiguredOwnerRejected(t *testing.T) {
	db, err := database.OpenSQLite(t.TempDir(), 0, 0, nopLog())
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	blobs, err := blobstore.New(t.TempDir(), nopLog())
	require.NoError(t, err)

	_, err = db.GetOrCreatePackage(t.Context(), packageType, "taken", "https://other.example.com/ap/actor")
	require.NoError(t, err)

	srv := httptest.NewServer(nil)
	t.Cleanup(srv.Close)
	b := New(Config{DB: db, Blobs: blobs, Endpoint: srv.URL, Token: testToken, Owner: testOwnerURL, Logger: nopLog()})
	srv.Config.Handler = b.Handler()

	body := publishBody(t, "taken", testVersion, []byte("data"), "")
	resp := doRequest(t, srv, http.MethodPut, "/npm/taken", body, true)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestTarballHEAD(t *testing.T) {
	srv := newTestServer(t)

	tarball := []byte("head-payload")
	body := publishBody(t, testPkgLodash, testVersion, tarball, "latest")
	resp := doRequest(t, srv, http.MethodPut, "/npm/lodash", body, true)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	_ = resp.Body.Close()

	req, err := http.NewRequest(http.MethodHead, srv.URL+"/npm/lodash/-/lodash-1.0.0.tgz", nil)
	require.NoError(t, err)
	resp2, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp2.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)
	assert.Equal(t, fmt.Sprintf("%d", len(tarball)), resp2.Header.Get("Content-Length"))
}

func TestPublishBodyParsesUpstream(t *testing.T) {
	tarball := []byte("payload-bytes")
	body := publishBody(t, testPkgLodash, testVersion, tarball, "")

	pkg, err := gtnpm.ParsePackage(bytes.NewReader(body))
	require.NoError(t, err)
	require.Equal(t, testPkgLodash, pkg.Name)
	require.Equal(t, testVersion, pkg.Version)
	require.Equal(t, tarball, pkg.Data)
}
