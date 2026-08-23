package pypi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	gtpypi "gitea.dev/modules/packages/pypi"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/upstream"
)

const (
	maxUploadBytes   = 200 << 20
	fileMediaType    = "application/octet-stream"
	versionMediaType = "application/json"
)

type storedVersion struct {
	Meta *gtpypi.Metadata `json:"meta"`
}

func (b *Backend) handleUpload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil { //nolint:gosec // body bounded by MaxBytesReader above
		writePlainError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	name := strings.TrimSpace(r.FormValue("name"))
	version := strings.TrimSpace(r.FormValue("version"))
	if name == "" || version == "" {
		writePlainError(w, http.StatusBadRequest, "name and version are required")
		return
	}
	normalizedName := normalizeName(name)

	files := r.MultipartForm.File["content"]
	if len(files) == 0 {
		writePlainError(w, http.StatusBadRequest, "content file is required")
		return
	}
	header := files[0]
	if header.Size > maxUploadBytes {
		writePlainError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}
	src, err := header.Open()
	if err != nil {
		writePlainError(w, http.StatusBadRequest, "open upload: "+err.Error())
		return
	}
	defer func() { _ = src.Close() }()

	body, err := io.ReadAll(io.LimitReader(src, maxUploadBytes+1))
	if err != nil {
		writePlainError(w, http.StatusBadRequest, "read upload: "+err.Error())
		return
	}
	if int64(len(body)) > maxUploadBytes {
		writePlainError(w, http.StatusRequestEntityTooLarge, "upload too large")
		return
	}

	if expected := r.FormValue("sha256_digest"); expected != "" {
		got := sha256.Sum256(body)
		if !strings.EqualFold(hex.EncodeToString(got[:]), expected) {
			writePlainError(w, http.StatusBadRequest, "sha256_digest mismatch")
			return
		}
	}

	digest, _, err := b.blobs.Put(ctx, bytes.NewReader(body), "")
	if err != nil {
		b.logger.Error("pypi upload: blob store failed", "err", err, "name", normalizedName, "version", version)
		writePlainError(w, http.StatusInternalServerError, "store blob")
		return
	}
	mediaType := fileMediaType
	if err := b.db.PutBlob(ctx, digest, int64(len(body)), &mediaType, true); err != nil {
		writePlainError(w, http.StatusInternalServerError, "record blob")
		return
	}

	dbPkg, err := b.db.GetOrCreatePackage(ctx, packageType, normalizedName, b.owner)
	if err != nil {
		if errors.Is(err, database.ErrPackageOwnerMismatch) {
			writePlainError(w, http.StatusForbidden, "package is owned by another actor")
			return
		}
		b.logger.Error("pypi upload: get-or-create package failed", "err", err, "name", normalizedName)
		writePlainError(w, http.StatusInternalServerError, "package access")
		return
	}

	v, err := b.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup version")
		return
	}
	if v == nil {
		stored := storedVersion{Meta: metadataFromForm(r)}
		raw, err := json.Marshal(stored)
		if err != nil {
			writePlainError(w, http.StatusInternalServerError, "encode metadata")
			return
		}
		v = &database.PackageVersion{
			PackageID: dbPkg.ID,
			Version:   version,
			Metadata:  raw,
			MediaType: versionMediaType,
			SizeBytes: int64(len(raw)),
		}
		if err := b.db.PutPackageVersion(ctx, v); err != nil {
			writePlainError(w, http.StatusInternalServerError, "put version")
			return
		}
	}

	if existing, err := b.db.GetPackageFile(ctx, v.ID, header.Filename); err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup file")
		return
	} else if existing != nil {
		writePlainError(w, http.StatusConflict, "file already exists: "+header.Filename)
		return
	}

	contentType := contentTypeForFilename(header.Filename)
	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    header.Filename,
		BlobDigest:  digest,
		SizeBytes:   int64(len(body)),
		ContentType: &contentType,
	}
	if err := b.db.PutPackageFile(ctx, file); err != nil {
		writePlainError(w, http.StatusInternalServerError, "put file")
		return
	}

	b.publishFile(ctx, normalizedName, version, file, metadataFromForm(r))

	w.WriteHeader(http.StatusOK)
}

func metadataFromForm(r *http.Request) *gtpypi.Metadata {
	return &gtpypi.Metadata{
		Author:          r.FormValue("author"),
		Description:     r.FormValue("description"),
		LongDescription: r.FormValue("long_description"),
		Summary:         r.FormValue("summary"),
		ProjectURL:      r.FormValue("home_page"),
		License:         r.FormValue("license"),
		RequiresPython:  r.FormValue("requires_python"),
	}
}

func (b *Backend) handleSimpleRoot(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!DOCTYPE html><html><head><title>Simple Index</title></head><body>\n")
	// Stream the index in name-ordered batches via the cursor instead of loading
	// the whole package table into memory, which would be a DoS vector on this
	// public endpoint.
	const batch = 1000
	startAfter := ""
	for {
		pkgs, err := b.db.ListPackages(ctx, packageType, startAfter, batch)
		if err != nil {
			b.logger.Error("pypi simple index: list packages", "err", err)
			return
		}
		for _, p := range pkgs {
			_, _ = fmt.Fprintf(w, "<a href=\"%s/\">%s</a><br>\n", html.EscapeString(p.Name), html.EscapeString(p.Name))
		}
		if len(pkgs) < batch {
			break
		}
		startAfter = pkgs[len(pkgs)-1].Name
	}
	_, _ = io.WriteString(w, "</body></html>\n")
}

func (b *Backend) handleSimplePackage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := normalizeName(chi.URLParam(r, "name"))

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup package")
		return
	}

	// Locally-pushed projects always shadow upstream. Anything else — unknown
	// or previously cached from upstream — asks upstream first so new releases
	// appear, with the cached listing as the fallback.
	if dbPkg == nil || dbPkg.OwnerID == upstreamOwner {
		if b.upstreamEnabled() {
			proj, err := b.upstream.FetchProject(ctx, name)
			switch {
			case err == nil:
				b.writeUpstreamIndex(w, name, proj)
				return
			case errors.Is(err, upstream.ErrProjectNotFound):
				// fall through to the local listing / 404 below
			default:
				b.logger.Warn("pypi upstream: index fetch failed", "project", name, "err", err)
				// fall through: serve the cached listing if we have one
			}
		}
		if dbPkg == nil {
			writePlainError(w, http.StatusNotFound, "project not found")
			return
		}
	}

	versions, err := b.db.ListPackageVersions(ctx, dbPkg.ID)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "list versions")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!DOCTYPE html><html><head><title>Links for %s</title></head><body>\n<h1>Links for %s</h1>\n", html.EscapeString(name), html.EscapeString(name))

	for _, v := range versions {
		files, err := b.db.ListPackageFiles(ctx, v.ID)
		if err != nil {
			b.logger.Warn("pypi simple: list files failed", "err", err, "name", name, "version", v.Version)
			continue
		}
		var stored storedVersion
		_ = json.Unmarshal(v.Metadata, &stored)
		for _, f := range files {
			href := html.EscapeString(b.fileURL(name, v.Version, f.Filename) + "#sha256=" + cksumFromDigest(f.BlobDigest))
			extra := ""
			if stored.Meta != nil && stored.Meta.RequiresPython != "" {
				extra = ` data-requires-python="` + html.EscapeString(stored.Meta.RequiresPython) + `"`
			}
			_, _ = fmt.Fprintf(w, "<a href=\"%s\"%s>%s</a><br>\n", href, extra, html.EscapeString(f.Filename)) //nolint:gosec // all interpolated values are html.EscapeString'd
		}
	}

	_, _ = io.WriteString(w, "</body></html>\n")
}

func (b *Backend) handleDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := normalizeName(chi.URLParam(r, "name"))
	version := chi.URLParam(r, "version")
	filename := chi.URLParam(r, "filename")

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, "lookup package")
		return
	}

	var v *database.PackageVersion
	var file *database.PackageFile
	if dbPkg != nil {
		v, err = b.db.GetPackageVersion(ctx, dbPkg.ID, version)
		if err != nil {
			writePlainError(w, http.StatusInternalServerError, "lookup version")
			return
		}
		if v != nil {
			file, err = b.db.GetPackageFile(ctx, v.ID, filename)
			if err != nil {
				writePlainError(w, http.StatusInternalServerError, "lookup file")
				return
			}
		}
	}

	// Cache-fill on miss — but never for locally-pushed projects, which always
	// shadow upstream entirely.
	if file == nil && b.upstreamEnabled() && (dbPkg == nil || dbPkg.OwnerID == upstreamOwner) {
		file, err = b.cacheFromUpstream(ctx, name, version, filename)
		if err != nil {
			b.logger.Warn("pypi upstream: cache fill failed", "project", name, "version", version, "file", filename, "err", err)
			writePlainError(w, http.StatusBadGateway, "upstream fetch failed")
			return
		}
	}
	if file == nil {
		switch {
		case dbPkg == nil:
			writePlainError(w, http.StatusNotFound, "project not found")
		case v == nil:
			writePlainError(w, http.StatusNotFound, "version not found")
		default:
			writePlainError(w, http.StatusNotFound, "file not found")
		}
		return
	}

	rc, size, err := b.blobs.Open(ctx, file.BlobDigest)
	if err != nil {
		if errors.Is(err, blobstore.ErrBlobNotFound) {
			if b.redirectToPeer(ctx, w, r, file.BlobDigest, name, version, filename) {
				return
			}
			writePlainError(w, http.StatusNotFound, "blob missing")
			return
		}
		writePlainError(w, http.StatusInternalServerError, "open blob")
		return
	}
	defer func() { _ = rc.Close() }()

	contentType := fileMediaType
	if file.ContentType != nil {
		contentType = *file.ContentType
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("ETag", `"`+file.BlobDigest+`"`)
	if _, err := io.Copy(w, rc); err != nil {
		b.logger.Warn("pypi download: copy failed", "err", err, "project", name, "version", version, "file", filename)
	}
}

// cacheFromUpstream backfills one distribution file from the upstream index:
// verify the upstream's declared sha256 BEFORE storing (a corrupt or tampered
// file must never enter the cache), then persist through the same store calls
// as a local upload, owned by the upstream sentinel. Returns (nil, nil) when
// the upstream doesn't have the project or the file.
func (b *Backend) cacheFromUpstream(ctx context.Context, name, version, filename string) (*database.PackageFile, error) {
	proj, err := b.upstream.FetchProject(ctx, name)
	if errors.Is(err, upstream.ErrProjectNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("fetching upstream index: %w", err)
	}

	var entry *upstream.PyPIProjectFile
	for i := range proj.Files {
		if proj.Files[i].Filename == filename {
			entry = &proj.Files[i]
			break
		}
	}
	if entry == nil {
		return nil, nil
	}
	if v, ok := versionFromFilename(filename); !ok || v != version {
		return nil, nil
	}
	expected := entry.Hashes["sha256"]
	if expected == "" {
		return nil, fmt.Errorf("upstream index has no sha256 for %s", filename)
	}

	body, err := b.upstream.FetchFile(ctx, entry.URL)
	if err != nil {
		return nil, fmt.Errorf("fetching upstream file: %w", err)
	}
	got := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(got[:]), expected) {
		return nil, fmt.Errorf("upstream file %s sha256 mismatch (index says %s)", filename, expected)
	}

	digest, _, err := b.blobs.Put(ctx, bytes.NewReader(body), "")
	if err != nil {
		return nil, fmt.Errorf("storing blob: %w", err)
	}
	mediaType := fileMediaType
	if err := b.db.PutBlob(ctx, digest, int64(len(body)), &mediaType, true); err != nil {
		return nil, fmt.Errorf("recording blob: %w", err)
	}

	dbPkg, err := b.db.GetOrCreatePackage(ctx, packageType, name, upstreamOwner)
	if err != nil {
		return nil, fmt.Errorf("creating cached package: %w", err)
	}
	v, err := b.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		return nil, fmt.Errorf("looking up cached version: %w", err)
	}
	if v == nil {
		stored := storedVersion{Meta: &gtpypi.Metadata{RequiresPython: entry.RequiresPython}}
		raw, err := json.Marshal(stored)
		if err != nil {
			return nil, fmt.Errorf("encoding cached metadata: %w", err)
		}
		v = &database.PackageVersion{
			PackageID: dbPkg.ID,
			Version:   version,
			Metadata:  raw,
			MediaType: versionMediaType,
			SizeBytes: int64(len(raw)),
		}
		if err := b.db.PutPackageVersion(ctx, v); err != nil {
			return nil, fmt.Errorf("recording cached version: %w", err)
		}
	}
	contentType := contentTypeForFilename(filename)
	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    filename,
		BlobDigest:  digest,
		SizeBytes:   int64(len(body)),
		ContentType: &contentType,
	}
	if err := b.db.PutPackageFile(ctx, file); err != nil {
		return nil, fmt.Errorf("recording cached file: %w", err)
	}
	// Deliberately NO publishFile: upstream-cached content is never federated
	// (same stance as goproxy's cacheFromUpstream).
	b.logger.Info("pypi: cached from upstream", "project", name, "version", version, "file", filename, "bytes", len(body))
	return file, nil
}

func (b *Backend) fileURL(name, version, filename string) string {
	return b.endpoint + routePrefix + "/files/" +
		url.PathEscape(name) + "/" +
		url.PathEscape(version) + "/" +
		url.PathEscape(filename)
}

func contentTypeForFilename(filename string) string {
	switch {
	case strings.HasSuffix(filename, ".whl"):
		return "application/octet-stream"
	case strings.HasSuffix(filename, ".tar.gz"), strings.HasSuffix(filename, ".tgz"):
		return "application/x-gtar"
	case strings.HasSuffix(filename, ".zip"):
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

func cksumFromDigest(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}

func writePlainError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n") //nolint:gosec // text/plain response, msg is from server-side strings
}

// versionFromFilename extracts the version from a wheel (PEP 427:
// name-version[-build]-python-abi-platform.whl) or sdist (PEP 625:
// name-version.tar.gz/.tgz/.zip) filename. Distribution names in these
// filenames use underscores, so the version is the second dash-separated
// field in wheels and the last dash-separated field in sdists.
func versionFromFilename(filename string) (string, bool) {
	base := filename
	isWheel := false
	switch {
	case strings.HasSuffix(base, ".whl"):
		base = strings.TrimSuffix(base, ".whl")
		isWheel = true
	case strings.HasSuffix(base, ".tar.gz"):
		base = strings.TrimSuffix(base, ".tar.gz")
	case strings.HasSuffix(base, ".tgz"):
		base = strings.TrimSuffix(base, ".tgz")
	case strings.HasSuffix(base, ".zip"):
		base = strings.TrimSuffix(base, ".zip")
	default:
		return "", false
	}
	parts := strings.Split(base, "-")
	if isWheel {
		// name-version[-build]-python-abi-platform → at least 5 fields.
		if len(parts) < 5 {
			return "", false
		}
		return parts[1], true
	}
	if len(parts) < 2 {
		return "", false
	}
	v := parts[len(parts)-1]
	if v == "" || !strings.ContainsAny(v, "0123456789") {
		return "", false
	}
	return v, true
}

// writeUpstreamIndex renders a PEP 503 HTML index from upstream PEP 691
// metadata, pointing every href at our own file route so downloads flow
// through the cache. The upstream's sha256 rides the fragment so pip's
// verification works before we have the file.
func (b *Backend) writeUpstreamIndex(w http.ResponseWriter, name string, proj *upstream.PyPIProject) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w, "<!DOCTYPE html><html><head><title>Links for %s</title></head><body>\n<h1>Links for %s</h1>\n", html.EscapeString(name), html.EscapeString(name))
	for _, f := range proj.Files {
		version, ok := versionFromFilename(f.Filename)
		if !ok {
			b.logger.Debug("pypi upstream: skipping file with unparseable version", "file", f.Filename)
			continue
		}
		// href (including the upstream-controlled #sha256= fragment) is raw
		// here — html.EscapeString at the Fprintf write site below is the
		// single escape point; keep them together so a future edit can't
		// move the escape and open an XSS surface.
		href := b.fileURL(name, version, f.Filename)
		if sha := f.Hashes["sha256"]; sha != "" {
			href += "#sha256=" + sha
		}
		extra := ""
		if f.RequiresPython != "" {
			extra = ` data-requires-python="` + html.EscapeString(f.RequiresPython) + `"`
		}
		_, _ = fmt.Fprintf(w, "<a href=\"%s\"%s>%s</a><br>\n", html.EscapeString(href), extra, html.EscapeString(f.Filename)) //nolint:gosec // all interpolated values are html.EscapeString'd
	}
	_, _ = io.WriteString(w, "</body></html>\n")
}
