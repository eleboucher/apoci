package nuget

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	gtnuget "gitea.dev/modules/packages/nuget"
	"github.com/go-chi/chi/v5"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	maxPushBytes     = 500 << 20 // 500 MiB
	nupkgMediaType   = "application/octet-stream"
	versionMediaType = "application/json"
)

type storedVersion struct {
	ID          string `json:"id"`
	Authors     string `json:"authors"`
	Description string `json:"description"`
	ProjectURL  string `json:"projectUrl"`
	Tags        string `json:"tags"`
}

type serviceIndex struct {
	Version   string            `json:"version"`
	Resources []serviceResource `json:"resources"`
}

type serviceResource struct {
	ID      string `json:"@id"`
	Type    string `json:"@type"`
	Comment string `json:"comment,omitempty"`
}

func (b *Backend) handleServiceIndex(w http.ResponseWriter, r *http.Request) {
	base := b.endpoint + routePrefix
	idx := serviceIndex{
		Version: "3.0.0",
		Resources: []serviceResource{
			{
				ID:      base + "/v3-flatcontainer/",
				Type:    "PackageBaseAddress/3.0.0",
				Comment: "Base URL for NuGet package downloads",
			},
			{
				ID:      base + "/v3/package",
				Type:    "PackagePublish/2.0.0",
				Comment: "Endpoint for pushing and delisting packages",
			},
			{
				ID:      base + "/v3/registration/",
				Type:    "RegistrationsBaseUrl/3.0.0",
				Comment: "Base URL for package registration info",
			},
		},
	}
	writeJSON(w, idx)
}

func (b *Backend) handlePush(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	r.Body = http.MaxBytesReader(w, r.Body, maxPushBytes)
	if err := r.ParseMultipartForm(maxPushBytes); err != nil { //nolint:gosec // body bounded by MaxBytesReader above
		writeError(w, http.StatusBadRequest, "parse multipart: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	files := r.MultipartForm.File["package"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "package file is required")
		return
	}
	src, err := files[0].Open()
	if err != nil {
		writeError(w, http.StatusBadRequest, "open package: "+err.Error())
		return
	}
	defer func() { _ = src.Close() }()

	body, err := io.ReadAll(io.LimitReader(src, maxPushBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read package: "+err.Error())
		return
	}
	if int64(len(body)) > maxPushBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "package too large")
		return
	}

	pkg, err := gtnuget.ParsePackageMetaData(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid nupkg: "+err.Error())
		return
	}

	pkgID := normalizeID(pkg.ID)
	version := pkg.Version
	filename := pkgID + "." + strings.ToLower(version) + ".nupkg"

	digest, _, err := b.blobs.Put(ctx, bytes.NewReader(body), "")
	if err != nil {
		b.logger.Error("nuget push: blob store failed", "err", err, "id", pkgID, "version", version)
		writeError(w, http.StatusInternalServerError, "store blob")
		return
	}
	ct := nupkgMediaType
	if err := b.db.PutBlob(ctx, digest, int64(len(body)), &ct, true); err != nil {
		writeError(w, http.StatusInternalServerError, "record blob")
		return
	}

	dbPkg, err := b.db.GetOrCreatePackage(ctx, packageType, pkgID, b.owner)
	if err != nil {
		if errors.Is(err, database.ErrPackageOwnerMismatch) {
			writeError(w, http.StatusForbidden, "package is owned by another actor")
			return
		}
		b.logger.Error("nuget push: get-or-create package failed", "err", err, "id", pkgID)
		writeError(w, http.StatusInternalServerError, "package access")
		return
	}

	existing, err := b.db.GetPackageVersion(ctx, dbPkg.ID, version)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup version")
		return
	}
	if existing != nil {
		writeError(w, http.StatusConflict, "version already exists")
		return
	}

	stored := storedVersion{
		ID:          pkg.ID,
		Authors:     pkg.Metadata.Authors,
		Description: pkg.Metadata.Description,
		ProjectURL:  pkg.Metadata.ProjectURL,
		Tags:        pkg.Metadata.Tags,
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode metadata")
		return
	}
	v := &database.PackageVersion{
		PackageID: dbPkg.ID,
		Version:   version,
		Metadata:  raw,
		MediaType: versionMediaType,
		SizeBytes: int64(len(raw)),
	}
	if err := b.db.PutPackageVersion(ctx, v); err != nil {
		writeError(w, http.StatusInternalServerError, "put version")
		return
	}

	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    filename,
		BlobDigest:  digest,
		SizeBytes:   int64(len(body)),
		ContentType: &ct,
	}
	if err := b.db.PutPackageFile(ctx, file); err != nil {
		writeError(w, http.StatusInternalServerError, "put file")
		return
	}

	b.publishPackage(ctx, pkgID, version, file, stored)

	w.WriteHeader(http.StatusCreated)
}

func (b *Backend) handleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := normalizeID(chi.URLParam(r, "id"))
	version := chi.URLParam(r, "version")

	dbPkg, err := b.db.GetPackage(ctx, packageType, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "package not found")
		return
	}

	if err := b.db.DeletePackageVersion(ctx, dbPkg.ID, version); err != nil {
		writeError(w, http.StatusInternalServerError, "delete version")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (b *Backend) handleVersionList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := normalizeID(chi.URLParam(r, "id"))

	dbPkg, err := b.db.GetPackage(ctx, packageType, id)
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

	vs := make([]string, 0, len(versions))
	for _, v := range versions {
		vs = append(vs, strings.ToLower(v.Version))
	}
	writeJSON(w, map[string][]string{"versions": vs})
}

func (b *Backend) handleDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := normalizeID(chi.URLParam(r, "id"))
	version := chi.URLParam(r, "version")
	filename := chi.URLParam(r, "filename")

	dbPkg, err := b.db.GetPackage(ctx, packageType, id)
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
		writeError(w, http.StatusNotFound, "version not found")
		return
	}

	file, err := b.db.GetPackageFile(ctx, v.ID, filename)
	if err != nil || file == nil {
		writeError(w, http.StatusNotFound, "file not found")
		return
	}

	rc, size, err := b.blobs.Open(ctx, file.BlobDigest)
	if err != nil {
		if errors.Is(err, blobstore.ErrBlobNotFound) {
			if b.redirectToPeer(ctx, w, r, file.BlobDigest, id, version, filename) {
				return
			}
			writeError(w, http.StatusNotFound, "blob missing")
			return
		}
		writeError(w, http.StatusInternalServerError, "open blob")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", nupkgMediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	if _, err := io.Copy(w, rc); err != nil {
		b.logger.Warn("nuget download: copy failed", "err", err, "id", id, "version", version)
	}
}

type registrationIndex struct {
	ID    string                  `json:"@id"`
	Count int                     `json:"count"`
	Items []registrationIndexPage `json:"items"`
}

type registrationIndexPage struct {
	ID    string             `json:"@id"`
	Count int                `json:"count"`
	Items []registrationLeaf `json:"items"`
	Lower string             `json:"lower"`
	Upper string             `json:"upper"`
}

type registrationLeaf struct {
	ID             string       `json:"@id"`
	CatalogEntry   catalogEntry `json:"catalogEntry"`
	PackageContent string       `json:"packageContent"`
	Registration   string       `json:"registration"`
}

type catalogEntry struct {
	ID          string `json:"@id"`
	PackageID   string `json:"id"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Authors     string `json:"authors,omitempty"`
	ProjectURL  string `json:"projectUrl,omitempty"`
	Tags        string `json:"tags,omitempty"`
	Published   string `json:"published"`
}

func (b *Backend) handleRegistrationIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := normalizeID(chi.URLParam(r, "id"))

	dbPkg, err := b.db.GetPackage(ctx, packageType, id)
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

	leaves := make([]registrationLeaf, 0, len(versions))
	for _, v := range versions {
		var stored storedVersion
		_ = json.Unmarshal(v.Metadata, &stored)
		leaves = append(leaves, b.buildLeaf(id, v.Version, v.CreatedAt, stored))
	}

	// NuGet V3 registration indices must list leaves ascending by version;
	// ListPackageVersions has no ordering guarantee, so sort here. This also
	// makes lowerVersion/upperVersion (first/last) correct.
	sort.SliceStable(leaves, func(i, j int) bool {
		return semver.Compare("v"+leaves[i].CatalogEntry.Version, "v"+leaves[j].CatalogEntry.Version) < 0
	})

	regBase := b.registrationBase(id)
	idx := registrationIndex{
		ID:    regBase + "index.json",
		Count: 1,
		Items: []registrationIndexPage{{
			ID:    regBase + "index.json",
			Count: len(leaves),
			Items: leaves,
			Lower: lowerVersion(leaves),
			Upper: upperVersion(leaves),
		}},
	}
	writeJSON(w, idx)
}

func (b *Backend) handleRegistrationLeaf(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := normalizeID(chi.URLParam(r, "id"))
	slug := chi.URLParam(r, "slug")
	version := strings.TrimSuffix(slug, ".json")

	if !strings.HasSuffix(slug, ".json") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	dbPkg, err := b.db.GetPackage(ctx, packageType, id)
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
		writeError(w, http.StatusNotFound, "version not found")
		return
	}

	var stored storedVersion
	_ = json.Unmarshal(v.Metadata, &stored)
	leaf := b.buildLeaf(id, v.Version, v.CreatedAt, stored)
	writeJSON(w, leaf)
}

func (b *Backend) buildLeaf(id, version string, createdAt time.Time, stored storedVersion) registrationLeaf {
	regBase := b.registrationBase(id)
	pkgContent := b.endpoint + routePrefix + "/v3-flatcontainer/" +
		url.PathEscape(id) + "/" + url.PathEscape(strings.ToLower(version)) + "/" +
		url.PathEscape(id+"."+strings.ToLower(version)+".nupkg")
	leafID := regBase + url.PathEscape(strings.ToLower(version)) + ".json"
	displayID := stored.ID
	if displayID == "" {
		displayID = id
	}
	entry := catalogEntry{
		ID:          leafID,
		PackageID:   displayID,
		Version:     version,
		Description: stored.Description,
		Authors:     stored.Authors,
		ProjectURL:  stored.ProjectURL,
		Tags:        stored.Tags,
		Published:   createdAt.UTC().Format(time.RFC3339),
	}
	return registrationLeaf{
		ID:             leafID,
		CatalogEntry:   entry,
		PackageContent: pkgContent,
		Registration:   regBase + "index.json",
	}
}

func (b *Backend) registrationBase(id string) string {
	return b.endpoint + routePrefix + "/v3/registration/" + url.PathEscape(id) + "/"
}

func lowerVersion(leaves []registrationLeaf) string {
	if len(leaves) == 0 {
		return ""
	}
	return strings.ToLower(leaves[0].CatalogEntry.Version)
}

func upperVersion(leaves []registrationLeaf) string {
	if len(leaves) == 0 {
		return ""
	}
	return strings.ToLower(leaves[len(leaves)-1].CatalogEntry.Version)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n") //nolint:gosec // text/plain, server-side message
}
