package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

const ConversationTaskSkillSetupWakeType = "skill_setup"

const conversationTaskSetupEvent = "run.skill_setup_completed"

// SkillSetupResolutionReceipt is kernel-authored evidence of the separately
// authorized setup interaction. Configuration saved is not provider access
// verified, and this receipt never grants action or credential authority.
type SkillSetupResolutionReceipt struct {
	Scope                   Scope  `json:"scope"`
	RunID                   string `json:"runId"`
	RequestID               string `json:"requestId"`
	Status                  string `json:"status"`
	ActionCallID            string `json:"actionCallId"`
	Revision                int64  `json:"revision"`
	ResolvedBindingID       string `json:"resolvedBindingId,omitempty"`
	ResolvedBindingRevision int64  `json:"resolvedBindingRevision,omitempty"`
}

type conversationTaskSetupStore interface {
	ConversationTaskProofStore
	SkillSetupRequestStore
	GetActionCall(context.Context, Scope, string) (*ActionCall, error)
}

// The immutable task ledger, never copied conversation context, establishes
// setup access for an independent task whose foreground source has completed.
func conversationTaskSetupOrigin(ctx context.Context, store ConversationTaskProofStore, run *AgentRun) (*ConversationTask, error) {
	if run == nil {
		return nil, ErrInvalidSkillSetup
	}
	task, err := store.FindConversationTaskByWorkRunID(ctx, run.Scope, run.ID)
	if errors.Is(err, ErrConversationTaskNotFound) {
		return nil, nil
	}
	if err != nil || task == nil {
		return nil, err
	}
	if task.Mode != ConversationTaskModeIndependent || task.Owner.Type != OwnerTypeAgent || task.TargetAgentID != task.Owner.ID {
		return nil, ErrInvalidSkillSetup
	}
	valid, err := VerifyConversationTaskWorkRun(ctx, store, task, run)
	if err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrInvalidSkillSetup
	}
	conversation, err := store.GetConversation(ctx, task.Scope, task.ConversationID)
	if err != nil {
		return nil, err
	}
	if conversation == nil || conversation.Status != ConversationStatusActive || conversation.Scope != task.Scope || conversation.Owner != task.Owner {
		return nil, ErrInvalidSkillSetup
	}
	return task, nil
}

func taskSetupActionMatches(call *ActionCall, run *AgentRun, deploymentID string) bool {
	return call != nil && run != nil && call.Scope == run.Scope && call.RunID == run.ID &&
		call.DeploymentID == deploymentID && call.SkillID == SkillManagementSkillID &&
		call.SkillVersion == SkillManagementSkillVersion && call.Action == SkillActionRequestSetup
}

func verifiedTaskSetupRequest(ctx context.Context, store conversationTaskSetupStore, run *AgentRun, requestID string, requireCompletedAction bool) (*ConversationTask, *SkillSetupRequest, error) {
	task, err := conversationTaskSetupOrigin(ctx, store, run)
	if err != nil || task == nil {
		return task, nil, err
	}
	request, err := store.GetSkillSetupRequest(ctx, run.Scope, requestID)
	if err != nil {
		return task, nil, err
	}
	if request == nil || request.Validate() != nil || request.ID != requestID || request.Scope != run.Scope ||
		request.RunID != run.ID || request.DeploymentID != task.TargetAgentID || request.ConversationID != task.ConversationID ||
		request.TriggerMessageID != task.SourceMessageID || request.ID != "skill-setup:"+request.ActionCallID {
		return task, nil, ErrInvalidSkillSetup
	}
	call, err := store.GetActionCall(ctx, run.Scope, request.ActionCallID)
	if err != nil {
		return task, nil, err
	}
	if !taskSetupActionMatches(call, run, task.TargetAgentID) {
		return task, nil, ErrInvalidSkillSetup
	}
	if call.Status != ActionCallStatusSucceeded {
		// The form can be resolved between its save and the action commit. It
		// already belongs to this task, but cannot wake it before a verified wait.
		if !requireCompletedAction && (call.Status == ActionCallStatusReady || call.Status == ActionCallStatusRunning) {
			return task, request, nil
		}
		return task, nil, ErrInvalidSkillSetup
	}
	// Bind the mutable terminal record back to the exact successful action's
	// original pending receipt. A supplied setup ID alone cannot park or wake work.
	var admitted SkillSetupRequest
	encoded, err := json.Marshal(call.Output["setupRequest"])
	if err != nil || json.Unmarshal(encoded, &admitted) != nil || admitted.Validate() != nil || requireCompletedAction && admitted.Status != "pending" ||
		admitted.ID != request.ID || admitted.Scope != request.Scope || admitted.RunID != request.RunID ||
		admitted.ActionCallID != request.ActionCallID || admitted.DeploymentID != request.DeploymentID ||
		admitted.ConversationID != request.ConversationID || admitted.TriggerMessageID != request.TriggerMessageID ||
		admitted.SkillID != request.SkillID || admitted.SourceIdentity != request.SourceIdentity || admitted.BindingID != request.BindingID ||
		admitted.EnablePrompt != request.EnablePrompt || !slices.Equal(admitted.RequiredActions, request.RequiredActions) ||
		admitted.Reason != request.Reason || !admitted.CreatedAt.Equal(request.CreatedAt) ||
		!taskSetupTargetFollowsReceipt(&admitted, request) {
		return task, nil, ErrInvalidSkillSetup
	}
	return task, request, nil
}

// An authorized host can upgrade a pending form to a reviewed installed version
// from the same source. The canonical store's CAS revision records that change;
// the historical action receipt remains unchanged. Only those target fields may
// differ: the requested capability, purpose, binding and task lineage stay fixed.
func taskSetupTargetFollowsReceipt(admitted, current *SkillSetupRequest) bool {
	if current.Revision < admitted.Revision || current.UpdatedAt.Before(admitted.UpdatedAt) {
		return false
	}
	minimumRevision := admitted.Revision
	if admitted.Status == "pending" && current.Status != "pending" {
		minimumRevision++
	}
	if current.SkillVersion == admitted.SkillVersion {
		return current.Revision >= minimumRevision && current.Kind == admitted.Kind &&
			current.SkillName == admitted.SkillName && current.BindingRevision == admitted.BindingRevision &&
			current.InstallationReference == admitted.InstallationReference && current.SourceDigest == admitted.SourceDigest
	}
	// A version upgrade and a later terminal resolution are separate CAS writes.
	if current.Revision < minimumRevision+1 || current.SourceIdentity == "" ||
		current.InstallationReference != "" || current.SourceDigest != "" {
		return false
	}
	if current.BindingID == "" {
		return current.Kind == "configure" && current.BindingRevision == admitted.BindingRevision
	}
	return current.Kind == admitted.Kind && current.BindingRevision >= admitted.BindingRevision
}

func validateConversationTaskSetupWait(ctx context.Context, portfolio PortfolioStore, run *AgentRun, outcome *TurnOutcome) error {
	if outcome == nil || outcome.WakeCondition == nil || outcome.WakeCondition.Type != ConversationTaskSkillSetupWakeType {
		return nil
	}
	wake := outcome.WakeCondition
	if outcome.NextRunStatus != AgentRunStatusWaitingForEvent || !validOpaqueIdentifier(wake.Reference, 256) || wake.WakeAt != nil ||
		wake.EventWait != nil || len(wake.Predicate) != 0 || len(outcome.ProposedActions) != 0 || outcome.ProposedTask != nil ||
		outcome.ProposedFork != nil || outcome.ProposedDelegation != nil || outcome.ProposedRunbook != nil {
		return ErrInvalidSkillSetup
	}
	store, ok := portfolio.(conversationTaskSetupStore)
	if !ok {
		return ErrInvalidSkillSetup
	}
	task, request, err := verifiedTaskSetupRequest(ctx, store, run, wake.Reference, true)
	if err != nil {
		return err
	}
	if task == nil {
		return ErrInvalidSkillSetup
	}
	activity, ok := portfolio.(RunActivityStore)
	if !ok {
		return ErrInvalidSkillSetup
	}
	accepted, err := skillSetupTaskAccepted(ctx, activity, run, request)
	if err != nil {
		return err
	}
	if accepted {
		return errors.New("Skill setup result was already delivered to this task")
	}
	return nil
}

func isExactTaskSetupWait(run *AgentRun, requestID string) bool {
	return run.Status == AgentRunStatusWaitingForEvent && run.WakeCondition != nil &&
		run.WakeCondition.Type == ConversationTaskSkillSetupWakeType && run.WakeCondition.Reference == requestID &&
		run.WakeCondition.WakeAt == nil && run.WakeCondition.EventWait == nil && len(run.WakeCondition.Predicate) == 0
}

// ReconcileSkillSetupTask resumes only the exact task waiting for this saved
// setup request. Handled remains true for pending, paused, canceled and already
// resumed tasks: a host must not create a new foreground request for those.
func (s *ConversationRunScheduler) ReconcileSkillSetupTask(ctx context.Context, scope Scope, requestID string) (*AgentRunCommandResult, bool, error) {
	if s == nil || s.runs == nil || scope.Validate() != nil || !validOpaqueIdentifier(requestID, 256) {
		return nil, false, ErrInvalidSkillSetup
	}
	store, ok := s.runs.store.(conversationTaskSetupStore)
	if !ok {
		return nil, false, errors.New("conversation task setup reconciliation is unavailable")
	}
	request, err := store.GetSkillSetupRequest(ctx, scope, requestID)
	if err != nil || request == nil {
		return nil, false, err
	}
	run, err := store.GetAgentRun(ctx, scope, request.RunID)
	if err != nil || run == nil {
		return nil, false, err
	}
	// Existing ordinary foreground setup completions retain their host path.
	if run.Kind != RunKindAgentWork || run.ParentRunID != "" {
		return nil, false, nil
	}
	task, request, err := verifiedTaskSetupRequest(ctx, store, run, requestID, isExactTaskSetupWait(run, requestID))
	if err != nil {
		return nil, true, err
	}
	if task == nil {
		return nil, false, nil
	}
	result := &AgentRunCommandResult{Run: run}
	accepted, err := skillSetupTaskAccepted(ctx, s.runs.store, run, request)
	if err != nil || accepted {
		return result, true, err
	}
	if request.Status == "pending" || !isExactTaskSetupWait(run, request.ID) {
		return result, true, nil
	}
	if request.Status != "resolved" && request.Status != "dismissed" {
		return nil, true, ErrInvalidSkillSetup
	}
	receipt := &SkillSetupResolutionReceipt{Scope: scope, RunID: run.ID, RequestID: request.ID, Status: request.Status,
		ActionCallID: request.ActionCallID, Revision: request.Revision, ResolvedBindingID: request.ResolvedBindingID,
		ResolvedBindingRevision: request.ResolvedBindingRevision}
	instruction := "The requested Skill setup was dismissed. Do not treat it as authorization or repeat the setup; explain the remaining blocker or use an already authorized alternative."
	if request.Status == "resolved" {
		instruction = "The requested Skill configuration was saved. Continue this task from its saved progress. Provider access is not yet verified; use only currently authorized actions to verify it and do not repeat completed work."
	}
	updated, event, err := NewRunActivityService(s.runs.store, s.runs.store).TransitionRun(ctx, scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusQueued,
		Summary: "Received the Skill setup result; continuing the task", EventType: conversationTaskSetupEvent,
		Actor: ActivityActor{Type: "service", ID: "skill-setup-coordinator"}, CorrelationID: request.ID, CausationID: request.ActionCallID,
		Payload: map[string]interface{}{"requestId": request.ID, "status": request.Status, "revision": request.Revision, "resolvedBy": request.ResolvedBy},
		Intervention: &AgentRunIntervention{ID: stableConversationID(scope, "skill-setup-result:"+request.ID, "intervention"),
			Actor: ActivityActor{Type: "service", ID: "skill-setup-coordinator"}, Instruction: instruction,
			CreatedAt: request.UpdatedAt, SkillSetupResolution: receipt},
	})
	if errors.Is(err, ErrRevisionConflict) {
		// A competing callback may already have committed the same atomic result.
		latest, readErr := store.GetAgentRun(ctx, scope, run.ID)
		if readErr != nil {
			return nil, true, readErr
		}
		if latest != nil {
			accepted, readErr := skillSetupTaskAccepted(ctx, s.runs.store, latest, request)
			if readErr != nil {
				return nil, true, readErr
			}
			if accepted || latest.Status != AgentRunStatusWaitingForEvent {
				return &AgentRunCommandResult{Run: latest}, true, nil
			}
		}
	}
	if err != nil {
		return nil, true, err
	}
	return &AgentRunCommandResult{Run: updated, Event: event}, true, nil
}

func skillSetupTaskAccepted(ctx context.Context, store RunActivityStore, run *AgentRun, request *SkillSetupRequest) (bool, error) {
	for after := int64(0); ; {
		events, err := store.ListActivity(ctx, ActivityFilter{Scope: run.Scope, RunID: run.ID, EventTypes: []string{conversationTaskSetupEvent}, AfterSequence: after, Limit: 100})
		if err != nil {
			return false, err
		}
		for _, event := range events {
			if event.EventType == conversationTaskSetupEvent && event.CorrelationID == request.ID && event.CausationID == request.ActionCallID {
				return true, nil
			}
		}
		if len(events) < 100 {
			return false, nil
		}
		after = events[len(events)-1].Sequence
	}
}

func (s *ConversationRunScheduler) reconcileSkillSetupTasks(ctx context.Context, scope Scope, result *ConversationRunReconcileResult) error {
	if _, ok := s.runs.store.(conversationTaskSetupStore); !ok {
		return nil
	}
	// Collect identities before changing status so offset pagination does not
	// skip later waits when the first page becomes queued.
	var requestIDs []string
	for offset := 0; ; offset += 100 {
		runs, err := s.runs.store.ListAgentRuns(ctx, AgentRunFilter{Scope: scope, Kind: RunKindAgentWork, Statuses: []AgentRunStatus{AgentRunStatusWaitingForEvent}, Limit: 100, Offset: offset})
		if err != nil {
			return err
		}
		for _, run := range runs {
			if run.WakeCondition != nil && run.WakeCondition.Type == ConversationTaskSkillSetupWakeType {
				requestIDs = append(requestIDs, run.WakeCondition.Reference)
			}
		}
		if len(runs) < 100 {
			break
		}
	}
	for _, id := range requestIDs {
		resumed, _, err := s.ReconcileSkillSetupTask(ctx, scope, id)
		if errors.Is(err, ErrInvalidSkillSetup) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reconcile task Skill setup: %w", err)
		}
		if resumed != nil && resumed.Event != nil {
			result.Results++
		}
	}
	return nil
}
