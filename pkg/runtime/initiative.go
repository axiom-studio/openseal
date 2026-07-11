package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInitiativeNotFound    = errors.New("initiative not found")
	ErrInitiativeConflict    = errors.New("initiative revision conflict")
	ErrInitiativeIdempotency = errors.New("initiative idempotency key was already used with different input")
	ErrInitiativeNoChanges   = errors.New("initiative update contains no changes")
	ErrInvalidInitiative     = errors.New("invalid initiative")
)

type InitiativeStatus string

const (
	InitiativeStatusDraft     InitiativeStatus = "draft"
	InitiativeStatusActive    InitiativeStatus = "active"
	InitiativeStatusPaused    InitiativeStatus = "paused"
	InitiativeStatusCompleted InitiativeStatus = "completed"
	InitiativeStatusFailed    InitiativeStatus = "failed"
	InitiativeStatusCanceled  InitiativeStatus = "canceled"
	InitiativeStatusArchived  InitiativeStatus = "archived"
)

// ResourceReference points at a canonical kernel resource without duplicating it.
type ResourceKind string

const (
	ResourceKindAgentDefinition ResourceKind = "agent_definition"
	ResourceKindAgentDeployment ResourceKind = "agent_deployment"
	ResourceKindTeamDefinition  ResourceKind = "team_definition"
	ResourceKindTeamDeployment  ResourceKind = "team_deployment"
	ResourceKindArtifact        ResourceKind = "artifact"
	ResourceKindEvidence        ResourceKind = "evidence"
)

type ResourceReference struct {
	Kind     ResourceKind `json:"kind"`
	ID       string       `json:"id"`
	Version  string       `json:"version,omitempty"`
	Revision int64        `json:"revision,omitempty"`
}
type InitiativeMilestone struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Status        MilestoneStatus `json:"status"`
	ObjectiveRefs []string        `json:"objectiveRefs,omitempty"`
	DueAt         *time.Time      `json:"dueAt,omitempty"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
}
type InitiativeHypothesis struct {
	ID           string              `json:"id"`
	Statement    string              `json:"statement"`
	Confidence   float64             `json:"confidence"`
	EvidenceRefs []ResourceReference `json:"evidenceRefs,omitempty"`
	Status       HypothesisStatus    `json:"status,omitempty"`
	UpdatedAt    time.Time           `json:"updatedAt"`
}
type SourceMonitorReference struct {
	ID              string `json:"id"`
	SkillID         string `json:"skillId"`
	ScheduleRef     string `json:"scheduleRef,omitempty"`
	SourcePolicyRef string `json:"sourcePolicyRef,omitempty"`
	CheckpointRef   string `json:"checkpointRef,omitempty"`
}
type InitiativeDeliverable struct {
	ID            string              `json:"id"`
	Title         string              `json:"title"`
	Status        DeliverableStatus   `json:"status"`
	ArtifactRefs  []ResourceReference `json:"artifactRefs,omitempty"`
	ObjectiveRefs []string            `json:"objectiveRefs,omitempty"`
	DueAt         *time.Time          `json:"dueAt,omitempty"`
}
type MilestoneStatus string

const (
	MilestonePending    MilestoneStatus = "pending"
	MilestoneInProgress MilestoneStatus = "in_progress"
	MilestoneCompleted  MilestoneStatus = "completed"
	MilestoneBlocked    MilestoneStatus = "blocked"
	MilestoneCanceled   MilestoneStatus = "canceled"
)

type HypothesisStatus string

const (
	HypothesisOpen         HypothesisStatus = "open"
	HypothesisSupported    HypothesisStatus = "supported"
	HypothesisContradicted HypothesisStatus = "contradicted"
	HypothesisInconclusive HypothesisStatus = "inconclusive"
)

type DeliverableStatus string

const (
	DeliverablePlanned    DeliverableStatus = "planned"
	DeliverableInProgress DeliverableStatus = "in_progress"
	DeliverableReview     DeliverableStatus = "review"
	DeliverableDelivered  DeliverableStatus = "delivered"
	DeliverableCanceled   DeliverableStatus = "canceled"
)

// Initiative is a durable coordination envelope. Objectives, Runs, Artifacts,
// schedules, Agents and Teams remain authoritative in their own stores.
type Initiative struct {
	ID                  string                   `json:"id"`
	Scope               Scope                    `json:"scope"`
	Title               string                   `json:"title"`
	Purpose             string                   `json:"purpose"`
	Status              InitiativeStatus         `json:"status"`
	Owner               ObjectiveOwner           `json:"owner"`
	AgentRefs           []ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs            []ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs       []string                 `json:"objectiveRefs"`
	RunRefs             []string                 `json:"runRefs,omitempty"`
	Milestones          []InitiativeMilestone    `json:"milestones,omitempty"`
	Hypotheses          []InitiativeHypothesis   `json:"hypotheses,omitempty"`
	SourceMonitors      []SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables        []InitiativeDeliverable  `json:"deliverables,omitempty"`
	Budget              *BudgetPolicy            `json:"budget,omitempty"`
	Policy              map[string]interface{}   `json:"policy,omitempty"`
	Checkpoint          map[string]interface{}   `json:"checkpoint,omitempty"`
	Revision            int64                    `json:"revision"`
	CreatedAt           time.Time                `json:"createdAt"`
	UpdatedAt           time.Time                `json:"updatedAt"`
	IdempotencyKeyHash  string                   `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string                   `json:"creationFingerprint,omitempty"`
}

func (i *Initiative) Validate() error {
	if i == nil {
		return errors.New("initiative is required")
	}
	if err := i.Scope.Validate(); err != nil {
		return err
	}
	if err := i.Owner.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(i.ID) == "" || strings.TrimSpace(i.Title) == "" || strings.TrimSpace(i.Purpose) == "" {
		return errors.New("initiative id, title, and purpose are required")
	}
	switch i.Status {
	case InitiativeStatusDraft, InitiativeStatusActive, InitiativeStatusPaused, InitiativeStatusCompleted, InitiativeStatusFailed, InitiativeStatusCanceled, InitiativeStatusArchived:
	default:
		return errors.New("initiative status is invalid")
	}
	if i.Revision < 1 {
		return errors.New("initiative revision must be positive")
	}
	if len(i.ObjectiveRefs) == 0 {
		return errors.New("initiative requires at least one objective reference")
	}
	if err := uniqueIDs(i.ObjectiveRefs, "objective"); err != nil {
		return err
	}
	if err := uniqueIDs(i.RunRefs, "run"); err != nil {
		return err
	}
	objectiveSet := map[string]bool{}
	for _, id := range i.ObjectiveRefs {
		objectiveSet[id] = true
	}
	seen := map[string]bool{}
	for _, m := range i.Milestones {
		if m.ID == "" || m.Title == "" || seen[m.ID] {
			return errors.New("milestone ids must be unique and titles required")
		}
		seen[m.ID] = true
		switch m.Status {
		case MilestonePending, MilestoneInProgress, MilestoneCompleted, MilestoneBlocked, MilestoneCanceled:
		default:
			return errors.New("invalid milestone status")
		}
		if err := uniqueIDs(m.ObjectiveRefs, "milestone objective"); err != nil {
			return err
		}
		for _, id := range m.ObjectiveRefs {
			if !objectiveSet[id] {
				return errors.New("milestone objective must belong to initiative")
			}
		}
	}
	seen = map[string]bool{}
	for _, m := range i.SourceMonitors {
		if m.ID == "" || m.SkillID == "" || seen[m.ID] {
			return errors.New("source monitor ids must be unique and skill refs required")
		}
		seen[m.ID] = true
	}
	seen = map[string]bool{}
	for _, d := range i.Deliverables {
		if d.ID == "" || d.Title == "" || seen[d.ID] {
			return errors.New("deliverable ids must be unique and titles required")
		}
		seen[d.ID] = true
		switch d.Status {
		case DeliverablePlanned, DeliverableInProgress, DeliverableReview, DeliverableDelivered, DeliverableCanceled:
		default:
			return errors.New("invalid deliverable status")
		}
		if err := validateResourceRefs(d.ArtifactRefs); err != nil {
			return err
		}
		if err := uniqueIDs(d.ObjectiveRefs, "deliverable objective"); err != nil {
			return err
		}
		for _, id := range d.ObjectiveRefs {
			if !objectiveSet[id] {
				return errors.New("deliverable objective must belong to initiative")
			}
		}
	}
	if err := validateResourceRefs(i.AgentRefs); err != nil {
		return err
	}
	for _, r := range i.AgentRefs {
		if r.Kind != ResourceKindAgentDefinition && r.Kind != ResourceKindAgentDeployment {
			return errors.New("agent refs require an agent resource kind")
		}
	}
	if err := validateResourceRefs(i.TeamRefs); err != nil {
		return err
	}
	for _, r := range i.TeamRefs {
		if r.Kind != ResourceKindTeamDefinition && r.Kind != ResourceKindTeamDeployment {
			return errors.New("team refs require a team resource kind")
		}
	}
	seen = map[string]bool{}
	for _, h := range i.Hypotheses {
		if strings.TrimSpace(h.ID) == "" || strings.TrimSpace(h.Statement) == "" || seen[h.ID] || h.Confidence < 0 || h.Confidence > 1 {
			return errors.New("initiative hypothesis id, statement, and confidence [0,1] are required")
		}
		seen[h.ID] = true
		switch h.Status {
		case "", HypothesisOpen, HypothesisSupported, HypothesisContradicted, HypothesisInconclusive:
		default:
			return errors.New("invalid hypothesis status")
		}
		if err := validateResourceRefs(h.EvidenceRefs); err != nil {
			return err
		}
	}
	if i.Budget != nil {
		if err := i.Budget.Validate(); err != nil {
			return err
		}
	}
	if err := validateCredentialFreeContext(i.Policy); err != nil {
		return fmt.Errorf("initiative policy: %w", err)
	}
	if err := validateCredentialFreeContext(i.Checkpoint); err != nil {
		return fmt.Errorf("initiative checkpoint: %w", err)
	}
	return nil
}
func validateResourceRefs(refs []ResourceReference) error {
	seen := map[string]bool{}
	for _, r := range refs {
		k := string(r.Kind) + "\x00" + r.ID + "\x00" + r.Version + fmt.Sprint(r.Revision)
		if !validResourceKind(r.Kind) || r.ID == "" || seen[k] || r.Revision < 0 {
			return errors.New("resource references must be valid and unique")
		}
		seen[k] = true
	}
	return nil
}
func validResourceKind(k ResourceKind) bool {
	switch k {
	case ResourceKindAgentDefinition, ResourceKindAgentDeployment, ResourceKindTeamDefinition, ResourceKindTeamDeployment, ResourceKindArtifact, ResourceKindEvidence:
		return true
	}
	return false
}

func uniqueIDs(ids []string, kind string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return fmt.Errorf("%s references must be non-empty and unique", kind)
		}
		seen[id] = true
	}
	return nil
}

type InitiativeFilter struct {
	Scope         Scope
	Owner         *ObjectiveOwner
	Statuses      []InitiativeStatus
	ObjectiveID   string
	Limit, Offset int
}
type InitiativeStore interface {
	CreateInitiative(context.Context, *Initiative) error
	CreateInitiativeWithEvent(context.Context, *Initiative, *ActivityEvent) (*ActivityEvent, error)
	GetInitiative(context.Context, Scope, string) (*Initiative, error)
	GetInitiativeByIdempotency(context.Context, Scope, string) (*Initiative, error)
	ListInitiatives(context.Context, InitiativeFilter) ([]*Initiative, error)
	UpdateInitiative(context.Context, *Initiative, int64) error
	UpdateInitiativeWithEvent(context.Context, *Initiative, int64, *ActivityEvent) (*ActivityEvent, error)
}
type CreateInitiativeRequest struct {
	Initiative     *Initiative
	IdempotencyKey string
	Actor          ActivityActor
	Visibility     ActivityVisibility
}
type UpdateInitiativeRequest struct {
	ExpectedRevision int64
	Title            *string
	Purpose          *string
	Status           *InitiativeStatus
	AgentRefs        *[]ResourceReference
	TeamRefs         *[]ResourceReference
	ObjectiveRefs    *[]string
	RunRefs          *[]string
	Milestones       *[]InitiativeMilestone
	Hypotheses       *[]InitiativeHypothesis
	SourceMonitors   *[]SourceMonitorReference
	Deliverables     *[]InitiativeDeliverable
	Budget           *BudgetPolicy
	ClearBudget      bool
	Policy           map[string]interface{}
	Checkpoint       map[string]interface{}
	Actor            ActivityActor
	Visibility       ActivityVisibility
}

type InitiativeService struct {
	store     InitiativeStore
	portfolio PortfolioStore
	now       func() time.Time
}

func NewInitiativeService(store InitiativeStore, portfolio PortfolioStore) *InitiativeService {
	return &InitiativeService{store: store, portfolio: portfolio, now: time.Now}
}

func (s *InitiativeService) Create(ctx context.Context, req CreateInitiativeRequest) (*Initiative, *ActivityEvent, error) {
	if s == nil || s.store == nil {
		return nil, nil, errors.New("initiative service is not configured")
	}
	i := cloneInitiative(req.Initiative)
	if i == nil {
		return nil, nil, errors.New("initiative is required")
	}
	now := s.now().UTC()
	if i.ID == "" {
		i.ID = uuid.NewString()
	}
	i.Status = normalizeInitiativeStatus(i.Status)
	i.Revision = 1
	i.CreatedAt = now
	i.UpdatedAt = now
	fp, err := initiativeCreationFingerprint(i)
	if err != nil {
		return nil, nil, err
	}
	i.CreationFingerprint = fp
	if req.IdempotencyKey != "" {
		sum := sha256.Sum256([]byte(req.IdempotencyKey))
		i.IdempotencyKeyHash = hex.EncodeToString(sum[:])
		existing, findErr := s.store.GetInitiativeByIdempotency(ctx, i.Scope, i.IdempotencyKeyHash)
		if errors.Is(findErr, ErrInitiativeNotFound) {
			findErr = nil
		}
		if findErr != nil {
			return nil, nil, findErr
		}
		if existing != nil {
			if existing.CreationFingerprint != fp {
				return nil, nil, ErrInitiativeIdempotency
			}
			return existing, nil, nil
		}
	}
	if err := i.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidInitiative, err)
	}
	if err := s.validateObjectives(ctx, i); err != nil {
		return nil, nil, err
	}
	e := initiativeEvent(i, "initiative.created", req.Actor, req.Visibility, "Initiative created")
	e, err = s.store.CreateInitiativeWithEvent(ctx, i, e)
	if err != nil {
		if i.IdempotencyKeyHash != "" {
			winner, lookupErr := s.store.GetInitiativeByIdempotency(ctx, i.Scope, i.IdempotencyKeyHash)
			if lookupErr == nil {
				if winner.CreationFingerprint == fp {
					return winner, nil, nil
				}
				return nil, nil, ErrInitiativeIdempotency
			}
		}
		return nil, nil, err
	}
	return cloneInitiative(i), e, nil
}
func (s *InitiativeService) Get(ctx context.Context, scope Scope, id string) (*Initiative, error) {
	return s.store.GetInitiative(ctx, scope, id)
}
func (s *InitiativeService) List(ctx context.Context, f InitiativeFilter) ([]*Initiative, error) {
	return s.store.ListInitiatives(ctx, f)
}
func (s *InitiativeService) Patch(ctx context.Context, scope Scope, id string, req UpdateInitiativeRequest) (*Initiative, *ActivityEvent, error) {
	current, err := s.store.GetInitiative(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, nil, ErrInitiativeConflict
	}
	before, _ := json.Marshal(current)
	if req.Title != nil {
		current.Title = *req.Title
	}
	if req.Purpose != nil {
		current.Purpose = *req.Purpose
	}
	if req.Status != nil {
		current.Status = *req.Status
	}
	if req.AgentRefs != nil {
		current.AgentRefs = *req.AgentRefs
	}
	if req.TeamRefs != nil {
		current.TeamRefs = *req.TeamRefs
	}
	if req.ObjectiveRefs != nil {
		current.ObjectiveRefs = *req.ObjectiveRefs
	}
	if req.RunRefs != nil {
		current.RunRefs = *req.RunRefs
	}
	if req.Milestones != nil {
		current.Milestones = *req.Milestones
	}
	if req.Hypotheses != nil {
		current.Hypotheses = *req.Hypotheses
	}
	if req.SourceMonitors != nil {
		current.SourceMonitors = *req.SourceMonitors
	}
	if req.Deliverables != nil {
		current.Deliverables = *req.Deliverables
	}
	if req.ClearBudget {
		current.Budget = nil
	} else if req.Budget != nil {
		current.Budget = req.Budget
	}
	if req.Policy != nil {
		current.Policy = req.Policy
	}
	if req.Checkpoint != nil {
		current.Checkpoint = req.Checkpoint
	}
	after, _ := json.Marshal(current)
	if string(before) == string(after) {
		return nil, nil, ErrInitiativeNoChanges
	}
	return s.Update(ctx, current, req.ExpectedRevision, req.Actor, req.Visibility)
}
func (s *InitiativeService) Update(ctx context.Context, i *Initiative, expected int64, actor ActivityActor, visibility ActivityVisibility) (*Initiative, *ActivityEvent, error) {
	if s == nil || s.store == nil {
		return nil, nil, errors.New("initiative service is not configured")
	}
	next := cloneInitiative(i)
	if next == nil {
		return nil, nil, errors.New("initiative is required")
	}
	if next.Revision != expected {
		return nil, nil, ErrInitiativeConflict
	}
	current, err := s.store.GetInitiative(ctx, next.Scope, next.ID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expected {
		return nil, nil, ErrInitiativeConflict
	}
	// Identity, ownership and creation provenance are immutable. Updates are a
	// replacement of mutable fields on the currently persisted aggregate.
	next.ID = current.ID
	next.Scope = current.Scope
	next.Owner = current.Owner
	next.CreatedAt = current.CreatedAt
	next.IdempotencyKeyHash = current.IdempotencyKeyHash
	next.CreationFingerprint = current.CreationFingerprint
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := next.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidInitiative, err)
	}
	if err := s.validateObjectives(ctx, next); err != nil {
		return nil, nil, err
	}
	e := initiativeEvent(next, "initiative.updated", actor, visibility, "Initiative updated")
	e, err = s.store.UpdateInitiativeWithEvent(ctx, next, expected, e)
	if err != nil {
		return nil, nil, err
	}
	return cloneInitiative(next), e, nil
}
func (s *InitiativeService) validateObjectives(ctx context.Context, i *Initiative) error {
	if s.portfolio == nil {
		return errors.New("initiative objective verifier is not configured")
	}
	for _, id := range i.ObjectiveRefs {
		objective, err := s.portfolio.GetObjective(ctx, i.Scope, id)
		if err != nil || objective == nil {
			if err == nil {
				err = ErrObjectiveNotFound
			}
			return fmt.Errorf("initiative objective %s: %w", id, err)
		}
	}
	return nil
}
func initiativeEvent(i *Initiative, typ string, actor ActivityActor, v ActivityVisibility, summary string) *ActivityEvent {
	if v == "" {
		v = ActivityVisibilityScope
	}
	return &ActivityEvent{ID: uuid.NewString(), Scope: i.Scope, EventType: typ, Severity: ActivitySeverityInfo, InitiativeID: i.ID, TeamID: func() string {
		if i.Owner.Type == OwnerTypeTeam {
			return i.Owner.ID
		}
		return ""
	}(), AgentID: func() string {
		if i.Owner.Type == OwnerTypeAgent {
			return i.Owner.ID
		}
		return ""
	}(), Actor: actor, Summary: summary, Visibility: v, Payload: map[string]interface{}{"status": i.Status, "revision": i.Revision}, CreatedAt: i.UpdatedAt}
}
func initiativeCreationFingerprint(i *Initiative) (string, error) {
	c := cloneInitiative(i)
	c.ID = ""
	c.Revision = 0
	c.CreatedAt = time.Time{}
	c.UpdatedAt = time.Time{}
	c.IdempotencyKeyHash = ""
	c.CreationFingerprint = ""
	sort.Strings(c.ObjectiveRefs)
	b, e := json.Marshal(c)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func normalizeInitiativeStatus(s InitiativeStatus) InitiativeStatus {
	if s == "" {
		return InitiativeStatusDraft
	}
	return s
}
func cloneInitiative(i *Initiative) *Initiative {
	if i == nil {
		return nil
	}
	b, _ := json.Marshal(i)
	var out Initiative
	_ = json.Unmarshal(b, &out)
	return &out
}
