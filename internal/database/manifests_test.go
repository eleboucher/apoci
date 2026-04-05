package database

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListManifestsBySubject(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	repo, err := db.GetOrCreateRepository(ctx, "test/referrers", "https://alice.example.com/ap/actor")
	require.NoError(t, err)

	// Put a base manifest (no subject)
	base := &Manifest{
		RepositoryID: repo.ID,
		Digest:       "sha256:basemanifest",
		MediaType:    "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:    100,
		Content:      []byte(`{"schemaVersion":2}`),
	}
	require.NoError(t, db.PutManifest(ctx, base))

	// Put a referrer manifest pointing to the base via subject_digest
	subjectDigest := "sha256:basemanifest"
	artifactType := "application/vnd.dev.cosign.simplesigning.v1+json"
	referrer := &Manifest{
		RepositoryID:  repo.ID,
		Digest:        "sha256:referrer1",
		MediaType:     "application/vnd.oci.image.manifest.v1+json",
		SizeBytes:     200,
		Content:       []byte(`{"schemaVersion":2,"subject":{}}`),
		SubjectDigest: &subjectDigest,
		ArtifactType:  &artifactType,
	}
	require.NoError(t, db.PutManifest(ctx, referrer))

	// ListManifestsBySubject should return only the referrer
	results, err := db.ListManifestsBySubject(ctx, repo.ID, "sha256:basemanifest")
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Equal(t, "sha256:referrer1", results[0].Digest)
	require.NotNil(t, results[0].ArtifactType)
	require.Equal(t, artifactType, *results[0].ArtifactType)

	// No referrers for a digest that has none
	empty, err := db.ListManifestsBySubject(ctx, repo.ID, "sha256:nonexistent")
	require.NoError(t, err)
	require.Len(t, empty, 0)
}
