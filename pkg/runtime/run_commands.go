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

type AgentRunCommandKind string

const (
	AgentRunCommandPause                    AgentRunCommandKind = "pause"
	AgentRunCommandResume                   AgentRunCommandKind = "resume"
	AgentRunCommandCancel                   AgentRunCommandKind = "cancel"
	AgentRunCommandIntervene                AgentRunCommandKind = "intervene"
	AgentRunCommandResolveHumanIntervention AgentRunCommandKind = "resolve_human_intervention"
)

type AgentRunCommandRequest struct {
	Scope               Scope
	RunID               string
	ExpectedRevision    int64
	Kind                AgentRunCommandKind
	Actor               ActivityActor
	Summary             string
	Instruction         string
	HumanInterventionID string
	Visibility          ActivityVisibility
}

type AgentRunCommandResult struct {
	Run   *AgentRun      `json:"run"`
	Event *ActivityEvent `json:"event,omitempty"`
}

type RunCommandStore interface {
	PortfolioStore
	RunActivityStore
	ActionStore
}

type RunCommandService struct {
	store RunCommandStore
	now   func() time.Time
}

func NewRunCommandService(store RunCommandStore) *RunCommandService {
	return &RunCommandService{store: store, now: time.Now}
}

// CreateAgentRun creates the canonical durable run and its first audit event
// atomically. A supplied idempotency key is scope-bound, stored only as a hash,
// and conflicts if replayed with different work.
func (s *RunCommandService) CreateAgentRun(ctx context.Context, req CreateAgentRunRequest) (*AgentRunCommandResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if len(key) > 256 {
		return nil, fmt.Errorf("%w: idempotency key cannot exceed 256 characters", ErrInvalidAgentRun)
	}
	fingerprint, err := runCreationFingerprint(req)
	if err != nil {
		return nil, err
	}
	runID := uuid.NewString()
	keyHash := ""
	if key != "" {
		keyHash = hashString(key)
		runID = runIDForIdempotencyKey(req.Scope, key)
		current, err := s.store.GetAgentRun(ctx, req.Scope, runID)
		if err != nil {
			return nil, err
		}
		if current != nil {
			return replayedRun(current, fingerprint)
		}
	}
	now := s.now()
	run, err := buildAgentRun(ctx, s.store, req, runID, now)
	if err != nil {
		return nil, err
	}
	run.IdempotencyKeyHash = keyHash
	run.CreationFingerprint = fingerprint
	actor := req.Actor
	if strings.TrimSpace(actor.Type) == "" {
		actor = ActivityActor{Type: "system", ID: "openseal"}
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.created", Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID,
		ParentRunID: run.ParentRunID, TeamID: teamIDForRun(run), Actor: actor,
		Summary: "Run created", Visibility: visibility, CreatedAt: now,
		Payload: map[string]interface{}{"status": run.Status, "source": run.Source, "owner": run.Owner},
	}
	if projectID, _ := run.Context["projectId"].(string); strings.TrimSpace(projectID) != "" {
		event.ProjectID = strings.TrimSpace(projectID)
		event.Payload["projectId"] = event.ProjectID
	}
	if snapshotID := evidenceSnapshotIdentity(run.Context); snapshotID != "" {
		event.Payload["evidenceSnapshotId"] = snapshotID
	}
	persisted, err := s.store.CreateAgentRunWithEvent(ctx, run, event)
	if err != nil {
		if key != "" {
			current, loadErr := s.store.GetAgentRun(ctx, req.Scope, runID)
			if loadErr == nil && current != nil {
				return replayedRun(current, fingerprint)
			}
		}
		return nil, err
	}
	return &AgentRunCommandResult{Run: run, Event: persisted}, nil
}

func runIDForIdempotencyKey(scope Scope, key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(scope.Kind+"\x00"+scope.ID+"\x00"+hashString(strings.TrimSpace(key)))).String()
}

func replayedRun(current *AgentRun, fingerprint string) (*AgentRunCommandResult, error) {
	if current.CreationFingerprint != fingerprint {
		return nil, ErrRunIdempotency
	}
	return &AgentRunCommandResult{Run: current}, nil
}

func (s *RunCommandService) CommandAgentRun(ctx context.Context, req AgentRunCommandRequest) (*AgentRunCommandResult, error) {
	result, err := s.commandAgentRun(ctx, req)
	if err != nil {
		return nil, err
	}
	if isTerminalAgentRunStatus(result.Run.Status) {
		if err := s.CascadeTerminalRun(ctx, result.Run); err != nil {
			return result, fmt.Errorf("cascade terminal Run %s: %w", result.Run.ID, err)
		}
	}
	return result, nil
}

func (s *RunCommandService) commandAgentRun(ctx context.Context, req AgentRunCommandRequest) (*AgentRunCommandResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return nil, fmt.Errorf("%w: run id is required", ErrInvalidRunCommand)
	}
	if req.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: expected revision must be positive", ErrInvalidRunCommand)
	}
	current, err := s.store.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrRunNotFound
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if req.Kind == AgentRunCommandCancel && current.Status == AgentRunStatusWaitingForApproval {
		return s.cancelWaitingApprovalRun(ctx, current, req)
	}
	transition, err := commandTransition(current, req, s.now())
	if err != nil {
		return nil, err
	}
	activity := NewRunActivityService(s.store, s.store)
	run, event, err := activity.TransitionRun(ctx, req.Scope, req.RunID, transition)
	if err != nil {
		return nil, err
	}
	return &AgentRunCommandResult{Run: run, Event: event}, nil
}

// CascadeTerminalRun cancels every non-terminal Run owned beneath a terminal
// parent. Descendants are visited leaf-first so a waiting parent never keeps a
// child lease, approval, or capability resource alive after its owner has
// stopped. The operation is replay-safe and is used both by direct commands
// and terminal reconciliation after process restarts.
func (s *RunCommandService) CascadeTerminalRun(ctx context.Context, parent *AgentRun) error {
	if s == nil || s.store == nil {
		return errors.New("run command store is not configured")
	}
	if parent == nil || !isTerminalAgentRunStatus(parent.Status) {
		return errors.New("only terminal Runs can cascade descendant cancellation")
	}
	return s.cancelRunDescendants(ctx, parent, map[string]struct{}{parent.ID: {}})
}

func (s *RunCommandService) cancelRunDescendants(ctx context.Context, parent *AgentRun, visited map[string]struct{}) error {
	const pageSize = 100
	children := make([]*AgentRun, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.store.ListAgentRuns(ctx, AgentRunFilter{
			Scope: parent.Scope, ParentRunID: parent.ID, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return err
		}
		children = append(children, page...)
		if len(page) < pageSize {
			break
		}
	}

	var cascadeErrors []error
	for _, child := range children {
		if child == nil {
			continue
		}
		if _, seen := visited[child.ID]; seen {
			cascadeErrors = append(cascadeErrors, fmt.Errorf("Run ownership cycle includes %s", child.ID))
			continue
		}
		visited[child.ID] = struct{}{}
		if err := s.cancelRunDescendants(ctx, child, visited); err != nil {
			cascadeErrors = append(cascadeErrors, err)
		}
		delete(visited, child.ID)

		current, err := s.store.GetAgentRun(ctx, child.Scope, child.ID)
		if err != nil {
			cascadeErrors = append(cascadeErrors, err)
			continue
		}
		if current == nil || isTerminalAgentRunStatus(current.Status) {
			continue
		}
		actor := ActivityActor{Type: "runtime", ID: "parent-run-terminalizer"}
		summary := fmt.Sprintf("Canceled because parent Run %s reached %s", parent.ID, parent.Status)
		_, err = s.commandAgentRun(ctx, AgentRunCommandRequest{
			Scope: current.Scope, RunID: current.ID, ExpectedRevision: current.Revision,
			Kind: AgentRunCommandCancel, Actor: actor, Summary: summary,
			Visibility: ActivityVisibilityScope,
		})
		if err != nil {
			// A concurrent worker may have terminalized the child between the
			// read and command. Treat that as successful convergence.
			reloaded, loadErr := s.store.GetAgentRun(ctx, child.Scope, child.ID)
			if loadErr != nil {
				cascadeErrors = append(cascadeErrors, errors.Join(err, loadErr))
			} else if reloaded != nil && !isTerminalAgentRunStatus(reloaded.Status) {
				cascadeErrors = append(cascadeErrors, err)
			}
		}
	}
	return errors.Join(cascadeErrors...)
}

// cancelWaitingApprovalRun atomically closes all three authoritative facts
// that otherwise permit future work: the pending approval, its ActionCall, and
// the waiting Run. A plain Run transition is insufficient because an approval
// could still be accepted later and make the external action executable.
func (s *RunCommandService) cancelWaitingApprovalRun(ctx context.Context, current *AgentRun, req AgentRunCommandRequest) (*AgentRunCommandResult, error) {
	if current.WakeCondition == nil || current.WakeCondition.Type != "approval" || strings.TrimSpace(current.WakeCondition.Reference) == "" {
		return nil, fmt.Errorf("%w: waiting approval run has no approval wake condition", ErrInvalidRunTransition)
	}
	approval, err := s.store.GetApproval(ctx, current.Scope, current.WakeCondition.Reference)
	if err != nil {
		return nil, err
	}
	if approval == nil || approval.Status != ApprovalStatusPending || approval.RunID != current.ID {
		return nil, fmt.Errorf("%w: run approval is unavailable or already resolved", ErrApprovalResolved)
	}
	call, err := s.store.GetActionCall(ctx, current.Scope, approval.ActionCallID)
	if err != nil {
		return nil, err
	}
	if call == nil || call.Status != ActionCallStatusWaitingApproval || call.RunID != current.ID || call.ApprovalID != approval.ID {
		return nil, fmt.Errorf("%w: run action is not waiting on its approval", ErrApprovalResolved)
	}

	now := s.now().UTC()
	actor := req.Actor
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" {
		actor = ActivityActor{Type: "system", ID: "openseal"}
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = "Run canceled"
	}
	decisionID := "run-cancel:" + hashString(current.Scope.Kind+"\x00"+current.Scope.ID+"\x00"+current.ID+"\x00"+fmt.Sprint(current.Revision))

	updatedApproval := cloneApprovalCheckpoint(approval)
	updatedApproval.Status = ApprovalStatusCanceled
	updatedApproval.DecisionID = decisionID
	updatedApproval.DecisionBy = &ApprovalPrincipal{Type: actor.Type, ID: actor.ID}
	updatedApproval.DecisionReason = summary
	updatedApproval.DecidedAt = &now
	updatedApproval.UpdatedAt = now
	updatedApproval.Revision++

	updatedCall := cloneActionCall(call)
	updatedCall.Status = ActionCallStatusCanceled
	updatedCall.Error = "run canceled: " + summary
	updatedCall.LeaseOwner = ""
	updatedCall.LeaseExpiresAt = nil
	updatedCall.CompletedAt = &now
	updatedCall.UpdatedAt = now
	updatedCall.Revision++

	updatedRun := cloneAgentRun(current)
	if updatedRun.Budget != nil {
		if _, reserved := updatedRun.BudgetReservations[actionBudgetReservationID(call.ID)]; reserved {
			if err := releaseRunBudgetReservation(updatedRun, actionBudgetReservationID(call.ID)); err != nil {
				return nil, err
			}
		}
	}
	updatedRun.Status = AgentRunStatusCanceled
	updatedRun.WakeCondition = nil
	updatedRun.LeaseOwner = ""
	updatedRun.LeaseExpiresAt = nil
	updatedRun.CompletedAt = &now
	updatedRun.UpdatedAt = now
	updatedRun.Revision++

	visibility := req.Visibility
	if visibility == "" {
		visibility = ActivityVisibilityScope
	}
	event := &ActivityEvent{
		ID: uuid.NewString(), Scope: current.Scope, EventType: "run.canceled", Severity: ActivitySeverityInfo,
		AgentID: current.AssignedAgentID, ObjectiveID: current.ObjectiveID, RunID: current.ID, TurnID: call.TurnID,
		ParentRunID: current.ParentRunID, TeamID: teamIDForRun(current), Actor: actor, Summary: summary,
		Visibility: visibility, CausationID: approval.ID, CreatedAt: now,
		Payload: map[string]interface{}{"status": AgentRunStatusCanceled, "approvalId": approval.ID, "actionCallId": call.ID},
	}
	resolved, err := s.store.ResolveApproval(ctx, ApprovalResolutionRecord{
		Approval: updatedApproval, ExpectedApprovalRevision: approval.Revision,
		Call: updatedCall, ExpectedCallRevision: call.Revision,
		Run: updatedRun, ExpectedRunRevision: current.Revision, Event: event,
	})
	if err != nil {
		return nil, err
	}
	return &AgentRunCommandResult{Run: resolved.Run, Event: resolved.Event}, nil
}

func commandTransition(current *AgentRun, req AgentRunCommandRequest, now time.Time) (RunTransitionRequest, error) {
	transition := RunTransitionRequest{
		ExpectedRevision: req.ExpectedRevision, Actor: req.Actor, Visibility: req.Visibility,
		Summary: strings.TrimSpace(req.Summary), OccurredAt: &now,
	}
	switch req.Kind {
	case AgentRunCommandPause:
		if current.Status == AgentRunStatusPaused || isTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot pause %s run", ErrInvalidRunTransition, current.Status)
		}
		transition.Status = AgentRunStatusPaused
		transition.EventType = "run.paused"
		if transition.Summary == "" {
			transition.Summary = "Run paused"
		}
	case AgentRunCommandResume:
		if current.Status != AgentRunStatusPaused {
			return transition, fmt.Errorf("%w: cannot resume %s run", ErrInvalidRunTransition, current.Status)
		}
		transition.Status = resumedRunStatus(current.PausedFrom)
		if isWaitingRunStatus(transition.Status) {
			transition.WakeCondition = current.PausedWakeCondition
			if transition.WakeCondition == nil {
				return transition, fmt.Errorf("%w: paused waiting run has no wake condition", ErrInvalidRunTransition)
			}
		}
		transition.EventType = "run.resumed"
		if transition.Summary == "" {
			transition.Summary = "Run resumed"
		}
	case AgentRunCommandCancel:
		if isTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot cancel %s run", ErrInvalidRunTransition, current.Status)
		}
		transition.Status = AgentRunStatusCanceled
		transition.HumanInterventions = resolvePendingHumanInterventions(current.HumanInterventions, HumanInterventionStatusCanceled, req.Actor, "Run canceled", now)
		transition.EventType = "run.canceled"
		if transition.Summary == "" {
			transition.Summary = "Run canceled"
		}
	case AgentRunCommandIntervene:
		instruction := strings.TrimSpace(req.Instruction)
		if instruction == "" {
			return transition, fmt.Errorf("%w: intervention instruction is required", ErrInvalidRunCommand)
		}
		if isTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot intervene on %s run", ErrInvalidRunTransition, current.Status)
		}
		if current.Status == AgentRunStatusSleeping {
			// Operator guidance is new durable input. A sleeping run must consume it
			// immediately instead of preserving a stale retry or schedule timer.
			transition.Status = AgentRunStatusQueued
		} else {
			transition.Status = current.Status
			transition.WakeCondition = current.WakeCondition
		}
		transition.EventType = "run.intervened"
		transition.Intervention = &AgentRunIntervention{ID: uuid.NewString(), Actor: req.Actor, Instruction: instruction, CreatedAt: now}
		transition.Payload = map[string]interface{}{"interventionId": transition.Intervention.ID, "instruction": instruction}
		if transition.Summary == "" {
			transition.Summary = "Operator steered the run"
		}
	case AgentRunCommandResolveHumanIntervention:
		requestID := strings.TrimSpace(req.HumanInterventionID)
		instruction := strings.TrimSpace(req.Instruction)
		if requestID == "" || instruction == "" {
			return transition, fmt.Errorf("%w: human intervention id and resolution instruction are required", ErrInvalidRunCommand)
		}
		if current.Status != AgentRunStatusWaitingForEvent || current.WakeCondition == nil || current.WakeCondition.Type != "human_intervention" || current.WakeCondition.Reference != requestID {
			return transition, fmt.Errorf("%w: run is not waiting on human intervention %s", ErrInvalidRunTransition, requestID)
		}
		resolved, found := resolveHumanIntervention(current.HumanInterventions, requestID, req.Actor, instruction, now)
		if !found {
			return transition, fmt.Errorf("%w: human intervention %s is not pending", ErrInvalidRunTransition, requestID)
		}
		transition.Status = AgentRunStatusQueued
		transition.HumanInterventions = resolved
		transition.Intervention = &AgentRunIntervention{ID: uuid.NewString(), Actor: req.Actor, Instruction: instruction, CreatedAt: now}
		transition.EventType = "run.human_intervention_resolved"
		transition.CausationID = requestID
		transition.Payload = map[string]interface{}{"humanInterventionId": requestID, "status": HumanInterventionStatusResolved}
		if transition.Summary == "" {
			transition.Summary = "Human intervention resolved"
		}
	default:
		return transition, fmt.Errorf("%w: kind must be pause, resume, cancel, intervene, or resolve_human_intervention", ErrInvalidRunCommand)
	}
	return transition, nil
}

func resolveHumanIntervention(values []HumanInterventionRequest, requestID string, actor ActivityActor, resolution string, now time.Time) ([]HumanInterventionRequest, bool) {
	result := append([]HumanInterventionRequest(nil), values...)
	for index := range result {
		if result[index].ID != requestID || result[index].Status != HumanInterventionStatusPending {
			continue
		}
		resolvedAt := now
		resolvedBy := actor
		result[index].Status = HumanInterventionStatusResolved
		result[index].Resolution = resolution
		result[index].ResolvedBy = &resolvedBy
		result[index].ResolvedAt = &resolvedAt
		return result, true
	}
	return result, false
}

func resolvePendingHumanInterventions(values []HumanInterventionRequest, status HumanInterventionStatus, actor ActivityActor, resolution string, now time.Time) []HumanInterventionRequest {
	result := append([]HumanInterventionRequest(nil), values...)
	for index := range result {
		if result[index].Status != HumanInterventionStatusPending {
			continue
		}
		resolvedAt := now
		resolvedBy := actor
		result[index].Status = status
		result[index].Resolution = resolution
		result[index].ResolvedBy = &resolvedBy
		result[index].ResolvedAt = &resolvedAt
	}
	return result
}

func resumedRunStatus(previous AgentRunStatus) AgentRunStatus {
	if isWaitingRunStatus(previous) {
		return previous
	}
	return AgentRunStatusQueued
}

func runCreationFingerprint(req CreateAgentRunRequest) (string, error) {
	payload := struct {
		Scope                Scope                  `json:"scope"`
		Kind                 RunKind                `json:"kind"`
		ObjectiveID          string                 `json:"objectiveId,omitempty"`
		ParentRunID          string                 `json:"parentRunId,omitempty"`
		Owner                ObjectiveOwner         `json:"owner"`
		AssignedAgentID      string                 `json:"assignedAgentId,omitempty"`
		Entrypoint           string                 `json:"entrypoint,omitempty"`
		ConcurrencyKey       string                 `json:"concurrencyKey,omitempty"`
		ResourceRequirements map[string]int         `json:"resourceRequirements,omitempty"`
		Goal                 string                 `json:"goal"`
		Source               RunSource              `json:"source"`
		Priority             int                    `json:"priority"`
		Deadline             *time.Time             `json:"deadline,omitempty"`
		AvailableAt          *time.Time             `json:"availableAt,omitempty"`
		Context              map[string]interface{} `json:"context,omitempty"`
		Plan                 map[string]interface{} `json:"plan,omitempty"`
		Checkpoint           map[string]interface{} `json:"checkpoint,omitempty"`
		WakeCondition        *WakeCondition         `json:"wakeCondition,omitempty"`
		Budget               *BudgetPolicy          `json:"budget,omitempty"`
		Policy               map[string]interface{} `json:"policy,omitempty"`
	}{
		Scope: req.Scope, Kind: normalizeRunKind(req.Kind), ObjectiveID: req.ObjectiveID,
		ParentRunID: req.ParentRunID, Owner: req.Owner, AssignedAgentID: req.AssignedAgentID, Entrypoint: strings.TrimSpace(req.Entrypoint),
		ConcurrencyKey:       strings.TrimSpace(req.ConcurrencyKey),
		ResourceRequirements: req.ResourceRequirements,
		Goal:                 req.Goal, Source: req.Source, Priority: req.Priority, Deadline: req.Deadline,
		AvailableAt: req.AvailableAt, Context: req.Context, Plan: req.Plan, Checkpoint: req.Checkpoint,
		WakeCondition: req.WakeCondition, Budget: req.Budget, Policy: req.Policy,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode run creation fingerprint: %w", err)
	}
	return hashBytes(encoded), nil
}

func hashString(value string) string { return hashBytes([]byte(value)) }

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
