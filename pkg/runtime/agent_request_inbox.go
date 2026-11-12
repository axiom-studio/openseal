package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const (
	// AgentRequestDecisionOutputKey is the only model output consumed as an
	// AgentRequest lifecycle decision.
	AgentRequestDecisionOutputKey = "agentRequestDecision"
	// AgentRequestCompletionReviewOutputKey is the only model output consumed
	// to resolve an independent completion-review checkpoint.
	AgentRequestCompletionReviewOutputKey = "agentRequestCompletionReviewDecision"
)

const AgentRequestDecisionSystemInstruction = `Evaluate the incoming AgentRequest before doing any requested work. Decide whether the request is relevant, sufficiently clear, safe, and within your role. Do not execute Skills, delegate, fork, or perform the requested work during this review. Finish with agent_request_decision: accept or reject with a concise reason, or request_clarification with one concrete question.`

const AgentRequestCompletionReviewSystemInstruction = `Independently review the completed delegated work against its goal, acceptance criteria, required artifacts, evidence, and output. Do not execute Skills, invoke runbooks, delegate, fork, or modify the work during this review. Finish with agent_request_review: approve or reject with a concise evidence-based reason.`

const AcceptedAgentRequestExecutionSystemInstruction = `This Run is the execution child of an AgentRequest that has already passed intake and was accepted. Perform the requested goal now using the authorized Skills and durable work primitives available to this Run. Do not evaluate or accept the request again.`

const agentRequestConversationProjectionInterval = 5 * time.Second

type agentRequestTeamDefinitionStore interface {
	GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error)
}

type agentRequestInboxConversationStore interface {
	models.AgentRequestInboxStore
	models.ConversationStore
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

// AgentRequestInboxReconciler converts pending recipient work into either a
// policy-authorized acceptance or a bounded decision Run. Stable run
// idempotency makes request review recoverable across process restarts without
// leasing Agent identity to a human caller.
type AgentRequestInboxReconciler struct {
	collaboration      *CollaborationService
	commands           *agentrun.RunCommandService
	projector          *AgentRequestConversationProjector
	projectionMu       sync.Mutex
	lastProjection     time.Time
	projectionDeferred bool
	now                func() time.Time
}

func NewAgentRequestInboxReconciler(store models.AgentRequestInboxStore, opts ...models.ServiceOption) (*AgentRequestInboxReconciler, error) {
	if store == nil {
		return nil, errors.New("AgentRequest inbox store is required")
	}
	reconciler := &AgentRequestInboxReconciler{
		collaboration: NewCollaborationService(store, opts...),
		commands:      agentrun.NewRunCommandService(store, opts...),
		now:           models.ClockFrom(opts),
	}
	if conversationStore, ok := store.(agentRequestInboxConversationStore); ok {
		reconciler.projector, _ = NewAgentRequestConversationProjector(conversationStore, opts...)
	}
	return reconciler, nil
}

func (r *AgentRequestInboxReconciler) Reconcile(ctx context.Context, scope models.Scope, assignedAgentID string) (*AgentRequestInboxReconcileResult, error) {
	return r.reconcile(ctx, scope, assignedAgentID, true)
}

// ReconcileWorker keeps durable request intake responsive while bounding the
// independently idempotent conversation projection work.
func (r *AgentRequestInboxReconciler) ReconcileWorker(ctx context.Context, scope models.Scope, assignedAgentID string) (*AgentRequestInboxReconcileResult, error) {
	projectConversations := false
	if r != nil {
		now := time.Now
		if r.now != nil {
			now = r.now
		}
		projectConversations = r.conversationProjectionDue(now())
	}
	return r.reconcile(ctx, scope, assignedAgentID, projectConversations)
}

func (r *AgentRequestInboxReconciler) reconcile(ctx context.Context, scope models.Scope, assignedAgentID string, projectConversations bool) (*AgentRequestInboxReconcileResult, error) {
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
	if projectConversations && r.projector != nil {
		projected, projectionErr := r.projector.Reconcile(ctx, scope)
		result.ConversationProjections = projected
		if projectionErr != nil {
			failures = append(failures, projectionErr)
		}
	}
	for _, request := range requests {
		if request.Status == models.AgentRequestStatusCompletionReview {
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
		actor := models.CollaborationParty{Type: models.OwnerTypeAgent, ID: assigned}
		if request.AcceptancePolicy == models.AgentRequestAcceptancePreauthorized || !requireReview {
			if _, responseErr := r.collaboration.RespondAgentRequest(ctx, RespondAgentRequestRequest{
				Scope: scope, RequestID: request.ID, ExpectedRevision: request.Revision,
				Decision: AgentRequestDecisionAccept, Principal: request.Recipient, Actor: actor,
				AssignedAgentID: assigned, Message: "Accepted under the configured recipient policy.",
			}); responseErr != nil && !errors.Is(responseErr, models.ErrRevisionConflict) && !errors.Is(responseErr, models.ErrInvalidAgentRequestState) {
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
		if models.IsTerminalAgentRunStatus(decisionRun.Status) {
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

func (r *AgentRequestInboxReconciler) conversationProjectionDue(now time.Time) bool {
	if r == nil {
		return false
	}
	r.projectionMu.Lock()
	defer r.projectionMu.Unlock()
	if !r.lastProjection.IsZero() && now.Sub(r.lastProjection) < agentRequestConversationProjectionInterval {
		r.projectionDeferred = true
		return false
	}
	r.lastProjection = now
	r.projectionDeferred = false
	return true
}

// DeferredProjectionAt reports when a projection skipped by throttling is
// due, so signal-driven workers can schedule it instead of polling for it.
func (r *AgentRequestInboxReconciler) DeferredProjectionAt() *time.Time {
	if r == nil {
		return nil
	}
	r.projectionMu.Lock()
	defer r.projectionMu.Unlock()
	if !r.projectionDeferred {
		return nil
	}
	return new(r.lastProjection.Add(agentRequestConversationProjectionInterval))
}

func (r *AgentRequestInboxReconciler) pendingRequests(ctx context.Context, scope models.Scope) ([]*models.AgentRequest, error) {
	const pageSize = 100
	result := make([]*models.AgentRequest, 0)
	for offset := 0; ; offset += pageSize {
		page, err := r.collaboration.ListAgentRequests(ctx, models.AgentRequestFilter{
			Scope: scope, Statuses: []models.AgentRequestStatus{models.AgentRequestStatusPending, models.AgentRequestStatusCompletionReview}, Limit: pageSize, Offset: offset,
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

func (r *AgentRequestInboxReconciler) recipientAssignment(ctx context.Context, request *models.AgentRequest) (string, bool, error) {
	if request == nil {
		return "", false, models.ErrAgentRequestNotFound
	}
	if request.Recipient.Type == models.OwnerTypeAgent {
		return request.Recipient.ID, true, nil
	}
	assigned, definition, err := r.teamPartyAssignment(ctx, request.Scope, request.Recipient, request.SemanticRole, "", false)
	if err != nil {
		return "", false, err
	}
	return assigned, definition.Delegation.RequireAcceptance, nil
}

func (r *AgentRequestInboxReconciler) completionReviewerAssignment(ctx context.Context, request *models.AgentRequest) (string, error) {
	reviewers, err := r.completionReviewerAssignments(ctx, request)
	if err != nil {
		return "", err
	}
	return reviewers[0], nil
}

func (r *AgentRequestInboxReconciler) completionReviewerAssignments(ctx context.Context, request *models.AgentRequest) ([]string, error) {
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	if request.Requester.Type == models.OwnerTypeAgent {
		if completionReviewQuorum(request.DelegationPolicy) <= 1 && (request.DelegationPolicy == nil || !request.DelegationPolicy.EscalateOnDisagreement) {
			if request.Requester.ID == request.AssignedAgentID {
				return nil, fmt.Errorf("%w: completed work cannot be reviewed by its worker", models.ErrAgentRequestAssignment)
			}
			return []string{request.Requester.ID}, nil
		}
		if request.DelegationPolicy == nil || strings.TrimSpace(request.DelegationPolicy.TeamDeploymentID) == "" {
			return nil, fmt.Errorf("%w: completed work cannot be reviewed by its worker", models.ErrAgentRequestAssignment)
		}
		requesterTeam := models.CollaborationParty{Type: models.OwnerTypeTeam, ID: request.DelegationPolicy.TeamDeploymentID}
		return r.teamCompletionReviewerAssignments(ctx, request, requesterTeam)
	}
	return r.teamCompletionReviewerAssignments(ctx, request, request.Requester)
}

func (r *AgentRequestInboxReconciler) teamCompletionReviewerAssignments(ctx context.Context, request *models.AgentRequest, team models.CollaborationParty) ([]string, error) {
	eligible, _, err := r.teamPartyAssignments(ctx, request.Scope, team, "", request.AssignedAgentID, true)
	if err != nil {
		return nil, err
	}
	wanted := completionReviewQuorum(request.DelegationPolicy)
	if request.ReviewDisagreement {
		wanted++
	}
	if wanted < 1 {
		wanted = 1
	}
	if len(eligible) < wanted {
		return nil, fmt.Errorf("%w: Team has %d independent reviewers but policy requires %d", models.ErrAgentRequestAssignment, len(eligible), wanted)
	}
	return eligible[:wanted], nil
}

func (r *AgentRequestInboxReconciler) teamPartyAssignment(
	ctx context.Context,
	scope models.Scope,
	party models.CollaborationParty,
	semanticRole string,
	excludedAgentID string,
	useApprovers bool,
) (string, *kernelteam.Definition, error) {
	eligible, definition, err := r.teamPartyAssignments(ctx, scope, party, semanticRole, excludedAgentID, useApprovers)
	if err != nil {
		return "", nil, err
	}
	return eligible[0], definition, nil
}

func (r *AgentRequestInboxReconciler) teamPartyAssignments(
	ctx context.Context,
	scope models.Scope,
	party models.CollaborationParty,
	semanticRole string,
	excludedAgentID string,
	useApprovers bool,
) ([]string, *kernelteam.Definition, error) {
	if r.collaboration.teams == nil {
		return nil, nil, fmt.Errorf("%w: Team roster assignment is unavailable", models.ErrAgentRequestAssignment)
	}
	deployment, err := r.collaboration.teams.GetTeamDeployment(ctx, capability.ScopeReference{
		Kind: scope.Kind, ID: scope.ID,
	}, party.ID)
	if err != nil {
		return nil, nil, err
	}
	if deployment == nil || deployment.Status != kernelteam.DeploymentActive {
		return nil, nil, fmt.Errorf("%w: Team deployment is not active", models.ErrAgentRequestAssignment)
	}
	definitions, ok := r.collaboration.store.(agentRequestTeamDefinitionStore)
	if !ok {
		return nil, nil, fmt.Errorf("%w: Team delegation policy is unavailable", models.ErrAgentRequestAssignment)
	}
	definition, err := definitions.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, nil, err
	}
	if definition == nil {
		return nil, nil, fmt.Errorf("%w: active Team definition is unavailable", models.ErrAgentRequestAssignment)
	}
	// With no member marked as an approver, every member may review.
	approversOnly := useApprovers && slices.ContainsFunc(deployment.Roster, func(assignment kernelteam.RosterAssignment) bool { return assignment.Approver })
	eligible := make([]kernelteam.RosterAssignment, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		if (semanticRole == "" || assignment.ID == semanticRole) &&
			(!approversOnly || assignment.Approver) && assignment.AgentDeploymentID != excludedAgentID {
			eligible = append(eligible, assignment)
		}
	}
	if len(eligible) == 0 {
		return nil, nil, fmt.Errorf("%w: Team has no independent Agent eligible for review as member %q", models.ErrAgentRequestAssignment, semanticRole)
	}
	sort.Slice(eligible, func(i, j int) bool {
		if eligible[i].AgentDeploymentID != eligible[j].AgentDeploymentID {
			return eligible[i].AgentDeploymentID < eligible[j].AgentDeploymentID
		}
		return eligible[i].ID < eligible[j].ID
	})
	result := make([]string, len(eligible))
	for index := range eligible {
		result[index] = eligible[index].AgentDeploymentID
	}
	return result, definition, nil
}

func (r *AgentRequestInboxReconciler) reconcileCompletionReview(
	ctx context.Context,
	request *models.AgentRequest,
	assignedAgentFilter string,
	result *AgentRequestInboxReconcileResult,
) error {
	reviewerIDs, err := r.completionReviewerAssignments(ctx, request)
	if err != nil {
		return err
	}
	decided := make(map[string]struct{}, len(request.CompletionReviews))
	for _, decision := range request.CompletionReviews {
		decided[decision.Reviewer.ID] = struct{}{}
	}
	for _, reviewerID := range reviewerIDs {
		if _, exists := decided[reviewerID]; exists {
			continue
		}
		if filter := strings.TrimSpace(assignedAgentFilter); filter != "" && filter != reviewerID {
			continue
		}
		reviewRun, created, ensureErr := r.ensureCompletionReviewRun(ctx, request, reviewerID)
		if ensureErr != nil {
			return ensureErr
		}
		if created {
			result.CompletionReviewsCreated++
		} else {
			result.CompletionReviewsReused++
		}
		if models.IsTerminalAgentRunStatus(reviewRun.Status) {
			applied, resolveErr := r.ResolveCompletionReviewRun(ctx, reviewRun)
			if resolveErr != nil {
				return resolveErr
			}
			if applied {
				result.CompletionReviewsApplied++
			}
		}
	}
	return nil
}

func (r *AgentRequestInboxReconciler) ensureCompletionReviewRun(
	ctx context.Context,
	request *models.AgentRequest,
	reviewerID string,
) (*models.AgentRun, bool, error) {
	source, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.SourceRunID)
	if err != nil {
		return nil, false, err
	}
	child, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.ChildRunID)
	if err != nil {
		return nil, false, err
	}
	if source == nil || child == nil || child.Status != models.AgentRunStatusCompleted {
		return nil, false, models.ErrInvalidAgentRequestState
	}
	existing, err := r.findCompletionReviewRun(ctx, request, source, reviewerID)
	if err != nil || existing != nil {
		return existing, false, err
	}
	review := map[string]interface{}{
		"requestId": request.ID, "completionKey": request.CompletionKey,
		"requester": request.Requester, "recipient": request.Recipient,
		"workerAgentId": request.AssignedAgentID, "goal": request.Goal,
		"instructions": request.Instructions, "acceptanceCriteria": maps.Clone(request.AcceptanceCriteria),
		"artifactRequirements": models.CloneArtifactRequirements(request.ArtifactRequirements),
		"completionSummary":    request.CompletionSummary, "acceptanceEvidence": maps.Clone(request.AcceptanceEvidence),
		"artifacts": request.Artifacts, "output": maps.Clone(child.Output),
		"conversationRefs": append([]string(nil), request.ConversationRefs...),
	}
	contextValue := map[string]interface{}{agentrun.AgentRequestCompletionReviewContextKey: review}
	key := fmt.Sprintf("agent-request-completion-review:%s:%s:%s", request.ID, request.CompletionKey, reviewerID)
	created, err := r.commands.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: request.Scope, Kind: models.RunKindAgentWork, ParentRunID: source.ID,
		Owner:           models.ObjectiveOwner{Type: request.Requester.Type, ID: request.Requester.ID},
		AssignedAgentID: reviewerID, ActingUser: source.ActingUser,
		ConcurrencyKey: "agent-request-completion-review:" + request.ID + ":" + reviewerID,
		Goal:           "Independently review completed delegated work: " + request.Goal,
		Source:         models.RunSourceCompletionReview, Priority: source.Priority, Context: contextValue,
		Budget: &models.BudgetPolicy{
			MaxAttempts: 5, MaxTurns: 12, MaxInputTokens: 256000, MaxOutputTokens: 4096,
			MaxTotalTokens: 272384, MaxDurationMS: 180000,
		},
		IdempotencyKey: key, Actor: models.ActivityActor{Type: "system", ID: "agent-request-inbox"},
		Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil {
		if errors.Is(err, models.ErrRunIdempotency) {
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
	request *models.AgentRequest,
	source *models.AgentRun,
	reviewerID string,
) (*models.AgentRun, error) {
	const pageSize = 100
	var matched *models.AgentRun
	for offset := 0; ; offset += pageSize {
		runs, err := r.collaboration.runs.ListAgentRuns(ctx, models.AgentRunFilter{
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

func completionReviewRunMatchesRequest(run *models.AgentRun, request *models.AgentRequest, reviewerID string) bool {
	if run == nil || request == nil || run.Source != models.RunSourceCompletionReview ||
		run.Scope != request.Scope || run.ParentRunID != request.SourceRunID ||
		run.Owner != (models.ObjectiveOwner{Type: request.Requester.Type, ID: request.Requester.ID}) ||
		run.AssignedAgentID != reviewerID || run.ConcurrencyKey != "agent-request-completion-review:"+request.ID+":"+reviewerID {
		return false
	}
	review, ok := run.Context[agentrun.AgentRequestCompletionReviewContextKey].(map[string]interface{})
	if !ok {
		return false
	}
	requestID, _ := review["requestId"].(string)
	completionKey, _ := review["completionKey"].(string)
	return strings.TrimSpace(requestID) == request.ID && strings.TrimSpace(completionKey) != "" && completionKey == request.CompletionKey
}

func (r *AgentRequestInboxReconciler) ensureDecisionRun(ctx context.Context, request *models.AgentRequest, assignedAgentID string) (*models.AgentRun, bool, bool, error) {
	source, err := r.collaboration.runs.GetAgentRun(ctx, request.Scope, request.SourceRunID)
	if err != nil {
		return nil, false, false, err
	}
	if source == nil {
		return nil, false, false, models.ErrRunNotFound
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
		"acceptanceCriteria":   maps.Clone(request.AcceptanceCriteria),
		"artifactRequirements": models.CloneArtifactRequirements(request.ArtifactRequirements),
		"sharedContext":        maps.Clone(request.SharedContext), "conversationRefs": append([]string(nil), request.ConversationRefs...),
	}
	contextValue := map[string]interface{}{agentrun.AgentRequestInboxContextKey: inbox}
	key := fmt.Sprintf("agent-request-decision:%s:%d:%s", request.ID, request.Revision, assignedAgentID)
	created, err := r.commands.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: request.Scope, Kind: models.RunKindAgentWork, ParentRunID: source.ID,
		Owner:           models.ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID},
		AssignedAgentID: assignedAgentID, ActingUser: source.ActingUser, ConcurrencyKey: "agent-request:" + request.ID,
		Goal: "Evaluate incoming work request: " + request.Goal, Source: models.RunSourceRequestDecision,
		Priority: source.Priority, Context: contextValue,
		Budget: &models.BudgetPolicy{
			MaxAttempts: 5, MaxTurns: 12, MaxInputTokens: 192000, MaxOutputTokens: 4096,
			MaxTotalTokens: 208384, MaxDurationMS: 180000,
		},
		IdempotencyKey: key, Actor: models.ActivityActor{Type: "system", ID: "agent-request-inbox"},
		Visibility: models.ActivityVisibilityTeam,
	})
	if err != nil {
		if errors.Is(err, models.ErrRunIdempotency) {
			existing, findErr := r.findDecisionRun(ctx, request, source, assignedAgentID)
			if findErr != nil {
				return nil, false, false, findErr
			}
			if existing != nil {
				return existing, false, false, nil
			}
			resolved, resolveErr := r.failDecisionReview(ctx, request, source, nil, models.AgentRequestStatusFailed,
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

func (r *AgentRequestInboxReconciler) findDecisionRun(ctx context.Context, request *models.AgentRequest, source *models.AgentRun, assignedAgentID string) (*models.AgentRun, error) {
	const pageSize = 100
	var matched *models.AgentRun
	for offset := 0; ; offset += pageSize {
		runs, err := r.collaboration.runs.ListAgentRuns(ctx, models.AgentRunFilter{
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

func decisionRunMatchesRequest(run *models.AgentRun, request *models.AgentRequest, assignedAgentID string) bool {
	if run == nil || request == nil || run.Source != models.RunSourceRequestDecision ||
		run.Scope != request.Scope || run.ParentRunID != request.SourceRunID ||
		run.Owner != (models.ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID}) ||
		run.AssignedAgentID != assignedAgentID || run.ConcurrencyKey != "agent-request:"+request.ID {
		return false
	}
	inbox, ok := run.Context[agentrun.AgentRequestInboxContextKey].(map[string]interface{})
	if !ok {
		return false
	}
	requestID, _ := inbox["requestId"].(string)
	requestRevision, err := positiveInt64(inbox["requestRevision"])
	return strings.TrimSpace(requestID) == request.ID && err == nil && requestRevision == request.Revision
}

func (r *AgentRequestInboxReconciler) ResolveDecisionRun(ctx context.Context, run *models.AgentRun) (bool, error) {
	if r == nil || r.collaboration == nil || run == nil || run.Source != models.RunSourceRequestDecision || !models.IsTerminalAgentRunStatus(run.Status) {
		return false, nil
	}
	inbox, ok := run.Context[agentrun.AgentRequestInboxContextKey].(map[string]interface{})
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
	if request.Status != models.AgentRequestStatusPending || request.Revision != requestRevision {
		return false, nil
	}
	if run.Status == models.AgentRunStatusFailed || run.Status == models.AgentRunStatusCanceled {
		status := models.AgentRequestStatusFailed
		if run.Status == models.AgentRunStatusCanceled {
			status = models.AgentRequestStatusCanceled
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
	decision, message, err := ParseAgentRequestDecisionOutput(run.Output)
	if err != nil {
		return false, err
	}
	actor := models.CollaborationParty{Type: models.OwnerTypeAgent, ID: run.AssignedAgentID}
	response := RespondAgentRequestRequest{
		Scope: run.Scope, RequestID: request.ID, ExpectedRevision: request.Revision,
		Decision: decision, Principal: request.Recipient, Actor: actor, Message: message,
		DecisionRunID: run.ID,
	}
	if decision == AgentRequestDecisionAccept {
		response.AssignedAgentID = run.AssignedAgentID
	}
	_, err = r.collaboration.RespondAgentRequest(ctx, response)
	if errors.Is(err, models.ErrRevisionConflict) || errors.Is(err, models.ErrInvalidAgentRequestState) {
		return false, nil
	}
	return err == nil, err
}

func (r *AgentRequestInboxReconciler) ResolveCompletionReviewRun(ctx context.Context, run *models.AgentRun) (bool, error) {
	if r == nil || r.collaboration == nil || run == nil || run.Source != models.RunSourceCompletionReview || !models.IsTerminalAgentRunStatus(run.Status) {
		return false, nil
	}
	review, ok := run.Context[agentrun.AgentRequestCompletionReviewContextKey].(map[string]interface{})
	if !ok {
		return false, errors.New("AgentRequest completion review Run has no durable review context")
	}
	requestID, _ := review["requestId"].(string)
	requestID = strings.TrimSpace(requestID)
	completionKey, _ := review["completionKey"].(string)
	completionKey = strings.TrimSpace(completionKey)
	if requestID == "" || completionKey == "" {
		return false, errors.New("AgentRequest completion review Run has invalid request identity")
	}
	request, err := r.collaboration.GetAgentRequest(ctx, run.Scope, requestID)
	if err != nil {
		return false, err
	}
	if request.Status != models.AgentRequestStatusCompletionReview || request.CompletionKey != completionKey {
		return false, nil
	}
	approve := false
	message := strings.TrimSpace(run.Error)
	if run.Status == models.AgentRunStatusCompleted {
		approve, message, err = ParseAgentRequestCompletionReviewOutput(run.Output)
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
		return false, models.ErrRunNotFound
	}
	_, err = r.collaboration.ReviewAgentRequestCompletion(ctx, ReviewAgentRequestCompletionRequest{
		Scope: run.Scope, RequestID: request.ID, ExpectedRevision: request.Revision,
		ExpectedChildRevision: child.Revision, Principal: request.Requester,
		Actor:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: run.AssignedAgentID},
		Approve: approve, ResolveDisagreement: request.ReviewDisagreement, Summary: message,
		IdempotencyKey: fmt.Sprintf("agent-request-completion-review:%s:%s", request.ID, run.ID),
	})
	if errors.Is(err, models.ErrRevisionConflict) || errors.Is(err, models.ErrInvalidAgentRequestState) {
		return false, nil
	}
	return err == nil, err
}

func (r *AgentRequestInboxReconciler) failDecisionReview(
	ctx context.Context,
	request *models.AgentRequest,
	source *models.AgentRun,
	decisionRun *models.AgentRun,
	status models.AgentRequestStatus,
	reason string,
) (bool, error) {
	if request == nil || source == nil || request.Status != models.AgentRequestStatusPending ||
		(status != models.AgentRequestStatusFailed && status != models.AgentRequestStatusCanceled) {
		return false, models.ErrInvalidAgentRequestState
	}
	now := r.collaboration.now()
	updated := models.CloneAgentRequest(request)
	updated.Status = status
	updated.ResolutionReason = strings.TrimSpace(reason)
	if updated.ResolutionReason == "" {
		updated.ResolutionReason = "AgentRequest review did not complete."
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.ResolvedAt = &now
	record := models.AgentRequestResponseRecord{Request: updated, ExpectedRequestRevision: request.Revision}
	actor := models.CollaborationParty{Type: models.OwnerTypeAgent, ID: request.Recipient.ID}
	if decisionRun != nil && strings.TrimSpace(decisionRun.AssignedAgentID) != "" {
		actor.ID = decisionRun.AssignedAgentID
	}
	if request.DependencyGroupID != "" {
		dependency, err := r.collaboration.groupedRequestDependency(ctx, request, source, true)
		if err != nil {
			return false, err
		}
		state := models.RunDependencyStateFailed
		if status == models.AgentRequestStatusCanceled {
			state = models.RunDependencyStateCanceled
		}
		record.DependencyResolution = &models.RunDependencyResolutionRecord{
			Scope: request.Scope, GroupID: request.DependencyGroupID, DependencyID: request.DependencyID,
			ExpectedDependencyRevision: dependency.Revision, State: state, Error: updated.ResolutionReason,
			Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID}, Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
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
	if status == models.AgentRequestStatusCanceled {
		eventType = "collaboration.review_canceled"
	}
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = "handoff.review_failed"
		if status == models.AgentRequestStatusCanceled {
			eventType = "handoff.review_canceled"
		}
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = "escalation.review_failed"
		if status == models.AgentRequestStatusCanceled {
			eventType = "escalation.review_canceled"
		}
	}
	summary := fmt.Sprintf("%s %s could not complete request review: %s", actor.Type, actor.ID, updated.ResolutionReason)
	record.SourceEvent = collaborationEvent(source, updated, eventType, summary, actor, now)
	if decisionRun != nil {
		linkAgentRequestDecisionRun(record.SourceEvent, decisionRun.ID)
	}
	if _, err := r.collaboration.store.RespondAgentRequest(ctx, record); err != nil {
		if errors.Is(err, models.ErrRevisionConflict) || errors.Is(err, models.ErrInvalidAgentRequestState) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func ParseAgentRequestDecisionOutput(output map[string]interface{}) (AgentRequestDecision, string, error) {
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

func ParseAgentRequestCompletionReviewOutput(output map[string]interface{}) (bool, string, error) {
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
