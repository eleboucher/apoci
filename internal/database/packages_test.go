package database

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPackageGetOrCreate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "npm", "@scope/foo", testAliceActor)
	require.NoError(t, err)
	require.Equal(t, "npm", pkg.Type)
	require.Equal(t, "@scope/foo", pkg.Name)
	require.Equal(t, testAliceActor, pkg.OwnerID)
	require.False(t, pkg.Private)
	require.NotZero(t, pkg.ID)

	again, err := db.GetOrCreatePackage(ctx, "npm", "@scope/foo", testAliceActor)
	require.NoError(t, err)
	require.Equal(t, pkg.ID, again.ID)

	_, err = db.GetOrCreatePackage(ctx, "npm", "@scope/foo", "https://bob.example.com/ap/actor")
	require.Error(t, err)

	// Same name under different type is fine.
	mvn, err := db.GetOrCreatePackage(ctx, "maven", "@scope/foo", testAliceActor)
	require.NoError(t, err)
	require.NotEqual(t, pkg.ID, mvn.ID)
}

func TestPackageGetMissing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	got, err := db.GetPackage(ctx, "npm", "missing")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestSetPackagePrivate(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "npm", "lodash", testAliceActor)
	require.NoError(t, err)
	require.False(t, pkg.Private)

	require.NoError(t, db.SetPackagePrivate(ctx, pkg.ID, true))
	got, err := db.GetPackage(ctx, "npm", "lodash")
	require.NoError(t, err)
	require.True(t, got.Private)

	require.NoError(t, db.SetPackagePrivate(ctx, pkg.ID, false))
	got, err = db.GetPackage(ctx, "npm", "lodash")
	require.NoError(t, err)
	require.False(t, got.Private)
}

func TestListPackages(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	for _, name := range []string{"alpha", "bravo", "charlie", "delta"} {
		_, err := db.GetOrCreatePackage(ctx, "npm", name, testAliceActor)
		require.NoError(t, err)
	}
	// Different type should not appear.
	_, err := db.GetOrCreatePackage(ctx, "maven", "alpha", testAliceActor)
	require.NoError(t, err)

	all, err := db.ListPackages(ctx, "npm", "", 10)
	require.NoError(t, err)
	require.Len(t, all, 4)
	require.Equal(t, "alpha", all[0].Name)
	require.Equal(t, "delta", all[3].Name)

	page, err := db.ListPackages(ctx, "npm", "bravo", 2)
	require.NoError(t, err)
	require.Len(t, page, 2)
	require.Equal(t, "charlie", page[0].Name)
	require.Equal(t, "delta", page[1].Name)
}

func TestPackageVersionPutGet(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "npm", "left-pad", testAliceActor)
	require.NoError(t, err)

	v := &PackageVersion{
		PackageID: pkg.ID,
		Version:   testVersion100,
		Metadata:  []byte(`{"name":"left-pad","version":"1.0.0"}`),
	}
	require.NoError(t, db.PutPackageVersion(ctx, v))
	require.NotZero(t, v.ID)
	require.NotZero(t, v.CreatedAt)

	got, err := db.GetPackageVersion(ctx, pkg.ID, testVersion100)
	require.NoError(t, err)
	require.Equal(t, v.ID, got.ID)
	require.JSONEq(t, `{"name":"left-pad","version":"1.0.0"}`, string(got.Metadata))
	require.Nil(t, got.SourceActor)

	// Upsert overwrites metadata and source_actor.
	source := "https://bob.example.com/ap/actor"
	v.Metadata = []byte(`{"name":"left-pad","version":"1.0.0","updated":true}`)
	v.SourceActor = &source
	require.NoError(t, db.PutPackageVersion(ctx, v))

	got, err = db.GetPackageVersion(ctx, pkg.ID, testVersion100)
	require.NoError(t, err)
	require.JSONEq(t, `{"name":"left-pad","version":"1.0.0","updated":true}`, string(got.Metadata))
	require.NotNil(t, got.SourceActor)
	require.Equal(t, source, *got.SourceActor)
}

func TestListPackageVersions(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "npm", "react", testAliceActor)
	require.NoError(t, err)

	for _, ver := range []string{"18.0.0", "18.1.0", "18.2.0"} {
		require.NoError(t, db.PutPackageVersion(ctx, &PackageVersion{
			PackageID: pkg.ID,
			Version:   ver,
			Metadata:  []byte(`{}`),
		}))
	}

	versions, err := db.ListPackageVersions(ctx, pkg.ID)
	require.NoError(t, err)
	require.Len(t, versions, 3)
}

func TestDeletePackageVersion(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "npm", "vue", testAliceActor)
	require.NoError(t, err)

	v := &PackageVersion{PackageID: pkg.ID, Version: "3.0.0", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))

	require.NoError(t, db.PutPackageFile(ctx, &PackageFile{
		VersionID:  v.ID,
		Filename:   "vue-3.0.0.tgz",
		BlobDigest: "sha256:aaa",
		SizeBytes:  100,
	}))
	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "latest", "3.0.0"))

	require.NoError(t, db.DeletePackageVersion(ctx, pkg.ID, "3.0.0"))

	got, err := db.GetPackageVersion(ctx, pkg.ID, "3.0.0")
	require.NoError(t, err)
	require.Nil(t, got)
	files, err := db.ListPackageFiles(ctx, v.ID)
	require.NoError(t, err)
	require.Empty(t, files)
	tags, err := db.ListPackageTags(ctx, pkg.ID)
	require.NoError(t, err)
	require.Empty(t, tags)

	// Deleting a non-existent version is a no-op.
	require.NoError(t, db.DeletePackageVersion(ctx, pkg.ID, "9.9.9"))
}

func TestDeletePackageCascade(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "ghcr.io/user/repo", "https://remote.example.com/ap/actor")
	require.NoError(t, err)

	const dgstA, dgstB = "sha256:cas1", "sha256:cas2"
	v1 := &PackageVersion{PackageID: pkg.ID, Version: dgstA, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v1))
	v2 := &PackageVersion{PackageID: pkg.ID, Version: dgstB, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v2))

	require.NoError(t, db.PutPackageFile(ctx, &PackageFile{
		VersionID:  v1.ID,
		Filename:   "layer1",
		BlobDigest: testLayerDigest,
		SizeBytes:  100,
	}))
	require.NoError(t, db.PutPackageFile(ctx, &PackageFile{
		VersionID:  v2.ID,
		Filename:   "layer2",
		BlobDigest: testLayerDigest2,
		SizeBytes:  200,
	}))

	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "v1", dgstA))
	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "latest", dgstB))

	// Upload sessions are keyed by repository_id, so the delete must clear them too.
	_, err = db.CreateUploadSession(ctx, "cascade-upload", pkg.ID, 1*time.Hour)
	require.NoError(t, err)

	require.NoError(t, db.DeletePackage(ctx, pkg.ID))

	uploads, err := db.UploadSessionUUIDsByRepo(ctx, pkg.ID)
	require.NoError(t, err)
	require.Empty(t, uploads, "upload sessions should be cascaded on package delete")

	gone, err := db.GetPackage(ctx, "oci", "ghcr.io/user/repo")
	require.NoError(t, err)
	require.Nil(t, gone)

	versions, err := db.ListPackageVersions(ctx, pkg.ID)
	require.NoError(t, err)
	require.Empty(t, versions)

	tags, err := db.ListPackageTags(ctx, pkg.ID)
	require.NoError(t, err)
	require.Empty(t, tags)

	files1, err := db.ListPackageFiles(ctx, v1.ID)
	require.NoError(t, err)
	require.Empty(t, files1)
	files2, err := db.ListPackageFiles(ctx, v2.ID)
	require.NoError(t, err)
	require.Empty(t, files2)
}

func TestDeleteRepositoryAlias(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, err := db.GetOrCreateRepository(ctx, "ghcr.io/user/mirror", "https://remote.example.com/ap/actor")
	require.NoError(t, err)
	require.NoError(t, db.PutManifest(ctx, &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:cafe",
		MediaType:    testManifestMediaType,
		Content:      []byte(`{}`),
	}))

	require.NoError(t, db.DeleteRepository(ctx, repo.ID))

	got, err := db.GetRepository(ctx, "ghcr.io/user/mirror")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestPackageFileCRUD(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "maven", "com.example:lib", testAliceActor)
	require.NoError(t, err)
	v := &PackageVersion{PackageID: pkg.ID, Version: testVersion100, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))

	contentType := "application/java-archive"
	require.NoError(t, db.PutPackageFile(ctx, &PackageFile{
		VersionID:   v.ID,
		Filename:    "lib-1.0.0.jar",
		BlobDigest:  "sha256:jar",
		SizeBytes:   2048,
		ContentType: &contentType,
	}))
	require.NoError(t, db.PutPackageFile(ctx, &PackageFile{
		VersionID:  v.ID,
		Filename:   "lib-1.0.0.pom",
		BlobDigest: "sha256:pom",
		SizeBytes:  512,
	}))

	files, err := db.ListPackageFiles(ctx, v.ID)
	require.NoError(t, err)
	require.Len(t, files, 2)
	require.Equal(t, "lib-1.0.0.jar", files[0].Filename)
	require.Equal(t, "lib-1.0.0.pom", files[1].Filename)

	got, err := db.GetPackageFile(ctx, v.ID, "lib-1.0.0.jar")
	require.NoError(t, err)
	require.Equal(t, "sha256:jar", got.BlobDigest)
	require.Equal(t, int64(2048), got.SizeBytes)
	require.NotNil(t, got.ContentType)
	require.Equal(t, contentType, *got.ContentType)

	// Upsert updates the digest.
	require.NoError(t, db.PutPackageFile(ctx, &PackageFile{
		VersionID:  v.ID,
		Filename:   "lib-1.0.0.jar",
		BlobDigest: "sha256:jar2",
		SizeBytes:  4096,
	}))
	got, err = db.GetPackageFile(ctx, v.ID, "lib-1.0.0.jar")
	require.NoError(t, err)
	require.Equal(t, "sha256:jar2", got.BlobDigest)
	require.Equal(t, int64(4096), got.SizeBytes)

	require.NoError(t, db.DeletePackageFile(ctx, v.ID, "lib-1.0.0.pom"))
	files, err = db.ListPackageFiles(ctx, v.ID)
	require.NoError(t, err)
	require.Len(t, files, 1)
}

func TestPackageTagPutGet(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "npm", "express", testAliceActor)
	require.NoError(t, err)
	v1 := &PackageVersion{PackageID: pkg.ID, Version: "4.0.0", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v1))
	v2 := &PackageVersion{PackageID: pkg.ID, Version: "5.0.0", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v2))

	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "latest", v1.Version))
	got, err := db.GetPackageTag(ctx, pkg.ID, "latest")
	require.NoError(t, err)
	require.Equal(t, v1.Version, got.Version)

	// Upsert moves the tag.
	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "latest", v2.Version))
	got, err = db.GetPackageTag(ctx, pkg.ID, "latest")
	require.NoError(t, err)
	require.Equal(t, v2.Version, got.Version)

	require.NoError(t, db.PutPackageTag(ctx, pkg.ID, "stable", v1.Version))
	tags, err := db.ListPackageTags(ctx, pkg.ID)
	require.NoError(t, err)
	require.Len(t, tags, 2)
	require.Equal(t, "latest", tags[0].Name)
	require.Equal(t, "stable", tags[1].Name)

	require.NoError(t, db.DeletePackageTag(ctx, pkg.ID, "stable"))
	tags, err = db.ListPackageTags(ctx, pkg.ID)
	require.NoError(t, err)
	require.Len(t, tags, 1)
}

func TestListPackageVersionsBySubject(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/signed", testAliceActor)
	require.NoError(t, err)

	base := &PackageVersion{PackageID: pkg.ID, Version: testBaseDigest, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, base))

	subject := testBaseDigest
	artifactType := "application/vnd.dev.cosign.simplesigning.v1+json"
	signer := &PackageVersion{
		PackageID:     pkg.ID,
		Version:       "sha256:sig",
		Metadata:      []byte(`{}`),
		SubjectDigest: &subject,
		ArtifactType:  &artifactType,
	}
	require.NoError(t, db.PutPackageVersion(ctx, signer))

	results, err := db.ListPackageVersionsBySubject(ctx, pkg.ID, subject)
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "sha256:sig", results[0].Version)
	require.NotNil(t, results[0].ArtifactType)
	require.Equal(t, artifactType, *results[0].ArtifactType)

	empty, err := db.ListPackageVersionsBySubject(ctx, pkg.ID, "sha256:nope")
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestPutBlobReferences(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/layered", testAliceActor)
	require.NoError(t, err)
	v := &PackageVersion{PackageID: pkg.ID, Version: "sha256:m", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))

	mt := testLayerMediaType
	require.NoError(t, db.PutBlob(ctx, "sha256:layerA", 1024, &mt, true))
	require.NoError(t, db.PutBlob(ctx, "sha256:layerB", 2048, &mt, true))

	require.NoError(t, db.PutBlobReferences(ctx, v.ID, []BlobRef{
		{Digest: "sha256:layerA", Size: 1024, MediaType: &mt},
		{Digest: "sha256:layerB", Size: 2048, MediaType: &mt},
	}))

	files, err := db.ListPackageFiles(ctx, v.ID)
	require.NoError(t, err)
	require.Len(t, files, 2)
	for _, f := range files {
		require.Equal(t, f.Filename, f.BlobDigest)
		require.NotNil(t, f.ContentType)
		require.Equal(t, mt, *f.ContentType)
	}
}

func TestPutBlobReferences_BlobRowMissing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/preblob", testAliceActor)
	require.NoError(t, err)
	v := &PackageVersion{PackageID: pkg.ID, Version: "sha256:mfst", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))

	mt := testLayerMediaType
	require.NoError(t, db.PutBlobReferences(ctx, v.ID, []BlobRef{
		{Digest: "sha256:notyet", Size: 4096, MediaType: &mt},
	}))

	files, err := db.ListPackageFiles(ctx, v.ID)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, "sha256:notyet", files[0].BlobDigest)
	require.Equal(t, int64(4096), files[0].SizeBytes)
	require.NotNil(t, files[0].ContentType)
	require.Equal(t, mt, *files[0].ContentType)
}

func TestPutBlobReferences_UpsertRefreshesSize(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "oci", "foo.com/upsert", testAliceActor)
	require.NoError(t, err)
	v := &PackageVersion{PackageID: pkg.ID, Version: "sha256:up", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))

	mt := testLayerMediaType
	require.NoError(t, db.PutBlobReferences(ctx, v.ID, []BlobRef{
		{Digest: "sha256:l", Size: 0, MediaType: &mt},
	}))
	require.NoError(t, db.PutBlobReferences(ctx, v.ID, []BlobRef{
		{Digest: "sha256:l", Size: 9999, MediaType: nil},
	}))

	files, err := db.ListPackageFiles(ctx, v.ID)
	require.NoError(t, err)
	require.Len(t, files, 1)
	require.Equal(t, int64(9999), files[0].SizeBytes)
	require.NotNil(t, files[0].ContentType)
	require.Equal(t, mt, *files[0].ContentType)
}

func TestLegacyRepositoryTranslation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, err := db.GetOrCreateRepository(ctx, "foo.com/legacy", testAliceActor)
	require.NoError(t, err)
	require.NotZero(t, repo.ID)
	require.Equal(t, "foo.com/legacy", repo.Name)
	require.Equal(t, testAliceActor, repo.OwnerID)

	// Underlying package row is type='oci'.
	pkg, err := db.GetPackage(ctx, "oci", "foo.com/legacy")
	require.NoError(t, err)
	require.NotNil(t, pkg)
	require.Equal(t, repo.ID, pkg.ID)

	got, err := db.GetRepository(ctx, "foo.com/legacy")
	require.NoError(t, err)
	require.Equal(t, repo.ID, got.ID)

	owner, err := db.IsRepositoryOwner(ctx, repo.ID, testAliceActor)
	require.NoError(t, err)
	require.True(t, owner)
	owner, err = db.IsRepositoryOwner(ctx, repo.ID, "https://bob.example.com/ap/actor")
	require.NoError(t, err)
	require.False(t, owner)
}

func TestLegacyManifestTranslation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, err := db.GetOrCreateRepository(ctx, "foo.com/img", testAliceActor)
	require.NoError(t, err)

	subject := testBaseDigest
	artifactType := "application/vnd.cncf.notary.signature"
	m := &Manifest{
		RepositoryID:  repo.ID,
		Digest:        testDigestABC,
		MediaType:     testManifestMediaType,
		SizeBytes:     321,
		Content:       []byte(`{"schemaVersion":2}`),
		SubjectDigest: &subject,
		ArtifactType:  &artifactType,
	}
	require.NoError(t, db.PutManifest(ctx, m))
	require.NotZero(t, m.ID)

	got, err := db.GetManifestByDigest(ctx, repo.ID, testDigestABC)
	require.NoError(t, err)
	require.Equal(t, m.ID, got.ID)
	require.Equal(t, testManifestMediaType, got.MediaType)
	require.Equal(t, int64(321), got.SizeBytes)
	require.Equal(t, []byte(`{"schemaVersion":2}`), got.Content)

	// Reads are visible through the canonical API too.
	v, err := db.GetPackageVersion(ctx, repo.ID, testDigestABC)
	require.NoError(t, err)
	require.Equal(t, m.Content, v.Metadata)
	require.Equal(t, m.MediaType, v.MediaType)

	// Tag → manifest lookup.
	require.NoError(t, db.PutTag(ctx, repo.ID, "latest", testDigestABC))
	gotByTag, err := db.GetManifestByTag(ctx, repo.ID, "latest")
	require.NoError(t, err)
	require.Equal(t, testDigestABC, gotByTag.Digest)

	// Subject lookup goes through translation.
	signerSubject := testDigestABC
	signer := &Manifest{
		RepositoryID:  repo.ID,
		Digest:        "sha256:sig",
		MediaType:     "application/vnd.cncf.notary.signature",
		SizeBytes:     100,
		Content:       []byte(`{}`),
		SubjectDigest: &signerSubject,
		ArtifactType:  &artifactType,
	}
	require.NoError(t, db.PutManifest(ctx, signer))

	refs, err := db.ListManifestsBySubject(ctx, repo.ID, testDigestABC)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.Equal(t, "sha256:sig", refs[0].Digest)
}

func TestLegacyManifestLayers(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, err := db.GetOrCreateRepository(ctx, "foo.com/layers", testAliceActor)
	require.NoError(t, err)
	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:m",
		MediaType:    testManifestMediaType,
		SizeBytes:    100,
		Content:      []byte(`{}`),
	}
	require.NoError(t, db.PutManifest(ctx, m))

	mt := testLayerMediaType
	require.NoError(t, db.PutBlob(ctx, testLayerDigest, 500, &mt, true))
	require.NoError(t, db.PutBlob(ctx, testLayerDigest2, 600, &mt, true))

	require.NoError(t, db.PutManifestLayers(ctx, m.ID, []BlobRef{
		{Digest: testLayerDigest, Size: 500, MediaType: &mt},
		{Digest: testLayerDigest2, Size: 600, MediaType: &mt},
	}))

	exists, err := db.BlobExistsInRepo(ctx, "foo.com/layers", testLayerDigest)
	require.NoError(t, err)
	require.True(t, exists)

	repoName, err := db.FindRepoForBlob(ctx, testLayerDigest2)
	require.NoError(t, err)
	require.Equal(t, "foo.com/layers", repoName)

	// Layers visible through canonical files API.
	files, err := db.ListPackageFiles(ctx, m.ID)
	require.NoError(t, err)
	require.Len(t, files, 2)
}

func TestLegacyDeletedManifestTranslation(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	require.NoError(t, db.RecordDeletedManifest(ctx, "sha256:gone", "foo.com/old", "https://alice.example.com/ap/actor"))

	deleted, err := db.IsManifestDeleted(ctx, "sha256:gone")
	require.NoError(t, err)
	require.True(t, deleted)

	deleted, err = db.IsManifestDeleted(ctx, "sha256:nope")
	require.NoError(t, err)
	require.False(t, deleted)

	// Tombstone is also visible via canonical API.
	deleted, err = db.IsVersionDeleted(ctx, "oci", "", "sha256:gone")
	require.NoError(t, err)
	require.True(t, deleted)
}

func TestGetPackageTagMissing(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	pkg, err := db.GetOrCreatePackage(ctx, "npm", "missing-tags", testAliceActor)
	require.NoError(t, err)

	got, err := db.GetPackageTag(ctx, pkg.ID, "latest")
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestListLocallyHostedRepos(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	mt := testLayerMediaType

	repo, err := db.GetOrCreateRepository(ctx, "docker.io/app/image", testAliceActor)
	require.NoError(t, err)
	v := &PackageVersion{PackageID: repo.ID, Version: testDigestABC, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v))
	require.NoError(t, db.PutBlob(ctx, testLayerDigest, 2048, &mt, true))
	require.NoError(t, db.PutBlobReferences(ctx, v.ID, []BlobRef{{Digest: testLayerDigest, Size: 2048, MediaType: &mt}}))
	require.NoError(t, db.PutPackageTag(ctx, repo.ID, testTagLatest, v.Version))

	repos, err := db.ListLocallyHostedRepos(ctx)
	require.NoError(t, err)
	require.Len(t, repos, 1)
	require.Equal(t, "docker.io/app/image", repos[0].Name)
	require.Equal(t, int64(2048), repos[0].SizeBytes)
	require.Equal(t, []string{testTagLatest}, repos[0].Tags)

	repo2, err := db.GetOrCreateRepository(ctx, "docker.io/app/bigger", testAliceActor)
	require.NoError(t, err)
	v2 := &PackageVersion{PackageID: repo2.ID, Version: "sha256:def", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v2))
	require.NoError(t, db.PutBlob(ctx, testLayerDigest2, 1024*1024, &mt, true))
	require.NoError(t, db.PutBlobReferences(ctx, v2.ID, []BlobRef{{Digest: testLayerDigest2, Size: 1024 * 1024, MediaType: &mt}}))

	repos, err = db.ListLocallyHostedRepos(ctx)
	require.NoError(t, err)
	require.Len(t, repos, 2)
	require.Equal(t, "docker.io/app/bigger", repos[0].Name)
	require.Equal(t, int64(1024*1024), repos[0].SizeBytes)
	require.Empty(t, repos[0].Tags)

	repo3, err := db.GetOrCreateRepository(ctx, "docker.io/app/remote", testAliceActor)
	require.NoError(t, err)
	v3 := &PackageVersion{PackageID: repo3.ID, Version: "sha256:ghi", Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, v3))
	require.NoError(t, db.PutBlob(ctx, "sha256:remote", 512, &mt, false))
	require.NoError(t, db.PutBlobReferences(ctx, v3.ID, []BlobRef{{Digest: "sha256:remote", Size: 512, MediaType: &mt}}))

	repos, err = db.ListLocallyHostedRepos(ctx)
	require.NoError(t, err)
	require.Len(t, repos, 2)

	npmPkg, err := db.GetOrCreatePackage(ctx, "npm", "left-pad", testAliceActor)
	require.NoError(t, err)
	npmV := &PackageVersion{PackageID: npmPkg.ID, Version: testVersion100, Metadata: []byte(`{}`)}
	require.NoError(t, db.PutPackageVersion(ctx, npmV))
	require.NoError(t, db.PutBlob(ctx, "sha256:npmblob", 100, &mt, true))
	require.NoError(t, db.PutBlobReferences(ctx, npmV.ID, []BlobRef{{Digest: "sha256:npmblob", Size: 100, MediaType: &mt}}))

	repos, err = db.ListLocallyHostedRepos(ctx)
	require.NoError(t, err)
	require.Len(t, repos, 2)
}

func TestMarkPackageWithdrawn(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	pkg, err := db.GetOrCreatePackage(ctx, "oci", "example/repo", "actor-url")
	require.NoError(t, err)

	pending, err := db.ListPackagesPendingWithdrawal(ctx, "oci")
	require.NoError(t, err)
	require.Len(t, pending, 1)

	require.NoError(t, db.MarkPackageWithdrawn(ctx, pkg.ID))

	pending, err = db.ListPackagesPendingWithdrawal(ctx, "oci")
	require.NoError(t, err)
	require.Empty(t, pending)
}

// seedOwnedRepo creates a locally-owned OCI repo with one tagged manifest whose
// layer references blobDigest, returning the repo. Callers pass distinct names.
func seedOwnedRepo(t *testing.T, db *DB, name, owner, manifestDigest, blobDigest string) *Repository {
	t.Helper()
	ctx := context.Background()
	repo, err := db.GetOrCreateRepository(ctx, name, owner)
	require.NoError(t, err)
	require.NoError(t, db.PutBlob(ctx, blobDigest, 42, nil, true))
	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       manifestDigest,
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    100,
		Content:      []byte("{}"),
	}
	require.NoError(t, db.PutManifestWithLayers(ctx, m, []BlobRef{{Digest: blobDigest, Size: 42}}))
	require.NoError(t, db.PutTag(ctx, repo.ID, "latest", manifestDigest))
	return repo
}

func TestDeleteOwnedRepositoryWithBlobs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	const owner = "https://local.example.com/ap/actor"
	repo := seedOwnedRepo(t, db, "test/owned-delete", owner,
		"sha256:aaaa000000000000000000000000000000000000000000000000000000000001",
		"sha256:bbbb000000000000000000000000000000000000000000000000000000000001")

	res, err := db.DeleteOwnedRepositoryWithBlobs(ctx, repo.ID, owner)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, "test/owned-delete", res.Name)
	require.Equal(t, []string{"sha256:aaaa000000000000000000000000000000000000000000000000000000000001"}, res.ManifestDigests)
	require.Equal(t, []string{"latest"}, res.TagNames)
	require.Equal(t, []string{"sha256:bbbb000000000000000000000000000000000000000000000000000000000001"}, res.PurgedBlobs)

	// Repo row is gone from the catalog.
	got, err := db.GetRepository(ctx, "test/owned-delete")
	require.NoError(t, err)
	require.Nil(t, got)

	// Tombstone recorded inside the same delete, so federation cannot resurrect it.
	deleted, err := db.IsManifestDeleted(ctx, "sha256:aaaa000000000000000000000000000000000000000000000000000000000001")
	require.NoError(t, err)
	require.True(t, deleted)
}

func TestDeleteOwnedRepositoryMissingReturnsNilNil(t *testing.T) {
	db := testDB(t)
	res, err := db.DeleteOwnedRepositoryWithBlobs(context.Background(), 999999, "https://local.example.com/ap/actor")
	require.NoError(t, err)
	require.Nil(t, res)
}

func TestDeleteOwnedRepositoryOwnerMismatch(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo := seedOwnedRepo(t, db, "test/peer-owned", "https://peer.example.com/ap/actor",
		"sha256:aaaa000000000000000000000000000000000000000000000000000000000002",
		"sha256:bbbb000000000000000000000000000000000000000000000000000000000002")

	res, err := db.DeleteOwnedRepositoryWithBlobs(ctx, repo.ID, "https://local.example.com/ap/actor")
	require.ErrorIs(t, err, ErrPackageOwnerMismatch)
	require.Nil(t, res)

	// Untouched.
	got, err := db.GetRepository(ctx, "test/peer-owned")
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestDeleteOwnedRepositoryBusyWithOpenUpload(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	const owner = "https://local.example.com/ap/actor"
	repo := seedOwnedRepo(t, db, "test/busy", owner,
		"sha256:aaaa000000000000000000000000000000000000000000000000000000000003",
		"sha256:bbbb000000000000000000000000000000000000000000000000000000000003")
	_, err := db.CreateUploadSession(ctx, "busy-uuid-1", repo.ID, time.Hour)
	require.NoError(t, err)

	res, err := db.DeleteOwnedRepositoryWithBlobs(ctx, repo.ID, owner)
	require.ErrorIs(t, err, ErrRepositoryBusy)
	require.Nil(t, res)

	// An EXPIRED session must not block.
	require.NoError(t, db.DeleteUploadSession(ctx, "busy-uuid-1"))
	_, err = db.CreateUploadSession(ctx, "busy-uuid-2", repo.ID, -time.Hour)
	require.NoError(t, err)
	res, err = db.DeleteOwnedRepositoryWithBlobs(ctx, repo.ID, owner)
	require.NoError(t, err)
	require.NotNil(t, res)
}

func TestDeleteOwnedRepositoryKeepsSharedBlobs(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	const owner = "https://local.example.com/ap/actor"
	const sharedBlob = "sha256:cccc000000000000000000000000000000000000000000000000000000000001"
	repoA := seedOwnedRepo(t, db, "test/share-a", owner,
		"sha256:aaaa000000000000000000000000000000000000000000000000000000000004", sharedBlob)
	_ = repoA
	repoB := seedOwnedRepo(t, db, "test/share-b", owner,
		"sha256:aaaa000000000000000000000000000000000000000000000000000000000005", sharedBlob)

	res, err := db.DeleteOwnedRepositoryWithBlobs(ctx, repoB.ID, owner)
	require.NoError(t, err)
	require.NotNil(t, res)
	// The blob is still referenced by repoA — it must NOT be purged.
	require.Empty(t, res.PurgedBlobs)
	blob, err := db.GetBlob(ctx, sharedBlob)
	require.NoError(t, err)
	require.NotNil(t, blob)
}

func TestPutManifestWithLayersAfterRepoDeleteFailsLoudly(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo, err := db.GetOrCreateRepository(ctx, "test/race-manifest", "https://local.example.com/ap/actor")
	require.NoError(t, err)

	// Simulate the delete winning the race between pushManifest's
	// GetOrCreateRepository and PutManifestWithLayers.
	require.NoError(t, db.DeletePackage(ctx, repo.ID))

	m := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:dddd000000000000000000000000000000000000000000000000000000000001",
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    2,
		Content:      []byte("{}"),
	}
	err = db.PutManifestWithLayers(ctx, m, nil)
	require.ErrorIs(t, err, ErrRepositoryGone)

	// Nothing was stored against the dead repo id.
	got, err := db.GetManifestByDigest(ctx, repo.ID, m.Digest)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestPutTagAfterRepoDeleteFailsLoudly(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	repo, err := db.GetOrCreateRepository(ctx, "test/race-tag", "https://local.example.com/ap/actor")
	require.NoError(t, err)
	require.NoError(t, db.DeletePackage(ctx, repo.ID))

	err = db.PutTag(ctx, repo.ID, "latest", "sha256:dddd000000000000000000000000000000000000000000000000000000000002")
	require.ErrorIs(t, err, ErrRepositoryGone)
}
