package artifactcatalog

import (
	"context"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

type ArtifactRetentionDeletion struct {
	ArtifactID string `json:"artifactId"`
	Version    int64  `json:"version"`
	ContentRef string `json:"contentRef"`
}

type ArtifactRetentionSweepResult struct {
	Deleted    []ArtifactRetentionDeletion `json:"deleted,omitempty"`
	Held       int                         `json:"held"`
	Missing    int                         `json:"missing"`
	Processed  int                         `json:"processed"`
	NextOffset int                         `json:"nextOffset"`
	HasMore    bool                        `json:"hasMore"`
}

type ArtifactRetentionService struct {
	catalog   *Catalog
	inspector ArtifactContentInspector
	deleter   ArtifactContentDeleter
	now       func() time.Time
}

func NewArtifactRetentionService(store models.ArtifactStore, inspector ArtifactContentInspector, deleter ArtifactContentDeleter, opts ...models.ServiceOption) (*ArtifactRetentionService, error) {
	if store == nil || inspector == nil || deleter == nil {
		return nil, errors.New("artifact catalog, content inspector, and content deleter are required")
	}
	return &ArtifactRetentionService{catalog: NewCatalog(store, opts...), inspector: inspector, deleter: deleter, now: models.ClockFrom(opts)}, nil
}

func (s *ArtifactRetentionService) Sweep(ctx context.Context, scope models.Scope, offset, limit int) (*ArtifactRetentionSweepResult, error) {
	if s == nil || s.catalog == nil || s.inspector == nil || s.deleter == nil {
		return nil, errors.New("artifact retention service is not configured")
	}
	if offset < 0 || limit <= 0 || limit > 100 {
		return nil, errors.New("artifact retention sweep offset must be non-negative and limit between 1 and 100")
	}
	now := s.now().UTC()
	artifacts, err := s.catalog.List(ctx, models.ArtifactFilter{Scope: scope, RetentionDueBefore: &now, Offset: offset, Limit: limit})
	if err != nil {
		return nil, err
	}
	result := &ArtifactRetentionSweepResult{Processed: len(artifacts), NextOffset: offset + len(artifacts), HasMore: len(artifacts) == limit}
	for _, artifact := range artifacts {
		if artifact.Retention.LegalHold {
			result.Held++
			continue
		}
		available, err := s.inspector.Available(ctx, scope, artifact.ContentRef)
		if err != nil {
			return nil, err
		}
		if !available {
			result.Missing++
			continue
		}
		deleted, err := s.deleter.Delete(ctx, scope, artifact.ContentRef)
		if err != nil {
			return nil, err
		}
		if !deleted {
			result.Missing++
			continue
		}
		result.Deleted = append(result.Deleted, ArtifactRetentionDeletion{ArtifactID: artifact.ID, Version: artifact.Version, ContentRef: artifact.ContentRef})
	}
	return result, nil
}
