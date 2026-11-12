// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/artifactcatalog. Remove once in-flight branches have landed.
//
// Re-exports of the artifact catalog, kept so that importers of
// pkg/runtime continue to compile.

package runtime

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
)

var ErrArtifactNotFound = artifactcatalog.ErrArtifactNotFound

type RegisterArtifactRequest = artifactcatalog.RegisterArtifactRequest

type ArtifactRegistrationResult = artifactcatalog.ArtifactRegistrationResult

type ArtifactCatalog = artifactcatalog.ArtifactCatalog

// NewArtifactCatalog forwards to artifactcatalog.NewArtifactCatalog.
func NewArtifactCatalog(store models.ArtifactStore, opts ...models.ServiceOption) *artifactcatalog.ArtifactCatalog {
	return artifactcatalog.NewArtifactCatalog(store, opts...)
}

type ArtifactContentInspector = artifactcatalog.ArtifactContentInspector

type ArtifactContentDeleter = artifactcatalog.ArtifactContentDeleter

type ArtifactContentResolver = artifactcatalog.ArtifactContentResolver

type ArtifactContentResolutionRequest = artifactcatalog.ArtifactContentResolutionRequest

type ArtifactContentResolution = artifactcatalog.ArtifactContentResolution

type ArtifactPlacement = artifactcatalog.ArtifactPlacement

// NewStoreArtifactPlacement forwards to artifactcatalog.NewStoreArtifactPlacement.
func NewStoreArtifactPlacement(store any) artifactcatalog.ArtifactPlacement {
	return artifactcatalog.NewStoreArtifactPlacement(store)
}

type ArtifactRetentionDeletion = artifactcatalog.ArtifactRetentionDeletion

type ArtifactRetentionSweepResult = artifactcatalog.ArtifactRetentionSweepResult

type ArtifactRetentionService = artifactcatalog.ArtifactRetentionService

// NewArtifactRetentionService forwards to artifactcatalog.NewArtifactRetentionService.
func NewArtifactRetentionService(store models.ArtifactStore, inspector artifactcatalog.ArtifactContentInspector, deleter artifactcatalog.ArtifactContentDeleter, opts ...models.ServiceOption) (*artifactcatalog.ArtifactRetentionService, error) {
	return artifactcatalog.NewArtifactRetentionService(store, inspector, deleter, opts...)
}
