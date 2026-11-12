package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

var (
	ErrArtifactNotFound = errors.New("artifact not found")
)

type RegisterArtifactRequest struct {
	Artifact              *Artifact `json:"artifact"`
	ExpectedLatestVersion int64     `json:"expectedLatestVersion"`
}

type ArtifactRegistrationResult struct {
	Artifact *Artifact `json:"artifact"`
	Replayed bool      `json:"replayed"`
}

type ArtifactCatalog struct {
	store     ArtifactStore
	placement ArtifactPlacement
	now       func() time.Time
}

func NewArtifactCatalog(store ArtifactStore) *ArtifactCatalog {
	return &ArtifactCatalog{store: store, now: time.Now}
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
	artifact := cloneArtifact(req.Artifact)
	if artifact == nil {
		return nil, fmt.Errorf("%w: artifact is required", ErrInvalidArtifactRecord)
	}
	normalizeArtifact(artifact)
	if err := c.placement.Place(ctx, artifact); err != nil {
		return nil, err
	}
	if artifact.Classification == "" {
		artifact.Classification = ArtifactClassificationInternal
	}
	artifact.CreatedAt = c.now().UTC()
	if artifact.Version != req.ExpectedLatestVersion+1 {
		return nil, fmt.Errorf("%w: version %d must follow expected latest %d", ErrArtifactVersionConflict, artifact.Version, req.ExpectedLatestVersion)
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
		if errors.Is(err, ErrArtifactVersionConflict) || errors.Is(err, ErrArtifactImmutable) {
			existing, loadErr := c.store.GetArtifact(ctx, artifact.Scope, artifact.ID, artifact.Version)
			if loadErr == nil && existing != nil {
				return replayArtifact(existing, fingerprint)
			}
		}
		return nil, err
	}
	return &ArtifactRegistrationResult{Artifact: cloneArtifact(artifact)}, nil
}

func (c *ArtifactCatalog) Get(ctx context.Context, scope Scope, id string, version int64) (*Artifact, error) {
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

func (c *ArtifactCatalog) List(ctx context.Context, filter ArtifactFilter) ([]*Artifact, error) {
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

// ArtifactContentStore is implemented by a host that owns bytes. Raw content
// and storage credentials must never be persisted in Artifact or Activity
// state. SizeBytes may be -1 when the incoming stream size is unknown.
type ArtifactContentStore interface {
	Put(context.Context, ArtifactContentWrite) (ArtifactStoredContent, error)
	Open(context.Context, models.Scope, string) (io.ReadCloser, error)
}

// ArtifactContentInspector lets a host truthfully project whether the bytes
// behind an opaque content reference are currently available without exposing
// a storage path or credential.
type ArtifactContentInspector interface {
	Available(context.Context, Scope, string) (bool, error)
}

// ArtifactContentDeleter removes bytes after portable retention policy says
// they are due. Deletion must be idempotent so multiple workers and retries
// remain safe.
type ArtifactContentDeleter interface {
	Delete(context.Context, Scope, string) (bool, error)
}

// ArtifactContentResolver optionally provides an authorized, short-lived
// delivery URL. Resolutions are ephemeral API results and must never be stored
// in kernel state or model context.
type ArtifactContentResolver interface {
	Resolve(context.Context, ArtifactContentResolutionRequest) (ArtifactContentResolution, error)
}

type ArtifactContentWrite struct {
	Scope     models.Scope
	MediaType string
	Reader    io.Reader
	SizeBytes int64
	Digest    string
	// Owner is the Artifact's owner, such as the Agent whose conversation
	// produced it. A host may use it to place bytes; it is never part of the
	// content reference contract.
	Owner *models.ObjectiveOwner
	// Space is the id of the Space the bytes belong to when the content is
	// the Space's own, such as its icon, rather than an Agent's or a Team's.
	// Like Owner it only tells a host where to place the bytes (an object
	// store keeps each Space's content in its own folder); it is never part
	// of the content reference contract, and a host that keeps all bytes in
	// one place may ignore it.
	Space string
}

type ArtifactStoredContent struct {
	ContentRef string `json:"contentRef"`
	Digest     string `json:"digest"`
	SizeBytes  int64  `json:"sizeBytes"`
}

type ArtifactContentResolutionRequest struct {
	Scope      Scope
	ContentRef string
	Actor      ActivityActor
	Purpose    string
	TTL        time.Duration
}

// ArtifactContentResolution is an ephemeral delivery result. It is returned
// to an authorized caller and is never part of catalog persistence.
type ArtifactContentResolution struct {
	URL       string    `json:"url"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func replayArtifact(existing *Artifact, fingerprint string) (*ArtifactRegistrationResult, error) {
	if existing.Fingerprint != fingerprint {
		return nil, ErrArtifactImmutable
	}
	return &ArtifactRegistrationResult{Artifact: cloneArtifact(existing), Replayed: true}, nil
}

func artifactFingerprint(artifact *Artifact) (string, error) {
	copy := cloneArtifact(artifact)
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

func normalizeArtifact(artifact *models.Artifact) {
	artifact.ID = strings.TrimSpace(artifact.ID)
	artifact.Name = strings.TrimSpace(artifact.Name)
	artifact.Type = strings.TrimSpace(artifact.Type)
	artifact.MediaType = strings.ToLower(strings.TrimSpace(artifact.MediaType))
	artifact.ContentRef = strings.TrimSpace(artifact.ContentRef)
	artifact.Digest = strings.ToLower(strings.TrimSpace(artifact.Digest))
	artifact.Provenance.Producer.Type = strings.TrimSpace(artifact.Provenance.Producer.Type)
	artifact.Provenance.Producer.ID = strings.TrimSpace(artifact.Provenance.Producer.ID)
	if artifact.Provenance.Owner != nil {
		owner := *artifact.Provenance.Owner
		owner.ID = strings.TrimSpace(owner.ID)
		artifact.Provenance.Owner = &owner
	}
	artifact.Provenance.RunID = strings.TrimSpace(artifact.Provenance.RunID)
	artifact.Provenance.ObjectiveID = strings.TrimSpace(artifact.Provenance.ObjectiveID)
	artifact.Provenance.TurnID = strings.TrimSpace(artifact.Provenance.TurnID)
	artifact.Provenance.ActionID = strings.TrimSpace(artifact.Provenance.ActionID)
	artifact.Provenance.RequestID = strings.TrimSpace(artifact.Provenance.RequestID)
	artifact.Provenance.ConversationID = strings.TrimSpace(artifact.Provenance.ConversationID)
	artifact.Provenance.SpaceID = strings.TrimSpace(artifact.Provenance.SpaceID)
	for index := range artifact.Evidence {
		artifact.Evidence[index].TargetRef = strings.TrimSpace(artifact.Evidence[index].TargetRef)
		artifact.Evidence[index].Summary = strings.TrimSpace(artifact.Evidence[index].Summary)
	}
	sort.SliceStable(artifact.Evidence, func(i, j int) bool {
		left, right := artifact.Evidence[i], artifact.Evidence[j]
		if left.TargetKind != right.TargetKind {
			return left.TargetKind < right.TargetKind
		}
		if left.TargetRef != right.TargetRef {
			return left.TargetRef < right.TargetRef
		}
		return left.Relation < right.Relation
	})
}
