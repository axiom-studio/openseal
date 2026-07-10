package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
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
	ID            string                   `json:"id"`
	Scope         Scope                    `json:"scope"`
	SourceRunID   string                   `json:"sourceRunId"`
	Policy        RunDependencyPolicy      `json:"policy"`
	ExpectedCount int                      `json:"expectedCount,omitempty"`
	Status        RunDependencyGroupStatus `json:"status"`
	Revision      int64                    `json:"revision"`
	CreatedAt     time.Time                `json:"createdAt"`
	UpdatedAt     time.Time                `json:"updatedAt"`
	SealedAt      *time.Time               `json:"sealedAt,omitempty"`
	ResolvedAt    *time.Time               `json:"resolvedAt,omitempty"`
	WakeSignalID  string                   `json:"wakeSignalId,omitempty"`
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
