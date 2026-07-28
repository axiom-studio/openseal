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
	ErrProjectNotFound    = errors.New("project not found")
	ErrProjectConflict    = errors.New("project revision conflict")
	ErrProjectIdempotency = errors.New("project idempotency key was already used with different input")
	ErrProjectNoChanges   = errors.New("project update contains no changes")
	ErrInvalidProject     = errors.New("invalid project")
)

type ProjectStatus string

const (
	ProjectStatusDraft     ProjectStatus = "draft"
	ProjectStatusActive    ProjectStatus = "active"
	ProjectStatusPaused    ProjectStatus = "paused"
	ProjectStatusCompleted ProjectStatus = "completed"
	ProjectStatusFailed    ProjectStatus = "failed"
	ProjectStatusCanceled  ProjectStatus = "canceled"
	ProjectStatusArchived  ProjectStatus = "archived"
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
type ProjectMilestone struct {
	ID            string          `json:"id"`
	Title         string          `json:"title"`
	Status        MilestoneStatus `json:"status"`
	ObjectiveRefs []string        `json:"objectiveRefs,omitempty"`
	DueAt         *time.Time      `json:"dueAt,omitempty"`
	CompletedAt   *time.Time      `json:"completedAt,omitempty"`
}
type ProjectHypothesis struct {
	ID           string              `json:"id"`
	Statement    string              `json:"statement"`
	Confidence   float64             `json:"confidence"`
	EvidenceRefs []ResourceReference `json:"evidenceRefs,omitempty"`
	Status       HypothesisStatus    `json:"status,omitempty"`
	UpdatedAt    time.Time           `json:"updatedAt"`
}
type SourceMonitorDeduplication string

const (
	SourceMonitorDeduplicateStableSource           SourceMonitorDeduplication = "stable_source"
	SourceMonitorDeduplicateContentDigest          SourceMonitorDeduplication = "content_digest"
	SourceMonitorDeduplicateStableSourceAndContent SourceMonitorDeduplication = "stable_source_and_content"
)

// SourceMonitorReference attributes recurring source work without introducing
// a parallel scheduler. ObjectiveID points at the outcome; an activated
// Runbook carries the assigned Agent, trigger, bounded Run policy, and exact
// governed actions. The capability identity here is an immutable projection
// used for drift detection and inspection.
type SourceMonitorReference struct {
	ID              string                     `json:"id"`
	ObjectiveID     string                     `json:"objectiveId"`
	AssignedAgentID string                     `json:"assignedAgentId"`
	SkillID         string                     `json:"skillId"`
	SkillVersion    string                     `json:"skillVersion"`
	Action          string                     `json:"action"`
	SourcePolicyRef string                     `json:"sourcePolicyRef,omitempty"`
	Deduplication   SourceMonitorDeduplication `json:"deduplication"`
}
type ProjectDeliverable struct {
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

// Project is an optional durable coordination envelope for related Objectives.
// Runbooks and Runs are derived through those Objective references and remain
// authoritative in their own stores; a Project never maintains a parallel
// execution list or scheduler.
type Project struct {
	ID                  string                   `json:"id"`
	Scope               Scope                    `json:"scope"`
	Title               string                   `json:"title"`
	Purpose             string                   `json:"purpose"`
	Status              ProjectStatus            `json:"status"`
	Owner               ObjectiveOwner           `json:"owner"`
	AgentRefs           []ResourceReference      `json:"agentRefs,omitempty"`
	TeamRefs            []ResourceReference      `json:"teamRefs,omitempty"`
	ObjectiveRefs       []string                 `json:"objectiveRefs"`
	Milestones          []ProjectMilestone       `json:"milestones,omitempty"`
	Hypotheses          []ProjectHypothesis      `json:"hypotheses,omitempty"`
	SourceMonitors      []SourceMonitorReference `json:"sourceMonitors,omitempty"`
	Deliverables        []ProjectDeliverable     `json:"deliverables,omitempty"`
	Budget              *BudgetPolicy            `json:"budget,omitempty"`
	Policy              map[string]interface{}   `json:"policy,omitempty"`
	Checkpoint          map[string]interface{}   `json:"checkpoint,omitempty"`
	Revision            int64                    `json:"revision"`
	CreatedAt           time.Time                `json:"createdAt"`
	UpdatedAt           time.Time                `json:"updatedAt"`
	IdempotencyKeyHash  string                   `json:"idempotencyKeyHash,omitempty"`
	CreationFingerprint string                   `json:"creationFingerprint,omitempty"`
}

func (i *Project) Validate() error {
	if i == nil {
		return errors.New("project is required")
	}
	if err := i.Scope.Validate(); err != nil {
		return err
	}
	if err := i.Owner.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(i.ID) == "" || strings.TrimSpace(i.Title) == "" || strings.TrimSpace(i.Purpose) == "" {
		return errors.New("project id, title, and purpose are required")
	}
	switch i.Status {
	case ProjectStatusDraft, ProjectStatusActive, ProjectStatusPaused, ProjectStatusCompleted, ProjectStatusFailed, ProjectStatusCanceled, ProjectStatusArchived:
	default:
		return errors.New("project status is invalid")
	}
	if i.Revision < 1 {
		return errors.New("project revision must be positive")
	}
	if len(i.ObjectiveRefs) == 0 {
		return errors.New("project requires at least one objective reference")
	}
	if err := uniqueIDs(i.ObjectiveRefs, "objective"); err != nil {
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
				return errors.New("milestone objective must belong to project")
			}
		}
	}
	seen = map[string]bool{}
	for _, m := range i.SourceMonitors {
		if !validOpaqueIdentifier(m.ID, 128) || !validOpaqueIdentifier(m.ObjectiveID, 128) ||
			!validOpaqueIdentifier(m.AssignedAgentID, 128) || !validOpaqueIdentifier(m.SkillID, 128) ||
			strings.TrimSpace(m.SkillVersion) == "" || len(m.SkillVersion) > 128 || !validOpaqueIdentifier(m.Action, 128) || seen[m.ID] {
			return errors.New("source monitors require unique portable ids, Objective, assigned Agent, and Skill action identity")
		}
		if !objectiveSet[m.ObjectiveID] {
			return errors.New("source monitor objective must belong to project")
		}
		switch m.Deduplication {
		case SourceMonitorDeduplicateStableSource, SourceMonitorDeduplicateContentDigest, SourceMonitorDeduplicateStableSourceAndContent:
		default:
			return errors.New("source monitor requires an explicit deduplication strategy")
		}
		if strings.TrimSpace(m.SourcePolicyRef) == "" || len(m.SourcePolicyRef) > 256 {
			return errors.New("source monitor requires a source policy reference")
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
				return errors.New("deliverable objective must belong to project")
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
			return errors.New("project hypothesis id, statement, and confidence [0,1] are required")
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
		return fmt.Errorf("project policy: %w", err)
	}
	if err := validateCredentialFreeContext(i.Checkpoint); err != nil {
		return fmt.Errorf("project checkpoint: %w", err)
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

type ProjectFilter struct {
	Scope         Scope
	Owner         *ObjectiveOwner
	Statuses      []ProjectStatus
	ObjectiveID   string
	Limit, Offset int
}
type ProjectStore interface {
	CreateProject(context.Context, *Project) error
	CreateProjectWithEvent(context.Context, *Project, *ActivityEvent) (*ActivityEvent, error)
	GetProject(context.Context, Scope, string) (*Project, error)
	GetProjectByIdempotency(context.Context, Scope, string) (*Project, error)
	ListProjects(context.Context, ProjectFilter) ([]*Project, error)
	UpdateProject(context.Context, *Project, int64) error
	UpdateProjectWithEvent(context.Context, *Project, int64, *ActivityEvent) (*ActivityEvent, error)
}
type CreateProjectRequest struct {
	Project        *Project
	IdempotencyKey string
	Actor          ActivityActor
	Visibility     ActivityVisibility
}
type UpdateProjectRequest struct {
	ExpectedRevision int64
	Title            *string
	Purpose          *string
	Status           *ProjectStatus
	AgentRefs        *[]ResourceReference
	TeamRefs         *[]ResourceReference
	ObjectiveRefs    *[]string
	Milestones       *[]ProjectMilestone
	Hypotheses       *[]ProjectHypothesis
	SourceMonitors   *[]SourceMonitorReference
	Deliverables     *[]ProjectDeliverable
	Budget           *BudgetPolicy
	ClearBudget      bool
	Policy           map[string]interface{}
	Checkpoint       map[string]interface{}
	Actor            ActivityActor
	Visibility       ActivityVisibility
}

type ProjectService struct {
	store     ProjectStore
	portfolio PortfolioStore
	runbooks  RunbookActivationStore
	now       func() time.Time
}

func NewProjectService(store ProjectStore, portfolio PortfolioStore) *ProjectService {
	runbooks, _ := any(store).(RunbookActivationStore)
	if runbooks == nil {
		runbooks, _ = any(portfolio).(RunbookActivationStore)
	}
	return &ProjectService{store: store, portfolio: portfolio, runbooks: runbooks, now: time.Now}
}

func (s *ProjectService) Create(ctx context.Context, req CreateProjectRequest) (*Project, *ActivityEvent, error) {
	if s == nil || s.store == nil {
		return nil, nil, errors.New("project service is not configured")
	}
	i := cloneProject(req.Project)
	if i == nil {
		return nil, nil, errors.New("project is required")
	}
	now := s.now().UTC()
	if i.ID == "" {
		i.ID = uuid.NewString()
	}
	i.Status = normalizeProjectStatus(i.Status)
	i.Revision = 1
	i.CreatedAt = now
	i.UpdatedAt = now
	fp, err := projectCreationFingerprint(i)
	if err != nil {
		return nil, nil, err
	}
	i.CreationFingerprint = fp
	if req.IdempotencyKey != "" {
		sum := sha256.Sum256([]byte(req.IdempotencyKey))
		i.IdempotencyKeyHash = hex.EncodeToString(sum[:])
		existing, findErr := s.store.GetProjectByIdempotency(ctx, i.Scope, i.IdempotencyKeyHash)
		if errors.Is(findErr, ErrProjectNotFound) {
			findErr = nil
		}
		if findErr != nil {
			return nil, nil, findErr
		}
		if existing != nil {
			if existing.CreationFingerprint != fp {
				return nil, nil, ErrProjectIdempotency
			}
			return existing, nil, nil
		}
	}
	if err := i.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidProject, err)
	}
	if err := s.validateObjectives(ctx, i); err != nil {
		return nil, nil, err
	}
	if err := s.validateSourceMonitors(ctx, i); err != nil {
		return nil, nil, err
	}
	e := projectEvent(i, "project.created", req.Actor, req.Visibility, "Project created")
	e, err = s.store.CreateProjectWithEvent(ctx, i, e)
	if err != nil {
		if i.IdempotencyKeyHash != "" {
			winner, lookupErr := s.store.GetProjectByIdempotency(ctx, i.Scope, i.IdempotencyKeyHash)
			if lookupErr == nil {
				if winner.CreationFingerprint == fp {
					return winner, nil, nil
				}
				return nil, nil, ErrProjectIdempotency
			}
		}
		return nil, nil, err
	}
	return cloneProject(i), e, nil
}
func (s *ProjectService) Get(ctx context.Context, scope Scope, id string) (*Project, error) {
	return s.store.GetProject(ctx, scope, id)
}
func (s *ProjectService) List(ctx context.Context, f ProjectFilter) ([]*Project, error) {
	return s.store.ListProjects(ctx, f)
}
func (s *ProjectService) Patch(ctx context.Context, scope Scope, id string, req UpdateProjectRequest) (*Project, *ActivityEvent, error) {
	current, err := s.store.GetProject(ctx, scope, id)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, nil, ErrProjectConflict
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
		return nil, nil, ErrProjectNoChanges
	}
	return s.Update(ctx, current, req.ExpectedRevision, req.Actor, req.Visibility)
}
func (s *ProjectService) Update(ctx context.Context, i *Project, expected int64, actor ActivityActor, visibility ActivityVisibility) (*Project, *ActivityEvent, error) {
	if s == nil || s.store == nil {
		return nil, nil, errors.New("project service is not configured")
	}
	next := cloneProject(i)
	if next == nil {
		return nil, nil, errors.New("project is required")
	}
	if next.Revision != expected {
		return nil, nil, ErrProjectConflict
	}
	current, err := s.store.GetProject(ctx, next.Scope, next.ID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expected {
		return nil, nil, ErrProjectConflict
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
		return nil, nil, fmt.Errorf("%w: %v", ErrInvalidProject, err)
	}
	if err := s.validateObjectives(ctx, next); err != nil {
		return nil, nil, err
	}
	if err := s.validateSourceMonitors(ctx, next); err != nil {
		return nil, nil, err
	}
	e := projectEvent(next, "project.updated", actor, visibility, "Project updated")
	e, err = s.store.UpdateProjectWithEvent(ctx, next, expected, e)
	if err != nil {
		return nil, nil, err
	}
	return cloneProject(next), e, nil
}
func (s *ProjectService) validateObjectives(ctx context.Context, i *Project) error {
	if s.portfolio == nil {
		return errors.New("project objective verifier is not configured")
	}
	for _, id := range i.ObjectiveRefs {
		objective, err := s.portfolio.GetObjective(ctx, i.Scope, id)
		if err != nil || objective == nil {
			if err == nil {
				err = ErrObjectiveNotFound
			}
			return fmt.Errorf("project objective %s: %w", id, err)
		}
		if objective.Owner != i.Owner {
			return fmt.Errorf("project objective %s owner must match Project owner", id)
		}
	}
	return nil
}

func (s *ProjectService) validateSourceMonitors(ctx context.Context, i *Project) error {
	if len(i.SourceMonitors) > 0 && s.runbooks == nil {
		return errors.New("project Runbook verifier is not configured")
	}
	for _, monitor := range i.SourceMonitors {
		objective, err := s.portfolio.GetObjective(ctx, i.Scope, monitor.ObjectiveID)
		if err != nil || objective == nil {
			if err == nil {
				err = ErrObjectiveNotFound
			}
			return fmt.Errorf("source monitor %s objective: %w", monitor.ID, err)
		}
		if objective.Owner != i.Owner {
			return fmt.Errorf("source monitor %s objective owner must match Project owner", monitor.ID)
		}
		runbooks, err := s.runbooks.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: i.Scope, ObjectiveID: objective.ID, Limit: 500})
		if err != nil {
			return err
		}
		matched := false
		for _, activation := range runbooks {
			if activation.AssignedAgentID == monitor.AssignedAgentID && activation.Input["projectId"] == i.ID && activation.Input["sourceMonitorId"] == monitor.ID && activation.Policy["sourcePolicyRef"] == monitor.SourcePolicyRef {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("source monitor %s requires a matching Objective-owned Runbook", monitor.ID)
		}
	}
	return nil
}
func projectEvent(i *Project, typ string, actor ActivityActor, v ActivityVisibility, summary string) *ActivityEvent {
	if v == "" {
		v = ActivityVisibilityScope
	}
	return &ActivityEvent{ID: uuid.NewString(), Scope: i.Scope, EventType: typ, Severity: ActivitySeverityInfo, ProjectID: i.ID, TeamID: func() string {
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
func projectCreationFingerprint(i *Project) (string, error) {
	c := cloneProject(i)
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
func normalizeProjectStatus(s ProjectStatus) ProjectStatus {
	if s == "" {
		return ProjectStatusDraft
	}
	return s
}
func cloneProject(i *Project) *Project {
	if i == nil {
		return nil
	}
	b, _ := json.Marshal(i)
	var out Project
	_ = json.Unmarshal(b, &out)
	return &out
}
