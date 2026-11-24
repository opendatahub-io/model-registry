package mlflow_test

import (
	"testing"

	"github.com/kubeflow/hub/internal/mlflow"
	"github.com/kubeflow/hub/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSchemaExactIdentity proves logical names and external IDs compare
// identically on both engines: trailing spaces, case, and distinct Unicode
// code points are different identities (F7), while byte-identical values
// remain duplicates. MySQL's utf8mb4_bin is PAD SPACE, so the identity
// columns must use the NO PAD collation utf8mb4_0900_bin.
func TestSchemaExactIdentity(t *testing.T) {
	fixtures := newStoreFixtures(t)
	for engine, fixture := range fixtures {
		t.Run(engine, func(t *testing.T) {
			reserve := func(name, externalID string) error {
				_, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
					Kind:       mlflow.KindRegisteredModel,
					Name:       name,
					ExternalID: &externalID,
				})
				return err
			}

			require.NoError(t, reserve("review-spaces", "ext-a"))

			// A trailing space is a different identity on PostgreSQL; it
			// must be one on MySQL too, not a silent duplicate.
			require.NoError(t, reserve("review-spaces ", "ext-a "),
				"a trailing space must not collide with the unpadded name")

			// Case differences are distinct identities on both engines.
			require.NoError(t, reserve("Review-Spaces", "Ext-a"))

			// Precomposed and combining forms are distinct code points.
			require.NoError(t, reserve("café", "ext-é"))
			require.NoError(t, reserve("café", "ext-é"))

			// Byte-identical values are still duplicates.
			err := reserve("review-spaces", "ext-a")
			require.ErrorIs(t, err, api.ErrConflict)

			// Exact-name lookup must not match the padded variant.
			found, err := fixture.store.FindObject(t.Context(), mlflow.KindRegisteredModel,
				new("review-spaces"), nil, nil)
			require.NoError(t, err)
			assert.Equal(t, "review-spaces", found.Name,
				"an exact-name query must not match the trailing-space variant")
			_, err = fixture.store.FindObject(t.Context(), mlflow.KindRegisteredModel,
				new("review-spaces  "), nil, nil)
			require.ErrorIs(t, err, api.ErrNotFound)

			// External IDs compare exactly within their scope as well.
			require.NoError(t, reserve("ext-other", "ext-a  "))
			_, err = fixture.store.ReserveObject(t.Context(), mlflow.Object{
				Kind:       mlflow.KindRegisteredModel,
				Name:       "ext-dup",
				ExternalID: new("ext-a "),
			})
			require.ErrorIs(t, err, api.ErrConflict,
				"an external ID equal to another after padding is a duplicate")
		})
	}
}

// TestSchemaArtifactLinkIntegrity proves an artifact link must reference
// existing MLflowObject rows on both engines (F9): inserting a link with
// nonexistent artifact or version IDs fails at the database level.
func TestSchemaArtifactLinkIntegrity(t *testing.T) {
	fixtures := newStoreFixtures(t)
	for engine, fixture := range fixtures {
		t.Run(engine, func(t *testing.T) {
			model, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
				Kind: mlflow.KindRegisteredModel, Name: "link-model",
			})
			require.NoError(t, err)
			version, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
				Kind: mlflow.KindModelVersion, Name: "1.0.0", ParentID: model.ID,
			})
			require.NoError(t, err)
			artifact, err := fixture.store.ReserveObject(t.Context(), mlflow.Object{
				Kind: mlflow.KindModelArtifact, Name: "artifact",
			})
			require.NoError(t, err)

			// A valid link referencing existing rows inserts cleanly.
			require.NoError(t, fixture.db.Create(&mlflow.ArtifactLink{
				ArtifactID: artifact.ID, VersionID: version.ID,
			}).Error)

			// Nonexistent artifact ID.
			err = fixture.db.Create(&mlflow.ArtifactLink{
				ArtifactID: artifact.ID + 100000, VersionID: version.ID,
			}).Error
			require.Error(t, err, "a link to a nonexistent artifact must be rejected")

			// Nonexistent version ID.
			err = fixture.db.Create(&mlflow.ArtifactLink{
				ArtifactID: artifact.ID, VersionID: version.ID + 100000,
			}).Error
			require.Error(t, err, "a link to a nonexistent version must be rejected")
		})
	}
}
