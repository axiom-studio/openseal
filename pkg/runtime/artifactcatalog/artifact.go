package artifactcatalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

var (
	ErrArtifactNotFound = errors.New("artifact not found")
)

type RegisterArtifactRequest struct {
	Artifact              *models.Artifact `json:"artifact"`
	ExpectedLatestVersion int64            `json:"expectedLatestVersion"`
}

type ArtifactRegistrationResult struct {
	Artifact *models.Artifact `json:"artifact"`
	Replayed bool             `json:"replayed"`
}

type ArtifactCatalog struct {
	store     models.ArtifactStore
	placement ArtifactPlacement
	now       func() time.Time
}

func NewArtifactCatalog(store models.ArtifactStore, opts ...models.ServiceOption) *ArtifactCatalog {
	return &ArtifactCatalog{store: store, now: models.ClockFrom(opts)}
}

// WithPlacement records the chat and space of each artifact the catalog
// registers from the run that made it.
func (c *ArtifactCatalog) WithPlacement(placement ArtifactPlacement) *ArtifactCatalog {
	c.placement = placement
	return c
}

func (c *ArtifactCatalog) Register(ctx context.Context, req RegisterArtifactRequest) (*ArtifactRegistrationResult, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("artifact catalog is not configured")
	}
	artifact := models.CloneArtifact(req.Artifact)
	if artifact == nil {
		return nil, fmt.Errorf("%w: artifact is required", models.ErrInvalidArtifactRecord)
	}
	models.NormalizeArtifact(artifact)
	if err := c.placement.Place(ctx, artifact); err != nil {
		return nil, err
	}
	if artifact.Classification == "" {
		artifact.Classification = models.ArtifactClassificationInternal
	}
	artifact.CreatedAt = c.now().UTC()
	if artifact.Version != req.ExpectedLatestVersion+1 {
		return nil, fmt.Errorf("%w: version %d must follow expected latest %d", models.ErrArtifactVersionConflict, artifact.Version, req.ExpectedLatestVersion)
	}
	fingerprint, err := artifactFingerprint(artifact)
	if err != nil {
		return nil, err
	}
	artifact.Fingerprint = fingerprint
	if err := artifact.Validate(); err != nil {
		return nil, err
	}
	existing, err := c.store.GetArtifact(ctx, artifact.Scope, artifact.ID, artifact.Version)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return replayArtifact(existing, fingerprint)
	}
	if err := c.store.CreateArtifactVersion(ctx, artifact, req.ExpectedLatestVersion); err != nil {
		if errors.Is(err, models.ErrArtifactVersionConflict) || errors.Is(err, models.ErrArtifactImmutable) {
			existing, loadErr := c.store.GetArtifact(ctx, artifact.Scope, artifact.ID, artifact.Version)
			if loadErr == nil && existing != nil {
				return replayArtifact(existing, fingerprint)
			}
		}
		return nil, err
	}
	return &ArtifactRegistrationResult{Artifact: models.CloneArtifact(artifact)}, nil
}

func (c *ArtifactCatalog) Get(ctx context.Context, scope models.Scope, id string, version int64) (*models.Artifact, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("artifact catalog is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	artifact, err := c.store.GetArtifact(ctx, scope, strings.TrimSpace(id), version)
	if err != nil {
		return nil, err
	}
	if artifact == nil {
		return nil, ErrArtifactNotFound
	}
	return artifact, nil
}

func (c *ArtifactCatalog) List(ctx context.Context, filter models.ArtifactFilter) ([]*models.Artifact, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("artifact catalog is not configured")
	}
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	if filter.Owner != nil {
		if err := filter.Owner.Validate(); err != nil {
			return nil, err
		}
	}
	filter.ConversationID = strings.TrimSpace(filter.ConversationID)
	filter.SpaceID = strings.TrimSpace(filter.SpaceID)
	if !filter.Files.Valid() {
		return nil, errors.New("artifact files must be results or working")
	}
	if filter.Limit <= 0 {
		filter.Limit = 50
	}
	if filter.Limit > 100 || filter.Offset < 0 {
		return nil, errors.New("artifact list limit must be at most 100 and offset cannot be negative")
	}
	return c.store.ListArtifacts(ctx, filter)
}

// ArtifactContentInspector lets a host truthfully project whether the bytes
// behind an opaque content reference are currently available without exposing
// a storage path or credential.
type ArtifactContentInspector interface {
	Available(context.Context, models.Scope, string) (bool, error)
}

// ArtifactContentDeleter removes bytes after portable retention policy says
// they are due. Deletion must be idempotent so multiple workers and retries
// remain safe.
type ArtifactContentDeleter interface {
	Delete(context.Context, models.Scope, string) (bool, error)
}

// ArtifactContentResolver optionally provides an authorized, short-lived
// delivery URL. Resolutions are ephemeral API results and must never be stored
// in kernel state or model context.
type ArtifactContentResolver interface {
	Resolve(context.Context, ArtifactContentResolutionRequest) (ArtifactContentResolution, error)
}

type ArtifactContentResolutionRequest struct {
	Scope      models.Scope
	ContentRef string
	Actor      models.ActivityActor
	Purpose    string
	TTL        time.Duration
}

// ArtifactContentResolution is an ephemeral delivery result. It is returned
// to an authorized caller and is never part of catalog persistence.
type ArtifactContentResolution struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func replayArtifact(existing *models.Artifact, fingerprint string) (*ArtifactRegistrationResult, error) {
	if existing.Fingerprint != fingerprint {
		return nil, models.ErrArtifactImmutable
	}
	return &ArtifactRegistrationResult{Artifact: models.CloneArtifact(existing), Replayed: true}, nil
}

func artifactFingerprint(artifact *models.Artifact) (string, error) {
	copy := models.CloneArtifact(artifact)
	copy.Fingerprint = ""
	copy.CreatedAt = time.Time{}
	copy.ContentAvailability = ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
