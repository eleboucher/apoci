package npm

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // npm dist.shasum is hex sha1 by spec
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	gtnpm "gitea.dev/modules/packages/npm"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	maxPublishBytes  = 200 << 20 // 200 MiB; npm payload is base64 tarball + metadata
	tarballMediaType = "application/octet-stream"
	versionMediaType = "application/json"
)

// gtnpm.ParsePackage drops per-version metadata; re-decode to capture it.
type publishPayload struct {
	gtnpm.PackageMetadata
	Attachments map[string]*gtnpm.PackageAttachment `json:"_attachments"`
}

// Captured at publish so packument reads don't rehash the tarball.
type storedVersion struct {
	Meta      *gtnpm.PackageMetadataVersion `json:"meta"`
	Integrity string                        `json:"integrity"`
	Shasum    string                        `json:"shasum"`
}

func (b *Backend) handlePublish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	urlName := packageNameFromURL(r)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPublishBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(body)) > maxPublishBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "publish body too large")
		return
	}

	pkg, err := gtnpm.ParsePackage(bytes.NewReader(body))
	if err != nil {
		b.logger.Info("npm publish parse failed", "err", err, "name", urlName)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if pkg.Name != urlName {
		writeError(w, http.StatusBadRequest, "package name mismatch between URL and body")
		return
	}

	var raw publishPayload
	if err := json.Unmarshal(body, &raw); err != nil {
		writeError(w, http.StatusBadRequest, "decode payload: "+err.Error())
		return
	}
	versionMeta := raw.Versions[pkg.Version]
	if versionMeta == nil {
		writeError(w, http.StatusBadRequest, "publish payload missing version "+pkg.Version)
		return
	}

	digest, _, err := b.blobs.Put(ctx, bytes.NewReader(pkg.Data), "")
	if err != nil {
		b.logger.Error("npm publish: blob store failed", "err", err, "name", pkg.Name, "version", pkg.Version)
		writeError(w, http.StatusInternalServerError, "store blob")
		return
	}
	mediaType := tarballMediaType
	if err := b.db.PutBlob(ctx, digest, int64(len(pkg.Data)), &mediaType, true); err != nil {
		writeError(w, http.StatusInternalServerError, "record blob")
		return
	}

	dbPkg, err := b.db.GetOrCreatePackage(ctx, packageType, pkg.Name, b.owner)
	if err != nil {
		if errors.Is(err, database.ErrPackageOwnerMismatch) {
			writeError(w, http.StatusForbidden, "package is owned by another actor")
			return
		}
		b.logger.Error("npm publish: get-or-create package failed", "err", err, "name", pkg.Name)
		writeError(w, http.StatusInternalServerError, "package access")
		return
	}

	stored := storedVersion{
		Meta:      versionMeta,
		Integrity: "sha512-" + base64.StdEncoding.EncodeToString(sha512Sum(pkg.Data)),
		Shasum:    hex.EncodeToString(sha1Sum(pkg.Data)),
	}
	metadataBytes, err := json.Marshal(stored)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode metadata")
		return
	}

	v := &database.PackageVersion{
		PackageID: dbPkg.ID,
		Version:   pkg.Version,
		Metadata:  metadataBytes,
		MediaType: versionMediaType,
		SizeBytes: int64(len(metadataBytes)),
	}
	// Insert-only so a duplicate/racing publish is rejected atomically rather
	// than silently overwriting an existing immutable version.
	inserted, err := b.db.InsertPackageVersionIfNew(ctx, v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "put version")
		return
	}
	if !inserted {
		writeError(w, http.StatusConflict, "version "+pkg.Version+" already exists")
		return
	}

	contentType := tarballMediaType
	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    pkg.Filename,
		BlobDigest:  digest,
		SizeBytes:   int64(len(pkg.Data)),
		ContentType: &contentType,
	}
	if err := b.db.PutPackageFile(ctx, file); err != nil {
		writeError(w, http.StatusInternalServerError, "put file")
		return
	}

	for _, tag := range pkg.DistTags {
		if err := b.db.PutPackageTag(ctx, dbPkg.ID, tag, pkg.Version); err != nil {
			writeError(w, http.StatusInternalServerError, "put dist-tag")
			return
		}
	}

	b.publishVersion(ctx, pkg.Name, v, file, stored.Integrity, stored.Shasum, versionMeta)
	for _, tag := range pkg.DistTags {
		b.publishTagSet(ctx, pkg.Name, tag, pkg.Version)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (b *Backend) handlePackument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := packageNameFromURL(r)

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}
	versions, err := b.db.ListPackageVersions(ctx, dbPkg.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list versions")
		return
	}
	tags, err := b.db.ListPackageTags(ctx, dbPkg.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list tags")
		return
	}

	pmVersions := make(map[string]*gtnpm.PackageMetadataVersion, len(versions))
	pmTime := make(map[string]time.Time, len(versions)+2)
	var modified time.Time
	for _, v := range versions {
		var stored storedVersion
		if err := json.Unmarshal(v.Metadata, &stored); err != nil || stored.Meta == nil {
			b.logger.Warn("npm packument: bad stored metadata", "package", name, "version", v.Version, "err", err)
			continue
		}
		stored.Meta.Dist.Integrity = stored.Integrity
		stored.Meta.Dist.Shasum = stored.Shasum
		stored.Meta.Dist.Tarball = b.tarballURL(name, tarballFilename(name, v.Version))
		pmVersions[v.Version] = stored.Meta
		pmTime[v.Version] = v.CreatedAt
		if v.CreatedAt.After(modified) {
			modified = v.CreatedAt
		}
	}
	if !modified.IsZero() {
		pmTime["modified"] = modified
		pmTime["created"] = versions[len(versions)-1].CreatedAt
	}

	distTags := make(map[string]string, len(tags))
	for _, t := range tags {
		distTags[t.Name] = t.Version
	}

	pm := gtnpm.PackageMetadata{
		ID:       name,
		Name:     name,
		DistTags: distTags,
		Versions: pmVersions,
		Time:     pmTime,
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(pm)
}

func (b *Backend) handleTarball(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := packageNameFromURL(r)
	tarball := chi.URLParam(r, "tarball")

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}
	file, err := b.findTarball(ctx, dbPkg, tarball)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup tarball")
		return
	}
	if file == nil {
		writeError(w, http.StatusNotFound, "tarball not found")
		return
	}

	rc, size, err := b.blobs.Open(ctx, file.BlobDigest)
	if err != nil {
		if errors.Is(err, blobstore.ErrBlobNotFound) {
			if b.redirectToPeer(ctx, w, r, file.BlobDigest, name, file.Filename) {
				return
			}
			writeError(w, http.StatusNotFound, "blob missing")
			return
		}
		writeError(w, http.StatusInternalServerError, "open blob")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", tarballMediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("ETag", `"`+file.BlobDigest+`"`)
	if r.Method == http.MethodHead {
		return
	}
	if _, err := io.Copy(w, rc); err != nil {
		b.logger.Warn("npm tarball: copy failed", "err", err, "package", name, "file", tarball)
	}
}

func (b *Backend) findTarball(ctx context.Context, pkg *database.Package, filename string) (*database.PackageFile, error) {
	version := versionFromTarballFilename(pkg.Name, filename)
	if version == "" {
		return nil, nil
	}
	v, err := b.db.GetPackageVersion(ctx, pkg.ID, version)
	if err != nil || v == nil {
		return nil, err
	}
	return b.db.GetPackageFile(ctx, v.ID, filename)
}

func versionFromTarballFilename(packageName, filename string) string {
	bare := packageName
	if _, after, ok := strings.Cut(packageName, "/"); ok {
		bare = after
	}
	prefix := bare + "-"
	suffix := ".tgz"
	if !strings.HasPrefix(filename, prefix) || !strings.HasSuffix(filename, suffix) {
		return ""
	}
	return filename[len(prefix) : len(filename)-len(suffix)]
}

func (b *Backend) handleDistTagsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := packageNameFromURL(r)

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}
	tags, err := b.db.ListPackageTags(ctx, dbPkg.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list tags")
		return
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[t.Name] = t.Version
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (b *Backend) handleDistTagPut(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := packageNameFromURL(r)
	tag := chi.URLParam(r, "tag")

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body")
		return
	}
	var version string
	if err := json.Unmarshal(body, &version); err != nil {
		writeError(w, http.StatusBadRequest, "body must be a JSON string")
		return
	}
	if version == "" {
		writeError(w, http.StatusBadRequest, "version is required")
		return
	}

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}
	v, err := b.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup version")
		return
	}
	if v == nil {
		writeError(w, http.StatusBadRequest, "version "+version+" does not exist")
		return
	}

	if err := b.db.PutPackageTag(ctx, dbPkg.ID, tag, version); err != nil {
		writeError(w, http.StatusInternalServerError, "put tag")
		return
	}
	b.publishTagSet(ctx, name, tag, version)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (b *Backend) handleDistTagDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := packageNameFromURL(r)
	tag := chi.URLParam(r, "tag")

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}
	if err := b.db.DeletePackageTag(ctx, dbPkg.ID, tag); err != nil {
		writeError(w, http.StatusInternalServerError, "delete tag")
		return
	}
	b.publishTagDelete(ctx, name, tag)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (b *Backend) tarballURL(name, filename string) string {
	return b.endpoint + routePrefix + "/" + name + "/-/" + filename
}

func tarballFilename(name, version string) string {
	bare := name
	if _, after, ok := strings.Cut(name, "/"); ok {
		bare = after
	}
	return fmt.Sprintf("%s-%s.tgz", bare, version)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":` + strconv.Quote(msg) + `}`))
}

func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b) //nolint:gosec // see import comment
	return h[:]
}

func sha512Sum(b []byte) []byte {
	h := sha512.Sum512(b)
	return h[:]
}
