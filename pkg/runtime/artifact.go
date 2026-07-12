package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"time"
)

var (
	ErrArtifactNotFound        = errors.New("artifact not found")
	ErrArtifactImmutable       = errors.New("artifact version is immutable")
	ErrArtifactVersionConflict = errors.New("artifact version conflict")
	ErrInvalidArtifactRecord   = errors.New("invalid artifact record")
)

type ArtifactClassification string

type ArtifactContentAvailability string

const (
	ArtifactClassificationPublic       ArtifactClassification = "public"
	ArtifactClassificationInternal     ArtifactClassification = "internal"
	ArtifactClassificationConfidential ArtifactClassification = "confidential"
	ArtifactClassificationRestricted   ArtifactClassification = "restricted"
)

const (
	ArtifactContentAvailable   ArtifactContentAvailability = "available"
	ArtifactContentUnavailable ArtifactContentAvailability = "unavailable"
)

type EvidenceRelation string

const (
	EvidenceRelationDerivedFrom EvidenceRelation = "derived_from"
	EvidenceRelationSupports    EvidenceRelation = "supports"
	EvidenceRelationContradicts EvidenceRelation = "contradicts"
	EvidenceRelationCites       EvidenceRelation = "cites"
	EvidenceRelationInputTo     EvidenceRelation = "input_to"
	EvidenceRelationOutputOf    EvidenceRelation = "output_of"
)

type EvidenceTargetKind string

const (
	EvidenceTargetArtifact       EvidenceTargetKind = "artifact"
	EvidenceTargetExternalSource EvidenceTargetKind = "external_source"
	EvidenceTargetRun            EvidenceTargetKind = "run"
	EvidenceTargetActivity       EvidenceTargetKind = "activity"
	EvidenceTargetRequest        EvidenceTargetKind = "request"
	EvidenceTargetAction         EvidenceTargetKind = "action"
	EvidenceTargetTurn           EvidenceTargetKind = "turn"
	EvidenceTargetClaim          EvidenceTargetKind = "claim"
)

// ArtifactRetention is portable policy metadata. Content deletion is always
// performed by the host ContentStore; legal hold prevents scheduled expiry.
type ArtifactRetention struct {
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	LegalHold bool       `json:"legalHold,omitempty"`
}

// ArtifactProvenance records who produced an artifact and the durable work
// lineage that produced it. Empty lineage fields are omitted for imported
// artifacts, but the producer is always required.
type ArtifactProvenance struct {
	Producer    ActivityActor   `json:"producer"`
	Owner       *ObjectiveOwner `json:"owner,omitempty"`
	RunID       string          `json:"runId,omitempty"`
	ObjectiveID string          `json:"objectiveId,omitempty"`
	TurnID      string          `json:"turnId,omitempty"`
	ActionID    string          `json:"actionId,omitempty"`
	RequestID   string          `json:"requestId,omitempty"`
}

// EvidenceLink forms a queryable graph without copying source content into
// model-visible state. Public source URLs are allowed only for
// external_source targets and are rejected when they resemble signed URLs.
type EvidenceLink struct {
	Relation   EvidenceRelation   `json:"relation"`
	TargetKind EvidenceTargetKind `json:"targetKind"`
	TargetRef  string             `json:"targetRef"`
	Summary    string             `json:"summary,omitempty"`
	Confidence *float64           `json:"confidence,omitempty"`
	ObservedAt *time.Time         `json:"observedAt,omitempty"`
}

// Artifact is immutable at (scope, id, version). A new version creates a new
// record and never mutates prior provenance, digest, evidence, or retention.
// ContentRef is an opaque host reference—not a URL, credential, or filesystem
// path exposed to the model.
type Artifact struct {
	ID                  string                      `json:"id"`
	Version             int64                       `json:"version"`
	Scope               Scope                       `json:"scope"`
	Name                string                      `json:"name"`
	Type                string                      `json:"type,omitempty"`
	MediaType           string                      `json:"mediaType,omitempty"`
	ContentRef          string                      `json:"contentRef"`
	ContentAvailability ArtifactContentAvailability `json:"contentAvailability,omitempty"`
	Digest              string                      `json:"digest"`
	SizeBytes           int64                       `json:"sizeBytes"`
	Classification      ArtifactClassification      `json:"classification"`
	Retention           ArtifactRetention           `json:"retention,omitempty"`
	MetadataSchema      map[string]interface{}      `json:"metadataSchema,omitempty"`
	Metadata            map[string]interface{}      `json:"metadata,omitempty"`
	Provenance          ArtifactProvenance          `json:"provenance"`
	Evidence            []EvidenceLink              `json:"evidence,omitempty"`
	CreatedAt           time.Time                   `json:"createdAt"`
	Fingerprint         string                      `json:"fingerprint"`
}

func (a *Artifact) Validate() error {
	if a == nil {
		return fmt.Errorf("%w: artifact is required", ErrInvalidArtifactRecord)
	}
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(a.ID, 128) {
		return fmt.Errorf("%w: artifact id must be a portable opaque identifier", ErrInvalidArtifactRecord)
	}
	if a.Version <= 0 {
		return fmt.Errorf("%w: artifact version must be positive", ErrInvalidArtifactRecord)
	}
	if strings.TrimSpace(a.Name) == "" || len(a.Name) > 512 {
		return fmt.Errorf("%w: artifact name is required and cannot exceed 512 characters", ErrInvalidArtifactRecord)
	}
	if err := validateOpaqueArtifactRef(a.ContentRef); err != nil {
		return fmt.Errorf("%w: content reference: %v", ErrInvalidArtifactRecord, err)
	}
	if err := validateSHA256Digest(a.Digest); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArtifactRecord, err)
	}
	if a.SizeBytes < 0 {
		return fmt.Errorf("%w: size cannot be negative", ErrInvalidArtifactRecord)
	}
	if !validArtifactClassification(a.Classification) {
		return fmt.Errorf("%w: invalid classification %q", ErrInvalidArtifactRecord, a.Classification)
	}
	if a.ContentAvailability != "" && a.ContentAvailability != ArtifactContentAvailable && a.ContentAvailability != ArtifactContentUnavailable {
		return fmt.Errorf("%w: invalid content availability %q", ErrInvalidArtifactRecord, a.ContentAvailability)
	}
	if strings.TrimSpace(a.Provenance.Producer.Type) == "" || strings.TrimSpace(a.Provenance.Producer.ID) == "" {
		return fmt.Errorf("%w: provenance producer is required", ErrInvalidArtifactRecord)
	}
	if a.Provenance.Owner != nil {
		if err := a.Provenance.Owner.Validate(); err != nil {
			return fmt.Errorf("%w: provenance owner: %v", ErrInvalidArtifactRecord, err)
		}
	}
	if err := validateCredentialFreeContext(a.Metadata); err != nil {
		return fmt.Errorf("%w: metadata: %v", ErrInvalidArtifactRecord, err)
	}
	if a.MetadataSchema != nil {
		compiled, err := compileArtifactSchema(a.ID, a.MetadataSchema)
		if err != nil {
			return fmt.Errorf("%w: metadata schema: %v", ErrInvalidArtifactRecord, err)
		}
		if err := validateArtifactSchema(compiled, a.Metadata); err != nil {
			return fmt.Errorf("%w: metadata: %v", ErrInvalidArtifactRecord, err)
		}
	}
	for index, link := range a.Evidence {
		if err := link.Validate(); err != nil {
			return fmt.Errorf("%w: evidence %d: %v", ErrInvalidArtifactRecord, index, err)
		}
	}
	return nil
}

func (l EvidenceLink) Validate() error {
	switch l.Relation {
	case EvidenceRelationDerivedFrom, EvidenceRelationSupports, EvidenceRelationContradicts,
		EvidenceRelationCites, EvidenceRelationInputTo, EvidenceRelationOutputOf:
	default:
		return errors.New("invalid evidence relation")
	}
	switch l.TargetKind {
	case EvidenceTargetArtifact, EvidenceTargetRun, EvidenceTargetActivity, EvidenceTargetRequest,
		EvidenceTargetAction, EvidenceTargetTurn, EvidenceTargetClaim:
		if err := validateOpaqueArtifactRef(l.TargetRef); err != nil {
			return err
		}
	case EvidenceTargetExternalSource:
		if err := validatePublicEvidenceURL(l.TargetRef); err != nil {
			return err
		}
	default:
		return errors.New("invalid evidence target kind")
	}
	if l.Confidence != nil && (*l.Confidence < 0 || *l.Confidence > 1) {
		return errors.New("evidence confidence must be between 0 and 1")
	}
	if len(l.Summary) > 2_000 {
		return errors.New("evidence summary cannot exceed 2000 characters")
	}
	return nil
}

type ArtifactFilter struct {
	Scope             Scope
	ID                string
	Owner             *ObjectiveOwner
	Types             []string
	MediaTypes        []string
	Classifications   []ArtifactClassification
	ProducerRunID     string
	ProducerRequestID string
	EvidenceTarget    string
	LatestOnly        bool
	Limit             int
	Offset            int
}

type ArtifactStore interface {
	CreateArtifactVersion(context.Context, *Artifact, int64) error
	GetArtifact(context.Context, Scope, string, int64) (*Artifact, error)
	ListArtifacts(context.Context, ArtifactFilter) ([]*Artifact, error)
}

type RegisterArtifactRequest struct {
	Artifact              *Artifact `json:"artifact"`
	ExpectedLatestVersion int64     `json:"expectedLatestVersion"`
}

type ArtifactRegistrationResult struct {
	Artifact *Artifact `json:"artifact"`
	Replayed bool      `json:"replayed"`
}

type ArtifactCatalog struct {
	store ArtifactStore
	now   func() time.Time
}

func NewArtifactCatalog(store ArtifactStore) *ArtifactCatalog {
	return &ArtifactCatalog{store: store, now: time.Now}
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
	Open(context.Context, Scope, string) (io.ReadCloser, error)
}

// ArtifactContentInspector lets a host truthfully project whether the bytes
// behind an opaque content reference are currently available without exposing
// a storage path or credential.
type ArtifactContentInspector interface {
	Available(context.Context, Scope, string) (bool, error)
}

// ArtifactContentResolver optionally provides an authorized, short-lived
// delivery URL. Resolutions are ephemeral API results and must never be stored
// in kernel state or model context.
type ArtifactContentResolver interface {
	Resolve(context.Context, ArtifactContentResolutionRequest) (ArtifactContentResolution, error)
}

type ArtifactContentWrite struct {
	Scope     Scope
	MediaType string
	Reader    io.Reader
	SizeBytes int64
	Digest    string
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

func normalizeArtifact(artifact *Artifact) {
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

func cloneArtifact(in *Artifact) *Artifact {
	if in == nil {
		return nil
	}
	out := *in
	if in.Provenance.Owner != nil {
		owner := *in.Provenance.Owner
		out.Provenance.Owner = &owner
	}
	out.Metadata = cloneMap(in.Metadata)
	out.MetadataSchema = cloneMap(in.MetadataSchema)
	out.Evidence = append([]EvidenceLink(nil), in.Evidence...)
	return &out
}

func validArtifactClassification(value ArtifactClassification) bool {
	switch value {
	case ArtifactClassificationPublic, ArtifactClassificationInternal, ArtifactClassificationConfidential, ArtifactClassificationRestricted:
		return true
	default:
		return false
	}
}

func validateSHA256Digest(value string) error {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(value)), ":", 2)
	if len(parts) != 2 || parts[0] != "sha256" || len(parts[1]) != 64 {
		return errors.New("digest must be sha256:<64 hex characters>")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return errors.New("digest is invalid")
	}
	return nil
}

func validOpaqueIdentifier(value string, maxLength int) bool {
	value = strings.TrimSpace(value)
	return value != "" && len(value) <= maxLength && !strings.ContainsAny(value, "\r\n?#@/\\") && !strings.Contains(value, "://")
}

func validatePublicEvidenceURL(value string) error {
	if len(value) == 0 || len(value) > 2_048 {
		return errors.New("external evidence URL must be between 1 and 2048 characters")
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return errors.New("external evidence must be a public HTTP(S) URL without user info")
	}
	for key := range parsed.Query() {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", ""))
		for _, forbidden := range []string{"token", "secret", "signature", "credential", "password", "apikey", "accesskey", "xamzcredential", "xamzsignature"} {
			if strings.Contains(normalized, forbidden) {
				return errors.New("external evidence URL appears to contain credentials or a signature")
			}
		}
	}
	return nil
}

func matchesArtifactFilter(artifact *Artifact, filter ArtifactFilter) bool {
	if artifact == nil || artifact.Scope != filter.Scope {
		return false
	}
	if filter.ID != "" && artifact.ID != filter.ID {
		return false
	}
	if filter.Owner != nil && (artifact.Provenance.Owner == nil || *artifact.Provenance.Owner != *filter.Owner) {
		return false
	}
	if !containsString(filter.Types, artifact.Type) || !containsString(filter.MediaTypes, artifact.MediaType) || !containsClassification(filter.Classifications, artifact.Classification) {
		return false
	}
	if filter.ProducerRunID != "" && artifact.Provenance.RunID != filter.ProducerRunID {
		return false
	}
	if filter.ProducerRequestID != "" && artifact.Provenance.RequestID != filter.ProducerRequestID {
		return false
	}
	if filter.EvidenceTarget != "" {
		found := false
		for _, link := range artifact.Evidence {
			if link.TargetRef == filter.EvidenceTarget {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsString(filter []string, value string) bool {
	if len(filter) == 0 {
		return true
	}
	for _, candidate := range filter {
		if strings.TrimSpace(candidate) == value {
			return true
		}
	}
	return false
}

func containsClassification(filter []ArtifactClassification, value ArtifactClassification) bool {
	if len(filter) == 0 {
		return true
	}
	for _, candidate := range filter {
		if candidate == value {
			return true
		}
	}
	return false
}
