package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const (
	// AgentRequestInboxContextKey is kernel-authored input for a bounded
	// recipient decision Run. It is model-visible but never model-writable.
	AgentRequestInboxContextKey = "agentRequestInbox"
	// AgentRequestDecisionOutputKey is the only model output consumed as an
	// AgentRequest lifecycle decision.
	AgentRequestDecisionOutputKey = "agentRequestDecision"
	// AgentRequestCompletionReviewContextKey is kernel-authored input for the
	// requester-side independent review of completed delegated work.
	AgentRequestCompletionReviewContextKey = "agentRequestCompletionReview"
	// AgentRequestCompletionReviewOutputKey is the only model output consumed
	// to resolve an independent completion-review checkpoint.
	AgentRequestCompletionReviewOutputKey    = "agentRequestCompletionReviewDecision"
	acceptedAgentRequestExecutionRecoveryKey = "_opensealAcceptedAgentRequestExecutionRecovery"
)

const agentRequestDecisionSystemInstruction = `Evaluate the incoming AgentRequest in inputContext.agentRequestInbox before doing any requested work. Decide whether the request is relevant, sufficiently clear, safe, and within your role. Do not execute Skills, delegate, fork, or perform the requested work during this review. Complete the review with runOutput.agentRequestDecision set to exactly {"decision":"accept","message":"concise reason"}, {"decision":"reject","message":"concise reason"}, or {"decision":"request_clarification","message":"one concrete question"}.`

const agentRequestCompletionReviewSystemInstruction = `Independently review the completed delegated work in inputContext.agentRequestCompletionReview against its goal, acceptance criteria, required artifacts, evidence, and output. Do not execute Skills, invoke runbooks, delegate, fork, or modify the work during this review. Complete the review with runOutput.agentRequestCompletionReviewDecision set to exactly {"decision":"approve","message":"concise evidence-based reason"} or {"decision":"reject","message":"concise evidence-based reason"}.`

const acceptedAgentRequestExecutionSystemInstruction = `This Run is the execution child of an AgentRequest that has already passed intake and was accepted. Perform the requested goal now using the authorized Skills and durable work primitives available to this Run. Do not evaluate or accept the request again, and do not emit runOutput.agentRequestDecision. If continuationCheckpoint._opensealAcceptedAgentRequestExecutionRecovery is present, the previous execution turn was discarded because it repeated the already-completed intake decision; continue directly with the requested work and do not repeat that output.`

type agentRequestTeamDefinitionStore interface {
	GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error)
}

type AgentRequestInboxStore interface {
	CollaborationKernelStore
	RunCommandStore
}

type agentRequestInboxConversationStore interface {
	AgentRequestInboxStore
	ConversationStore
}

type AgentRequestInboxReconcileResult struct {
	RequestsScanned          int `json:"requestsScanned"`
	RequestsAccepted         int `json:"requestsAccepted"`
	DecisionRunsCreated      int `json:"decisionRunsCreated"`
	DecisionRunsReused       int `json:"decisionRunsReused"`
	DecisionsApplied         int `json:"decisionsApplied"`
	CompletionReviewsCreated int `json:"completionReviewsCreated"`
	CompletionReviewsReused  int `json:"completionReviewsReused"`
	CompletionReviewsApplied int `json:"completionReviewsApplied"`
	ConversationProjections  int `json:"conversationProjections"`
}

type agentRequestDecisionTurnRunner struct {
	inner TurnRunner
}

type agentRequestCompletionReviewTurnRunner struct {
	inner TurnRunner
}

type acceptedAgentRequestExecutionTurnRunner struct {
	inner TurnRunner
}

func (r *agentRequestDecisionTurnRunner) PlanTurnBudget(ctx context.Context, input TurnExecutionContext) (BudgetUsage, error) {
	if planner, ok := r.inner.(TurnBudgetPlanner); ok {
		return planner.PlanTurnBudget(ctx, input)
	}
	return BudgetUsage{}, nil
}

func (r *agentRequestDecisionTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || r.inner == nil {
		return nil, errors.New("AgentRequest decision runner is unavailable")
	}
	outcome, err := r.inner.RunTurn(ctx, input)
	if err != nil || outcome == nil {
		return outcome, err
	}
	if len(outcome.ProposedActions) > 0 || outcome.ProposedFork != nil || outcome.ProposedDelegation != nil {
		return nil, errors.New("AgentRequest decision turns cannot execute actions, fork, or delegate")
	}
	if outcome.NextRunStatus == AgentRunStatusCompleted {
		if _, _, err := parseAgentRequestDecisionOutput(outcome.RunOutput); err != nil {
			return nil, err
		}
	}
	return outcome, nil
}

func (r *agentRequestCompletionReviewTurnRunner) PlanTurnBudget(ctx context.Context, input TurnExecutionContext) (BudgetUsage, error) {
	if planner, ok := r.inner.(TurnBudgetPlanner); ok {
		return planner.PlanTurnBudget(ctx, input)
	}
	return BudgetUsage{}, nil
}

func (r *agentRequestCompletionReviewTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || r.inner == nil {
		return nil, errors.New("AgentRequest completion review runner is unavailable")
	}
	outcome, err := r.inner.RunTurn(ctx, input)
	if err != nil || outcome == nil {
		return outcome, err
	}
	if len(outcome.ProposedActions) > 0 || outcome.ProposedRunbook != nil || outcome.ProposedFork != nil || outcome.ProposedDelegation != nil {
		return nil, errors.New("AgentRequest completion review turns cannot execute actions, invoke runbooks, fork, or delegate")
	}
	if outcome.NextRunStatus == AgentRunStatusCompleted {
		if _, _, err := parseAgentRequestCompletionReviewOutput(outcome.RunOutput); err != nil {
			return nil, err
		}
	}
	return outcome, nil
}

func (r *acceptedAgentRequestExecutionTurnRunner) PlanTurnBudget(ctx context.Context, input TurnExecutionContext) (BudgetUsage, error) {
	if planner, ok := r.inner.(TurnBudgetPlanner); ok {
		return planner.PlanTurnBudget(ctx, input)
	}
	return BudgetUsage{}, nil
}

func (r *acceptedAgentRequestExecutionTurnRunner) RunTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if r == nil || r.inner == nil {
		return nil, errors.New("accepted AgentRequest execution runner is unavailable")
	}
	outcome, err := r.inner.RunTurn(ctx, input)
	if err != nil || outcome == nil {
		return outcome, err
	}
	if _, decisionOnly := outcome.RunOutput[AgentRequestDecisionOutputKey]; decisionOnly {
		if acceptedAgentRequestExecutionRecoveryAttempt(input.Run.Checkpoint) > 0 {
			return nil, errors.New("accepted AgentRequest execution repeatedly emitted an intake decision")
		}
		checkpoint := cloneMap(outcome.ContinuationCheckpoint)
		if checkpoint == nil {
			checkpoint = map[string]interface{}{}
		}
		checkpoint[acceptedAgentRequestExecutionRecoveryKey] = map[string]interface{}{
			"attempt": 1,
			"error":   "The AgentRequest was already accepted; continue with its requested work.",
		}
		outcome.ContinuationCheckpoint = checkpoint
		outcome.NextRunStatus = AgentRunStatusRunning
		outcome.WakeCondition = nil
		outcome.RunOutput = nil
		outcome.RunError = ""
		outcome.ProposedActions = nil
		outcome.ProposedFork = nil
		outcome.ProposedDelegation = nil
		outcome.ProposedRunbook = nil
		outcome.OutputSummary = "Discarded a repeated intake decision and continued accepted work"
	}
	return outcome, nil
}

func acceptedAgentRequestExecutionRecoveryAttempt(checkpoint map[string]interface{}) int {
	recovery, _ := checkpoint[acceptedAgentRequestExecutionRecoveryKey].(map[string]interface{})
	switch value := recovery["attempt"].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

// AgentRequestInboxReconciler converts pending recipient work into either a
// policy-authorized acceptance or a bounded decision Run. Stable run
// idempotency makes request review recoverable across process restarts without
// leasing Agent identity to a human caller.
type AgentRequestInboxReconciler struct {
	collaboration *CollaborationService
	commands      *RunCommandService
	projector     *AgentRequestConversationProjector
}

func NewAgentRequestInboxReconciler(store AgentRequestInboxStore) (*AgentRequestInboxReconciler, error) {
	if store == nil {
		return nil, errors.New("AgentRequest inbox store is required")
	}
	reconciler := &AgentRequestInboxReconciler{
		collaboration: NewCollaborationService(store),
		commands:      NewRunCommandService(store),
	}
	if conversationStore, ok := store.(agentRequestInboxConversationStore); ok {
		reconciler.projector, _ = NewAgentRequestConversationProjector(conversationStore)
	}
	return reconciler, nil
}

func (r *AgentRequestInboxReconciler) Reconcile(ctx context.Context, scope Scope, assignedAgentID string) (*AgentRequestInboxReconcileResult, error) {
	if r == nil || r.collaboration == nil || r.commands == nil {
		return nil, errors.New("AgentRequest inbox reconciler is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	requests, err := r.pendingRequests(ctx, scope)
	if err != nil {
		return nil, err
	}
	result := &AgentRequestInboxReconcileResult{RequestsScanned: len(requests)}
	var failures []error
	if r.projector != nil {
		projected, projectionErr := r.projector.Reconcile(ctx, scope)
		result.ConversationProjections = projected
		if projectionErr != nil {
			failures = append(failures, projectionErr)
		}
	}
	for _, request := range requests {
		if request.Status == AgentRequestStatusCompletionReview {
			if err := r.reconcileCompletionReview(ctx, request, assignedAgentID, result); err != nil {
				failures = append(failures, fmt.Errorf("completion review %s: %w", request.ID, err))
			}
			continue
		}
		assigned, requireReview, assignmentErr := r.recipientAssignment(ctx, request)
		if assignmentErr != nil {
			failures = append(failures, fmt.Errorf("request %s: %w", request.ID, assignmentErr))
			continue
		}
		if filter := strings.TrimSpace(assignedAgentID); filter != "" && filter != assigned {
			continue
		}
		actor := CollaborationParty{Type: OwnerTypeAgent, ID: assigned}
		if request.AcceptancePolicy == AgentRequestAcceptancePreauthorized || !requireReview {
			if _, responseErr := r.collaboration.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
				Decision: AgentRequestDecisionAccept, Principal: request.Recipient, Actor: actor,
				AssignedAgentID: assigned, Message: "Accepted under the configured recipient policy.",
			}); responseErr != nil && !errors.Is(responseErr, ErrRevisionConflict) && !errors.Is(responseErr, ErrInvalidAgentRequestState) {
				failures = append(failures, fmt.Errorf("accept request %s: %w", request.ID, responseErr))
			} else if responseErr == nil {
				result.RequestsAccepted++
			}
			continue
		}
		decisionRun, created, resolved, createErr := r.ensureDecisionRun(ctx, request, assigned)
		if createErr != nil {
			failures = append(failures, fmt.Errorf("review request %s: %w", request.ID, createErr))
			continue
		}
		if resolved {
			result.DecisionsApplied++
			continue
		}
		if created {
			result.DecisionRunsCreated++
		} else {
			result.DecisionRunsReused++
		}
		if isTerminalAgentRunStatus(decisionRun.Status) {
			applied, resolveErr := r.ResolveDecisionRun(ctx, decisionRun)
			if resolveErr != nil {
				failures = append(failures, fmt.Errorf("resolve review %s: %w", request.ID, resolveErr))
			} else if applied {
				result.DecisionsApplied++
			}
		}
	}
	return result, errors.Join(failures...)
}

func (r *AgentRequestInboxReconciler) pendingRequests(ctx context.Context, scope Scope) ([]*AgentRequest, error) {
	const pageSize = 100
	result := make([]*AgentRequest, 0)
	for offset := 0; ; offset += pageSize {
		page, err := r.collaboration.ListAgentRequests(ctx, AgentRequestFilter{
			Scope: scope, Statuses: []AgentRequestStatus{AgentRequestStatusPending, AgentRequestStatusCompletionReview}, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		result = append(result, page...)
		if len(page) < pageSize {
			return result, nil
		}
	}
}

func (r *AgentRequestInboxReconciler) recipientAssignment(ctx context.Context, request *AgentRequest) (string, bool, error) {
	if request == nil {
		return "", false, ErrAgentRequestNotFound
	}
	if request.Recipient.Type == OwnerTypeAgent {
		return request.Recipient.ID, true, nil
	}
	assigned, definition, err := r.teamPartyAssignment(ctx, request.Scope, request.Recipient, request.SemanticRole, "", false)
	if err != nil {
		return "", false, err
	}
	return assigned, definition.Delegation.RequireAcceptance, nil
}

func (r *AgentRequestInboxReconciler) completionReviewerAssignment(ctx context.Context, request *AgentRequest) (string, error) {
	if request == nil {
		return "", ErrAgentRequestNotFound
	}
	if request.Requester.Type == OwnerTypeAgent {
		if request.Requester.ID == request.AssignedAgentID {
			return "", fmt.Errorf("%w: completed work cannot be reviewed by its worker", ErrAgentRequestAssignment)
		}
		return request.Requester.ID, nil
	}
	assigned, _, err := r.teamPartyAssignment(ctx, request.Scope, request.Requester, "", request.AssignedAgentID, true)
	return assigned, err
}

func (r *AgentRequestInboxReconciler) teamPartyAssignment(
	ctx context.Context,
	scope Scope,
	party CollaborationParty,
	semanticRole string,
	excludedAgentID string,
	useApproverRoles bool,
) (string, *kernelteam.Definition, error) {
	if r.collaboration.teams == nil {
		return "", nil, fmt.Errorf("%w: Team roster assignment is unavailable", ErrAgentRequestAssignment)
	}
	deployment, err := r.collaboration.teams.GetTeamDeployment(ctx, capability.ScopeReference{
		Kind: scope.Kind, ID: scope.ID,
	}, party.ID)
	if err != nil {
		return "", nil, err
	}
	if deployment == nil || deployment.Status != kernelteam.DeploymentActive {
		return "", nil, fmt.Errorf("%w: Team deployment is not active", ErrAgentRequestAssignment)
	}
	definitions, ok := r.collaboration.store.(agentRequestTeamDefinitionStore)
	if !ok {
		return "", nil, fmt.Errorf("%w: Team delegation policy is unavailable", ErrAgentRequestAssignment)
	}
	definition, err := definitions.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return "", nil, err
	}
	if definition == nil {
		return "", nil, fmt.Errorf("%w: active Team definition is unavailable", ErrAgentRequestAssignment)
	}
	eligibleRoles := make(map[string]struct{})
	if useApproverRoles {
		for _, roleID := range definition.Approvals.ApproverRoleIDs {
			eligibleRoles[strings.TrimSpace(roleID)] = struct{}{}
		}
	}
	eligible := make([]kernelteam.RosterAssignment, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		_, approvedRole := eligibleRoles[assignment.RoleID]
		if (semanticRole == "" || assignment.RoleID == semanticRole) &&
			(!useApproverRoles || len(eligibleRoles) == 0 || approvedRole) && assignment.AgentDeploymentID != excludedAgentID {
			eligible = append(eligible, assignment)
		}
	}
	if len(eligible) == 0 {
		return "", nil, fmt.Errorf("%w: Team has no independent Agent eligible for review role %q", ErrAgentRequestAssignment, semanticRole)
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].AgentDeploymentID != eligible[j].AgentDeploymentID {
			return eligible[i].AgentDeploymentID < eligible[j].AgentDeploymentID
		}
		return eligible[i].ID < eligible[j].ID
	})
	return eligible[0].AgentDeploymentID, definition, nil
}

func (r *AgentRequestInboxReconciler) reconcileCompletionReview(
	ctx context.Context,
	request *AgentRequest,
	assignedAgentFilter string,
	result *AgentRequestInboxReconcileResult,
) error {
	reviewerID, err := r.completionReviewerAssignment(ctx, request)
	if err != nil {
		return err
	}
	if filter := strings.TrimSpace(assignedAgentFilter); filter != "" && filter != reviewerID {
		return nil
	}
	reviewRun, created, err := r.ensureCompletionReviewRun(ctx, request, reviewerID)
	if err != nil {
		return err
	}
	if created {
		result.CompletionReviewsCreated++
	} else {
		result.CompletionReviewsReused++
	}
	if isTerminalAgentRunStatus(reviewRun.Status) {
		applied, resolveErr := r.ResolveCompletionReviewRun(ctx, reviewRun)
		if resolveErr != nil {
			return resolveErr
		}
		if applied {
			result.CompletionReviewsApplied++
		}
	}
	return nil
}

func (r *AgentRequestInboxReconciler) ensureCompletionReviewRun(
	ctx context.Context,
	request *AgentRequest,
	reviewerID string,
) (*AgentRun, bool, error) {
	source, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.SourceRunID)
	if err != nil {
		return nil, false, err
	}
	child, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.ChildRunID)
	if err != nil {
		return nil, false, err
	}
	if source == nil || child == nil || child.Status != AgentRunStatusCompleted {
		return nil, false, ErrInvalidAgentRequestState
	}
	existing, err := r.findCompletionReviewRun(ctx, request, source, reviewerID)
	if err != nil || existing != nil {
		return existing, false, err
	}
	review := map[string]interface{}{
		"requestId": request.ID, "requestRevision": request.Revision,
		"requester": request.Requester, "recipient": request.Recipient,
		"workerAgentId": request.AssignedAgentID, "goal": request.Goal,
		"instructions": request.Instructions, "acceptanceCriteria": cloneMap(request.AcceptanceCriteria),
		"artifactRequirements": cloneArtifactRequirements(request.ArtifactRequirements),
		"completionSummary":    request.CompletionSummary, "acceptanceEvidence": cloneMap(request.AcceptanceEvidence),
		"artifacts": request.Artifacts, "output": cloneMap(child.Output),
		"conversationRefs": append([]string(nil), request.ConversationRefs...),
	}
	contextValue := map[string]interface{}{AgentRequestCompletionReviewContextKey: review}
	if projectID, _ := source.Context["projectId"].(string); strings.TrimSpace(projectID) != "" {
		contextValue["projectId"] = strings.TrimSpace(projectID)
	}
	key := fmt.Sprintf("agent-request-completion-review:%s:%d:%s", request.ID, request.Revision, reviewerID)
	created, err := r.commands.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: request.Scope, Kind: RunKindAgentWork, ParentRunID: source.ID,
		Owner:           ObjectiveOwner{Type: request.Requester.Type, ID: request.Requester.ID},
		AssignedAgentID: reviewerID, ConcurrencyKey: "agent-request-completion-review:" + request.ID,
		Goal:   "Independently review completed delegated work: " + request.Goal,
		Source: RunSourceCompletionReview, Priority: source.Priority, Context: contextValue,
		Budget: &BudgetPolicy{
			MaxAttempts: 5, MaxTurns: 3, MaxInputTokens: 64000, MaxOutputTokens: 4096,
			MaxTotalTokens: 68096, MaxDurationMS: 180000,
		},
		IdempotencyKey: key, Actor: ActivityActor{Type: "system", ID: "agent-request-inbox"},
		Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		if errors.Is(err, ErrRunIdempotency) {
			existing, findErr := r.findCompletionReviewRun(ctx, request, source, reviewerID)
			if findErr != nil {
				return nil, false, findErr
			}
			if existing != nil {
				return existing, false, nil
			}
		}
		return nil, false, err
	}
	return created.Run, created.Event != nil, nil
}

func (r *AgentRequestInboxReconciler) findCompletionReviewRun(
	ctx context.Context,
	request *AgentRequest,
	source *AgentRun,
	reviewerID string,
) (*AgentRun, error) {
	const pageSize = 100
	var matched *AgentRun
	for offset := 0; ; offset += pageSize {
		runs, err := r.collaboration.runs.ListAgentRuns(ctx, AgentRunFilter{
			Scope: request.Scope, ParentRunID: source.ID, AssignedAgentID: reviewerID, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, candidate := range runs {
			if !completionReviewRunMatchesRequest(candidate, request, reviewerID) {
				continue
			}
			if matched != nil && matched.ID != candidate.ID {
				return nil, errors.New("AgentRequest has multiple durable completion review Runs")
			}
			matched = candidate
		}
		if len(runs) < pageSize {
			return matched, nil
		}
	}
}

func completionReviewRunMatchesRequest(run *AgentRun, request *AgentRequest, reviewerID string) bool {
	if run == nil || request == nil || run.Source != RunSourceCompletionReview ||
		run.Scope != request.Scope || run.ParentRunID != request.SourceRunID ||
		run.Owner != (ObjectiveOwner{Type: request.Requester.Type, ID: request.Requester.ID}) ||
		run.AssignedAgentID != reviewerID || run.ConcurrencyKey != "agent-request-completion-review:"+request.ID {
		return false
	}
	review, ok := run.Context[AgentRequestCompletionReviewContextKey].(map[string]interface{})
	if !ok {
		return false
	}
	requestID, _ := review["requestId"].(string)
	requestRevision, err := positiveInt64(review["requestRevision"])
	return strings.TrimSpace(requestID) == request.ID && err == nil && requestRevision == request.Revision
}

func (r *AgentRequestInboxReconciler) ensureDecisionRun(ctx context.Context, request *AgentRequest, assignedAgentID string) (*AgentRun, bool, bool, error) {
	source, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.SourceRunID)
	if err != nil {
		return nil, false, false, err
	}
	if source == nil {
		return nil, false, false, ErrRunNotFound
	}
	existing, err := r.findDecisionRun(ctx, request, source, assignedAgentID)
	if err != nil {
		return nil, false, false, err
	}
	if existing != nil {
		return existing, false, false, nil
	}
	inbox := map[string]interface{}{
		"requestId": request.ID, "requestRevision": request.Revision, "kind": request.Kind,
		"requester": request.Requester, "recipient": request.Recipient, "goal": request.Goal,
		"instructions": request.Instructions, "semanticRole": request.SemanticRole,
		"clarificationQuestion": request.Clarification, "clarificationResponse": request.Response,
		"acceptanceCriteria":   cloneMap(request.AcceptanceCriteria),
		"artifactRequirements": cloneArtifactRequirements(request.ArtifactRequirements),
		"sharedContext":        cloneMap(request.SharedContext), "conversationRefs": append([]string(nil), request.ConversationRefs...),
	}
	contextValue := map[string]interface{}{AgentRequestInboxContextKey: inbox}
	if projectID, _ := source.Context["projectId"].(string); strings.TrimSpace(projectID) != "" {
		contextValue["projectId"] = strings.TrimSpace(projectID)
	}
	key := fmt.Sprintf("agent-request-decision:%s:%d:%s", request.ID, request.Revision, assignedAgentID)
	created, err := r.commands.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: request.Scope, Kind: RunKindAgentWork, ParentRunID: source.ID,
		Owner:           ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID},
		AssignedAgentID: assignedAgentID, ConcurrencyKey: "agent-request:" + request.ID,
		Goal: "Evaluate incoming work request: " + request.Goal, Source: RunSourceRequestDecision,
		Priority: source.Priority, Context: contextValue,
		Budget: &BudgetPolicy{
			MaxAttempts: 5, MaxTurns: 3, MaxInputTokens: 48000, MaxOutputTokens: 4096,
			MaxTotalTokens: 52096, MaxDurationMS: 180000,
		},
		IdempotencyKey: key, Actor: ActivityActor{Type: "system", ID: "agent-request-inbox"},
		Visibility: ActivityVisibilityTeam,
	})
	if err != nil {
		if errors.Is(err, ErrRunIdempotency) {
			existing, findErr := r.findDecisionRun(ctx, request, source, assignedAgentID)
			if findErr != nil {
				return nil, false, false, findErr
			}
			if existing != nil {
				return existing, false, false, nil
			}
			resolved, resolveErr := r.failDecisionReview(ctx, request, source, nil, AgentRequestStatusFailed,
				"AgentRequest review could not recover its durable decision Run.")
			if resolveErr != nil {
				return nil, false, false, errors.Join(err, resolveErr)
			}
			return nil, false, resolved, nil
		}
		return nil, false, false, err
	}
	return created.Run, created.Event != nil, false, nil
}

func (r *AgentRequestInboxReconciler) findDecisionRun(ctx context.Context, request *AgentRequest, source *AgentRun, assignedAgentID string) (*AgentRun, error) {
	const pageSize = 100
	var matched *AgentRun
	for offset := 0; ; offset += pageSize {
		runs, err := r.collaboration.runs.ListAgentRuns(ctx, AgentRunFilter{
			Scope: request.Scope, ParentRunID: source.ID, AssignedAgentID: assignedAgentID, Limit: pageSize, Offset: offset,
		})
		if err != nil {
			return nil, err
		}
		for _, candidate := range runs {
			if !decisionRunMatchesRequest(candidate, request, assignedAgentID) {
				continue
			}
			if matched != nil && matched.ID != candidate.ID {
				return nil, errors.New("AgentRequest has multiple durable decision Runs")
			}
			matched = candidate
		}
		if len(runs) < pageSize {
			return matched, nil
		}
	}
}

func decisionRunMatchesRequest(run *AgentRun, request *AgentRequest, assignedAgentID string) bool {
	if run == nil || request == nil || run.Source != RunSourceRequestDecision ||
		run.Scope != request.Scope || run.ParentRunID != request.SourceRunID ||
		run.Owner != (ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID}) ||
		run.AssignedAgentID != assignedAgentID || run.ConcurrencyKey != "agent-request:"+request.ID {
		return false
	}
	inbox, ok := run.Context[AgentRequestInboxContextKey].(map[string]interface{})
	if !ok {
		return false
	}
	requestID, _ := inbox["requestId"].(string)
	requestRevision, err := positiveInt64(inbox["requestRevision"])
	return strings.TrimSpace(requestID) == request.ID && err == nil && requestRevision == request.Revision
}

func (r *AgentRequestInboxReconciler) ResolveDecisionRun(ctx context.Context, run *AgentRun) (bool, error) {
	if r == nil || r.collaboration == nil || run == nil || run.Source != RunSourceRequestDecision || !isTerminalAgentRunStatus(run.Status) {
		return false, nil
	}
	inbox, ok := run.Context[AgentRequestInboxContextKey].(map[string]interface{})
	if !ok {
		return false, errors.New("AgentRequest decision Run has no durable inbox context")
	}
	requestID, _ := inbox["requestId"].(string)
	requestID = strings.TrimSpace(requestID)
	requestRevision, err := positiveInt64(inbox["requestRevision"])
	if requestID == "" || err != nil {
		return false, errors.New("AgentRequest decision Run has invalid request identity")
	}
	request, err := r.collaboration.GetAgentRequest(ctx, run.Scope, requestID)
	if err != nil {
		return false, err
	}
	if request.Status != AgentRequestStatusPending || request.Revision != requestRevision {
		return false, nil
	}
	if run.Status == AgentRunStatusFailed || run.Status == AgentRunStatusCanceled {
		status := AgentRequestStatusFailed
		if run.Status == AgentRunStatusCanceled {
			status = AgentRequestStatusCanceled
		}
		source, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.SourceRunID)
		if err != nil {
			return false, err
		}
		reason := strings.TrimSpace(run.Error)
		if reason == "" {
			reason = "AgentRequest review Run " + string(run.Status) + "."
		}
		return r.failDecisionReview(ctx, request, source, run, status, reason)
	}
	decision, message, err := parseAgentRequestDecisionOutput(run.Output)
	if err != nil {
		return false, err
	}
	actor := CollaborationParty{Type: OwnerTypeAgent, ID: run.AssignedAgentID}
	response := RespondAgentRequestRequest{
		Scope: run.Scope, RequestID: request.ID, ExpectedRevision: request.Revision,
		Decision: decision, Principal: request.Recipient, Actor: actor, Message: message,
		DecisionRunID: run.ID,
	}
	if decision == AgentRequestDecisionAccept {
		response.AssignedAgentID = run.AssignedAgentID
	}
	_, err = r.collaboration.RespondAgentRequest(ctx, response)
	if errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrInvalidAgentRequestState) {
		return false, nil
	}
	return err == nil, err
}

func (r *AgentRequestInboxReconciler) ResolveCompletionReviewRun(ctx context.Context, run *AgentRun) (bool, error) {
	if r == nil || r.collaboration == nil || run == nil || run.Source != RunSourceCompletionReview || !isTerminalAgentRunStatus(run.Status) {
		return false, nil
	}
	review, ok := run.Context[AgentRequestCompletionReviewContextKey].(map[string]interface{})
	if !ok {
		return false, errors.New("AgentRequest completion review Run has no durable review context")
	}
	requestID, _ := review["requestId"].(string)
	requestID = strings.TrimSpace(requestID)
	requestRevision, err := positiveInt64(review["requestRevision"])
	if requestID == "" || err != nil {
		return false, errors.New("AgentRequest completion review Run has invalid request identity")
	}
	request, err := r.collaboration.GetAgentRequest(ctx, run.Scope, requestID)
	if err != nil {
		return false, err
	}
	if request.Status != AgentRequestStatusCompletionReview || request.Revision != requestRevision {
		return false, nil
	}
	approve := false
	message := strings.TrimSpace(run.Error)
	if run.Status == AgentRunStatusCompleted {
		approve, message, err = parseAgentRequestCompletionReviewOutput(run.Output)
		if err != nil {
			return false, err
		}
	} else if message == "" {
		message = "Independent completion review did not complete."
	}
	child, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.ChildRunID)
	if err != nil {
		return false, err
	}
	if child == nil {
		return false, ErrRunNotFound
	}
	_, err = r.collaboration.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: run.Scope, RequestID: request.ID, ExpectedRevision: request.Revision,
		ExpectedChildRevision: child.Revision, Principal: request.Requester,
		Actor:   CollaborationParty{Type: OwnerTypeAgent, ID: run.AssignedAgentID},
		Approve: approve, Summary: message,
		IdempotencyKey: fmt.Sprintf("agent-request-completion-review:%s:%s", request.ID, run.ID),
	})
	if errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrInvalidAgentRequestState) {
		return false, nil
	}
	return err == nil, err
}

func (r *AgentRequestInboxReconciler) failDecisionReview(
	ctx context.Context,
	request *AgentRequest,
	source *AgentRun,
	decisionRun *AgentRun,
	status AgentRequestStatus,
	reason string,
) (bool, error) {
	if request == nil || source == nil || request.Status != AgentRequestStatusPending ||
		(status != AgentRequestStatusFailed && status != AgentRequestStatusCanceled) {
		return false, ErrInvalidAgentRequestState
	}
	now := r.collaboration.now()
	updated := cloneAgentRequest(request)
	updated.Status = status
	updated.ResolutionReason = strings.TrimSpace(reason)
	if updated.ResolutionReason == "" {
		updated.ResolutionReason = "AgentRequest review did not complete."
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.ResolvedAt = &now
	record := AgentRequestResponseRecord{Request: updated, ExpectedRequestRevision: request.Revision}
	actor := CollaborationParty{Type: OwnerTypeAgent, ID: request.Recipient.ID}
	if decisionRun != nil && strings.TrimSpace(decisionRun.AssignedAgentID) != "" {
		actor.ID = decisionRun.AssignedAgentID
	}
	if request.DependencyGroupID != "" {
		dependency, err := r.collaboration.groupedRequestDependency(ctx, request, source, true)
		if err != nil {
			return false, err
		}
		state := RunDependencyStateFailed
		if status == AgentRequestStatusCanceled {
			state = RunDependencyStateCanceled
		}
		record.DependencyResolution = &RunDependencyResolutionRecord{
			Scope: request.Scope, GroupID: request.DependencyGroupID, DependencyID: request.DependencyID,
			ExpectedDependencyRevision: dependency.Revision, State: state, Error: updated.ResolutionReason,
			Actor: ActivityActor{Type: string(actor.Type), ID: actor.ID}, Visibility: ActivityVisibilityTeam, OccurredAt: now,
		}
	} else if sourceWaitsForAgentRequestDecision(source, request.ID) {
		resumed, err := sourceResumingAfterAgentRequestDecision(source, updated, now)
		if err != nil {
			return false, err
		}
		resumed.Output = collaborationResolutionOutput(resumed.Output, updated)
		record.SourceRun = resumed
		record.ExpectedSourceRevision = source.Revision
	}
	eventType := "collaboration.review_failed"
	if status == AgentRequestStatusCanceled {
		eventType = "collaboration.review_canceled"
	}
	if request.Kind == AgentRequestKindHandoff {
		eventType = "handoff.review_failed"
		if status == AgentRequestStatusCanceled {
			eventType = "handoff.review_canceled"
		}
	} else if request.Kind == AgentRequestKindEscalation {
		eventType = "escalation.review_failed"
		if status == AgentRequestStatusCanceled {
			eventType = "escalation.review_canceled"
		}
	}
	summary := fmt.Sprintf("%s %s could not complete request review: %s", actor.Type, actor.ID, updated.ResolutionReason)
	record.SourceEvent = collaborationEvent(source, updated, eventType, summary, actor, now)
	if decisionRun != nil {
		linkAgentRequestDecisionRun(record.SourceEvent, decisionRun.ID)
	}
	if _, err := r.collaboration.store.RespondAgentRequest(ctx, record); err != nil {
		if errors.Is(err, ErrRevisionConflict) || errors.Is(err, ErrInvalidAgentRequestState) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func parseAgentRequestDecisionOutput(output map[string]interface{}) (AgentRequestDecision, string, error) {
	raw, ok := output[AgentRequestDecisionOutputKey].(map[string]interface{})
	if !ok {
		return "", "", errors.New("AgentRequest decision output is missing")
	}
	decisionValue, _ := raw["decision"].(string)
	message, _ := raw["message"].(string)
	decision := AgentRequestDecision(strings.TrimSpace(decisionValue))
	message = strings.TrimSpace(message)
	switch decision {
	case AgentRequestDecisionAccept, AgentRequestDecisionReject:
	case AgentRequestDecisionRequestClarification:
		if message == "" {
			return "", "", errors.New("AgentRequest clarification decision requires one concrete question")
		}
	default:
		return "", "", errors.New("AgentRequest decision output is invalid")
	}
	return decision, message, nil
}

func parseAgentRequestCompletionReviewOutput(output map[string]interface{}) (bool, string, error) {
	raw, ok := output[AgentRequestCompletionReviewOutputKey].(map[string]interface{})
	if !ok {
		return false, "", errors.New("AgentRequest completion review output is missing")
	}
	decision, _ := raw["decision"].(string)
	message, _ := raw["message"].(string)
	decision = strings.TrimSpace(decision)
	message = strings.TrimSpace(message)
	if message == "" {
		return false, "", errors.New("AgentRequest completion review requires an evidence-based reason")
	}
	switch decision {
	case "approve":
		return true, message, nil
	case "reject":
		return false, message, nil
	default:
		return false, "", errors.New("AgentRequest completion review decision is invalid")
	}
}

func positiveInt64(value interface{}) (int64, error) {
	switch typed := value.(type) {
	case int64:
		if typed > 0 {
			return typed, nil
		}
	case int:
		if typed > 0 {
			return int64(typed), nil
		}
	case float64:
		if typed > 0 && typed == float64(int64(typed)) {
			return int64(typed), nil
		}
	}
	return 0, errors.New("positive integer is required")
}
