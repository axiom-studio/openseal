package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrSourceObservationNotFound = errors.New("source observation not found")
	ErrSourceObservationConflict = errors.New("source observation conflicts with immutable evidence")
	ErrSourceMonitorCheckpoint   = errors.New("source monitor checkpoint revision conflict")
	ErrInvalidSourceObservation  = errors.New("invalid source observation")
)

// SourceObservation is a durable, credential-free fact returned by a governed
// monitor action. Raw source content belongs in ArtifactContentStore; this
// record keeps stable identity, digest, public provenance, and linked evidence.
type SourceObservation struct {
	ID                 string                 `json:"id"`
	Scope              Scope                  `json:"scope"`
	InitiativeID       string                 `json:"initiativeId"`
	MonitorID          string                 `json:"monitorId"`
	DedupeKey          string                 `json:"dedupeKey"`
	StableSourceID     string                 `json:"stableSourceId,omitempty"`
	SourceURI          string                 `json:"sourceUri"`
	ContentDigest      string                 `json:"contentDigest"`
	Summary            string                 `json:"summary"`
	ObservedAt         time.Time              `json:"observedAt"`
	IngestedAt         time.Time              `json:"ingestedAt"`
	RetentionExpiresAt *time.Time             `json:"retentionExpiresAt,omitempty"`
	RunID              string                 `json:"runId"`
	AgentID            string                 `json:"agentId"`
	SkillID            string                 `json:"skillId"`
	SkillVersion       string                 `json:"skillVersion"`
	Action             string                 `json:"action"`
	ActionCallID       string                 `json:"actionCallId"`
	ArtifactRef        *ResourceReference     `json:"artifactRef,omitempty"`
	Metadata           map[string]interface{} `json:"metadata,omitempty"`
	Fingerprint        string                 `json:"fingerprint"`
}

type SourceMonitorCheckpoint struct {
	Scope             Scope     `json:"scope"`
	InitiativeID      string    `json:"initiativeId"`
	MonitorID         string    `json:"monitorId"`
	Cursor            string    `json:"cursor,omitempty"`
	LastRunID         string    `json:"lastRunId"`
	LastObservationID string    `json:"lastObservationId"`
	LastActionCallID  string    `json:"lastActionCallId"`
	ObservationCount  int64     `json:"observationCount"`
	LastSuccessAt     time.Time `json:"lastSuccessAt"`
	Revision          int64     `json:"revision"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type SourceObservationFilter struct {
	Scope        Scope
	InitiativeID string
	MonitorID    string
	RunID        string
	ActionCallID string
	Limit        int
	Offset       int
	// RetainedAt excludes observations whose configured retention window has
	// elapsed. SourceMonitorService sets it for product reads; a zero value is
	// reserved for immutable store-level audit access.
	RetainedAt time.Time
}

type SourceMonitorStore interface {
	IngestSourceObservation(context.Context, *SourceObservation, *SourceMonitorCheckpoint, int64, *ActivityEvent) (*SourceObservation, *SourceMonitorCheckpoint, *ActivityEvent, bool, error)
	AdvanceSourceMonitorCheckpoint(context.Context, *SourceMonitorCheckpoint, int64, *ActivityEvent) (*SourceMonitorCheckpoint, *ActivityEvent, bool, error)
	GetSourceObservation(context.Context, Scope, string) (*SourceObservation, error)
	ListSourceObservations(context.Context, SourceObservationFilter) ([]*SourceObservation, error)
	GetSourceMonitorCheckpoint(context.Context, Scope, string, string) (*SourceMonitorCheckpoint, error)
}

type IngestSourceObservationRequest struct {
	Scope                      Scope
	InitiativeID               string
	MonitorID                  string
	ExpectedCheckpointRevision int64
	Cursor                     string
	StableSourceID             string
	SourceURI                  string
	ContentDigest              string
	Summary                    string
	ObservedAt                 time.Time
	RetentionExpiresAt         *time.Time
	RunID                      string
	AgentID                    string
	SkillID                    string
	SkillVersion               string
	Action                     string
	ActionCallID               string
	ArtifactRef                *ResourceReference
	Metadata                   map[string]interface{}
	Actor                      ActivityActor
	Visibility                 ActivityVisibility
}

type SourceObservationIngestResult struct {
	Observation *SourceObservation       `json:"observation"`
	Checkpoint  *SourceMonitorCheckpoint `json:"checkpoint"`
	Event       *ActivityEvent           `json:"event,omitempty"`
	Replayed    bool                     `json:"replayed"`
}

type AdvanceSourceMonitorCheckpointRequest struct {
	Scope                      Scope
	InitiativeID               string
	MonitorID                  string
	ExpectedCheckpointRevision int64
	Cursor                     string
	RunID                      string
	AgentID                    string
	SkillID                    string
	SkillVersion               string
	Action                     string
	ActionCallID               string
	Actor                      ActivityActor
	Visibility                 ActivityVisibility
}

type SourceMonitorCheckpointResult struct {
	Checkpoint *SourceMonitorCheckpoint `json:"checkpoint"`
	Event      *ActivityEvent           `json:"event,omitempty"`
	Replayed   bool                     `json:"replayed"`
}

type SourceMonitorService struct {
	store       SourceMonitorStore
	initiatives InitiativeStore
	portfolio   PortfolioStore
	artifacts   ArtifactStore
	now         func() time.Time
}

func NewSourceMonitorService(store SourceMonitorStore, initiatives InitiativeStore, portfolio PortfolioStore, artifacts ArtifactStore) *SourceMonitorService {
	return &SourceMonitorService{store: store, initiatives: initiatives, portfolio: portfolio, artifacts: artifacts, now: time.Now}
}

func (s *SourceMonitorService) Ingest(ctx context.Context, req IngestSourceObservationRequest) (*SourceObservationIngestResult, error) {
	if s == nil || s.store == nil || s.initiatives == nil || s.portfolio == nil {
		return nil, errors.New("source monitor service is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	initiative, err := s.initiatives.GetInitiative(ctx, req.Scope, strings.TrimSpace(req.InitiativeID))
	if err != nil {
		return nil, err
	}
	monitor, ok := initiativeSourceMonitor(initiative, req.MonitorID)
	if !ok {
		return nil, fmt.Errorf("%w: source monitor does not belong to Initiative", ErrInvalidSourceObservation)
	}
	run, err := s.portfolio.GetAgentRun(ctx, req.Scope, strings.TrimSpace(req.RunID))
	if err != nil {
		return nil, err
	}
	if run.ObjectiveID != monitor.ObjectiveID || run.AssignedAgentID != monitor.AssignedAgentID || req.AgentID != monitor.AssignedAgentID ||
		run.Context["initiativeId"] != initiative.ID || run.Context["sourceMonitorId"] != monitor.ID ||
		req.SkillID != monitor.SkillID || req.SkillVersion != monitor.SkillVersion || req.Action != monitor.Action {
		return nil, fmt.Errorf("%w: observation provenance does not match monitor execution", ErrInvalidSourceObservation)
	}
	now := s.now().UTC()
	observation := &SourceObservation{
		Scope: req.Scope, InitiativeID: initiative.ID, MonitorID: monitor.ID,
		StableSourceID: strings.TrimSpace(req.StableSourceID), SourceURI: strings.TrimSpace(req.SourceURI),
		ContentDigest: strings.ToLower(strings.TrimSpace(req.ContentDigest)), Summary: strings.TrimSpace(req.Summary),
		ObservedAt: req.ObservedAt.UTC(), IngestedAt: now, RetentionExpiresAt: cloneTimePointer(req.RetentionExpiresAt), RunID: run.ID, AgentID: req.AgentID,
		SkillID: req.SkillID, SkillVersion: req.SkillVersion, Action: req.Action, ActionCallID: strings.TrimSpace(req.ActionCallID),
		ArtifactRef: cloneResourceReference(req.ArtifactRef), Metadata: cloneMap(req.Metadata),
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = now
	}
	if observation.ArtifactRef != nil {
		if s.artifacts == nil {
			return nil, errors.New("source monitor artifact verifier is unavailable")
		}
		artifact, artifactErr := NewArtifactCatalog(s.artifacts).Get(ctx, req.Scope, observation.ArtifactRef.ID, observation.ArtifactRef.Revision)
		if artifactErr != nil {
			return nil, artifactErr
		}
		if artifact.Provenance.RunID != run.ID || artifact.Digest != observation.ContentDigest {
			return nil, fmt.Errorf("%w: linked Artifact provenance or digest does not match observation", ErrInvalidSourceObservation)
		}
	}
	observation.DedupeKey, err = sourceObservationDedupeKey(monitor.Deduplication, monitor.ID, observation.StableSourceID, observation.ContentDigest)
	if err != nil {
		return nil, err
	}
	observation.ID = "observation-" + strings.TrimPrefix(observation.DedupeKey, "sha256:")[:32]
	observation.Fingerprint, err = sourceObservationFingerprint(observation)
	if err != nil {
		return nil, err
	}
	if err := observation.Validate(); err != nil {
		return nil, err
	}
	checkpoint := &SourceMonitorCheckpoint{
		Scope: req.Scope, InitiativeID: initiative.ID, MonitorID: monitor.ID, Cursor: strings.TrimSpace(req.Cursor),
		LastRunID: run.ID, LastObservationID: observation.ID, LastActionCallID: observation.ActionCallID, LastSuccessAt: now,
		Revision: req.ExpectedCheckpointRevision + 1, UpdatedAt: now,
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: req.Scope, InitiativeID: initiative.ID, RunID: run.ID, ObjectiveID: run.ObjectiveID,
		AgentID: run.AssignedAgentID, EventType: "source_monitor.observation_ingested", Severity: ActivitySeverityInfo,
		Actor: req.Actor, Visibility: visibility, Summary: "Source monitor ingested new evidence",
		Payload: map[string]interface{}{"monitorId": monitor.ID, "observationId": observation.ID, "sourceUri": observation.SourceURI}, CreatedAt: now,
	}
	if initiative.Owner.Type == OwnerTypeTeam {
		event.TeamID = initiative.Owner.ID
	}
	if event.Actor.Type == "" {
		event.Actor = ActivityActor{Type: "agent", ID: run.AssignedAgentID}
	}
	stored, next, persistedEvent, replayed, err := s.store.IngestSourceObservation(ctx, observation, checkpoint, req.ExpectedCheckpointRevision, event)
	if err != nil {
		return nil, err
	}
	return &SourceObservationIngestResult{Observation: stored, Checkpoint: next, Event: persistedEvent, Replayed: replayed}, nil
}

func (s *SourceMonitorService) AdvanceCheckpoint(ctx context.Context, req AdvanceSourceMonitorCheckpointRequest) (*SourceMonitorCheckpointResult, error) {
	if s == nil || s.store == nil || s.initiatives == nil || s.portfolio == nil {
		return nil, errors.New("source monitor service is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	initiative, err := s.initiatives.GetInitiative(ctx, req.Scope, strings.TrimSpace(req.InitiativeID))
	if err != nil {
		return nil, err
	}
	monitor, ok := initiativeSourceMonitor(initiative, req.MonitorID)
	if !ok {
		return nil, fmt.Errorf("%w: source monitor does not belong to Initiative", ErrInvalidSourceObservation)
	}
	run, err := s.portfolio.GetAgentRun(ctx, req.Scope, strings.TrimSpace(req.RunID))
	if err != nil {
		return nil, err
	}
	if run.ObjectiveID != monitor.ObjectiveID || run.AssignedAgentID != monitor.AssignedAgentID || req.AgentID != monitor.AssignedAgentID ||
		run.Context["initiativeId"] != initiative.ID || run.Context["sourceMonitorId"] != monitor.ID ||
		req.SkillID != monitor.SkillID || req.SkillVersion != monitor.SkillVersion || req.Action != monitor.Action || !validOpaqueIdentifier(strings.TrimSpace(req.ActionCallID), 128) {
		return nil, fmt.Errorf("%w: checkpoint provenance does not match monitor execution", ErrInvalidSourceObservation)
	}
	now := s.now().UTC()
	checkpoint := &SourceMonitorCheckpoint{
		Scope: req.Scope, InitiativeID: initiative.ID, MonitorID: monitor.ID, Cursor: strings.TrimSpace(req.Cursor),
		LastRunID: run.ID, LastActionCallID: strings.TrimSpace(req.ActionCallID), LastSuccessAt: now,
		Revision: req.ExpectedCheckpointRevision + 1, UpdatedAt: now,
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	actor := req.Actor
	if actor.Type == "" {
		actor = ActivityActor{Type: "agent", ID: run.AssignedAgentID}
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: req.Scope, InitiativeID: initiative.ID, RunID: run.ID, ObjectiveID: run.ObjectiveID,
		AgentID: run.AssignedAgentID, EventType: "source_monitor.checkpoint_advanced", Severity: ActivitySeverityInfo,
		Actor: actor, Visibility: visibility, Summary: "Source monitor completed with no new evidence",
		Payload: map[string]interface{}{"monitorId": monitor.ID, "cursor": checkpoint.Cursor, "actionCallId": checkpoint.LastActionCallID}, CreatedAt: now,
	}
	if initiative.Owner.Type == OwnerTypeTeam {
		event.TeamID = initiative.Owner.ID
	}
	next, persisted, replayed, err := s.store.AdvanceSourceMonitorCheckpoint(ctx, checkpoint, req.ExpectedCheckpointRevision, event)
	if err != nil {
		return nil, err
	}
	return &SourceMonitorCheckpointResult{Checkpoint: next, Event: persisted, Replayed: replayed}, nil
}

func (s *SourceMonitorService) GetCheckpoint(ctx context.Context, scope Scope, initiativeID, monitorID string) (*SourceMonitorCheckpoint, error) {
	return s.store.GetSourceMonitorCheckpoint(ctx, scope, initiativeID, monitorID)
}

func (s *SourceMonitorService) Get(ctx context.Context, scope Scope, id string) (*SourceObservation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("source monitor service is not configured")
	}
	value, err := s.store.GetSourceObservation(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if sourceObservationExpiredAt(value, s.now().UTC()) {
		return nil, ErrSourceObservationNotFound
	}
	return value, nil
}

func (s *SourceMonitorService) List(ctx context.Context, filter SourceObservationFilter) ([]*SourceObservation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("source monitor service is not configured")
	}
	filter.RetainedAt = s.now().UTC()
	return s.store.ListSourceObservations(ctx, filter)
}

func sourceObservationExpiredAt(value *SourceObservation, at time.Time) bool {
	return value != nil && value.RetentionExpiresAt != nil && !value.RetentionExpiresAt.After(at)
}

func (o *SourceObservation) Validate() error {
	if o == nil || o.Scope.Validate() != nil || !validOpaqueIdentifier(o.ID, 128) || !validOpaqueIdentifier(o.InitiativeID, 128) ||
		!validOpaqueIdentifier(o.MonitorID, 128) || !validOpaqueIdentifier(o.RunID, 128) || !validOpaqueIdentifier(o.AgentID, 128) ||
		!validOpaqueIdentifier(o.SkillID, 128) || strings.TrimSpace(o.SkillVersion) == "" || !validOpaqueIdentifier(o.Action, 128) || !validOpaqueIdentifier(o.ActionCallID, 128) {
		return ErrInvalidSourceObservation
	}
	if err := validateSHA256Digest(o.DedupeKey); err != nil {
		return fmt.Errorf("%w: dedupe key: %v", ErrInvalidSourceObservation, err)
	}
	if err := validateSHA256Digest(o.ContentDigest); err != nil {
		return fmt.Errorf("%w: content digest: %v", ErrInvalidSourceObservation, err)
	}
	if err := validatePublicEvidenceURL(o.SourceURI); err != nil {
		return fmt.Errorf("%w: source URI: %v", ErrInvalidSourceObservation, err)
	}
	if o.Summary == "" || len(o.Summary) > 4000 || o.ObservedAt.IsZero() || o.IngestedAt.IsZero() {
		return fmt.Errorf("%w: summary and timestamps are required", ErrInvalidSourceObservation)
	}
	if o.RetentionExpiresAt != nil && !o.RetentionExpiresAt.After(o.IngestedAt) {
		return fmt.Errorf("%w: retention expiry must be after ingestion", ErrInvalidSourceObservation)
	}
	if o.ArtifactRef != nil {
		if o.ArtifactRef.Kind != ResourceKindArtifact || validateResourceRefs([]ResourceReference{*o.ArtifactRef}) != nil {
			return fmt.Errorf("%w: artifact reference is invalid", ErrInvalidSourceObservation)
		}
	}
	if err := validateCredentialFreeContext(o.Metadata); err != nil {
		return fmt.Errorf("%w: metadata: %v", ErrInvalidSourceObservation, err)
	}
	return nil
}

func sourceObservationDedupeKey(mode SourceMonitorDeduplication, monitorID, stableSourceID, contentDigest string) (string, error) {
	monitorID = strings.TrimSpace(monitorID)
	stableSourceID = strings.TrimSpace(stableSourceID)
	contentDigest = strings.ToLower(strings.TrimSpace(contentDigest))
	if err := validateSHA256Digest(contentDigest); err != nil {
		return "", fmt.Errorf("%w: content digest: %v", ErrInvalidSourceObservation, err)
	}
	var input string
	switch mode {
	case SourceMonitorDeduplicateStableSource:
		if stableSourceID == "" {
			return "", fmt.Errorf("%w: stable source id is required", ErrInvalidSourceObservation)
		}
		input = monitorID + "\x00stable\x00" + stableSourceID
	case SourceMonitorDeduplicateContentDigest:
		input = monitorID + "\x00content\x00" + contentDigest
	case SourceMonitorDeduplicateStableSourceAndContent:
		if stableSourceID == "" {
			return "", fmt.Errorf("%w: stable source id is required", ErrInvalidSourceObservation)
		}
		input = monitorID + "\x00combined\x00" + stableSourceID + "\x00" + contentDigest
	default:
		return "", fmt.Errorf("%w: unsupported deduplication strategy", ErrInvalidSourceObservation)
	}
	digest := sha256.Sum256([]byte(input))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func sourceObservationFingerprint(observation *SourceObservation) (string, error) {
	copy := cloneSourceObservation(observation)
	copy.IngestedAt, copy.Fingerprint = time.Time{}, ""
	encoded, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func initiativeSourceMonitor(initiative *Initiative, monitorID string) (SourceMonitorReference, bool) {
	for _, monitor := range initiative.SourceMonitors {
		if monitor.ID == strings.TrimSpace(monitorID) {
			return monitor, true
		}
	}
	return SourceMonitorReference{}, false
}

func cloneSourceObservation(value *SourceObservation) *SourceObservation {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var cloned SourceObservation
	_ = json.Unmarshal(encoded, &cloned)
	return &cloned
}

func cloneSourceMonitorCheckpoint(value *SourceMonitorCheckpoint) *SourceMonitorCheckpoint {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneResourceReference(value *ResourceReference) *ResourceReference {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}
