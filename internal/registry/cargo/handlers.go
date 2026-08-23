package cargo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	gtcargo "gitea.dev/modules/packages/cargo"

	"git.erwanleboucher.dev/eleboucher/apoci/internal/blobstore"
	"git.erwanleboucher.dev/eleboucher/apoci/internal/database"
)

const (
	maxPublishBytes  = 200 << 20
	crateMediaType   = "application/octet-stream"
	versionMediaType = "application/json"
)

type storedVersion struct {
	Meta   *gtcargo.Metadata `json:"meta"`
	Cksum  string            `json:"cksum"` // hex sha256 of the .crate bytes
	Yanked bool              `json:"yanked"`
}

// One line per version in the sparse index, per Cargo's registry spec.
type indexEntry struct {
	Name     string              `json:"name"`
	Vers     string              `json:"vers"`
	Deps     []indexDep          `json:"deps"`
	Cksum    string              `json:"cksum"`
	Features map[string][]string `json:"features,omitempty"`
	Yanked   bool                `json:"yanked"`
	Links    *string             `json:"links,omitempty"`
	V        int                 `json:"v"`
}

type indexDep struct {
	Name            string   `json:"name"`
	Req             string   `json:"req"`
	Features        []string `json:"features"`
	Optional        bool     `json:"optional"`
	DefaultFeatures bool     `json:"default_features"`
	Target          *string  `json:"target,omitempty"`
	Kind            string   `json:"kind"`
	Registry        *string  `json:"registry,omitempty"`
	Package         *string  `json:"package,omitempty"`
}

func (b *Backend) handleConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	cfg := map[string]any{
		"dl":            b.endpoint + routePrefix + "/api/v1/crates",
		"api":           b.endpoint + routePrefix,
		"auth-required": b.token != "",
	}
	_ = json.NewEncoder(w).Encode(cfg)
}

func (b *Backend) handlePublish(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxPublishBytes+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	if int64(len(body)) > maxPublishBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "publish body too large")
		return
	}

	pkg, err := gtcargo.ParsePackage(bytes.NewReader(body))
	if err != nil {
		b.logger.Info("cargo publish parse failed", "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Cargo treats crate names case-insensitively and every read path lowercases
	// the name, so store under the lowercase form too; otherwise a crate
	// published as "MyCrate" is unreachable (404) on index/download/yank.
	pkg.Name = strings.ToLower(pkg.Name)
	crateBytes, err := io.ReadAll(pkg.Content)
	if err != nil {
		writeError(w, http.StatusBadRequest, "read crate: "+err.Error())
		return
	}

	digest, _, err := b.blobs.Put(ctx, bytes.NewReader(crateBytes), "")
	if err != nil {
		b.logger.Error("cargo publish: blob store failed", "err", err, "name", pkg.Name, "version", pkg.Version)
		writeError(w, http.StatusInternalServerError, "store blob")
		return
	}
	mediaType := crateMediaType
	if err := b.db.PutBlob(ctx, digest, int64(len(crateBytes)), &mediaType, true); err != nil {
		writeError(w, http.StatusInternalServerError, "record blob")
		return
	}

	dbPkg, err := b.db.GetOrCreatePackage(ctx, packageType, pkg.Name, b.owner)
	if err != nil {
		if errors.Is(err, database.ErrPackageOwnerMismatch) {
			writeError(w, http.StatusForbidden, "package is owned by another actor")
			return
		}
		b.logger.Error("cargo publish: get-or-create package failed", "err", err, "name", pkg.Name)
		writeError(w, http.StatusInternalServerError, "package access")
		return
	}

	stored := storedVersion{
		Meta:   pkg.Metadata,
		Cksum:  cksumFromDigest(digest),
		Yanked: false,
	}
	metaBytes, err := json.Marshal(stored)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode metadata")
		return
	}

	v := &database.PackageVersion{
		PackageID: dbPkg.ID,
		Version:   pkg.Version,
		Metadata:  metaBytes,
		MediaType: versionMediaType,
		SizeBytes: int64(len(metaBytes)),
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

	contentType := crateMediaType
	file := &database.PackageFile{
		VersionID:   v.ID,
		Filename:    crateFilename(pkg.Name, pkg.Version),
		BlobDigest:  digest,
		SizeBytes:   int64(len(crateBytes)),
		ContentType: &contentType,
	}
	if err := b.db.PutPackageFile(ctx, file); err != nil {
		writeError(w, http.StatusInternalServerError, "put file")
		return
	}

	b.publishVersion(ctx, pkg.Name, v, file, stored.Cksum, pkg.Metadata)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"warnings":{"invalid_categories":[],"invalid_badges":[],"other":[]}}`))
}

func (b *Backend) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.ToLower(chi.URLParam(r, "name"))

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "crate not found")
		return
	}
	versions, err := b.db.ListPackageVersions(ctx, dbPkg.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list versions")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	enc := json.NewEncoder(w)
	for _, v := range versions {
		var stored storedVersion
		if err := json.Unmarshal(v.Metadata, &stored); err != nil || stored.Meta == nil {
			b.logger.Warn("cargo index: bad stored metadata", "crate", name, "version", v.Version, "err", err)
			continue
		}
		entry := buildIndexEntry(name, v.Version, &stored)
		if err := enc.Encode(entry); err != nil {
			b.logger.Warn("cargo index: encode failed", "err", err)
			return
		}
	}
}

func (b *Backend) handleDownload(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.ToLower(chi.URLParam(r, "name"))
	version := chi.URLParam(r, "version")

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "crate not found")
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
	file, err := b.db.GetPackageFile(ctx, v.ID, crateFilename(name, version))
	if err != nil || file == nil {
		writeError(w, http.StatusNotFound, "crate file missing")
		return
	}

	rc, size, err := b.blobs.Open(ctx, file.BlobDigest)
	if err != nil {
		if errors.Is(err, blobstore.ErrBlobNotFound) {
			if b.redirectToPeer(ctx, w, r, file.BlobDigest, name, version) {
				return
			}
			writeError(w, http.StatusNotFound, "blob missing")
			return
		}
		writeError(w, http.StatusInternalServerError, "open blob")
		return
	}
	defer func() { _ = rc.Close() }()

	w.Header().Set("Content-Type", crateMediaType)
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.Header().Set("ETag", `"`+file.BlobDigest+`"`)
	if _, err := io.Copy(w, rc); err != nil {
		b.logger.Warn("cargo download: copy failed", "err", err, "crate", name, "version", version)
	}
}

func (b *Backend) handleYank(w http.ResponseWriter, r *http.Request) {
	b.setYanked(w, r, true)
}

func (b *Backend) handleUnyank(w http.ResponseWriter, r *http.Request) {
	b.setYanked(w, r, false)
}

func (b *Backend) setYanked(w http.ResponseWriter, r *http.Request, yanked bool) {
	ctx := r.Context()
	name := strings.ToLower(chi.URLParam(r, "name"))
	version := chi.URLParam(r, "version")

	dbPkg, err := b.db.GetPackage(ctx, packageType, name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "lookup package")
		return
	}
	if dbPkg == nil {
		writeError(w, http.StatusNotFound, "crate not found")
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
	if err := json.Unmarshal(v.Metadata, &stored); err != nil {
		writeError(w, http.StatusInternalServerError, "decode metadata")
		return
	}
	if stored.Yanked == yanked {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
		return
	}
	stored.Yanked = yanked
	updated, err := json.Marshal(stored)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode metadata")
		return
	}
	v.Metadata = updated
	v.SizeBytes = int64(len(updated))
	if err := b.db.PutPackageVersion(ctx, v); err != nil {
		writeError(w, http.StatusInternalServerError, "update version")
		return
	}

	b.publishYank(ctx, name, version, yanked)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func buildIndexEntry(name, version string, stored *storedVersion) indexEntry {
	entry := indexEntry{
		Name:     name,
		Vers:     version,
		Deps:     make([]indexDep, 0, len(stored.Meta.Dependencies)),
		Cksum:    stored.Cksum,
		Features: stored.Meta.Features,
		Yanked:   stored.Yanked,
		V:        2,
	}
	for _, dep := range stored.Meta.Dependencies {
		entry.Deps = append(entry.Deps, indexDep{
			Name:            dep.Name,
			Req:             dep.Req,
			Features:        dep.Features,
			Optional:        dep.Optional,
			DefaultFeatures: dep.DefaultFeatures,
			Target:          dep.Target,
			Kind:            dep.Kind,
			Registry:        dep.Registry,
			Package:         dep.Package,
		})
	}
	if stored.Meta.Links != "" {
		links := stored.Meta.Links
		entry.Links = &links
	}
	return entry
}

func crateFilename(name, version string) string {
	return fmt.Sprintf("%s-%s.crate", name, version)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"errors":[{"detail":` + strconv.Quote(msg) + `}]}`))
}

// blobstore returns "sha256:<hex>"; cargo's cksum is the bare hex.
func cksumFromDigest(digest string) string {
	return strings.TrimPrefix(digest, "sha256:")
}
