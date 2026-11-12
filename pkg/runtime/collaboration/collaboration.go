package collaboration

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/google/uuid"
)

var (
	ErrAgentRequestUnauthorized = errors.New("agent request principal is not authorized")
)

type AgentRequestDecision string

const (
	AgentRequestDecisionAccept               AgentRequestDecision = "accept"
	AgentRequestDecisionReject               AgentRequestDecision = "reject"
	AgentRequestDecisionRequestClarification AgentRequestDecision = "request_clarification"
	AgentRequestDecisionProvideClarification AgentRequestDecision = "provide_clarification"
)

func completionReviewQuorum(policy *models.AgentRequestDelegationPolicy) int {
	if policy == nil || !policy.RequireCompletionReview {
		return 0
	}
	if policy.CompletionReviewQuorum > 0 {
		return policy.CompletionReviewQuorum
	}
	return 1
}

type CreateAgentRequestRequest struct {
	ID                   string
	Scope                models.Scope
	Kind                 models.AgentRequestKind
	Requester            models.CollaborationParty
	Recipient            models.CollaborationParty
	SourceRunID          string
	Goal                 string
	Instructions         string
	SemanticRole         string
	AcceptanceCriteria   map[string]interface{}
	ArtifactRequirements []models.ArtifactRequirement
	SharedContext        map[string]interface{}
	ChildCheckpoint      map[string]interface{}
	AcceptancePolicy     models.AgentRequestAcceptancePolicy
	ConversationRefs     []string
	BudgetAllocation     *models.BudgetPolicy
	IdempotencyKey       string
	DependencyGroupID    string
	DependencyID         string
	// ToolTransition (singular requests only) moves the source Run's
	// delegating tool entry in the create transaction (CTL-6a).
	ToolTransition *models.ToolEntryTransitionRequest
	// RequireInterventionCount (singular requests only) refuses the create
	// with ErrSteeringArrived when the source gained an intervention since
	// the caller read it.
	RequireInterventionCount *int
}

type AgentRequestGroupSpec struct {
	ID                   string
	DependencyID         string
	Kind                 models.AgentRequestKind
	Recipient            models.CollaborationParty
	Goal                 string
	Instructions         string
	SemanticRole         string
	AcceptanceCriteria   map[string]interface{}
	ArtifactRequirements []models.ArtifactRequirement
	SharedContext        map[string]interface{}
	ChildCheckpoint      map[string]interface{}
	AcceptancePolicy     models.AgentRequestAcceptancePolicy
	ConversationRefs     []string
	BudgetAllocation     *models.BudgetPolicy
	Required             *bool
}

type CreateAgentRequestGroupRequest struct {
	ID                     string
	Scope                  models.Scope
	SourceRunID            string
	ExpectedSourceRevision int64
	Requester              models.CollaborationParty
	Policy                 models.RunDependencyPolicy
	Requests               []AgentRequestGroupSpec
	IdempotencyKey         string
	Actor                  models.ActivityActor
	Visibility             models.ActivityVisibility
}

type AgentRequestGroupResult struct {
	Group    *models.RunDependencyResult `json:"dependencyGroup"`
	Requests []*models.AgentRequest      `json:"requests"`
	Events   []*models.ActivityEvent     `json:"events,omitempty"`
}

type RespondAgentRequestRequest struct {
	Scope            models.Scope
	RequestID        string
	ExpectedRevision int64
	Decision         AgentRequestDecision
	Principal        models.CollaborationParty
	Actor            models.CollaborationParty
	AssignedAgentID  string
	Message          string
	DecisionRunID    string
}

type SubmitAgentRequestBidRequest struct {
	Scope             models.Scope
	RequestID         string
	ExpectedRevision  int64
	AgentDeploymentID string
	Decision          models.AgentRequestBidDecision
	Reason            string
	IdempotencyKey    string
}

type CompleteAgentRequestRequest struct {
	Scope                 models.Scope
	RequestID             string
	ExpectedRevision      int64
	ExpectedChildRevision int64
	Principal             models.CollaborationParty
	Actor                 models.CollaborationParty
	Summary               string
	AcceptanceEvidence    map[string]interface{}
	Artifacts             []models.ArtifactReference
	CompletionKey         string
}

type ReviewAgentRequestCompletionRequest struct {
	Scope                 models.Scope
	RequestID             string
	ExpectedRevision      int64
	ExpectedChildRevision int64
	Principal             models.CollaborationParty
	Actor                 models.CollaborationParty
	Approve               bool
	ResolveDisagreement   bool
	Summary               string
	IdempotencyKey        string
}

type AgentRequestResult struct {
	Request *models.AgentRequest    `json:"request"`
	Source  *models.AgentRun        `json:"sourceRun,omitempty"`
	Child   *models.AgentRun        `json:"childRun,omitempty"`
	Events  []*models.ActivityEvent `json:"events,omitempty"`
}

type collaborationAgentStore interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	GetDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
}

type Service struct {
	store             models.CollaborationStore
	runs              models.PortfolioStore
	artifacts         models.ArtifactStore
	dependencies      *portfolio.DependencyCoordinator
	teams             models.TeamRosterReader
	executionPreparer models.AcceptedRunExecutionPreparer
	now               func() time.Time
	newID             func() string
}

func NewService(store models.CollaborationKernelStore, opts ...models.ServiceOption) *Service {
	service := &Service{store: store, runs: store, artifacts: store, dependencies: portfolio.NewDependencyCoordinator(store, opts...), now: models.ClockFrom(opts), newID: uuid.NewString}
	service.teams = models.TeamRosterReaderOf(store)
	return service
}

func (s *Service) resolveSourceDelegationPolicy(ctx context.Context, source *models.AgentRun) (*models.AgentRequestDelegationPolicy, error) {
	if source == nil || source.Owner.Type != models.OwnerTypeTeam || s.teams == nil {
		return nil, nil
	}
	deployment, err := s.teams.GetTeamDeployment(ctx, capability.ScopeReference{Kind: source.Scope.Kind, ID: source.Scope.ID}, source.Owner.ID)
	if errors.Is(err, kernelteam.ErrDeploymentNotFound) {
		// Historical Runs may predate the Team registry. A currently deployed
		// Team always takes the strict, snapshotted path below.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if deployment == nil || deployment.Status != kernelteam.DeploymentActive {
		return nil, fmt.Errorf("%w: source Team deployment is not active", models.ErrAgentRequestAssignment)
	}
	definitions, ok := s.teams.(agentRequestTeamDefinitionStore)
	if !ok {
		return nil, fmt.Errorf("%w: source Team delegation policy is unavailable", models.ErrAgentRequestAssignment)
	}
	definition, err := definitions.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if definition == nil {
		return nil, fmt.Errorf("%w: source Team definition is unavailable", models.ErrAgentRequestAssignment)
	}
	depth, err := s.sourceDelegationDepth(ctx, source)
	if err != nil {
		return nil, err
	}
	return &models.AgentRequestDelegationPolicy{
		TeamDeploymentID: deployment.ID, TeamDefinitionID: definition.ID, TeamDefinitionVersion: definition.Version,
		DelegationDepth: depth + 1,
		MaximumDepth:    definition.Delegation.MaximumDepth, MaximumConcurrent: definition.Delegation.MaximumConcurrent,
		AllowPeerDelegation: definition.Delegation.AllowPeerDelegation, RequireAcceptance: definition.Delegation.RequireAcceptance,
		RequireCompletionReview: definition.Delegation.RequireCompletionReview, CompletionReviewQuorum: definition.Delegation.CompletionReviewQuorum,
		EscalateOnDisagreement: definition.Delegation.EscalateOnDisagreement,
	}, nil
}

func (s *Service) sourceDelegationDepth(ctx context.Context, source *models.AgentRun) (int, error) {
	depth := 0
	current := source
	for current != nil {
		if collaboration, ok := current.Context["collaboration"].(map[string]interface{}); ok {
			if requestID, _ := collaboration["requestId"].(string); strings.TrimSpace(requestID) != "" {
				depth++
			}
		}
		if strings.TrimSpace(current.ParentRunID) == "" {
			break
		}
		parent, err := s.runs.GetAgentRun(ctx, current.Scope, current.ParentRunID)
		if err != nil {
			return 0, err
		}
		if parent == nil {
			return 0, models.ErrRunNotFound
		}
		current = parent
	}
	return depth, nil
}

func (s *Service) validateSourceDelegationPolicy(ctx context.Context, req CreateAgentRequestRequest, policy *models.AgentRequestDelegationPolicy) error {
	if policy == nil {
		return nil
	}
	if policy.MaximumDepth > 0 && policy.DelegationDepth > policy.MaximumDepth {
		return fmt.Errorf("%w: Team delegation depth %d exceeds maximum %d", models.ErrAgentRequestAssignment, policy.DelegationDepth, policy.MaximumDepth)
	}
	if !policy.AllowPeerDelegation && req.Requester.Type == models.OwnerTypeAgent && req.Recipient.Type == models.OwnerTypeAgent && req.Requester.ID != req.Recipient.ID {
		return fmt.Errorf("%w: Team policy does not allow peer delegation", ErrAgentRequestUnauthorized)
	}
	return nil
}

// CanStartAgentRequestFromRun reports whether a Run can safely become the
// source of singular delegated work. Collaboration acceptance reserves the
// Run's one wake condition, so terminal, paused, sleeping, and already-waiting
// Runs must start a new Run before requesting or handing off more work.
func CanStartAgentRequestFromRun(run *models.AgentRun) bool {
	return models.CanEnterDependencyWait(run)
}

func validateAgentRequestSource(run *models.AgentRun) error {
	if CanStartAgentRequestFromRun(run) {
		return nil
	}
	status := models.AgentRunStatus("")
	if run != nil {
		status = run.Status
	}
	return fmt.Errorf("%w: source run cannot start collaboration from %s", models.ErrInvalidAgentRequestState, status)
}

func (s *Service) CreateAgentRequest(ctx context.Context, req CreateAgentRequestRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil || s.artifacts == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if !models.IsValidAgentRequestKind(req.Kind) {
		return nil, errors.New("agent request kind must be request, handoff, or escalation")
	}
	if err := req.Requester.Validate(); err != nil {
		return nil, fmt.Errorf("requester: %w", err)
	}
	if err := req.Recipient.Validate(); err != nil {
		return nil, fmt.Errorf("recipient: %w", err)
	}
	if strings.TrimSpace(req.SourceRunID) == "" || strings.TrimSpace(req.Goal) == "" {
		return nil, errors.New("source run and goal are required")
	}
	if err := models.ValidateCredentialFreeContext(req.SharedContext); err != nil {
		return nil, err
	}
	if err := models.ValidateCredentialFreeContext(req.ChildCheckpoint); err != nil {
		return nil, err
	}
	if err := models.ValidateArtifactRequirements(req.ArtifactRequirements); err != nil {
		return nil, fmt.Errorf("%w: %w", models.ErrInvalidArtifact, err)
	}
	if key := strings.TrimSpace(req.IdempotencyKey); key != "" {
		existing, err := s.store.FindAgentRequestByIdempotencyKey(ctx, req.Scope, key)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if sameAgentRequestIntent(existing, req) {
				return &AgentRequestResult{Request: existing}, nil
			}
			return nil, models.ErrAgentRequestIdempotency
		}
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	if !models.RequesterControlsRun(req.Requester, source) {
		return nil, ErrAgentRequestUnauthorized
	}
	if err := ValidateTaskDelegationSource(ctx, s.runs, source); err != nil {
		return nil, err
	}
	if err := s.validateChatDelegation(ctx, source, req.Recipient); err != nil {
		return nil, err
	}
	delegationPolicy, err := s.resolveSourceDelegationPolicy(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := s.validateSourceDelegationPolicy(ctx, req, delegationPolicy); err != nil {
		return nil, err
	}
	acceptancePolicy := models.NormalizeAgentRequestAcceptancePolicy(req.AcceptancePolicy)
	if delegationPolicy != nil && delegationPolicy.RequireAcceptance {
		acceptancePolicy = models.AgentRequestAcceptanceRecipientReview
	}
	// Grouped requests are created only after their dependency coordinator has
	// atomically moved the source into the group's wait state. Their membership
	// is validated below; singular requests must still own the wake condition.
	if strings.TrimSpace(req.DependencyGroupID) == "" {
		if err := validateAgentRequestSource(source); err != nil {
			return nil, err
		}
	}
	now := s.now()
	requestID := strings.TrimSpace(req.ID)
	if requestID == "" {
		requestID = s.newID()
	}
	request := &models.AgentRequest{
		ID: requestID, Scope: req.Scope, Kind: req.Kind, Status: models.AgentRequestStatusPending,
		Requester: req.Requester, Recipient: req.Recipient, SourceRunID: source.ID, ObjectiveID: source.ObjectiveID,
		DependencyGroupID: strings.TrimSpace(req.DependencyGroupID), DependencyID: strings.TrimSpace(req.DependencyID),
		Goal: strings.TrimSpace(req.Goal), Instructions: strings.TrimSpace(req.Instructions), SemanticRole: strings.TrimSpace(req.SemanticRole),
		AcceptanceCriteria: maps.Clone(req.AcceptanceCriteria), ArtifactRequirements: models.CloneArtifactRequirements(req.ArtifactRequirements),
		SharedContext: maps.Clone(req.SharedContext), ChildCheckpoint: maps.Clone(req.ChildCheckpoint),
		AcceptancePolicy: acceptancePolicy, DelegationPolicy: models.CloneAgentRequestDelegationPolicy(delegationPolicy),
		ConversationRefs: append([]string(nil), req.ConversationRefs...),
		BudgetAllocation: models.CloneBudgetPolicy(req.BudgetAllocation),
		IdempotencyKey:   strings.TrimSpace(req.IdempotencyKey), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if request.Recipient.Type == models.OwnerTypeTeam {
		request.Candidates, err = s.resolveTeamRequestCandidates(ctx, request)
		if err != nil {
			return nil, err
		}
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if err := models.ValidateChildBudgetAllocation(source, request.BudgetAllocation); err != nil {
		return nil, err
	}
	if request.DependencyGroupID != "" {
		if _, err := s.groupedRequestDependency(ctx, request, source, false); err != nil {
			return nil, err
		}
	}
	eventType := "collaboration.requested"
	summary := fmt.Sprintf("Requested work from %s %s", request.Recipient.Type, request.Recipient.ID)
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = "handoff.proposed"
		summary = fmt.Sprintf("Proposed handoff to %s %s", request.Recipient.Type, request.Recipient.ID)
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = "escalation.requested"
		summary = fmt.Sprintf("Escalated work to %s %s", request.Recipient.Type, request.Recipient.ID)
	}
	event := collaborationEvent(source, request, eventType, summary, req.Requester, now)
	record := models.AgentRequestCreateRecord{Request: request, Event: event}
	if delegationPolicy != nil && delegationPolicy.MaximumConcurrent > 0 {
		record.DelegationTeamID = delegationPolicy.TeamDeploymentID
		record.MaximumConcurrent = delegationPolicy.MaximumConcurrent
	}
	if request.DependencyGroupID == "" {
		record.SourceRun, err = sourceAwaitingAgentRequestDecision(source, request, now)
		if err != nil {
			return nil, err
		}
		record.ExpectedSourceRevision = source.Revision
		record.ToolTransition = req.ToolTransition
		record.RequireInterventionCount = req.RequireInterventionCount
	} else if req.ToolTransition != nil || req.RequireInterventionCount != nil {
		return nil, errors.New("a grouped agent request cannot move a tool entry or check interventions")
	}
	persisted, err := s.store.CreateAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	return &AgentRequestResult{
		Request: models.CloneAgentRequest(request), Source: models.CloneAgentRun(record.SourceRun),
		Events: []*models.ActivityEvent{persisted},
	}, nil
}

// CreateAgentRequestGroup durably seals a complete fan-out before exposing its
// collaboration requests. A stable idempotency key makes partial creation
// recoverable after process or storage interruptions.
func (s *Service) CreateAgentRequestGroup(ctx context.Context, req CreateAgentRequestGroupRequest) (*AgentRequestGroupResult, error) {
	if s == nil || s.dependencies == nil {
		return nil, errors.New("collaboration dependency store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if err := req.Requester.Validate(); err != nil {
		return nil, fmt.Errorf("requester: %w", err)
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if key == "" || len(req.Requests) == 0 {
		return nil, errors.New("agent request group requires an idempotency key and at least one request")
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, strings.TrimSpace(req.SourceRunID))
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	replaying := false
	if source.Revision != req.ExpectedSourceRevision {
		if existing, findErr := s.dependencies.FindRunDependencyGroupByIdempotencyKey(ctx, req.Scope, key); findErr != nil || existing == nil {
			if findErr != nil {
				return nil, findErr
			}
			return nil, models.ErrRevisionConflict
		}
		replaying = true
	}
	if !models.RequesterControlsRun(req.Requester, source) {
		return nil, ErrAgentRequestUnauthorized
	}
	if !replaying {
		if err := validateAgentRequestSource(source); err != nil {
			return nil, err
		}
	}
	groupID := strings.TrimSpace(req.ID)
	if groupID == "" {
		groupID = StableCollaborationID(req.Scope, key, "group")
	}
	dependencySpecs := make([]models.RunDependencySpec, 0, len(req.Requests))
	requestInputs := make([]CreateAgentRequestRequest, 0, len(req.Requests))
	seenDependencies := make(map[string]struct{}, len(req.Requests))
	allocations := make([]*models.BudgetPolicy, 0, len(req.Requests))
	for index, spec := range req.Requests {
		if spec.Kind != models.AgentRequestKindRequest && spec.Kind != models.AgentRequestKindEscalation {
			return nil, errors.New("grouped collaboration supports request and escalation fan-out; handoff transfers must remain singular")
		}
		if err := spec.Recipient.Validate(); err != nil {
			return nil, fmt.Errorf("request %d recipient: %w", index+1, err)
		}
		if err := ValidateTaskDelegationSource(ctx, s.runs, source); err != nil {
			return nil, err
		}
		if err := s.validateChatDelegation(ctx, source, spec.Recipient); err != nil {
			return nil, err
		}
		if strings.TrimSpace(spec.Goal) == "" {
			return nil, fmt.Errorf("request %d goal is required", index+1)
		}
		if err := models.ValidateCredentialFreeContext(spec.SharedContext); err != nil {
			return nil, err
		}
		if err := models.ValidateArtifactRequirements(spec.ArtifactRequirements); err != nil {
			return nil, fmt.Errorf("%w: request %d: %w", models.ErrInvalidArtifact, index+1, err)
		}
		if err := models.ValidateChildBudgetAllocation(source, spec.BudgetAllocation); err != nil {
			return nil, fmt.Errorf("request %d: %w", index+1, err)
		}
		allocations = append(allocations, spec.BudgetAllocation)
		dependencyID := strings.TrimSpace(spec.DependencyID)
		if dependencyID == "" {
			dependencyID = fmt.Sprintf("request-%d", index+1)
		}
		if _, exists := seenDependencies[dependencyID]; exists {
			return nil, fmt.Errorf("duplicate dependency id %q", dependencyID)
		}
		seenDependencies[dependencyID] = struct{}{}
		requestID := strings.TrimSpace(spec.ID)
		if requestID == "" {
			requestID = StableCollaborationID(req.Scope, key, dependencyID)
		}
		dependencySpecs = append(dependencySpecs, models.RunDependencySpec{
			ID: dependencyID, RequestID: requestID, Kind: models.RunDependencyKindAgentRequest, Required: spec.Required,
		})
		requestInputs = append(requestInputs, CreateAgentRequestRequest{
			ID: requestID, Scope: req.Scope, Kind: spec.Kind, Requester: req.Requester, Recipient: spec.Recipient,
			SourceRunID: req.SourceRunID, Goal: spec.Goal, Instructions: spec.Instructions, SemanticRole: spec.SemanticRole,
			AcceptanceCriteria: spec.AcceptanceCriteria, ArtifactRequirements: spec.ArtifactRequirements,
			SharedContext: spec.SharedContext, ChildCheckpoint: spec.ChildCheckpoint, AcceptancePolicy: spec.AcceptancePolicy,
			ConversationRefs: spec.ConversationRefs,
			BudgetAllocation: spec.BudgetAllocation,
			IdempotencyKey:   key + ":request:" + dependencyID, DependencyGroupID: groupID, DependencyID: dependencyID,
		})
	}
	if err := models.ValidateGroupedBudgetAllocations(source, allocations); err != nil {
		return nil, err
	}
	group, err := s.dependencies.CreateRunDependencyGroup(ctx, models.CreateRunDependencyGroupRequest{
		ID: groupID, Scope: req.Scope, SourceRunID: req.SourceRunID, ExpectedSourceRevision: req.ExpectedSourceRevision,
		Policy: req.Policy, Dependencies: dependencySpecs, IdempotencyKey: key, Actor: req.Actor, Visibility: req.Visibility,
	})
	if err != nil {
		return nil, err
	}
	result := &AgentRequestGroupResult{Group: group, Requests: make([]*models.AgentRequest, 0, len(requestInputs))}
	result.Events = append(result.Events, group.Events...)
	for _, input := range requestInputs {
		created, err := s.CreateAgentRequest(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("create grouped agent request %s: %w", input.DependencyID, err)
		}
		result.Requests = append(result.Requests, created.Request)
		result.Events = append(result.Events, created.Events...)
	}
	return result, nil
}

func ValidateTaskDelegationSource(ctx context.Context, store models.PortfolioStore, source *models.AgentRun) error {
	task, err := agentrun.PersistedTaskWorkOrigin(ctx, store, source)
	if err != nil {
		return err
	}
	if task != nil {
		return models.ErrInvalidConversationTask
	}
	return nil
}

func (s *Service) groupedRequestDependency(ctx context.Context, request *models.AgentRequest, source *models.AgentRun, allowTerminalGroup bool) (*models.RunDependency, error) {
	if request == nil || source == nil || request.DependencyGroupID == "" || request.DependencyID == "" {
		return nil, models.ErrInvalidAgentRequestState
	}
	group, err := s.dependencies.GetRunDependencyGroup(ctx, request.Scope, request.DependencyGroupID)
	if err != nil {
		return nil, err
	}
	waiting := source.Status == models.AgentRunStatusWaitingForDependency && source.WakeCondition != nil &&
		source.WakeCondition.Type == "run_dependencies" && source.WakeCondition.Reference == group.ID
	terminal := group.Status == models.RunDependencyGroupSatisfied || group.Status == models.RunDependencyGroupFailed || group.Status == models.RunDependencyGroupCanceled
	if group.SourceRunID != request.SourceRunID || (!waiting && !(allowTerminalGroup && terminal)) {
		return nil, fmt.Errorf("%w: source run is not waiting on dependency group %s", models.ErrInvalidAgentRequestState, group.ID)
	}
	edges, err := s.dependencies.ListRunDependencies(ctx, request.Scope, group.ID)
	if err != nil {
		return nil, err
	}
	for _, edge := range edges {
		if edge.ID == request.DependencyID && edge.RequestID == request.ID && edge.Kind == models.RunDependencyKindAgentRequest {
			return edge, nil
		}
	}
	return nil, fmt.Errorf("%w: request %s is not linked to dependency %s", models.ErrInvalidAgentRequestState, request.ID, request.DependencyID)
}

func (s *Service) GetAgentRequest(ctx context.Context, scope models.Scope, requestID string) (*models.AgentRequest, error) {
	request, err := s.store.GetAgentRequest(ctx, scope, requestID)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	return request, nil
}

func (s *Service) ListAgentRequests(ctx context.Context, filter models.AgentRequestFilter) ([]*models.AgentRequest, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	return s.store.ListAgentRequests(ctx, filter)
}

func buildCollaborationChildRun(source *models.AgentRun, request *models.AgentRequest, now time.Time, id string) *models.AgentRun {
	owner := models.CollaborationChildOwner(source, request)
	assignedAgent := request.AssignedAgentID
	context := maps.Clone(request.SharedContext)
	if context == nil {
		context = make(map[string]interface{})
	}
	context = models.InheritRoutineEventBatch(source, context)
	context["collaboration"] = map[string]interface{}{
		"requestId": request.ID, "kind": request.Kind, "requester": request.Requester,
		"recipient": request.Recipient, "semanticRole": request.SemanticRole,
		"acceptanceCriteria": maps.Clone(request.AcceptanceCriteria), "artifactRequirements": models.CloneArtifactRequirements(request.ArtifactRequirements),
	}
	sourceKind := models.RunSourceRequest
	if request.Kind == models.AgentRequestKindHandoff {
		sourceKind = models.RunSourceHandoff
	} else if request.Kind == models.AgentRequestKindEscalation {
		sourceKind = models.RunSourceEscalation
	}
	kind, lane := models.NormalizeRunKind(source.Kind), source.ConcurrencyKey
	if kind == models.RunKindConversation {
		lane = collaborationChatPlacement(source, assignedAgent, context)
	}
	child := &models.AgentRun{
		ID: id, Kind: kind, Scope: source.Scope, ObjectiveID: source.ObjectiveID, ParentRunID: source.ID, RootRunID: source.RootRunID,
		Owner: owner, AssignedAgentID: assignedAgent, ActingUser: source.ActingUser, ConcurrencyKey: lane,
		Goal: request.Goal, Source: sourceKind, Status: models.AgentRunStatusQueued,
		Priority: source.Priority, AvailableAt: now, QueueEnteredAt: now, Context: context,
		Checkpoint: maps.Clone(request.ChildCheckpoint), Budget: models.CloneBudgetPolicy(request.BudgetAllocation),
		Policy: maps.Clone(source.Policy), Revision: 1, CreatedAt: now, UpdatedAt: now, Engine: models.RunEngineLoop,
	}
	if child.Budget != nil {
		child.BudgetState = models.BudgetStateActive
	}
	return child
}

// collaborationChatPlacement puts work handed over in a Team chat into that
// same chat: the recipient answers there as an ordinary chat Run, in its own
// lane, with the whole conversation, exactly as if the message reached it first.
func collaborationChatPlacement(source *models.AgentRun, assignedAgent string, context map[string]interface{}) string {
	for _, key := range []string{models.ConversationRunConversationIDContextKey, models.ConversationRunTriggerIDContextKey, conversation.RunContextTriggerSeq, "threadRootMessageId", "responseMode"} {
		if value, ok := source.Context[key]; ok {
			context[key] = value
		}
	}
	if from := strings.TrimSpace(source.AssignedAgentID); from != "" {
		context[conversation.CollaborationHandedOffByContextKey] = from
	}
	conversationID, _ := context[models.ConversationRunConversationIDContextKey].(string)
	root, _ := context["threadRootMessageId"].(string)
	return models.ConversationRunLane(conversationID, root, assignedAgent)
}

// validateChatDelegationSource allows chat hand-offs only between Team
// members. An Agent's 1:1 chat belongs to that Agent, so no other Agent can
// answer in it.
func validateChatDelegationSource(source *models.AgentRun) error {
	if source != nil && source.Kind == models.RunKindConversation && source.Owner.Type != models.OwnerTypeTeam {
		return fmt.Errorf("%w: only Team chats hand work to another Agent", models.ErrInvalidAgentRequestState)
	}
	return nil
}

// validateChatDelegation keeps chat work on the chat's roster: a Team chat
// hands work to the Team, which routes among its members, or to an Agent on
// its current active roster. It is checked when the request is created and
// again when it is accepted.
func (s *Service) validateChatDelegation(ctx context.Context, source *models.AgentRun, recipient models.CollaborationParty) error {
	if err := validateChatDelegationSource(source); err != nil {
		return err
	}
	if source == nil || source.Kind != models.RunKindConversation {
		return nil
	}
	if recipient == (models.CollaborationParty{Type: models.OwnerTypeTeam, ID: source.Owner.ID}) {
		return nil
	}
	if recipient.Type == models.OwnerTypeAgent {
		onRoster, err := models.AgentOnChatRoster(ctx, s.teams, source.Scope, source.Owner, recipient.ID)
		if err != nil {
			return err
		}
		if onRoster {
			return nil
		}
	}
	return fmt.Errorf("%w: chat work goes only to an Agent on the chat's roster", ErrAgentRequestUnauthorized)
}

// verifyChatAssignment keeps accepted Team chat work on the chat's roster:
// the child answers in that chat, so a member removed after the request was
// made cannot accept it.
func (s *Service) verifyChatAssignment(ctx context.Context, source *models.AgentRun, assignedAgentID string) error {
	if source == nil || models.NormalizeRunKind(source.Kind) != models.RunKindConversation || source.Owner.Type != models.OwnerTypeTeam {
		return nil
	}
	onRoster, err := models.AgentOnChatRoster(ctx, s.teams, source.Scope, source.Owner, assignedAgentID)
	if err != nil {
		return err
	}
	if !onRoster {
		return fmt.Errorf("%w: chat work goes only to an Agent on the chat's roster", ErrAgentRequestUnauthorized)
	}
	return nil
}
