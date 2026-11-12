package agentrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/google/uuid"
)

type CommandKind string

const (
	CommandPause                    CommandKind = "pause"
	CommandResume                   CommandKind = "resume"
	CommandRetry                    CommandKind = "retry"
	CommandCancel                   CommandKind = "cancel"
	CommandIntervene                CommandKind = "intervene"
	CommandResolveHumanIntervention CommandKind = "resolve_human_intervention"
)

type CommandRequest struct {
	Scope               models.Scope
	RunID               string
	ExpectedRevision    int64
	Kind                CommandKind
	Actor               models.ActivityActor
	Summary             string
	Instruction         string
	InterventionID      string
	HumanInterventionID string
	Visibility          models.ActivityVisibility
}

type CommandResult struct {
	Run   *models.AgentRun      `json:"run"`
	Event *models.ActivityEvent `json:"event,omitempty"`
}

type RunCommandService struct {
	store models.RunCommandStore
	now   func() time.Time
}

func NewRunCommandService(store models.RunCommandStore, opts ...models.ServiceOption) *RunCommandService {
	return &RunCommandService{store: store, now: models.ClockFrom(opts)}
}

// TransitionRun applies req to the Run through its activity log, stamped
// with the service's clock.
func (s *RunCommandService) TransitionRun(ctx context.Context, scope models.Scope, runID string, req activity.RunTransitionRequest) (*models.AgentRun, *models.ActivityEvent, error) {
	return activity.NewRunActivityService(s.store, s.store, models.WithClock(s.now)).TransitionRun(ctx, scope, runID, req)
}

// FindCreatedAgentRun recovers an exact accepted request without creating work.
// Hosts may use it before checking mutable admission state on a delivery retry.
func (s *RunCommandService) FindCreatedAgentRun(ctx context.Context, req models.CreateAgentRunRequest) (*CommandResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(key) > 256 {
		return nil, fmt.Errorf("%w: a bounded idempotency key is required", models.ErrInvalidAgentRun)
	}
	fingerprint, err := runCreationFingerprint(req)
	if err != nil {
		return nil, err
	}
	current, err := s.store.GetAgentRun(ctx, req.Scope, RunIDForIdempotencyKey(req.Scope, key))
	if err != nil || current == nil {
		return nil, err
	}
	return replayedRun(current, fingerprint, req.ActingUser)
}

// CreateAgentRun creates the canonical durable run and its first audit event
// atomically. A supplied idempotency key is scope-bound, stored only as a hash,
// and conflicts if replayed with different work.
func (s *RunCommandService) CreateAgentRun(ctx context.Context, req models.CreateAgentRunRequest) (*CommandResult, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if len(key) > 256 {
		return nil, fmt.Errorf("%w: idempotency key cannot exceed 256 characters", models.ErrInvalidAgentRun)
	}
	fingerprint, err := runCreationFingerprint(req)
	if err != nil {
		return nil, err
	}
	runID := uuid.NewString()
	keyHash := ""
	if key != "" {
		keyHash = models.SHA256DigestString(key)
		runID = RunIDForIdempotencyKey(req.Scope, key)
		current, err := s.store.GetAgentRun(ctx, req.Scope, runID)
		if err != nil {
			return nil, err
		}
		if current != nil {
			return replayedRun(current, fingerprint, req.ActingUser)
		}
	}
	now := s.now()
	run, err := portfolio.BuildAgentRun(ctx, s.store, req, runID, now)
	if err != nil {
		return nil, err
	}
	run.IdempotencyKeyHash = keyHash
	run.CreationFingerprint = fingerprint
	actor := req.Actor
	if strings.TrimSpace(actor.Type) == "" {
		actor = models.ActivityActor{Type: "system", ID: "openseal"}
	}
	visibility := req.Visibility
	if visibility == "" {
		visibility = models.ActivityVisibilityScope
	}
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: "run.created", Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID,
		ParentRunID: run.ParentRunID, TeamID: models.TeamIDForRun(run), Actor: actor,
		Summary: "Run created", Visibility: visibility, CreatedAt: now,
		Payload: map[string]interface{}{"status": run.Status, "source": run.Source, "owner": run.Owner},
	}
	persisted, err := s.store.CreateAgentRunWithEvent(ctx, run, event)
	if err != nil {
		if key != "" {
			current, loadErr := s.store.GetAgentRun(ctx, req.Scope, runID)
			if loadErr == nil && current != nil {
				return replayedRun(current, fingerprint, req.ActingUser)
			}
		}
		return nil, err
	}
	return &CommandResult{Run: run, Event: persisted}, nil
}

func RunIDForIdempotencyKey(scope models.Scope, key string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(scope.Kind+"\x00"+scope.ID+"\x00"+models.SHA256DigestString(strings.TrimSpace(key)))).String()
}

// replayedRun returns the stored run for an exact replay. ActingUser is not
// part of the creation fingerprint, so a run created before it existed still
// replays; a stored acting user must match the replayed request.
func replayedRun(current *models.AgentRun, fingerprint, actingUser string) (*CommandResult, error) {
	if current.CreationFingerprint != fingerprint || (current.ActingUser != "" && current.ActingUser != actingUser) {
		return nil, models.ErrRunIdempotency
	}
	return &CommandResult{Run: current}, nil
}

func (s *RunCommandService) CommandAgentRun(ctx context.Context, req CommandRequest) (*CommandResult, error) {
	result, err := s.commandAgentRun(ctx, req)
	if err != nil {
		return nil, err
	}
	if models.IsTerminalAgentRunStatus(result.Run.Status) {
		if err := s.CascadeTerminalRun(ctx, result.Run); err != nil {
			return result, fmt.Errorf("cascade terminal Run %s: %w", result.Run.ID, err)
		}
	}
	return result, nil
}

// ErrRunCommandNotAuthorized refuses a user other than the person a run acts
// for when they steer it.
var ErrRunCommandNotAuthorized = errors.New("Only the person this run acts for can steer it.")

// authorizeRunSteering lets only the person a run acts for intervene in,
// answer, resume or retry it. Pause and cancel stay open to everyone allowed
// today, and kernel actors (system, service, agent) are not people.
func authorizeRunSteering(run *models.AgentRun, req CommandRequest) error {
	switch req.Kind {
	case CommandIntervene, CommandResolveHumanIntervention, CommandResume, CommandRetry:
	default:
		return nil
	}
	if run == nil || run.ActingUser == "" || req.Actor.Type != "user" || req.Actor.ID == run.ActingUser {
		return nil
	}
	return ErrRunCommandNotAuthorized
}

func (s *RunCommandService) commandAgentRun(ctx context.Context, req CommandRequest) (*CommandResult, error) {
	if req.Kind == CommandRetry {
		return s.RetryConversationRun(ctx, req)
	}
	if s == nil || s.store == nil {
		return nil, errors.New("run command store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.RunID) == "" {
		return nil, fmt.Errorf("%w: run id is required", portfolio.ErrInvalidRunCommand)
	}
	if req.ExpectedRevision <= 0 {
		return nil, fmt.Errorf("%w: expected revision must be positive", portfolio.ErrInvalidRunCommand)
	}
	current, err := s.store.GetAgentRun(ctx, req.Scope, req.RunID)
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, models.ErrRunNotFound
	}
	if err := authorizeRunSteering(current, req); err != nil {
		return nil, err
	}
	if req.InterventionID != "" {
		if req.Kind != CommandIntervene {
			return nil, fmt.Errorf("%w: intervention id is only supported for guidance", portfolio.ErrInvalidRunCommand)
		}
		if _, err := uuid.Parse(req.InterventionID); err != nil {
			return nil, fmt.Errorf("%w: intervention id must be a UUID", portfolio.ErrInvalidRunCommand)
		}
		for _, existing := range current.PendingInterventions {
			if existing.ID == req.InterventionID {
				if existing.Instruction != strings.TrimSpace(req.Instruction) || existing.Actor != req.Actor {
					return nil, fmt.Errorf("%w: intervention id already belongs to different guidance", portfolio.ErrInvalidRunCommand)
				}
				return &CommandResult{Run: current}, nil
			}
		}
	}
	if current.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	if req.Kind == CommandCancel && current.Status == models.AgentRunStatusWaitingForApproval {
		return s.cancelWaitingApprovalRun(ctx, current, req)
	}
	if req.Kind == CommandCancel && current.Status == models.AgentRunStatusPaused && current.PausedFrom == models.AgentRunStatusWaitingForApproval {
		condition := current.PausedWakeCondition
		if condition != nil && condition.Type == "approval" && condition.Reference != "" {
			approval, err := s.store.GetApproval(ctx, current.Scope, condition.Reference)
			if err != nil {
				return nil, err
			}
			// Expired/rejected approvals already closed their ActionCall. A
			// pending approval still needs the same atomic cancellation as an
			// active approval wait so it cannot keep the grant alive forever.
			if approval != nil && approval.Status == models.ApprovalStatusPending {
				return s.cancelWaitingApprovalRun(ctx, current, req)
			}
		}
	}
	transition, err := commandTransition(current, req, s.now())
	if err != nil {
		return nil, err
	}
	activity := activity.NewRunActivityService(s.store, s.store, models.WithClock(s.now))
	run, event, err := activity.TransitionRun(ctx, req.Scope, req.RunID, transition)
	if err != nil {
		return nil, err
	}
	return &CommandResult{Run: run, Event: event}, nil
}

// CascadeTerminalRun closes a terminal Run's running step and open tool
// entries (closeTerminalRunWork) and cancels every non-terminal Run owned
// beneath it. Descendants are visited leaf-first so a waiting parent never keeps a
// child lease, approval, or capability resource alive after its owner has
// stopped. The operation is replay-safe and is used both by direct commands
// and terminal reconciliation after process restarts. An accepted handoff
// retains parent lineage but transfers cancellation ownership to its recipient.
func (s *RunCommandService) CascadeTerminalRun(ctx context.Context, parent *models.AgentRun) error {
	if s == nil || s.store == nil {
		return errors.New("run command store is not configured")
	}
	if parent == nil || !models.IsTerminalAgentRunStatus(parent.Status) {
		return errors.New("only terminal Runs can cascade descendant cancellation")
	}
	current, err := s.store.GetAgentRun(ctx, parent.Scope, parent.ID)
	if err != nil {
		return err
	}
	if current == nil {
		return models.ErrRunNotFound
	}
	if current.Scope != parent.Scope || current.ID != parent.ID || !models.IsTerminalAgentRunStatus(current.Status) {
		return fmt.Errorf("%w: persisted parent Run is not terminal", models.ErrInvalidRunTransition)
	}
	// Its running step and open tool entries can no longer finish: close them.
	workErr := closeTerminalRunWork(ctx, s.store, current, s.now().UTC())
	// Its open in-chat requests can no longer resume it: close their cards.
	closeErr := closeTerminalRunInteractionRequests(ctx, s.store, current)
	return errors.Join(workErr, closeErr, s.cancelRunDescendants(ctx, current, map[string]struct{}{current.ID: {}}))
}

func (s *RunCommandService) cancelRunDescendants(ctx context.Context, parent *models.AgentRun, visited map[string]struct{}) error {
	const pageSize = 100
	children := make([]*models.AgentRun, 0)
	for offset := 0; ; offset += pageSize {
		page, err := s.store.ListAgentRuns(ctx, models.AgentRunFilter{
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
	// Acceptance commits the completed source, receipt, and child together.
	// Reload after discovering children so a handoff accepted concurrently
	// cannot be judged from the ancestor traversal's earlier source snapshot.
	currentParent, err := s.store.GetAgentRun(ctx, parent.Scope, parent.ID)
	if err != nil {
		return err
	}
	if currentParent == nil {
		return models.ErrRunNotFound
	}
	if currentParent.ID != parent.ID || currentParent.Scope != parent.Scope {
		return fmt.Errorf("%w: persisted parent Run identity changed", models.ErrInvalidRunTransition)
	}
	parent = currentParent
	transfer, err := s.acceptedHandoffTransfer(ctx, parent)
	if err != nil {
		return err
	}

	var cascadeErrors []error
	for _, child := range children {
		if child == nil {
			continue
		}
		if handoffOwnsChild(transfer, parent, child) {
			// The recipient owns this entire branch after accepting the
			// handoff. ParentRunID remains the immutable historical lineage.
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
		if current == nil || models.IsTerminalAgentRunStatus(current.Status) {
			continue
		}
		actor := models.ActivityActor{Type: "runtime", ID: "parent-run-terminalizer"}
		summary := fmt.Sprintf("Canceled because parent Run %s reached %s", parent.ID, parent.Status)
		_, err = s.commandAgentRun(ctx, CommandRequest{
			Scope: current.Scope, RunID: current.ID, ExpectedRevision: current.Revision,
			Kind: CommandCancel, Actor: actor, Summary: summary,
			Visibility: models.ActivityVisibilityScope,
		})
		if err != nil {
			// A concurrent worker may have terminalized the child between the
			// read and command. Treat that as successful convergence.
			reloaded, loadErr := s.store.GetAgentRun(ctx, child.Scope, child.ID)
			if loadErr != nil {
				cascadeErrors = append(cascadeErrors, errors.Join(err, loadErr))
			} else if reloaded != nil && !models.IsTerminalAgentRunStatus(reloaded.Status) {
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
func (s *RunCommandService) cancelWaitingApprovalRun(ctx context.Context, current *models.AgentRun, req CommandRequest) (*CommandResult, error) {
	condition := current.WakeCondition
	if current.Status == models.AgentRunStatusPaused && current.PausedFrom == models.AgentRunStatusWaitingForApproval {
		condition = current.PausedWakeCondition
	}
	if condition == nil || condition.Type != "approval" || strings.TrimSpace(condition.Reference) == "" {
		return nil, fmt.Errorf("%w: waiting approval run has no approval wake condition", models.ErrInvalidRunTransition)
	}
	approval, err := s.store.GetApproval(ctx, current.Scope, condition.Reference)
	if err != nil {
		return nil, err
	}
	if approval == nil || approval.Status != models.ApprovalStatusPending || approval.RunID != current.ID {
		return nil, fmt.Errorf("%w: run approval is unavailable or already resolved", models.ErrApprovalResolved)
	}
	call, err := s.store.GetActionCall(ctx, current.Scope, approval.ActionCallID)
	if err != nil {
		return nil, err
	}
	if call == nil || call.Status != models.ActionCallStatusWaitingApproval || call.RunID != current.ID || call.ApprovalID != approval.ID {
		return nil, fmt.Errorf("%w: run action is not waiting on its approval", models.ErrApprovalResolved)
	}

	now := s.now().UTC()
	actor := req.Actor
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" {
		actor = models.ActivityActor{Type: "system", ID: "openseal"}
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" {
		summary = "Run canceled"
	}
	decisionID := "run-cancel:" + models.SHA256DigestString(current.Scope.Kind+"\x00"+current.Scope.ID+"\x00"+current.ID+"\x00"+fmt.Sprint(current.Revision))

	updatedApproval := models.CloneApprovalCheckpoint(approval)
	updatedApproval.Status = models.ApprovalStatusCanceled
	updatedApproval.DecisionID = decisionID
	updatedApproval.DecisionBy = &models.ApprovalPrincipal{Type: actor.Type, ID: actor.ID}
	updatedApproval.DecisionReason = summary
	updatedApproval.DecidedAt = &now
	updatedApproval.UpdatedAt = now
	updatedApproval.Revision++

	updatedCall := models.CloneActionCall(call)
	updatedCall.Status = models.ActionCallStatusCanceled
	updatedCall.Error = "run canceled: " + summary
	updatedCall.LeaseOwner = ""
	updatedCall.LeaseExpiresAt = nil
	updatedCall.CompletedAt = &now
	updatedCall.UpdatedAt = now
	updatedCall.Revision++

	updatedRun := models.CloneAgentRun(current)
	if updatedRun.Budget != nil {
		if _, reserved := updatedRun.BudgetReservations[ActionBudgetReservationID(call.ID)]; reserved {
			if err := models.ReleaseRunBudgetReservation(updatedRun, ActionBudgetReservationID(call.ID)); err != nil {
				return nil, err
			}
		}
	}
	updatedRun.Status = models.AgentRunStatusCanceled
	updatedRun.WakeCondition = nil
	updatedRun.PausedFrom = ""
	updatedRun.PausedWakeCondition = nil
	updatedRun.LeaseOwner = ""
	updatedRun.LeaseExpiresAt = nil
	updatedRun.CompletedAt = &now
	updatedRun.UpdatedAt = now
	updatedRun.Revision++

	visibility := req.Visibility
	if visibility == "" {
		visibility = models.ActivityVisibilityScope
	}
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: current.Scope, EventType: "run.canceled", Severity: models.ActivitySeverityInfo,
		AgentID: current.AssignedAgentID, ObjectiveID: current.ObjectiveID, RunID: current.ID, TurnID: call.TurnID,
		ParentRunID: current.ParentRunID, TeamID: models.TeamIDForRun(current), Actor: actor, Summary: summary,
		Visibility: visibility, CausationID: approval.ID, CreatedAt: now,
		Payload: map[string]interface{}{"status": models.AgentRunStatusCanceled, "approvalId": approval.ID, "actionCallId": call.ID},
	}
	resolved, err := s.store.ResolveApproval(ctx, models.ApprovalResolutionRecord{
		Approval: updatedApproval, ExpectedApprovalRevision: approval.Revision,
		Call: updatedCall, ExpectedCallRevision: call.Revision,
		Run: updatedRun, ExpectedRunRevision: current.Revision, Event: event,
	})
	if err != nil {
		return nil, err
	}
	return &CommandResult{Run: resolved.Run, Event: resolved.Event}, nil
}

func commandTransition(current *models.AgentRun, req CommandRequest, now time.Time) (activity.RunTransitionRequest, error) {
	transition := activity.RunTransitionRequest{
		ExpectedRevision: req.ExpectedRevision, Actor: req.Actor, Visibility: req.Visibility,
		Summary: strings.TrimSpace(req.Summary), OccurredAt: &now,
	}
	switch req.Kind {
	case CommandPause:
		if current.Status == models.AgentRunStatusPaused || models.IsTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot pause %s run", models.ErrInvalidRunTransition, current.Status)
		}
		transition.Status = models.AgentRunStatusPaused
		transition.EventType = "run.paused"
		if transition.Summary == "" {
			transition.Summary = "Run paused"
		}
	case CommandResume:
		if current.Status != models.AgentRunStatusPaused {
			return transition, fmt.Errorf("%w: cannot resume %s run", models.ErrInvalidRunTransition, current.Status)
		}
		transition.Status = resumedRunStatus(current.PausedFrom)
		if models.IsWaitingRunStatus(transition.Status) {
			transition.WakeCondition = current.PausedWakeCondition
			if transition.WakeCondition == nil {
				return transition, fmt.Errorf("%w: paused waiting run has no wake condition", models.ErrInvalidRunTransition)
			}
		}
		transition.EventType = "run.resumed"
		if transition.Summary == "" {
			transition.Summary = "Run resumed"
		}
	case CommandCancel:
		if models.IsTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot cancel %s run", models.ErrInvalidRunTransition, current.Status)
		}
		transition.Status = models.AgentRunStatusCanceled
		transition.HumanInterventions = resolvePendingHumanInterventions(current.HumanInterventions, models.HumanInterventionStatusCanceled, req.Actor, "Run canceled", now)
		transition.EventType = "run.canceled"
		if transition.Summary == "" {
			transition.Summary = "Run canceled"
		}
	case CommandIntervene:
		instruction := strings.TrimSpace(req.Instruction)
		if instruction == "" || len(instruction) > 16000 {
			return transition, fmt.Errorf("%w: guidance must contain 1 to 16000 UTF-8 bytes", portfolio.ErrInvalidRunCommand)
		}
		if models.IsTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot intervene on %s run", models.ErrInvalidRunTransition, current.Status)
		}
		if current.Status == models.AgentRunStatusSleeping {
			// Operator guidance is new durable input. A sleeping run must consume it
			// immediately instead of preserving a stale retry or schedule timer.
			transition.Status = models.AgentRunStatusQueued
		} else {
			transition.Status = current.Status
			transition.WakeCondition = current.WakeCondition
		}
		transition.EventType = "run.intervened"
		id := req.InterventionID
		if id == "" {
			id = uuid.NewString()
		}
		transition.Intervention = &models.AgentRunIntervention{ID: id, Actor: req.Actor, Instruction: instruction, CreatedAt: now}
		transition.Payload = map[string]interface{}{"interventionId": transition.Intervention.ID, "instruction": instruction}
		if transition.Summary == "" {
			transition.Summary = "Operator steered the run"
		}
	case CommandResolveHumanIntervention:
		requestID := strings.TrimSpace(req.HumanInterventionID)
		instruction := strings.TrimSpace(req.Instruction)
		if requestID == "" || instruction == "" {
			return transition, fmt.Errorf("%w: human intervention id and resolution instruction are required", portfolio.ErrInvalidRunCommand)
		}
		if models.IsTerminalAgentRunStatus(current.Status) {
			return transition, fmt.Errorf("%w: cannot resolve a human intervention of a %s run", models.ErrInvalidRunTransition, current.Status)
		}
		humanWait := func(condition *models.WakeCondition) bool {
			return condition != nil && condition.Type == "human_intervention" && condition.Reference == requestID
		}
		if current.Status == models.AgentRunStatusPaused && humanWait(current.PausedWakeCondition) {
			return transition, fmt.Errorf("%w: resume the run before resolving human intervention %s", models.ErrInvalidRunTransition, requestID)
		}
		resolved, actionCallID, found := resolveHumanIntervention(current.HumanInterventions, requestID, req.Actor, instruction, now)
		if !found {
			return transition, fmt.Errorf("%w: human intervention %s is not pending", models.ErrInvalidRunTransition, requestID)
		}
		if current.Status == models.AgentRunStatusWaitingForEvent && humanWait(current.WakeCondition) {
			transition.Status = models.AgentRunStatusQueued
		} else {
			// The step that asked for this person is still held (A5): the
			// resolution only bumps the Run revision, so the step-end park
			// CAS fails and re-plans instead of waiting on a settled request.
			transition.Status = current.Status
			transition.WakeCondition = current.WakeCondition
		}
		transition.HumanInterventions = resolved
		transition.Intervention = &models.AgentRunIntervention{ID: uuid.NewString(), Actor: req.Actor, Instruction: instruction, CreatedAt: now, ActionCallID: actionCallID}
		transition.EventType = "run.human_intervention_resolved"
		transition.CausationID = requestID
		transition.Payload = map[string]interface{}{"humanInterventionId": requestID, "status": models.HumanInterventionStatusResolved}
		if transition.Summary == "" {
			transition.Summary = "Human intervention resolved"
		}
	default:
		return transition, fmt.Errorf("%w: kind must be pause, resume, cancel, intervene, or resolve_human_intervention", portfolio.ErrInvalidRunCommand)
	}
	return transition, nil
}

// resolveHumanIntervention resolves the pending request and returns the
// action call that asked for it.
func resolveHumanIntervention(values []models.HumanInterventionRequest, requestID string, actor models.ActivityActor, resolution string, now time.Time) ([]models.HumanInterventionRequest, string, bool) {
	result := append([]models.HumanInterventionRequest(nil), values...)
	for index := range result {
		if result[index].ID != requestID || result[index].Status != models.HumanInterventionStatusPending {
			continue
		}
		resolvedAt := now
		resolvedBy := actor
		result[index].Status = models.HumanInterventionStatusResolved
		result[index].Resolution = resolution
		result[index].ResolvedBy = &resolvedBy
		result[index].ResolvedAt = &resolvedAt
		return result, result[index].ActionCallID, true
	}
	return result, "", false
}

func resolvePendingHumanInterventions(values []models.HumanInterventionRequest, status models.HumanInterventionStatus, actor models.ActivityActor, resolution string, now time.Time) []models.HumanInterventionRequest {
	result := append([]models.HumanInterventionRequest(nil), values...)
	for index := range result {
		if result[index].Status != models.HumanInterventionStatusPending {
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

func resumedRunStatus(previous models.AgentRunStatus) models.AgentRunStatus {
	if models.IsWaitingRunStatus(previous) {
		return previous
	}
	return models.AgentRunStatusQueued
}

func runCreationFingerprint(req models.CreateAgentRunRequest) (string, error) {
	contextValues := req.Context
	if _, present := contextValues[models.AcceptedRunExecutionContextKey]; present {
		pin, err := models.AcceptedRunExecutionForRun(&models.AgentRun{Scope: req.Scope, AssignedAgentID: req.AssignedAgentID, Entrypoint: strings.TrimSpace(req.Entrypoint), Context: contextValues})
		if err != nil {
			return "", err
		}
		contextValues = maps.Clone(contextValues)
		delete(contextValues, models.AcceptedRunExecutionContextKey)
		if pin.DerivedRunbookIdentity {
			delete(contextValues, "runbookDefinitionId")
			delete(contextValues, "runbookDefinitionVersion")
		}
		if len(contextValues) == 0 {
			contextValues = nil
		}
	}
	payload := struct {
		Scope                models.Scope           `json:"scope"`
		Kind                 models.RunKind         `json:"kind"`
		ObjectiveID          string                 `json:"objectiveId,omitempty"`
		ParentRunID          string                 `json:"parentRunId,omitempty"`
		Owner                models.ObjectiveOwner  `json:"owner"`
		AssignedAgentID      string                 `json:"assignedAgentId,omitempty"`
		Entrypoint           string                 `json:"entrypoint,omitempty"`
		ConcurrencyKey       string                 `json:"concurrencyKey,omitempty"`
		ResourceRequirements map[string]int         `json:"resourceRequirements,omitempty"`
		Goal                 string                 `json:"goal"`
		Source               models.RunSource       `json:"source"`
		Priority             int                    `json:"priority"`
		Deadline             *time.Time             `json:"deadline,omitempty"`
		AvailableAt          *time.Time             `json:"availableAt,omitempty"`
		Context              map[string]interface{} `json:"context,omitempty"`
		Plan                 map[string]interface{} `json:"plan,omitempty"`
		Checkpoint           map[string]interface{} `json:"checkpoint,omitempty"`
		WakeCondition        *models.WakeCondition  `json:"wakeCondition,omitempty"`
		Budget               *models.BudgetPolicy   `json:"budget,omitempty"`
		Policy               map[string]interface{} `json:"policy,omitempty"`
	}{
		Scope: req.Scope, Kind: models.NormalizeRunKind(req.Kind), ObjectiveID: req.ObjectiveID,
		ParentRunID: req.ParentRunID, Owner: req.Owner, AssignedAgentID: req.AssignedAgentID, Entrypoint: strings.TrimSpace(req.Entrypoint),
		ConcurrencyKey:       strings.TrimSpace(req.ConcurrencyKey),
		ResourceRequirements: req.ResourceRequirements,
		Goal:                 req.Goal, Source: req.Source, Priority: req.Priority, Deadline: req.Deadline,
		AvailableAt: req.AvailableAt, Context: contextValues, Plan: req.Plan, Checkpoint: req.Checkpoint,
		WakeCondition: req.WakeCondition, Budget: req.Budget, Policy: req.Policy,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode run creation fingerprint: %w", err)
	}
	return models.SHA256Digest(encoded), nil
}
