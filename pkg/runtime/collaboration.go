package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/artifactcatalog"
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

type CollaborationService struct {
	store             models.CollaborationStore
	runs              models.PortfolioStore
	artifacts         models.ArtifactStore
	dependencies      *portfolio.DependencyCoordinator
	teams             models.TeamRosterReader
	executionPreparer models.AcceptedRunExecutionPreparer
	now               func() time.Time
	newID             func() string
}

func NewCollaborationService(store models.CollaborationKernelStore, opts ...models.ServiceOption) *CollaborationService {
	service := &CollaborationService{store: store, runs: store, artifacts: store, dependencies: portfolio.NewDependencyCoordinator(store, opts...), now: models.ClockFrom(opts), newID: uuid.NewString}
	service.teams = models.TeamRosterReaderOf(store)
	return service
}

func (s *CollaborationService) resolveSourceDelegationPolicy(ctx context.Context, source *models.AgentRun) (*models.AgentRequestDelegationPolicy, error) {
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

func (s *CollaborationService) sourceDelegationDepth(ctx context.Context, source *models.AgentRun) (int, error) {
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

func (s *CollaborationService) validateSourceDelegationPolicy(ctx context.Context, req CreateAgentRequestRequest, policy *models.AgentRequestDelegationPolicy) error {
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

func (s *CollaborationService) CreateAgentRequest(ctx context.Context, req CreateAgentRequestRequest) (*AgentRequestResult, error) {
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
func (s *CollaborationService) CreateAgentRequestGroup(ctx context.Context, req CreateAgentRequestGroupRequest) (*AgentRequestGroupResult, error) {
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

func (s *CollaborationService) RespondAgentRequest(ctx context.Context, req RespondAgentRequestRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, req.RequestID)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	if request.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	if request.Status != models.AgentRequestStatusPending && request.Status != models.AgentRequestStatusClarificationRequested {
		return nil, models.ErrInvalidAgentRequestState
	}
	if req.Decision == AgentRequestDecisionProvideClarification {
		if req.Principal != request.Requester {
			return nil, ErrAgentRequestUnauthorized
		}
	} else if req.Principal != request.Recipient {
		return nil, ErrAgentRequestUnauthorized
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	var groupedDependency *models.RunDependency
	if request.DependencyGroupID != "" {
		groupedDependency, err = s.groupedRequestDependency(ctx, request, source, false)
		if err != nil {
			return nil, err
		}
	}
	now := s.now()
	actor := req.Actor
	if strings.TrimSpace(actor.ID) == "" {
		actor = req.Principal
	}
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("actor: %w", err)
	}
	if err := s.validateDecisionRunAuthority(ctx, request, actor, req.DecisionRunID); err != nil {
		return nil, err
	}
	updated := models.CloneAgentRequest(request)
	updated.Revision++
	updated.UpdatedAt = now
	updated.Response = strings.TrimSpace(req.Message)
	record := models.AgentRequestResponseRecord{Request: updated, ExpectedRequestRevision: request.Revision}
	eventType := ""
	summary := ""
	switch req.Decision {
	case AgentRequestDecisionProvideClarification:
		if request.Status != models.AgentRequestStatusClarificationRequested || strings.TrimSpace(req.Message) == "" {
			return nil, errors.New("clarification response is required for a request awaiting clarification")
		}
		updated.Status = models.AgentRequestStatusPending
		eventType = "collaboration.clarification_provided"
		summary = fmt.Sprintf("%s %s provided clarification", actor.Type, actor.ID)
		if groupedDependency == nil {
			record.SourceRun, err = sourceAwaitingAgentRequestDecision(source, updated, now)
			if err != nil {
				return nil, err
			}
			record.ExpectedSourceRevision = source.Revision
		}
	case AgentRequestDecisionRequestClarification:
		if request.Status != models.AgentRequestStatusPending {
			return nil, models.ErrInvalidAgentRequestState
		}
		if strings.TrimSpace(req.Message) == "" {
			return nil, errors.New("clarification question is required")
		}
		updated.Status = models.AgentRequestStatusClarificationRequested
		updated.Clarification = strings.TrimSpace(req.Message)
		eventType = "collaboration.clarification_requested"
		summary = fmt.Sprintf("%s %s requested clarification", actor.Type, actor.ID)
		if groupedDependency == nil {
			record.SourceRun, err = sourceResumingAfterAgentRequestDecision(source, updated, now)
			if err != nil {
				return nil, err
			}
			record.ExpectedSourceRevision = source.Revision
		}
	case AgentRequestDecisionReject:
		updated.Status = models.AgentRequestStatusRejected
		updated.ResolvedAt = &now
		eventType = "collaboration.rejected"
		summary = fmt.Sprintf("%s %s rejected the request", actor.Type, actor.ID)
		if groupedDependency != nil {
			reason := strings.TrimSpace(req.Message)
			if reason == "" {
				reason = "recipient rejected the request"
			}
			record.DependencyResolution = &models.RunDependencyResolutionRecord{
				Scope: updated.Scope, GroupID: updated.DependencyGroupID, DependencyID: updated.DependencyID,
				ExpectedDependencyRevision: groupedDependency.Revision, State: models.RunDependencyStateFailed, Error: reason,
				Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID}, Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
			}
		} else {
			record.SourceRun, err = sourceResumingAfterAgentRequestDecision(source, updated, now)
			if err != nil {
				return nil, err
			}
			record.ExpectedSourceRevision = source.Revision
		}
	case AgentRequestDecisionAccept:
		if groupedDependency == nil {
			if !sourceWaitsForAgentRequestDecision(source, request.ID) {
				if err := validateAgentRequestSource(source); err != nil {
					return nil, err
				}
			}
		}
		assignedAgentID, assignmentErr := s.resolveRequestAssignment(ctx, request, req.AssignedAgentID)
		if assignmentErr != nil {
			return nil, assignmentErr
		}
		if err := s.verifyChatAssignment(ctx, source, assignedAgentID); err != nil {
			return nil, err
		}
		updated.Status = models.AgentRequestStatusAccepted
		updated.AcceptedAt = &now
		updated.AssignedAgentID = assignedAgentID
		if request.Recipient.Type == models.OwnerTypeTeam {
			updated.AssignmentDecision = &models.AgentRequestAssignmentDecision{
				AgentDeploymentID: assignedAgentID,
				Reason:            teamAssignmentReason(request, assignedAgentID),
				DecidedAt:         now,
			}
		}
		child := buildCollaborationChildRun(source, updated, now, s.newID())
		task, err := agentrun.PersistedConversationTaskSchedulingOrigin(ctx, s.runs, source, s.store.GetAgentRequest)
		if err != nil {
			return nil, err
		}
		if task != nil && task.Mode == models.ConversationTaskModeContinuation {
			child.Kind = models.RunKindAgentWork
		}
		entrypoint, err := agentrun.RequestedAcceptedChildEntrypoint(updated.SharedContext)
		if err != nil {
			return nil, err
		}
		childRequest := models.CreateAgentRunRequest{
			Kind: child.Kind, Scope: child.Scope, ObjectiveID: child.ObjectiveID, ParentRunID: child.ParentRunID,
			Owner: child.Owner, AssignedAgentID: child.AssignedAgentID, ActingUser: child.ActingUser, Entrypoint: entrypoint,
			ConcurrencyKey: child.ConcurrencyKey, ResourceRequirements: portfolio.CloneResourceQuantities(child.ResourceRequirements),
			Goal: child.Goal, Source: child.Source, Priority: child.Priority, Deadline: child.Deadline,
			Context: child.Context, Plan: child.Plan, Checkpoint: child.Checkpoint, WakeCondition: child.WakeCondition,
			Budget: child.Budget, Policy: child.Policy,
		}
		inherit := entrypoint != "" && child.AssignedAgentID == source.AssignedAgentID && entrypoint == source.Entrypoint
		if err := agentrun.PrepareAcceptedChildRun(ctx, source, &childRequest, s.executionPreparer, inherit); err != nil {
			return nil, err
		}
		child.Entrypoint, child.Context, child.Plan = childRequest.Entrypoint, childRequest.Context, childRequest.Plan
		if child.Entrypoint != "" {
			child.Kind = models.RunKindAgentWork
		}
		if err := child.Validate(); err != nil {
			return nil, err
		}
		updated.ChildRunID = child.ID
		record.ChildRun = child
		record.SourceRun, err = acceptedSourceRun(source, updated, now)
		if err != nil {
			return nil, err
		}
		record.ExpectedSourceRevision = source.Revision
		if groupedDependency != nil {
			record.SourceRun.Status = source.Status
			record.SourceRun.WakeCondition = models.CloneWakeCondition(source.WakeCondition)
		}
		eventType = "collaboration.accepted"
		if updated.Kind == models.AgentRequestKindHandoff {
			eventType = "handoff.accepted"
		} else if updated.Kind == models.AgentRequestKindEscalation {
			eventType = "escalation.accepted"
		}
		summary = fmt.Sprintf("%s %s accepted the work", actor.Type, actor.ID)
		record.ChildEvent = collaborationEvent(child, updated, eventType, summary, actor, now)
	default:
		return nil, errors.New("agent request decision is invalid")
	}
	record.SourceEvent = collaborationEvent(source, updated, eventType, summary, actor, now)
	linkAgentRequestDecisionRun(record.SourceEvent, req.DecisionRunID)
	linkAgentRequestDecisionRun(record.ChildEvent, req.DecisionRunID)
	events, err := s.store.RespondAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if record.DependencyResolution != nil {
		source, err = s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	} else if record.SourceRun != nil {
		source = record.SourceRun
	}
	return &AgentRequestResult{Request: models.CloneAgentRequest(updated), Source: models.CloneAgentRun(source), Child: models.CloneAgentRun(record.ChildRun), Events: events}, nil
}

func (s *CollaborationService) validateDecisionRunAuthority(ctx context.Context, request *models.AgentRequest, actor models.CollaborationParty, decisionRunID string) error {
	decisionRunID = strings.TrimSpace(decisionRunID)
	if decisionRunID == "" {
		return nil
	}
	run, err := s.runs.GetAgentRun(ctx, request.Scope, decisionRunID)
	if err != nil {
		return err
	}
	if run == nil || run.Source != models.RunSourceRequestDecision || run.Status != models.AgentRunStatusCompleted ||
		run.AssignedAgentID != actor.ID || actor.Type != models.OwnerTypeAgent ||
		run.Owner != (models.ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID}) {
		return ErrAgentRequestUnauthorized
	}
	inbox, _ := run.Context[agentrun.AgentRequestInboxContextKey].(map[string]interface{})
	inboxRequestID, _ := inbox["requestId"].(string)
	if strings.TrimSpace(inboxRequestID) != request.ID {
		return ErrAgentRequestUnauthorized
	}
	revision, revisionErr := positiveInt64(inbox["requestRevision"])
	if revisionErr != nil || revision != request.Revision {
		return ErrAgentRequestUnauthorized
	}
	return nil
}

func linkAgentRequestDecisionRun(event *models.ActivityEvent, decisionRunID string) {
	if event == nil || strings.TrimSpace(decisionRunID) == "" {
		return
	}
	event.CausationID = strings.TrimSpace(decisionRunID)
	if event.Payload == nil {
		event.Payload = make(map[string]interface{})
	}
	event.Payload["decisionRunId"] = strings.TrimSpace(decisionRunID)
}

// CompleteAgentRequest atomically records the recipient's result and completes
// the child run. It wakes the source immediately unless the snapshotted Team
// policy requires an independent completion review. The
// Principal is the authorized recipient; Actor is the agent or Team member
// that actually performed the completion and is retained in the audit trail.
func (s *CollaborationService) CompleteAgentRequest(ctx context.Context, req CompleteAgentRequestRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil || s.artifacts == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, strings.TrimSpace(req.RequestID))
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	if req.Principal != request.Recipient {
		return nil, ErrAgentRequestUnauthorized
	}
	artifacts, err := s.resolveArtifactReferences(ctx, req.Scope, request.ArtifactRequirements, req.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", models.ErrInvalidArtifact, err)
	}
	if request.Status == models.AgentRequestStatusCompleted || request.Status == models.AgentRequestStatusCompletionReview {
		if request.CompletionKey != strings.TrimSpace(req.CompletionKey) || request.CompletionKey == "" {
			return nil, models.ErrInvalidAgentRequestState
		}
		if !sameAgentRequestCompletion(request, req, artifacts) {
			return nil, models.ErrAgentRequestIdempotency
		}
		source, sourceErr := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if sourceErr != nil {
			return nil, sourceErr
		}
		child, childErr := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
		if childErr != nil {
			return nil, childErr
		}
		return &AgentRequestResult{Request: request, Source: source, Child: child}, nil
	}
	if request.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	if request.Status != models.AgentRequestStatusAccepted || strings.TrimSpace(request.ChildRunID) == "" {
		return nil, models.ErrInvalidAgentRequestState
	}
	actor := req.Actor
	if strings.TrimSpace(actor.ID) == "" {
		actor = req.Principal
	}
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("actor: %w", err)
	}
	if strings.TrimSpace(req.Summary) == "" || strings.TrimSpace(req.CompletionKey) == "" {
		return nil, errors.New("completion summary and idempotency key are required")
	}
	if len(request.AcceptanceCriteria) > 0 && len(req.AcceptanceEvidence) == 0 {
		return nil, errors.New("acceptance evidence is required")
	}
	if err := models.ValidateCredentialFreeContext(req.AcceptanceEvidence); err != nil {
		return nil, err
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	child, err := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
	if err != nil {
		return nil, err
	}
	if source == nil || child == nil {
		return nil, models.ErrRunNotFound
	}
	if child.ParentRunID != source.ID || child.RootRunID != source.RootRunID || child.Status == models.AgentRunStatusFailed || child.Status == models.AgentRunStatusCanceled {
		return nil, models.ErrInvalidAgentRequestState
	}
	if req.ExpectedChildRevision != child.Revision {
		return nil, models.ErrRevisionConflict
	}
	now := s.now()
	updatedRequest := models.CloneAgentRequest(request)
	requiresReview := updatedRequest.DelegationPolicy != nil && updatedRequest.DelegationPolicy.RequireCompletionReview
	updatedRequest.Status = models.AgentRequestStatusCompleted
	if requiresReview {
		updatedRequest.Status = models.AgentRequestStatusCompletionReview
	}
	updatedRequest.CompletionSummary = strings.TrimSpace(req.Summary)
	updatedRequest.AcceptanceEvidence = maps.Clone(req.AcceptanceEvidence)
	updatedRequest.Artifacts = artifacts
	updatedRequest.CompletionKey = strings.TrimSpace(req.CompletionKey)
	updatedRequest.CompletionSubmittedAt = &now
	updatedRequest.Revision++
	updatedRequest.UpdatedAt = now
	if !requiresReview {
		updatedRequest.CompletedAt = &now
		updatedRequest.ResolvedAt = &now
	}

	sharedChildOutput := maps.Clone(child.Output)
	updatedChild := completedCollaborationChildRun(child, updatedRequest, now)
	var updatedSource *models.AgentRun
	var dependencyResolution *models.RunDependencyResolutionRecord
	if requiresReview {
		updatedSource = models.CloneAgentRun(source)
	} else if updatedRequest.DependencyGroupID != "" {
		edge, edgeErr := s.groupedRequestDependency(ctx, updatedRequest, source, true)
		if edgeErr != nil {
			return nil, edgeErr
		}
		dependencyResolution = &models.RunDependencyResolutionRecord{
			Scope: updatedRequest.Scope, GroupID: updatedRequest.DependencyGroupID, DependencyID: updatedRequest.DependencyID,
			ExpectedDependencyRevision: edge.Revision, State: models.RunDependencyStateSatisfied,
			Result: map[string]interface{}{
				"requestId": updatedRequest.ID, "childRunId": updatedRequest.ChildRunID,
				"summary": updatedRequest.CompletionSummary, "acceptanceEvidence": maps.Clone(updatedRequest.AcceptanceEvidence),
				"output": sharedChildOutput,
			},
			Artifacts: updatedRequest.Artifacts, Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID},
			Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
		}
	} else {
		updatedSource, err = completedCollaborationSourceRun(source, updatedRequest, sharedChildOutput, now)
		if err != nil {
			return nil, err
		}
	}
	eventType := "collaboration.completed"
	if requiresReview {
		eventType = "collaboration.completion_review_requested"
	}
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = "handoff.completed"
		if requiresReview {
			eventType = "handoff.completion_review_requested"
		}
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = "escalation.completed"
		if requiresReview {
			eventType = "escalation.completion_review_requested"
		}
	}
	summary := fmt.Sprintf("%s %s completed the work", actor.Type, actor.ID)
	if requiresReview {
		summary = fmt.Sprintf("%s %s submitted completed work for independent review", actor.Type, actor.ID)
	}
	record := models.AgentRequestCompletionRecord{
		Request: updatedRequest, ExpectedRequestRevision: request.Revision,
		ExpectedSourceRevision: source.Revision, DependencyResolution: dependencyResolution,
		ChildRun: updatedChild, ExpectedChildRevision: child.Revision,
		SourceEvent: collaborationCompletionEvent(source, updatedRequest, eventType, summary, actor, now),
		ChildEvent:  collaborationCompletionEvent(child, updatedRequest, eventType, summary, actor, now),
	}
	if !requiresReview {
		record.SourceRun = updatedSource
	}
	events, err := s.store.CompleteAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if dependencyResolution != nil {
		updatedSource, err = s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	}
	return &AgentRequestResult{
		Request: models.CloneAgentRequest(updatedRequest), Source: models.CloneAgentRun(updatedSource), Child: models.CloneAgentRun(updatedChild), Events: events,
	}, nil
}

// ReviewAgentRequestCompletion resolves the independent review checkpoint
// required by the snapshotted Team policy. The requester reviews the result;
// the recipient that performed the work cannot approve its own completion.
func (s *CollaborationService) ReviewAgentRequestCompletion(ctx context.Context, req ReviewAgentRequestCompletionRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, strings.TrimSpace(req.RequestID))
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	key := strings.TrimSpace(req.IdempotencyKey)
	if request.CompletionReviewKey != "" {
		if request.CompletionReviewKey != key || key == "" {
			return nil, models.ErrInvalidAgentRequestState
		}
		source, sourceErr := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if sourceErr != nil {
			return nil, sourceErr
		}
		child, childErr := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
		if childErr != nil {
			return nil, childErr
		}
		return &AgentRequestResult{Request: request, Source: source, Child: child}, nil
	}
	if request.Revision != req.ExpectedRevision {
		return nil, models.ErrRevisionConflict
	}
	if request.Status != models.AgentRequestStatusCompletionReview || request.DelegationPolicy == nil || !request.DelegationPolicy.RequireCompletionReview {
		return nil, models.ErrInvalidAgentRequestState
	}
	if req.ResolveDisagreement != request.ReviewDisagreement {
		return nil, fmt.Errorf("%w: disagreement resolution must match the durable review state", models.ErrInvalidAgentRequestState)
	}
	if req.Principal != request.Requester || req.Principal == request.Recipient {
		return nil, ErrAgentRequestUnauthorized
	}
	actor := req.Actor
	if strings.TrimSpace(actor.ID) == "" {
		actor = req.Principal
	}
	if err := actor.Validate(); err != nil {
		return nil, fmt.Errorf("actor: %w", err)
	}
	if actor.Type != models.OwnerTypeAgent || actor.ID == request.AssignedAgentID {
		return nil, fmt.Errorf("%w: completed work requires an independent Agent reviewer", ErrAgentRequestUnauthorized)
	}
	for _, decision := range request.CompletionReviews {
		if decision.IdempotencyKey == key && key != "" {
			if decision.Reviewer == actor && decision.Approve == req.Approve && decision.Summary == strings.TrimSpace(req.Summary) {
				return &AgentRequestResult{Request: request}, nil
			}
			return nil, models.ErrAgentRequestIdempotency
		}
		if decision.Reviewer == actor {
			return nil, fmt.Errorf("%w: reviewer already submitted a completion decision", models.ErrInvalidAgentRequestState)
		}
	}
	summary := strings.TrimSpace(req.Summary)
	if summary == "" || key == "" {
		return nil, errors.New("completion review summary and idempotency key are required")
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	child, err := s.runs.GetAgentRun(ctx, req.Scope, request.ChildRunID)
	if err != nil {
		return nil, err
	}
	if source == nil || child == nil || child.Status != models.AgentRunStatusCompleted || child.ParentRunID != source.ID || req.ExpectedChildRevision != child.Revision {
		return nil, models.ErrInvalidAgentRequestState
	}
	now := s.now()
	updatedRequest := models.CloneAgentRequest(request)
	updatedRequest.Revision++
	updatedRequest.UpdatedAt = now
	updatedRequest.CompletionReviews = append(updatedRequest.CompletionReviews, models.AgentRequestCompletionReviewDecision{
		Reviewer: actor, Approve: req.Approve, Summary: summary, IdempotencyKey: key, DecidedAt: now,
	})
	quorum := completionReviewQuorum(updatedRequest.DelegationPolicy)
	approvals, rejections := models.CompletionReviewCounts(updatedRequest.CompletionReviews)
	disagreement := approvals > 0 && rejections > 0 && updatedRequest.DelegationPolicy.EscalateOnDisagreement && !req.ResolveDisagreement
	if disagreement || approvals < quorum && rejections < quorum {
		updatedRequest.ReviewDisagreement = disagreement
		eventType := "collaboration.completion_review_recorded"
		eventSummary := fmt.Sprintf("Agent %s recorded completion review %d of %d", actor.ID, len(updatedRequest.CompletionReviews), quorum)
		if disagreement {
			eventType = "collaboration.completion_review_disagreement"
			eventSummary = fmt.Sprintf("Completion reviewers disagreed after %d decisions; escalation is required", len(updatedRequest.CompletionReviews))
		}
		event := collaborationCompletionEvent(source, updatedRequest, eventType, eventSummary, actor, now)
		persisted, persistErr := s.store.CoordinateAgentRequest(ctx, models.AgentRequestCoordinationRecord{
			Request: updatedRequest, ExpectedRequestRevision: request.Revision, Event: event,
		})
		if persistErr != nil {
			return nil, persistErr
		}
		return &AgentRequestResult{Request: models.CloneAgentRequest(updatedRequest), Source: models.CloneAgentRun(source), Child: models.CloneAgentRun(child), Events: []*models.ActivityEvent{persisted}}, nil
	}
	updatedRequest.CompletionReviewer = &actor
	updatedRequest.CompletionReviewSummary = summary
	updatedRequest.CompletionReviewKey = key
	updatedRequest.ReviewDisagreement = false
	updatedRequest.ReviewedAt = &now
	updatedRequest.ResolvedAt = &now
	updatedChild := models.CloneAgentRun(child)
	updatedChild.Revision++
	updatedChild.UpdatedAt = now
	var updatedSource *models.AgentRun
	var dependencyResolution *models.RunDependencyResolutionRecord
	eventType := "collaboration.completion_review_approved"
	eventSummary := fmt.Sprintf("%s %s approved the completed work", actor.Type, actor.ID)
	dependencyState := models.RunDependencyStateSatisfied
	if req.ResolveDisagreement {
		eventType = "collaboration.completion_review_disagreement_resolved"
		eventSummary = fmt.Sprintf("Agent %s resolved the completion review disagreement", actor.ID)
	}
	if approvals >= quorum {
		updatedRequest.Status = models.AgentRequestStatusCompleted
		updatedRequest.CompletedAt = &now
		updatedChild.Output = collaborationCompletionOutput(updatedChild.Output, updatedRequest, maps.Clone(child.Output))
		if updatedRequest.DependencyGroupID == "" {
			updatedSource, err = completedCollaborationSourceRun(source, updatedRequest, maps.Clone(child.Output), now)
		}
	} else {
		updatedRequest.Status = models.AgentRequestStatusFailed
		updatedRequest.ResolutionReason = summary
		eventType = "collaboration.completion_review_rejected"
		eventSummary = fmt.Sprintf("%s %s rejected the completed work", actor.Type, actor.ID)
		dependencyState = models.RunDependencyStateFailed
		if updatedRequest.DependencyGroupID == "" {
			updatedSource, err = failedCollaborationSourceRun(source, updatedRequest, now)
		}
	}
	if err != nil {
		return nil, err
	}
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = strings.Replace(eventType, "collaboration.", "handoff.", 1)
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = strings.Replace(eventType, "collaboration.", "escalation.", 1)
	}
	if updatedRequest.DependencyGroupID != "" {
		edge, edgeErr := s.groupedRequestDependency(ctx, updatedRequest, source, true)
		if edgeErr != nil {
			return nil, edgeErr
		}
		dependencyResolution = &models.RunDependencyResolutionRecord{
			Scope: updatedRequest.Scope, GroupID: updatedRequest.DependencyGroupID, DependencyID: updatedRequest.DependencyID,
			ExpectedDependencyRevision: edge.Revision, State: dependencyState,
			Result: map[string]interface{}{
				"requestId": updatedRequest.ID, "childRunId": updatedRequest.ChildRunID,
				"summary": updatedRequest.CompletionSummary, "review": summary,
				"acceptanceEvidence": maps.Clone(updatedRequest.AcceptanceEvidence), "output": maps.Clone(child.Output),
			},
			Error: updatedRequest.ResolutionReason, Artifacts: updatedRequest.Artifacts,
			Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID}, Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
		}
	}
	record := models.AgentRequestCompletionRecord{
		Request: updatedRequest, ExpectedRequestRevision: request.Revision,
		SourceRun: updatedSource, ExpectedSourceRevision: source.Revision, DependencyResolution: dependencyResolution,
		ChildRun: updatedChild, ExpectedChildRevision: child.Revision,
		SourceEvent: collaborationCompletionEvent(source, updatedRequest, eventType, eventSummary, actor, now),
		ChildEvent:  collaborationCompletionEvent(child, updatedRequest, eventType, eventSummary, actor, now),
	}
	events, err := s.store.CompleteAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if dependencyResolution != nil {
		updatedSource, err = s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	}
	return &AgentRequestResult{Request: models.CloneAgentRequest(updatedRequest), Source: models.CloneAgentRun(updatedSource), Child: models.CloneAgentRun(updatedChild), Events: events}, nil
}

// ResolveTerminalAgentRequestChild closes the collaboration lifecycle from an
// authoritative terminal child Run. Artifact-bearing requests remain accepted
// until their required immutable artifacts are explicitly supplied.
func (s *CollaborationService) ResolveTerminalAgentRequestChild(ctx context.Context, child *models.AgentRun) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil || child == nil {
		return nil, errors.New("collaboration service and child run are required")
	}
	collaboration, ok := child.Context["collaboration"].(map[string]interface{})
	if !ok {
		return nil, nil
	}
	requestID, _ := collaboration["requestId"].(string)
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return nil, nil
	}
	request, err := s.GetAgentRequest(ctx, child.Scope, requestID)
	if err != nil {
		return nil, err
	}
	if request.ChildRunID != child.ID {
		return nil, models.ErrInvalidAgentRequestState
	}
	if request.Status == models.AgentRequestStatusCompletionReview || request.Status == models.AgentRequestStatusCompleted || request.Status == models.AgentRequestStatusFailed || request.Status == models.AgentRequestStatusCanceled {
		return &AgentRequestResult{Request: request, Child: child}, nil
	}
	if request.Status != models.AgentRequestStatusAccepted {
		return nil, models.ErrInvalidAgentRequestState
	}
	if child.Status == models.AgentRunStatusCompleted {
		if len(request.ArtifactRequirements) > 0 {
			artifacts, present, err := terminalChildArtifactReferences(child.Output)
			if err != nil {
				return nil, err
			}
			if !present {
				return &AgentRequestResult{Request: request, Child: child}, nil
			}
			evidence := map[string]interface{}{"runOutput": maps.Clone(child.Output)}
			return s.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
				Scope: child.Scope, RequestID: request.ID, ExpectedRevision: request.Revision, ExpectedChildRevision: child.Revision,
				Principal: request.Recipient, Actor: request.Recipient, Summary: terminalChildSummary(child.Output),
				AcceptanceEvidence: evidence, Artifacts: artifacts, CompletionKey: "terminal-child:" + child.ID,
			})
		}
		evidence := map[string]interface{}{}
		if len(request.AcceptanceCriteria) > 0 {
			evidence["runOutput"] = maps.Clone(child.Output)
			if len(child.Output) == 0 {
				evidence["runStatus"] = string(child.Status)
			}
		}
		return s.CompleteAgentRequest(ctx, CompleteAgentRequestRequest{
			Scope: child.Scope, RequestID: request.ID, ExpectedRevision: request.Revision, ExpectedChildRevision: child.Revision,
			Principal: request.Recipient, Actor: request.Recipient, Summary: terminalChildSummary(child.Output),
			AcceptanceEvidence: evidence, CompletionKey: "terminal-child:" + child.ID,
		})
	}
	if child.Status != models.AgentRunStatusFailed && child.Status != models.AgentRunStatusCanceled {
		return nil, models.ErrInvalidAgentRequestState
	}
	now := s.now()
	updated := models.CloneAgentRequest(request)
	updated.Status = models.AgentRequestStatusFailed
	if child.Status == models.AgentRunStatusCanceled {
		updated.Status = models.AgentRequestStatusCanceled
	}
	updated.ResolutionReason = strings.TrimSpace(child.Error)
	if updated.ResolutionReason == "" {
		updated.ResolutionReason = "delegated child run " + string(child.Status)
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.ResolvedAt = &now
	record := models.AgentRequestResponseRecord{Request: updated, ExpectedRequestRevision: request.Revision}
	source, err := s.runs.GetAgentRun(ctx, child.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, models.ErrRunNotFound
	}
	if request.DependencyGroupID != "" {
		edge, edgeErr := s.groupedRequestDependency(ctx, request, source, true)
		if edgeErr != nil {
			return nil, edgeErr
		}
		state := models.RunDependencyStateFailed
		if child.Status == models.AgentRunStatusCanceled {
			state = models.RunDependencyStateCanceled
		}
		record.DependencyResolution = &models.RunDependencyResolutionRecord{
			Scope: request.Scope, GroupID: request.DependencyGroupID, DependencyID: request.DependencyID,
			ExpectedDependencyRevision: edge.Revision, State: state, Error: updated.ResolutionReason,
			Actor:      models.ActivityActor{Type: string(request.Recipient.Type), ID: request.Recipient.ID},
			Visibility: models.ActivityVisibilityTeam, OccurredAt: now,
		}
	} else {
		record.SourceRun, err = failedCollaborationSourceRun(source, updated, now)
		if err != nil {
			return nil, err
		}
		record.ExpectedSourceRevision = source.Revision
	}
	eventType := "collaboration.failed"
	if request.Kind == models.AgentRequestKindHandoff {
		eventType = "handoff.failed"
	} else if request.Kind == models.AgentRequestKindEscalation {
		eventType = "escalation.failed"
	}
	if child.Status == models.AgentRunStatusCanceled {
		eventType = "collaboration.canceled"
		if request.Kind == models.AgentRequestKindHandoff {
			eventType = "handoff.canceled"
		} else if request.Kind == models.AgentRequestKindEscalation {
			eventType = "escalation.canceled"
		}
	}
	summary := fmt.Sprintf("%s %s ended delegated work: %s", request.Recipient.Type, request.Recipient.ID, updated.ResolutionReason)
	record.SourceEvent = collaborationEvent(source, updated, eventType, summary, request.Recipient, now)
	record.ChildEvent = collaborationEvent(child, updated, eventType, summary, request.Recipient, now)
	events, err := s.store.RespondAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	if record.DependencyResolution != nil {
		record.SourceRun, err = s.runs.GetAgentRun(ctx, child.Scope, request.SourceRunID)
		if err != nil {
			return nil, err
		}
	}
	return &AgentRequestResult{Request: updated, Source: record.SourceRun, Child: child, Events: events}, nil
}

func terminalChildArtifactReferences(output map[string]interface{}) ([]models.ArtifactReference, bool, error) {
	if len(output) == 0 {
		return nil, false, nil
	}
	raw, present := output["artifactRefs"]
	if !present {
		raw, present = output["artifacts"]
	}
	if !present {
		return nil, false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, true, fmt.Errorf("encode terminal child artifact references: %w", err)
	}
	var references []models.ArtifactReference
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&references); err != nil {
		return nil, true, fmt.Errorf("decode terminal child artifact references: %w", err)
	}
	if len(references) == 0 {
		return nil, true, errors.New("terminal child artifact references cannot be empty")
	}
	return references, true, nil
}

func (s *CollaborationService) groupedRequestDependency(ctx context.Context, request *models.AgentRequest, source *models.AgentRun, allowTerminalGroup bool) (*models.RunDependency, error) {
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

func (s *CollaborationService) GetAgentRequest(ctx context.Context, scope models.Scope, requestID string) (*models.AgentRequest, error) {
	request, err := s.store.GetAgentRequest(ctx, scope, requestID)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	return request, nil
}

func (s *CollaborationService) ListAgentRequests(ctx context.Context, filter models.AgentRequestFilter) ([]*models.AgentRequest, error) {
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
	for _, key := range []string{models.ConversationRunConversationIDContextKey, models.ConversationRunTriggerIDContextKey, conversation.ConversationRunContextTriggerSeq, "threadRootMessageId", "responseMode"} {
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
func (s *CollaborationService) validateChatDelegation(ctx context.Context, source *models.AgentRun, recipient models.CollaborationParty) error {
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
func (s *CollaborationService) verifyChatAssignment(ctx context.Context, source *models.AgentRun, assignedAgentID string) error {
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

func (s *CollaborationService) resolveTeamRequestCandidates(ctx context.Context, request *models.AgentRequest) ([]models.AgentRequestCandidate, error) {
	if s.teams == nil {
		return nil, fmt.Errorf("%w: Team roster assignment is unavailable", models.ErrAgentRequestAssignment)
	}
	scope := capability.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}
	deployment, err := s.teams.GetTeamDeployment(ctx, scope, request.Recipient.ID)
	if err != nil {
		return nil, err
	}
	if deployment == nil || deployment.Status != kernelteam.DeploymentActive {
		return nil, fmt.Errorf("%w: recipient Team deployment is not active", models.ErrAgentRequestAssignment)
	}
	agents, hasAgentCatalog := s.store.(collaborationAgentStore)
	candidates := make([]models.AgentRequestCandidate, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		if request.SemanticRole != "" && assignment.ID != request.SemanticRole {
			continue
		}
		limit := 1
		if hasAgentCatalog {
			agentDeployment, getErr := agents.GetDeployment(ctx, scope, assignment.AgentDeploymentID)
			if getErr != nil && !errors.Is(getErr, kernelagent.ErrDeploymentNotFound) {
				return nil, getErr
			}
			if agentDeployment == nil {
				if len(assignment.RequiredSkillIDs) > 0 {
					return nil, fmt.Errorf("%w: Agent authority catalog cannot verify member %q", models.ErrAgentRequestAssignment, assignment.ID)
				}
			} else if agentDeployment.RolloutStatus != kernelagent.RolloutActive {
				continue
			} else {
				agentDefinition, getErr := agents.GetDefinition(ctx, agentDeployment.DefinitionID, agentDeployment.ActiveVersion)
				if getErr != nil {
					return nil, getErr
				}
				if agentDefinition == nil || !agentDefinitionProvidesSkills(agentDefinition, assignment.RequiredSkillIDs) {
					continue
				}
				limit = agentDeployment.Capacity.MaxConcurrentRuns
				if agentDeployment.Restrictions.MaxConcurrentRuns != nil && *agentDeployment.Restrictions.MaxConcurrentRuns < limit {
					limit = *agentDeployment.Restrictions.MaxConcurrentRuns
				}
			}
		} else if len(assignment.RequiredSkillIDs) > 0 {
			return nil, fmt.Errorf("%w: Agent authority catalog is required to evaluate member %q", models.ErrAgentRequestAssignment, assignment.ID)
		}
		runs, listErr := s.runs.ListAgentRuns(ctx, models.AgentRunFilter{Scope: request.Scope, AssignedAgentID: assignment.AgentDeploymentID, Limit: 500})
		if listErr != nil {
			return nil, listErr
		}
		active := 0
		for _, run := range runs {
			if !models.IsTerminalAgentRunStatus(run.Status) {
				active++
			}
		}
		available := limit - active
		if available < 0 {
			available = 0
		}
		candidates = append(candidates, models.AgentRequestCandidate{
			AgentDeploymentID: assignment.AgentDeploymentID, RosterAssignmentID: assignment.ID,
			RequiredSkillIDs: append([]string(nil), assignment.RequiredSkillIDs...), CapacityLimit: limit, ActiveRuns: active, AvailableSlots: available,
		})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].AgentDeploymentID < candidates[j].AgentDeploymentID })
	if len(candidates) == 0 {
		return nil, fmt.Errorf("%w: recipient Team has no eligible Agent for semantic role %q", models.ErrAgentRequestAssignment, request.SemanticRole)
	}
	return candidates, nil
}

func agentDefinitionProvidesSkills(definition *kernelagent.AgentDefinition, required []string) bool {
	if len(required) == 0 {
		return true
	}
	provided := make(map[string]struct{}, len(definition.Authority.AllowedSkillIDs)+len(definition.SkillRequirements))
	for _, id := range definition.Authority.AllowedSkillIDs {
		provided[strings.TrimSpace(id)] = struct{}{}
	}
	for _, requirement := range definition.SkillRequirements {
		provided[strings.TrimSpace(requirement.SkillID)] = struct{}{}
	}
	for _, id := range required {
		if _, ok := provided[strings.TrimSpace(id)]; !ok {
			return false
		}
	}
	return true
}

func (s *CollaborationService) SubmitAgentRequestBid(ctx context.Context, req SubmitAgentRequestBidRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	request, err := s.store.GetAgentRequest(ctx, req.Scope, strings.TrimSpace(req.RequestID))
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, models.ErrAgentRequestNotFound
	}
	if request.Revision != req.ExpectedRevision || request.Status != models.AgentRequestStatusPending || request.Recipient.Type != models.OwnerTypeTeam {
		return nil, models.ErrInvalidAgentRequestState
	}
	agentID := strings.TrimSpace(req.AgentDeploymentID)
	key := strings.TrimSpace(req.IdempotencyKey)
	if agentID == "" || key == "" || req.Decision != models.AgentRequestBidAccept && req.Decision != models.AgentRequestBidDecline {
		return nil, errors.New("Team work bid requires an eligible Agent, accept or decline decision, and idempotency key")
	}
	eligible := false
	for _, candidate := range request.Candidates {
		if candidate.AgentDeploymentID == agentID {
			eligible = true
			break
		}
	}
	if !eligible {
		return nil, fmt.Errorf("%w: bidding Agent is not an eligible Team member", models.ErrAgentRequestAssignment)
	}
	for _, bid := range request.Bids {
		if bid.AgentDeploymentID == agentID {
			if bid.IdempotencyKey == key && bid.Decision == req.Decision && bid.Reason == strings.TrimSpace(req.Reason) {
				return &AgentRequestResult{Request: request}, nil
			}
			return nil, models.ErrAgentRequestIdempotency
		}
	}
	now := s.now()
	updated := models.CloneAgentRequest(request)
	updated.Bids = append(updated.Bids, models.AgentRequestBid{AgentDeploymentID: agentID, Decision: req.Decision, Reason: strings.TrimSpace(req.Reason), IdempotencyKey: key, SubmittedAt: now})
	updated.Revision++
	updated.UpdatedAt = now
	source, err := s.runs.GetAgentRun(ctx, req.Scope, request.SourceRunID)
	if err != nil {
		return nil, err
	}
	event := collaborationEvent(source, updated, "collaboration.work_bid_submitted", fmt.Sprintf("Agent %s %sed the Team work offer", agentID, req.Decision), models.CollaborationParty{Type: models.OwnerTypeAgent, ID: agentID}, now)
	persisted, err := s.store.CoordinateAgentRequest(ctx, models.AgentRequestCoordinationRecord{Request: updated, ExpectedRequestRevision: request.Revision, Event: event})
	if err != nil {
		return nil, err
	}
	return &AgentRequestResult{Request: updated, Source: source, Events: []*models.ActivityEvent{persisted}}, nil
}

func (s *CollaborationService) resolveRequestAssignment(ctx context.Context, request *models.AgentRequest, assignedAgentID string) (string, error) {
	assignedAgentID = strings.TrimSpace(assignedAgentID)
	if request.Recipient.Type == models.OwnerTypeAgent {
		if assignedAgentID != "" && assignedAgentID != request.Recipient.ID {
			return "", fmt.Errorf("%w: assigned Agent does not match the recipient", models.ErrAgentRequestAssignment)
		}
		return request.Recipient.ID, nil
	}
	if s.teams == nil {
		return "", fmt.Errorf("%w: Team roster assignment is unavailable", models.ErrAgentRequestAssignment)
	}
	deployment, err := s.teams.GetTeamDeployment(ctx, capability.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}, request.Recipient.ID)
	if err != nil {
		return "", err
	}
	if deployment == nil || deployment.Status != kernelteam.DeploymentActive {
		return "", fmt.Errorf("%w: recipient Team deployment is not active", models.ErrAgentRequestAssignment)
	}
	eligible := make([]kernelteam.RosterAssignment, 0, len(deployment.Roster))
	for _, assignment := range deployment.Roster {
		if request.SemanticRole == "" || assignment.ID == request.SemanticRole {
			eligible = append(eligible, assignment)
		}
	}
	if assignedAgentID == "" {
		if len(eligible) == 0 {
			return "", fmt.Errorf("%w: recipient Team has no Agent for semantic role %q", models.ErrAgentRequestAssignment, request.SemanticRole)
		}
		accepted := acceptedTeamRequestCandidates(request)
		if len(accepted) > 0 {
			return accepted[0].AgentDeploymentID, nil
		}
		if len(eligible) == 1 {
			return eligible[0].AgentDeploymentID, nil
		}
		return "", fmt.Errorf("%w: Team request acceptance requires an explicit assigned Agent or accepted bid because %d roster members are eligible", models.ErrAgentRequestAssignment, len(eligible))
	}
	for _, assignment := range eligible {
		if assignment.AgentDeploymentID == assignedAgentID {
			return assignedAgentID, nil
		}
	}
	return "", fmt.Errorf("%w: assigned Agent is not an eligible member of the recipient Team", models.ErrAgentRequestAssignment)
}

func acceptedTeamRequestCandidates(request *models.AgentRequest) []models.AgentRequestCandidate {
	accepted := make(map[string]struct{})
	for _, bid := range request.Bids {
		if bid.Decision == models.AgentRequestBidAccept {
			accepted[bid.AgentDeploymentID] = struct{}{}
		}
	}
	result := make([]models.AgentRequestCandidate, 0, len(accepted))
	for _, candidate := range request.Candidates {
		if _, ok := accepted[candidate.AgentDeploymentID]; ok && candidate.AvailableSlots > 0 {
			result = append(result, candidate)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AvailableSlots != result[j].AvailableSlots {
			return result[i].AvailableSlots > result[j].AvailableSlots
		}
		if result[i].ActiveRuns != result[j].ActiveRuns {
			return result[i].ActiveRuns < result[j].ActiveRuns
		}
		return result[i].AgentDeploymentID < result[j].AgentDeploymentID
	})
	return result
}

func teamAssignmentReason(request *models.AgentRequest, selected string) string {
	accepted := acceptedTeamRequestCandidates(request)
	for index, candidate := range accepted {
		if candidate.AgentDeploymentID == selected {
			return fmt.Sprintf("selected accepted bid rank %d by available capacity (%d slots, %d active Runs)", index+1, candidate.AvailableSlots, candidate.ActiveRuns)
		}
	}
	if len(request.Candidates) == 1 {
		return "selected the only eligible roster member under the reviewed Team role policy"
	}
	return "selected explicitly from the eligible roster under the reviewed Team role policy"
}

func sourceWaitsForAgentRequestDecision(source *models.AgentRun, requestID string) bool {
	return source != nil && source.Status == models.AgentRunStatusWaitingForAgent && source.WakeCondition != nil &&
		source.WakeCondition.Type == "agent_request_decision" &&
		source.WakeCondition.Reference == strings.TrimSpace(requestID)
}

func sourceAwaitingAgentRequestDecision(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	if source == nil || request == nil {
		return nil, models.ErrRunNotFound
	}
	if sourceWaitsForAgentRequestDecision(source, request.ID) {
		return models.CloneAgentRun(source), nil
	}
	if !CanStartAgentRequestFromRun(source) {
		return nil, fmt.Errorf("%w: source run cannot await request decision from %s", models.ErrInvalidAgentRequestState, source.Status)
	}
	updated := models.CloneAgentRun(source)
	updated.Status = models.AgentRunStatusWaitingForAgent
	updated.WakeCondition = &models.WakeCondition{Type: "agent_request_decision", Reference: request.ID}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	return updated, updated.Validate()
}

func sourceResumingAfterAgentRequestDecision(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	if source == nil || request == nil {
		return nil, models.ErrRunNotFound
	}
	if !sourceWaitsForAgentRequestDecision(source, request.ID) {
		return nil, fmt.Errorf("%w: source run is not awaiting request %s", models.ErrInvalidAgentRequestState, request.ID)
	}
	updated := models.CloneAgentRun(source)
	updated.Status = models.AgentRunStatusQueued
	updated.WakeCondition = nil
	updated.Revision++
	updated.UpdatedAt = now
	updated.AvailableAt = now
	updated.QueueEnteredAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	if updated.Output == nil {
		updated.Output = make(map[string]interface{})
	}
	results, _ := updated.Output["collaborationResults"].(map[string]interface{})
	results = maps.Clone(results)
	if results == nil {
		results = make(map[string]interface{})
	}
	results[request.ID] = map[string]interface{}{
		"requestId": request.ID, "status": request.Status, "message": request.Response,
		"clarification": request.Clarification, "recipient": request.Recipient,
	}
	updated.Output["collaborationResults"] = results
	return updated, updated.Validate()
}

func acceptedSourceRun(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	updated := models.CloneAgentRun(source)
	if err := models.AddRunBudgetAllocation(updated, request.ID, request.BudgetAllocation); err != nil {
		return nil, err
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	requiresCompletionReview := request.DelegationPolicy != nil && request.DelegationPolicy.RequireCompletionReview
	if request.Kind == models.AgentRequestKindHandoff && !requiresCompletionReview {
		updated.Status = models.AgentRunStatusCompleted
		updated.WakeCondition = nil
		updated.Output = map[string]interface{}{"handoffRequestId": request.ID, "childRunId": request.ChildRunID}
		updated.CompletedAt = &now
	} else {
		updated.Status = models.AgentRunStatusWaitingForDependency
		updated.WakeCondition = &models.WakeCondition{Type: "agent_request", Reference: request.ID}
	}
	return updated, nil
}

func completedCollaborationChildRun(child *models.AgentRun, request *models.AgentRequest, now time.Time) *models.AgentRun {
	updated := models.CloneAgentRun(child)
	sharedOutput := maps.Clone(updated.Output)
	updated.Status = models.AgentRunStatusCompleted
	updated.Revision++
	updated.UpdatedAt = now
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.WakeCondition = nil
	updated.Output = collaborationCompletionOutput(updated.Output, request, sharedOutput)
	return updated
}

func completedCollaborationSourceRun(source *models.AgentRun, request *models.AgentRequest, childOutput map[string]interface{}, now time.Time) (*models.AgentRun, error) {
	updated := models.CloneAgentRun(source)
	if updated.Status == models.AgentRunStatusWaitingForDependency && updated.WakeCondition != nil &&
		updated.WakeCondition.Type == "agent_request" && updated.WakeCondition.Reference == request.ID {
		updated.WakeCondition = nil
		updated.LastWakeSignalID = "agent_request:" + request.ID + ":" + fmt.Sprint(request.Revision)
		if request.Kind == models.AgentRequestKindHandoff {
			updated.Status = models.AgentRunStatusCompleted
			updated.CompletedAt = &now
		} else {
			updated.Status = models.AgentRunStatusQueued
			updated.AvailableAt = now
			updated.QueueEnteredAt = now
		}
	} else if updated.Status != models.AgentRunStatusCompleted {
		return nil, fmt.Errorf("%w: source run is not waiting on request %s", models.ErrInvalidAgentRequestState, request.ID)
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Output = collaborationCompletionOutput(updated.Output, request, childOutput)
	return updated, nil
}

func failedCollaborationSourceRun(source *models.AgentRun, request *models.AgentRequest, now time.Time) (*models.AgentRun, error) {
	updated := models.CloneAgentRun(source)
	if updated.Status == models.AgentRunStatusWaitingForDependency && updated.WakeCondition != nil &&
		updated.WakeCondition.Type == "agent_request" && updated.WakeCondition.Reference == request.ID {
		updated.Status = models.AgentRunStatusQueued
		updated.AvailableAt = now
		updated.QueueEnteredAt = now
		updated.WakeCondition = nil
		updated.LastWakeSignalID = "agent_request:" + request.ID + ":" + fmt.Sprint(request.Revision)
	} else if updated.Status != models.AgentRunStatusCompleted {
		return nil, fmt.Errorf("%w: source run is not waiting on request %s", models.ErrInvalidAgentRequestState, request.ID)
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Output = collaborationResolutionOutput(updated.Output, request)
	return updated, nil
}

func collaborationCompletionOutput(existing map[string]interface{}, request *models.AgentRequest, childOutput map[string]interface{}) map[string]interface{} {
	result := maps.Clone(existing)
	if result == nil {
		result = make(map[string]interface{})
	}
	results := make(map[string]interface{})
	if current, ok := result["collaborationResults"].(map[string]interface{}); ok {
		for key, value := range current {
			results[key] = value
		}
	}
	results[request.ID] = map[string]interface{}{
		"requestId": request.ID, "kind": request.Kind, "childRunId": request.ChildRunID,
		"status":  request.Status,
		"summary": request.CompletionSummary, "acceptanceEvidence": maps.Clone(request.AcceptanceEvidence),
		"artifacts": models.CloneArtifactReferences(request.Artifacts), "output": maps.Clone(childOutput),
	}
	result["collaborationResults"] = results
	return result
}

func collaborationResolutionOutput(existing map[string]interface{}, request *models.AgentRequest) map[string]interface{} {
	result := maps.Clone(existing)
	if result == nil {
		result = make(map[string]interface{})
	}
	results := make(map[string]interface{})
	if current, ok := result["collaborationResults"].(map[string]interface{}); ok {
		for key, value := range current {
			results[key] = value
		}
	}
	results[request.ID] = map[string]interface{}{
		"requestId": request.ID, "kind": request.Kind, "childRunId": request.ChildRunID,
		"status": request.Status, "reason": request.ResolutionReason,
	}
	result["collaborationResults"] = results
	return result
}

func terminalChildSummary(output map[string]interface{}) string {
	preferred := []string{"summary", "response", "result", "outcome", "followUp"}
	for _, key := range preferred {
		if value, ok := output[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	keys := make([]string, 0, len(output))
	for key := range output {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if value, ok := output[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return "Completed delegated work"
}

func collaborationEvent(run *models.AgentRun, request *models.AgentRequest, eventType, summary string, actor models.CollaborationParty, now time.Time) *models.ActivityEvent {
	teamID := models.TeamIDForRun(run)
	if teamID == "" && request.Recipient.Type == models.OwnerTypeTeam {
		teamID = request.Recipient.ID
	}
	return &models.ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: eventType, Severity: models.ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: teamID, ConversationRefs: append([]string(nil), request.ConversationRefs...),
		Actor: models.ActivityActor{Type: string(actor.Type), ID: actor.ID}, Summary: summary, Visibility: models.ActivityVisibilityTeam,
		CorrelationID: request.ID, CausationID: request.SourceRunID, CreatedAt: now,
		Payload: map[string]interface{}{
			"requestId": request.ID, "kind": request.Kind, "status": request.Status, "recipient": request.Recipient,
			"semanticRole": request.SemanticRole, "acceptancePolicy": request.AcceptancePolicy,
			"assignedAgentId": request.AssignedAgentID, "childRunId": request.ChildRunID,
		},
	}
}

func collaborationCompletionEvent(run *models.AgentRun, request *models.AgentRequest, eventType, summary string, actor models.CollaborationParty, now time.Time) *models.ActivityEvent {
	event := collaborationEvent(run, request, eventType, summary, actor, now)
	artifacts := make([]map[string]interface{}, 0, len(request.Artifacts))
	for _, artifact := range request.Artifacts {
		artifacts = append(artifacts, map[string]interface{}{
			"id": artifact.ID, "version": artifact.Version, "requirementName": artifact.RequirementName, "name": artifact.Name,
			"type": artifact.Type, "mediaType": artifact.MediaType, "digest": artifact.Digest, "sizeBytes": artifact.SizeBytes,
		})
	}
	evidenceKeys := make([]string, 0, len(request.AcceptanceEvidence))
	for key := range request.AcceptanceEvidence {
		evidenceKeys = append(evidenceKeys, key)
	}
	sort.Strings(evidenceKeys)
	event.Payload["completionSummary"] = request.CompletionSummary
	event.Payload["artifacts"] = artifacts
	event.Payload["acceptanceEvidenceKeys"] = evidenceKeys
	return event
}

func validateArtifactReferenceIntents(requirements []models.ArtifactRequirement, references []models.ArtifactReference) error {
	requirementsByName := make(map[string]models.ArtifactRequirement, len(requirements))
	for _, requirement := range requirements {
		requirementsByName[strings.TrimSpace(requirement.Name)] = requirement
	}
	seen := make(map[string]struct{}, len(references))
	provided := make(map[string]struct{}, len(references))
	for index, reference := range references {
		id := strings.TrimSpace(reference.ID)
		if id == "" || reference.Version <= 0 {
			return fmt.Errorf("artifact reference %d id and positive version are required", index)
		}
		key := fmt.Sprintf("%s:%d", id, reference.Version)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("artifact reference %q version %d is duplicated", id, reference.Version)
		}
		seen[key] = struct{}{}
		if reference.Name != "" || reference.Type != "" || reference.MediaType != "" || reference.ContentRef != "" ||
			reference.Digest != "" || reference.SizeBytes != 0 || reference.Metadata != nil || len(reference.EvidenceRefs) > 0 {
			return fmt.Errorf("artifact reference %q must contain only id, version, and requirementName", id)
		}
		requirementName := strings.TrimSpace(reference.RequirementName)
		if requirementName == "" {
			continue
		}
		if _, exists := requirementsByName[requirementName]; !exists {
			return fmt.Errorf("artifact reference %q names unknown requirement %q", id, requirementName)
		}
		provided[requirementName] = struct{}{}
	}
	for _, requirement := range requirements {
		if requirement.Required {
			if _, exists := provided[strings.TrimSpace(requirement.Name)]; !exists {
				return fmt.Errorf("required artifact %q was not provided", requirement.Name)
			}
		}
	}
	return nil
}

func (s *CollaborationService) resolveArtifactReferences(ctx context.Context, scope models.Scope, requirements []models.ArtifactRequirement, references []models.ArtifactReference) ([]models.ArtifactReference, error) {
	if err := validateArtifactReferenceIntents(requirements, references); err != nil {
		return nil, err
	}
	resolved := make([]models.ArtifactReference, 0, len(references))
	for _, reference := range references {
		artifact, err := s.artifacts.GetArtifact(ctx, scope, strings.TrimSpace(reference.ID), reference.Version)
		if err != nil {
			return nil, err
		}
		if artifact == nil {
			return nil, fmt.Errorf("%w: %s version %d", artifactcatalog.ErrArtifactNotFound, reference.ID, reference.Version)
		}
		evidenceRefs := make([]string, 0, len(artifact.Evidence))
		for _, evidence := range artifact.Evidence {
			evidenceRefs = append(evidenceRefs, evidence.TargetRef)
		}
		resolved = append(resolved, models.ArtifactReference{
			ID: artifact.ID, Version: artifact.Version, RequirementName: strings.TrimSpace(reference.RequirementName),
			Name: artifact.Name, Type: artifact.Type, MediaType: artifact.MediaType, ContentRef: artifact.ContentRef,
			Digest: artifact.Digest, SizeBytes: artifact.SizeBytes, Metadata: maps.Clone(artifact.Metadata), EvidenceRefs: evidenceRefs,
		})
	}
	if err := models.ValidateArtifactReferences(requirements, resolved); err != nil {
		return nil, err
	}
	return normalizeArtifactReferences(resolved), nil
}

func sameAgentRequestIntent(existing *models.AgentRequest, req CreateAgentRequestRequest) bool {
	acceptancePolicy := models.NormalizeAgentRequestAcceptancePolicy(req.AcceptancePolicy)
	if existing.DelegationPolicy != nil && existing.DelegationPolicy.RequireAcceptance {
		acceptancePolicy = models.AgentRequestAcceptanceRecipientReview
	}
	return existing.Kind == req.Kind && existing.Requester == req.Requester && existing.Recipient == req.Recipient &&
		existing.SourceRunID == strings.TrimSpace(req.SourceRunID) && existing.Goal == strings.TrimSpace(req.Goal) &&
		existing.Instructions == strings.TrimSpace(req.Instructions) && existing.SemanticRole == strings.TrimSpace(req.SemanticRole) &&
		existing.AcceptancePolicy == acceptancePolicy &&
		existing.DependencyGroupID == strings.TrimSpace(req.DependencyGroupID) && existing.DependencyID == strings.TrimSpace(req.DependencyID) &&
		sameJSONValue(existing.AcceptanceCriteria, req.AcceptanceCriteria) &&
		sameJSONValue(existing.ArtifactRequirements, req.ArtifactRequirements) &&
		sameJSONValue(existing.SharedContext, req.SharedContext) &&
		sameJSONValue(existing.ChildCheckpoint, req.ChildCheckpoint) &&
		sameJSONValue(existing.ConversationRefs, req.ConversationRefs) &&
		sameBudgetPolicy(existing.BudgetAllocation, req.BudgetAllocation)
}

func sameJSONValue(left, right interface{}) bool {
	if emptyJSONCollection(left) && emptyJSONCollection(right) {
		return true
	}
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func emptyJSONCollection(value interface{}) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	return (reflected.Kind() == reflect.Map || reflected.Kind() == reflect.Slice) && reflected.Len() == 0
}

func sameBudgetPolicy(left, right *models.BudgetPolicy) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func StableCollaborationID(scope models.Scope, key, suffix string) string {
	seed := strings.Join([]string{"openseal", "collaboration", scope.Key(), strings.TrimSpace(key), strings.TrimSpace(suffix)}, ":")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(seed)).String()
}

func sameAgentRequestCompletion(existing *models.AgentRequest, req CompleteAgentRequestRequest, artifacts []models.ArtifactReference) bool {
	left, _ := json.Marshal(struct {
		Summary   string
		Evidence  map[string]interface{}
		Artifacts []models.ArtifactReference
	}{existing.CompletionSummary, existing.AcceptanceEvidence, existing.Artifacts})
	right, _ := json.Marshal(struct {
		Summary   string
		Evidence  map[string]interface{}
		Artifacts []models.ArtifactReference
	}{strings.TrimSpace(req.Summary), req.AcceptanceEvidence, artifacts})
	return bytes.Equal(left, right)
}

func normalizeArtifactReferences(in []models.ArtifactReference) []models.ArtifactReference {
	result := models.CloneArtifactReferences(in)
	for index := range result {
		result[index].ID = strings.TrimSpace(result[index].ID)
		result[index].RequirementName = strings.TrimSpace(result[index].RequirementName)
		result[index].Name = strings.TrimSpace(result[index].Name)
		result[index].Type = strings.TrimSpace(result[index].Type)
		result[index].MediaType = strings.TrimSpace(result[index].MediaType)
		result[index].ContentRef = strings.TrimSpace(result[index].ContentRef)
		result[index].Digest = strings.ToLower(strings.TrimSpace(result[index].Digest))
		for evidenceIndex := range result[index].EvidenceRefs {
			result[index].EvidenceRefs[evidenceIndex] = strings.TrimSpace(result[index].EvidenceRefs[evidenceIndex])
		}
	}
	return result
}
