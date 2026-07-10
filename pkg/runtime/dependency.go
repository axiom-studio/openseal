package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrRunDependencyNotFound   = errors.New("run dependency not found")
	ErrDependencyGroupNotFound = errors.New("run dependency group not found")
	ErrInvalidRunDependency    = errors.New("invalid run dependency")
	ErrDependencyConflict      = errors.New("run dependency conflict")
)

type RunDependencyKind string

const (
	RunDependencyKindRun          RunDependencyKind = "run"
	RunDependencyKindAgentRequest RunDependencyKind = "agent_request"
	RunDependencyKindHandoff      RunDependencyKind = "handoff"
	RunDependencyKindAction       RunDependencyKind = "action"
	RunDependencyKindApproval     RunDependencyKind = "approval"
)

type RunDependencyState string

const (
	RunDependencyStatePending   RunDependencyState = "pending"
	RunDependencyStateRunning   RunDependencyState = "running"
	RunDependencyStateSatisfied RunDependencyState = "satisfied"
	RunDependencyStateFailed    RunDependencyState = "failed"
	RunDependencyStateCanceled  RunDependencyState = "canceled"
)

type FanInMode string

const (
	FanInModeAll    FanInMode = "all"
	FanInModeAny    FanInMode = "any"
	FanInModeQuorum FanInMode = "quorum"
)

type DependencyFailureMode string

const (
	DependencyFailureFailFast DependencyFailureMode = "fail_fast"
	DependencyFailureWait     DependencyFailureMode = "wait"
)

type RunDependencyGroupStatus string

const (
	RunDependencyGroupOpen      RunDependencyGroupStatus = "open"
	RunDependencyGroupWaiting   RunDependencyGroupStatus = "waiting"
	RunDependencyGroupSatisfied RunDependencyGroupStatus = "satisfied"
	RunDependencyGroupFailed    RunDependencyGroupStatus = "failed"
	RunDependencyGroupCanceled  RunDependencyGroupStatus = "canceled"
)

// RunDependencyPolicy controls how a sealed set of dependency edges joins.
// Groups are populated before sealing so a fast child cannot wake its source
// before the complete fan-out is durably known.
type RunDependencyPolicy struct {
	Mode        FanInMode             `json:"mode"`
	Quorum      int                   `json:"quorum,omitempty"`
	FailureMode DependencyFailureMode `json:"failureMode"`
}

func (p RunDependencyPolicy) Validate() error {
	switch p.Mode {
	case FanInModeAll, FanInModeAny:
		if p.Quorum != 0 {
			return fmt.Errorf("%w: quorum is only valid for quorum fan-in", ErrInvalidRunDependency)
		}
	case FanInModeQuorum:
		if p.Quorum <= 0 {
			return fmt.Errorf("%w: quorum fan-in requires a positive quorum", ErrInvalidRunDependency)
		}
	default:
		return fmt.Errorf("%w: fan-in mode must be all, any, or quorum", ErrInvalidRunDependency)
	}
	if p.FailureMode != DependencyFailureFailFast && p.FailureMode != DependencyFailureWait {
		return fmt.Errorf("%w: failure mode must be fail_fast or wait", ErrInvalidRunDependency)
	}
	return nil
}

// RunDependencyGroup is the durable source of truth for one fan-out/fan-in
// phase. WakeCondition is only a compiled projection of this record.
type RunDependencyGroup struct {
	ID             string                   `json:"id"`
	Scope          Scope                    `json:"scope"`
	SourceRunID    string                   `json:"sourceRunId"`
	Policy         RunDependencyPolicy      `json:"policy"`
	ExpectedCount  int                      `json:"expectedCount,omitempty"`
	Status         RunDependencyGroupStatus `json:"status"`
	Revision       int64                    `json:"revision"`
	CreatedAt      time.Time                `json:"createdAt"`
	UpdatedAt      time.Time                `json:"updatedAt"`
	SealedAt       *time.Time               `json:"sealedAt,omitempty"`
	ResolvedAt     *time.Time               `json:"resolvedAt,omitempty"`
	WakeSignalID   string                   `json:"wakeSignalId,omitempty"`
	IdempotencyKey string                   `json:"idempotencyKey,omitempty"`
}

type RunDependencySpec struct {
	ID          string
	TargetRunID string
	RequestID   string
	Kind        RunDependencyKind
	Required    *bool
}

type CreateRunDependencyGroupRequest struct {
	ID                     string
	Scope                  Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	Policy                 RunDependencyPolicy
	Dependencies           []RunDependencySpec
	IdempotencyKey         string
	Actor                  ActivityActor
	Visibility             ActivityVisibility
}

type ResolveRunDependencyRequest struct {
	Scope                      Scope
	GroupID                    string
	DependencyID               string
	ExpectedDependencyRevision int64
	State                      RunDependencyState
	Result                     map[string]interface{}
	Artifacts                  []ArtifactReference
	Error                      string
	Actor                      ActivityActor
	Visibility                 ActivityVisibility
}

type RunDependencyResult struct {
	Group        *RunDependencyGroup     `json:"group"`
	Dependency   *RunDependency          `json:"dependency,omitempty"`
	Dependencies []*RunDependency        `json:"dependencies,omitempty"`
	Source       *AgentRun               `json:"sourceRun"`
	Evaluation   RunDependencyEvaluation `json:"evaluation"`
	Events       []*ActivityEvent        `json:"events,omitempty"`
	Replayed     bool                    `json:"replayed"`
}

type RunDependencyGroupCreateRecord struct {
	Group                  *RunDependencyGroup
	Dependencies           []*RunDependency
	SourceRun              *AgentRun
	ExpectedSourceRevision int64
	Event                  *ActivityEvent
}

type RunDependencyResolutionRecord struct {
	Scope                      Scope
	GroupID                    string
	DependencyID               string
	ExpectedDependencyRevision int64
	State                      RunDependencyState
	Result                     map[string]interface{}
	Artifacts                  []ArtifactReference
	Error                      string
	Actor                      ActivityActor
	Visibility                 ActivityVisibility
	OccurredAt                 time.Time
}

type RunDependencyStore interface {
	CreateRunDependencyGroup(ctx context.Context, record RunDependencyGroupCreateRecord) (*RunDependencyResult, error)
	GetRunDependencyGroup(ctx context.Context, scope Scope, groupID string) (*RunDependencyGroup, error)
	FindRunDependencyGroupByIdempotencyKey(ctx context.Context, scope Scope, key string) (*RunDependencyGroup, error)
	ListRunDependencies(ctx context.Context, scope Scope, groupID string) ([]*RunDependency, error)
	ResolveRunDependency(ctx context.Context, record RunDependencyResolutionRecord) (*RunDependencyResult, error)
}

type DependencyKernelStore interface {
	PortfolioStore
	RunActivityStore
	RunDependencyStore
}

type DependencyCoordinator struct {
	store RunDependencyStore
	runs  PortfolioStore
	now   func() time.Time
	newID func() string
}

func NewDependencyCoordinator(store DependencyKernelStore) *DependencyCoordinator {
	return &DependencyCoordinator{store: store, runs: store, now: time.Now, newID: uuid.NewString}
}

func (c *DependencyCoordinator) CreateRunDependencyGroup(ctx context.Context, req CreateRunDependencyGroupRequest) (*RunDependencyResult, error) {
	if c == nil || c.store == nil || c.runs == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(req.SourceRunID, 128) || len(req.Dependencies) == 0 {
		return nil, fmt.Errorf("%w: source run and at least one dependency are required", ErrInvalidRunDependency)
	}
	if req.ExpectedSourceRevision <= 0 {
		return nil, fmt.Errorf("%w: expected source revision must be positive", ErrInvalidRunDependency)
	}
	if err := req.Policy.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" {
		return nil, fmt.Errorf("%w: activity actor is required", ErrInvalidRunDependency)
	}
	if req.Visibility == "" {
		req.Visibility = ActivityVisibilityScope
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key != "" {
		existing, err := c.store.FindRunDependencyGroupByIdempotencyKey(ctx, req.Scope, key)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			edges, err := c.store.ListRunDependencies(ctx, req.Scope, existing.ID)
			if err != nil {
				return nil, err
			}
			if !sameDependencyGroupRequest(existing, edges, req) {
				return nil, ErrDependencyConflict
			}
			source, err := c.runs.GetAgentRun(ctx, req.Scope, existing.SourceRunID)
			if err != nil {
				return nil, err
			}
			evaluation, err := EvaluateRunDependencies(existing, edges)
			return &RunDependencyResult{Group: existing, Dependencies: edges, Source: source, Evaluation: evaluation, Replayed: true}, err
		}
	}
	source, err := c.runs.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrRunNotFound
	}
	if source.Revision != req.ExpectedSourceRevision {
		return nil, ErrRevisionConflict
	}
	if !canEnterDependencyWait(source) {
		return nil, fmt.Errorf("%w: source run cannot enter dependency wait from %s", ErrInvalidRunTransition, source.Status)
	}
	now := c.now().UTC()
	groupID := strings.TrimSpace(req.ID)
	if groupID == "" {
		groupID = c.newID()
	}
	sealedAt := now
	group := &RunDependencyGroup{
		ID: groupID, Scope: req.Scope, SourceRunID: source.ID, Policy: req.Policy, ExpectedCount: len(req.Dependencies),
		Status: RunDependencyGroupWaiting, Revision: 1, CreatedAt: now, UpdatedAt: now, SealedAt: &sealedAt,
		IdempotencyKey: key,
	}
	edges := make([]*RunDependency, 0, len(req.Dependencies))
	for _, spec := range req.Dependencies {
		edgeID := strings.TrimSpace(spec.ID)
		if edgeID == "" {
			edgeID = c.newID()
		}
		required := true
		if spec.Required != nil {
			required = *spec.Required
		}
		edge := &RunDependency{
			ID: edgeID, Scope: req.Scope, GroupID: group.ID, SourceRunID: source.ID,
			TargetRunID: strings.TrimSpace(spec.TargetRunID), RequestID: strings.TrimSpace(spec.RequestID), Kind: spec.Kind,
			State: RunDependencyStatePending, Required: required, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		if err := edge.Validate(); err != nil {
			return nil, err
		}
		edges = append(edges, edge)
	}
	if _, err := EvaluateRunDependencies(group, edges); err != nil {
		return nil, err
	}
	updatedSource := cloneAgentRun(source)
	updatedSource.Status = AgentRunStatusWaitingForDependency
	updatedSource.WakeCondition = &WakeCondition{Type: "run_dependencies", Reference: group.ID}
	updatedSource.Revision++
	updatedSource.UpdatedAt = now
	updatedSource.LeaseOwner = ""
	updatedSource.LeaseExpiresAt = nil
	event := dependencyGroupEvent(updatedSource, group, edges, req.Actor, req.Visibility, now)
	return c.store.CreateRunDependencyGroup(ctx, RunDependencyGroupCreateRecord{
		Group: group, Dependencies: edges, SourceRun: updatedSource, ExpectedSourceRevision: source.Revision, Event: event,
	})
}

func (c *DependencyCoordinator) ResolveRunDependency(ctx context.Context, req ResolveRunDependencyRequest) (*RunDependencyResult, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !validOpaqueIdentifier(req.GroupID, 128) || !validOpaqueIdentifier(req.DependencyID, 128) || req.ExpectedDependencyRevision <= 0 {
		return nil, fmt.Errorf("%w: group, dependency, and expected revision are required", ErrInvalidRunDependency)
	}
	if req.State != RunDependencyStateSatisfied && req.State != RunDependencyStateFailed && req.State != RunDependencyStateCanceled {
		return nil, fmt.Errorf("%w: dependency resolution must be satisfied, failed, or canceled", ErrInvalidRunDependency)
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" {
		return nil, fmt.Errorf("%w: activity actor is required", ErrInvalidRunDependency)
	}
	if req.Visibility == "" {
		req.Visibility = ActivityVisibilityScope
	}
	record := RunDependencyResolutionRecord{
		Scope: req.Scope, GroupID: strings.TrimSpace(req.GroupID), DependencyID: strings.TrimSpace(req.DependencyID),
		ExpectedDependencyRevision: req.ExpectedDependencyRevision, State: req.State, Result: cloneMap(req.Result),
		Artifacts: cloneArtifactReferences(req.Artifacts), Error: strings.TrimSpace(req.Error), Actor: req.Actor,
		Visibility: req.Visibility, OccurredAt: c.now().UTC(),
	}
	return c.store.ResolveRunDependency(ctx, record)
}

func (c *DependencyCoordinator) GetRunDependencyGroup(ctx context.Context, scope Scope, groupID string) (*RunDependencyGroup, error) {
	group, err := c.store.GetRunDependencyGroup(ctx, scope, strings.TrimSpace(groupID))
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, ErrDependencyGroupNotFound
	}
	return group, nil
}

func (c *DependencyCoordinator) ListRunDependencies(ctx context.Context, scope Scope, groupID string) ([]*RunDependency, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("run dependency store is not configured")
	}
	return c.store.ListRunDependencies(ctx, scope, strings.TrimSpace(groupID))
}

func (g *RunDependencyGroup) Validate() error {
	if g == nil {
		return fmt.Errorf("%w: dependency group is required", ErrInvalidRunDependency)
	}
	if err := g.Scope.Validate(); err != nil {
		return err
	}
	if !validOpaqueIdentifier(g.ID, 128) || !validOpaqueIdentifier(g.SourceRunID, 128) {
		return fmt.Errorf("%w: dependency group and source run ids must be portable opaque identifiers", ErrInvalidRunDependency)
	}
	if err := g.Policy.Validate(); err != nil {
		return err
	}
	if g.ExpectedCount < 0 || g.Revision <= 0 || g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: expected count cannot be negative and revision/timestamps are required", ErrInvalidRunDependency)
	}
	if strings.ContainsAny(g.IdempotencyKey, "\r\n") || len(g.IdempotencyKey) > 256 {
		return fmt.Errorf("%w: dependency idempotency key cannot exceed 256 characters or contain line breaks", ErrInvalidRunDependency)
	}
	switch g.Status {
	case RunDependencyGroupOpen:
		if g.SealedAt != nil || g.ResolvedAt != nil || g.WakeSignalID != "" {
			return fmt.Errorf("%w: open dependency group cannot be sealed or resolved", ErrInvalidRunDependency)
		}
	case RunDependencyGroupWaiting:
		if g.SealedAt == nil || g.ResolvedAt != nil || g.WakeSignalID != "" {
			return fmt.Errorf("%w: waiting dependency group must be sealed but unresolved", ErrInvalidRunDependency)
		}
	case RunDependencyGroupSatisfied, RunDependencyGroupFailed, RunDependencyGroupCanceled:
		if g.SealedAt == nil || g.ResolvedAt == nil || strings.TrimSpace(g.WakeSignalID) == "" {
			return fmt.Errorf("%w: terminal dependency group requires seal, resolution, and wake signal", ErrInvalidRunDependency)
		}
	default:
		return fmt.Errorf("%w: invalid dependency group status %q", ErrInvalidRunDependency, g.Status)
	}
	return nil
}

// RunDependency is one typed edge from the source run to independently
// progressing work. RequestID identifies the collaboration fact while
// TargetRunID identifies the child execution once it exists.
type RunDependency struct {
	ID          string                 `json:"id"`
	Scope       Scope                  `json:"scope"`
	GroupID     string                 `json:"groupId"`
	SourceRunID string                 `json:"sourceRunId"`
	TargetRunID string                 `json:"targetRunId,omitempty"`
	RequestID   string                 `json:"requestId,omitempty"`
	Kind        RunDependencyKind      `json:"kind"`
	State       RunDependencyState     `json:"state"`
	Required    bool                   `json:"required"`
	Result      map[string]interface{} `json:"result,omitempty"`
	Artifacts   []ArtifactReference    `json:"artifacts,omitempty"`
	Error       string                 `json:"error,omitempty"`
	Revision    int64                  `json:"revision"`
	CreatedAt   time.Time              `json:"createdAt"`
	UpdatedAt   time.Time              `json:"updatedAt"`
	ResolvedAt  *time.Time             `json:"resolvedAt,omitempty"`
}

func (d *RunDependency) Validate() error {
	if d == nil {
		return fmt.Errorf("%w: dependency is required", ErrInvalidRunDependency)
	}
	if err := d.Scope.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]string{"dependency": d.ID, "group": d.GroupID, "source run": d.SourceRunID} {
		if !validOpaqueIdentifier(value, 128) {
			return fmt.Errorf("%w: %s id must be a portable opaque identifier", ErrInvalidRunDependency, name)
		}
	}
	switch d.Kind {
	case RunDependencyKindRun:
		if !validOpaqueIdentifier(d.TargetRunID, 128) {
			return fmt.Errorf("%w: run dependency requires a target run", ErrInvalidRunDependency)
		}
	case RunDependencyKindAgentRequest, RunDependencyKindHandoff:
		if !validOpaqueIdentifier(d.RequestID, 128) {
			return fmt.Errorf("%w: collaboration dependency requires an agent request", ErrInvalidRunDependency)
		}
	case RunDependencyKindAction, RunDependencyKindApproval:
		if strings.TrimSpace(d.RequestID) == "" && strings.TrimSpace(d.TargetRunID) == "" {
			return fmt.Errorf("%w: governed dependency requires a request or target reference", ErrInvalidRunDependency)
		}
	default:
		return fmt.Errorf("%w: invalid dependency kind %q", ErrInvalidRunDependency, d.Kind)
	}
	if d.TargetRunID != "" && !validOpaqueIdentifier(d.TargetRunID, 128) {
		return fmt.Errorf("%w: target run id must be portable", ErrInvalidRunDependency)
	}
	if d.RequestID != "" && !validOpaqueIdentifier(d.RequestID, 128) {
		return fmt.Errorf("%w: request id must be portable", ErrInvalidRunDependency)
	}
	if d.Revision <= 0 || d.CreatedAt.IsZero() || d.UpdatedAt.IsZero() {
		return fmt.Errorf("%w: dependency revision and timestamps are required", ErrInvalidRunDependency)
	}
	switch d.State {
	case RunDependencyStatePending, RunDependencyStateRunning:
		if d.ResolvedAt != nil || len(d.Result) > 0 || len(d.Artifacts) > 0 || strings.TrimSpace(d.Error) != "" {
			return fmt.Errorf("%w: unresolved dependency cannot contain terminal output", ErrInvalidRunDependency)
		}
	case RunDependencyStateSatisfied:
		if d.ResolvedAt == nil || strings.TrimSpace(d.Error) != "" {
			return fmt.Errorf("%w: satisfied dependency requires resolution without error", ErrInvalidRunDependency)
		}
	case RunDependencyStateFailed, RunDependencyStateCanceled:
		if d.ResolvedAt == nil {
			return fmt.Errorf("%w: terminal dependency requires resolution", ErrInvalidRunDependency)
		}
	default:
		return fmt.Errorf("%w: invalid dependency state %q", ErrInvalidRunDependency, d.State)
	}
	if err := validateCredentialFreeContext(d.Result); err != nil {
		return fmt.Errorf("%w: dependency result: %v", ErrInvalidRunDependency, err)
	}
	if err := validateArtifactReferences(nil, d.Artifacts); err != nil {
		return fmt.Errorf("%w: dependency artifacts: %v", ErrInvalidRunDependency, err)
	}
	return nil
}

type RunDependencyEvaluation struct {
	Status                RunDependencyGroupStatus `json:"status"`
	Wake                  bool                     `json:"wake"`
	Satisfied             int                      `json:"satisfied"`
	Failed                int                      `json:"failed"`
	Canceled              int                      `json:"canceled"`
	Pending               int                      `json:"pending"`
	Required              int                      `json:"required"`
	SatisfiedDependencies []string                 `json:"satisfiedDependencies,omitempty"`
	FailedDependencies    []string                 `json:"failedDependencies,omitempty"`
}

// EvaluateRunDependencies is pure and order-independent. Stores use its
// decision inside the same transaction that resolves an edge and updates the
// source run, making duplicate or reordered completions harmless.
func EvaluateRunDependencies(group *RunDependencyGroup, edges []*RunDependency) (RunDependencyEvaluation, error) {
	if err := group.Validate(); err != nil {
		return RunDependencyEvaluation{}, err
	}
	evaluation := RunDependencyEvaluation{Status: group.Status}
	seen := make(map[string]struct{}, len(edges))
	allTerminal := true
	requiredTerminal := true
	requiredFailure := false
	for _, edge := range edges {
		if err := edge.Validate(); err != nil {
			return RunDependencyEvaluation{}, err
		}
		if edge.Scope != group.Scope || edge.GroupID != group.ID || edge.SourceRunID != group.SourceRunID {
			return RunDependencyEvaluation{}, fmt.Errorf("%w: dependency %s does not belong to group %s", ErrInvalidRunDependency, edge.ID, group.ID)
		}
		if _, exists := seen[edge.ID]; exists {
			return RunDependencyEvaluation{}, fmt.Errorf("%w: duplicate dependency %s", ErrInvalidRunDependency, edge.ID)
		}
		seen[edge.ID] = struct{}{}
		if edge.Required {
			evaluation.Required++
		}
		switch edge.State {
		case RunDependencyStateSatisfied:
			evaluation.Satisfied++
			evaluation.SatisfiedDependencies = append(evaluation.SatisfiedDependencies, edge.ID)
		case RunDependencyStateFailed:
			evaluation.Failed++
			evaluation.FailedDependencies = append(evaluation.FailedDependencies, edge.ID)
			requiredFailure = requiredFailure || edge.Required
		case RunDependencyStateCanceled:
			evaluation.Canceled++
			evaluation.FailedDependencies = append(evaluation.FailedDependencies, edge.ID)
			requiredFailure = requiredFailure || edge.Required
		case RunDependencyStatePending, RunDependencyStateRunning:
			evaluation.Pending++
			allTerminal = false
			if edge.Required {
				requiredTerminal = false
			}
		}
	}
	sort.Strings(evaluation.SatisfiedDependencies)
	sort.Strings(evaluation.FailedDependencies)
	if group.Status == RunDependencyGroupSatisfied || group.Status == RunDependencyGroupFailed || group.Status == RunDependencyGroupCanceled {
		return evaluation, nil
	}
	if group.Status == RunDependencyGroupOpen {
		return evaluation, nil
	}
	if len(edges) == 0 || (group.ExpectedCount > 0 && len(edges) != group.ExpectedCount) {
		return RunDependencyEvaluation{}, fmt.Errorf("%w: sealed dependency group has %d edges, expected %d", ErrInvalidRunDependency, len(edges), group.ExpectedCount)
	}

	succeed := false
	fail := false
	switch group.Policy.Mode {
	case FanInModeAll:
		if evaluation.Required == 0 {
			return RunDependencyEvaluation{}, fmt.Errorf("%w: all fan-in requires at least one required dependency", ErrInvalidRunDependency)
		}
		succeed = requiredTerminal && !requiredFailure
		fail = requiredFailure && (group.Policy.FailureMode == DependencyFailureFailFast || allTerminal)
	case FanInModeAny:
		succeed = evaluation.Satisfied > 0
		fail = !succeed && allTerminal
	case FanInModeQuorum:
		if group.Policy.Quorum > len(edges) {
			return RunDependencyEvaluation{}, fmt.Errorf("%w: quorum %d exceeds %d dependencies", ErrInvalidRunDependency, group.Policy.Quorum, len(edges))
		}
		succeed = evaluation.Satisfied >= group.Policy.Quorum
		possible := evaluation.Satisfied + evaluation.Pending
		fail = !succeed && ((group.Policy.FailureMode == DependencyFailureFailFast && possible < group.Policy.Quorum) || allTerminal)
	}
	if succeed {
		evaluation.Status = RunDependencyGroupSatisfied
		evaluation.Wake = true
	} else if fail {
		evaluation.Status = RunDependencyGroupFailed
		evaluation.Wake = true
	} else {
		evaluation.Status = RunDependencyGroupWaiting
	}
	return evaluation, nil
}

func canEnterDependencyWait(run *AgentRun) bool {
	if run == nil || run.WakeCondition != nil {
		return false
	}
	switch run.Status {
	case AgentRunStatusQueued, AgentRunStatusPlanning, AgentRunStatusRunning:
		return true
	default:
		return false
	}
}

func sameDependencyGroupRequest(group *RunDependencyGroup, edges []*RunDependency, req CreateRunDependencyGroupRequest) bool {
	if group == nil || group.Scope != req.Scope || group.SourceRunID != strings.TrimSpace(req.SourceRunID) ||
		group.Policy != req.Policy || group.IdempotencyKey != strings.TrimSpace(req.IdempotencyKey) || len(edges) != len(req.Dependencies) {
		return false
	}
	type intent struct {
		target, request string
		kind            RunDependencyKind
		required        bool
	}
	existing := make([]intent, 0, len(edges))
	for _, edge := range edges {
		existing = append(existing, intent{target: edge.TargetRunID, request: edge.RequestID, kind: edge.Kind, required: edge.Required})
	}
	wanted := make([]intent, 0, len(req.Dependencies))
	for _, spec := range req.Dependencies {
		required := true
		if spec.Required != nil {
			required = *spec.Required
		}
		wanted = append(wanted, intent{target: strings.TrimSpace(spec.TargetRunID), request: strings.TrimSpace(spec.RequestID), kind: spec.Kind, required: required})
	}
	sort.Slice(existing, func(i, j int) bool { return fmt.Sprint(existing[i]) < fmt.Sprint(existing[j]) })
	sort.Slice(wanted, func(i, j int) bool { return fmt.Sprint(wanted[i]) < fmt.Sprint(wanted[j]) })
	return reflect.DeepEqual(existing, wanted)
}

func applyRunDependencyResolution(group *RunDependencyGroup, edges []*RunDependency, source *AgentRun, record RunDependencyResolutionRecord) (*RunDependencyResult, error) {
	if group == nil || source == nil {
		return nil, ErrDependencyGroupNotFound
	}
	if group.Scope != record.Scope || source.Scope != record.Scope || group.SourceRunID != source.ID || group.ID != record.GroupID {
		return nil, ErrInvalidScope
	}
	if record.OccurredAt.IsZero() {
		return nil, fmt.Errorf("%w: dependency resolution timestamp is required", ErrInvalidRunDependency)
	}
	clonedEdges := make([]*RunDependency, len(edges))
	var current *RunDependency
	for index, edge := range edges {
		clonedEdges[index] = cloneRunDependency(edge)
		if edge != nil && edge.ID == record.DependencyID {
			current = clonedEdges[index]
		}
	}
	if current == nil {
		return nil, ErrRunDependencyNotFound
	}
	if current.State == RunDependencyStateSatisfied || current.State == RunDependencyStateFailed || current.State == RunDependencyStateCanceled {
		if current.State != record.State || !reflect.DeepEqual(current.Result, record.Result) ||
			!reflect.DeepEqual(current.Artifacts, record.Artifacts) || current.Error != record.Error {
			return nil, ErrDependencyConflict
		}
		evaluation, err := EvaluateRunDependencies(group, clonedEdges)
		return &RunDependencyResult{
			Group: cloneRunDependencyGroup(group), Dependency: cloneRunDependency(current), Dependencies: clonedEdges,
			Source: cloneAgentRun(source), Evaluation: evaluation, Replayed: true,
		}, err
	}
	if current.Revision != record.ExpectedDependencyRevision {
		return nil, ErrRevisionConflict
	}
	current.State = record.State
	current.Result = cloneMap(record.Result)
	current.Artifacts = cloneArtifactReferences(record.Artifacts)
	current.Error = strings.TrimSpace(record.Error)
	current.Revision++
	current.UpdatedAt = record.OccurredAt
	resolvedAt := record.OccurredAt
	current.ResolvedAt = &resolvedAt
	if err := current.Validate(); err != nil {
		return nil, err
	}
	evaluation, err := EvaluateRunDependencies(group, clonedEdges)
	if err != nil {
		return nil, err
	}
	updatedGroup := cloneRunDependencyGroup(group)
	updatedGroup.Status = evaluation.Status
	updatedGroup.Revision++
	updatedGroup.UpdatedAt = record.OccurredAt
	if evaluation.Wake {
		updatedGroup.ResolvedAt = &resolvedAt
		updatedGroup.WakeSignalID = fmt.Sprintf("dependency-group:%s:%d", group.ID, updatedGroup.Revision)
	}
	updatedSource := cloneAgentRun(source)
	if group.Status == RunDependencyGroupWaiting {
		if source.Status != AgentRunStatusWaitingForDependency || source.WakeCondition == nil ||
			source.WakeCondition.Type != "run_dependencies" || source.WakeCondition.Reference != group.ID {
			return nil, fmt.Errorf("%w: source run is not waiting on dependency group %s", ErrInvalidRunTransition, group.ID)
		}
	}
	updatedSource.Output = dependencyGroupOutput(updatedSource.Output, updatedGroup, current, evaluation)
	updatedSource.Revision++
	updatedSource.UpdatedAt = record.OccurredAt
	if evaluation.Wake {
		updatedSource.Status = AgentRunStatusQueued
		updatedSource.AvailableAt = record.OccurredAt
		updatedSource.QueueEnteredAt = record.OccurredAt
		updatedSource.WakeCondition = nil
		updatedSource.LastWakeSignalID = updatedGroup.WakeSignalID
		updatedSource.LeaseOwner = ""
		updatedSource.LeaseExpiresAt = nil
	}
	if err := updatedGroup.Validate(); err != nil {
		return nil, err
	}
	if err := updatedSource.Validate(); err != nil {
		return nil, err
	}
	edgeEvent := dependencyResolvedEvent(updatedSource, updatedGroup, current, record.Actor, record.Visibility, record.OccurredAt)
	events := []*ActivityEvent{edgeEvent}
	if evaluation.Wake {
		events = append(events, dependencyGroupResolvedEvent(updatedSource, updatedGroup, evaluation, record.Actor, record.Visibility, record.OccurredAt))
	}
	return &RunDependencyResult{
		Group: updatedGroup, Dependency: cloneRunDependency(current), Dependencies: clonedEdges,
		Source: updatedSource, Evaluation: evaluation, Events: events,
	}, nil
}

func dependencyGroupOutput(existing map[string]interface{}, group *RunDependencyGroup, edge *RunDependency, evaluation RunDependencyEvaluation) map[string]interface{} {
	output := cloneMap(existing)
	if output == nil {
		output = make(map[string]interface{})
	}
	groups := make(map[string]interface{})
	if current, ok := output["dependencyGroups"].(map[string]interface{}); ok {
		for key, value := range current {
			groups[key] = value
		}
	}
	groupResult := map[string]interface{}{}
	if current, ok := groups[group.ID].(map[string]interface{}); ok {
		for key, value := range current {
			groupResult[key] = value
		}
	}
	dependencies := make(map[string]interface{})
	if current, ok := groupResult["dependencies"].(map[string]interface{}); ok {
		for key, value := range current {
			dependencies[key] = value
		}
	}
	dependencies[edge.ID] = map[string]interface{}{
		"id": edge.ID, "kind": edge.Kind, "state": edge.State, "required": edge.Required,
		"targetRunId": edge.TargetRunID, "requestId": edge.RequestID, "result": cloneMap(edge.Result),
		"artifacts": cloneArtifactReferences(edge.Artifacts), "error": edge.Error,
	}
	groupResult["id"] = group.ID
	groupResult["status"] = group.Status
	groupResult["policy"] = group.Policy
	groupResult["evaluation"] = evaluation
	groupResult["dependencies"] = dependencies
	groups[group.ID] = groupResult
	output["dependencyGroups"] = groups
	return output
}

func dependencyGroupEvent(run *AgentRun, group *RunDependencyGroup, edges []*RunDependency, actor ActivityActor, visibility ActivityVisibility, now time.Time) *ActivityEvent {
	ids := make([]string, 0, len(edges))
	for _, edge := range edges {
		ids = append(ids, edge.ID)
	}
	sort.Strings(ids)
	return &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "dependency.group_waiting", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: teamIDForRun(run), Actor: actor, Summary: fmt.Sprintf("Waiting on %d coordinated dependencies", len(edges)),
		Payload:    map[string]interface{}{"groupId": group.ID, "policy": group.Policy, "dependencyIds": ids},
		Visibility: visibility, CorrelationID: group.ID, CausationID: run.ID, CreatedAt: now,
	}
}

func dependencyResolvedEvent(run *AgentRun, group *RunDependencyGroup, edge *RunDependency, actor ActivityActor, visibility ActivityVisibility, now time.Time) *ActivityEvent {
	severity := ActivitySeverityInfo
	if edge.State == RunDependencyStateFailed || edge.State == RunDependencyStateCanceled {
		severity = ActivitySeverityWarning
	}
	return &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "dependency." + string(edge.State), Severity: severity,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: teamIDForRun(run), Actor: actor, Summary: fmt.Sprintf("Dependency %s is %s", edge.ID, edge.State),
		Payload:    map[string]interface{}{"groupId": group.ID, "dependencyId": edge.ID, "kind": edge.Kind, "state": edge.State, "required": edge.Required, "targetRunId": edge.TargetRunID, "requestId": edge.RequestID, "artifacts": cloneArtifactReferences(edge.Artifacts)},
		Visibility: visibility, CorrelationID: group.ID, CausationID: edge.ID, CreatedAt: now,
	}
}

func dependencyGroupResolvedEvent(run *AgentRun, group *RunDependencyGroup, evaluation RunDependencyEvaluation, actor ActivityActor, visibility ActivityVisibility, now time.Time) *ActivityEvent {
	severity := ActivitySeverityInfo
	if group.Status == RunDependencyGroupFailed || group.Status == RunDependencyGroupCanceled {
		severity = ActivitySeverityWarning
	}
	return &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "dependency.group_" + string(group.Status), Severity: severity,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: teamIDForRun(run), Actor: actor, Summary: fmt.Sprintf("Dependency group %s is %s", group.ID, group.Status),
		Payload:    map[string]interface{}{"groupId": group.ID, "policy": group.Policy, "evaluation": evaluation, "wakeSignalId": group.WakeSignalID},
		Visibility: visibility, CorrelationID: group.ID, CausationID: group.ID, CreatedAt: now,
	}
}

func cloneRunDependencyGroup(in *RunDependencyGroup) *RunDependencyGroup {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneRunDependency(in *RunDependency) *RunDependency {
	if in == nil {
		return nil
	}
	out := *in
	out.Result = cloneMap(in.Result)
	out.Artifacts = cloneArtifactReferences(in.Artifacts)
	return &out
}
