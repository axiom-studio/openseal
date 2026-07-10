package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrAgentRequestNotFound     = errors.New("agent request not found")
	ErrInvalidAgentRequestState = errors.New("invalid agent request state")
	ErrAgentRequestUnauthorized = errors.New("agent request principal is not authorized")
	ErrAgentRequestIdempotency  = errors.New("agent request idempotency conflict")
	ErrUnsafeSharedContext      = errors.New("shared context cannot contain credentials or secrets")
)

type AgentRequestKind string

const (
	AgentRequestKindRequest AgentRequestKind = "request"
	AgentRequestKindHandoff AgentRequestKind = "handoff"
)

type AgentRequestStatus string

const (
	AgentRequestStatusPending                AgentRequestStatus = "pending"
	AgentRequestStatusClarificationRequested AgentRequestStatus = "clarification_requested"
	AgentRequestStatusAccepted               AgentRequestStatus = "accepted"
	AgentRequestStatusRejected               AgentRequestStatus = "rejected"
	AgentRequestStatusCanceled               AgentRequestStatus = "canceled"
)

type AgentRequestDecision string

const (
	AgentRequestDecisionAccept               AgentRequestDecision = "accept"
	AgentRequestDecisionReject               AgentRequestDecision = "reject"
	AgentRequestDecisionRequestClarification AgentRequestDecision = "request_clarification"
	AgentRequestDecisionProvideClarification AgentRequestDecision = "provide_clarification"
)

// CollaborationParty identifies an agent or Team without coupling OpenSeal to
// an enterprise identity system. Semantic role labels live on the request and
// are deliberately independent from permissions and approval authority.
type CollaborationParty struct {
	Type OwnerType `json:"type"`
	ID   string    `json:"id"`
}

func (p CollaborationParty) Validate() error {
	return ObjectiveOwner{Type: p.Type, ID: p.ID}.Validate()
}

type ArtifactRequirement struct {
	Name        string                 `json:"name"`
	Type        string                 `json:"type,omitempty"`
	Description string                 `json:"description,omitempty"`
	Schema      map[string]interface{} `json:"schema,omitempty"`
	Required    bool                   `json:"required"`
}

// AgentRequest is the durable collaboration fact shared by agent, Team, chat,
// and activity projections. It contains references and explicitly shared
// context only; credential values and bindings are never transferable.
type AgentRequest struct {
	ID                   string                 `json:"id"`
	Scope                Scope                  `json:"scope"`
	Kind                 AgentRequestKind       `json:"kind"`
	Status               AgentRequestStatus     `json:"status"`
	Requester            CollaborationParty     `json:"requester"`
	Recipient            CollaborationParty     `json:"recipient"`
	SourceRunID          string                 `json:"sourceRunId"`
	ChildRunID           string                 `json:"childRunId,omitempty"`
	ObjectiveID          string                 `json:"objectiveId,omitempty"`
	Goal                 string                 `json:"goal"`
	Instructions         string                 `json:"instructions,omitempty"`
	SemanticRole         string                 `json:"semanticRole,omitempty"`
	AcceptanceCriteria   map[string]interface{} `json:"acceptanceCriteria,omitempty"`
	ArtifactRequirements []ArtifactRequirement  `json:"artifactRequirements,omitempty"`
	SharedContext        map[string]interface{} `json:"sharedContext,omitempty"`
	ConversationRefs     []string               `json:"conversationRefs,omitempty"`
	Clarification        string                 `json:"clarification,omitempty"`
	Response             string                 `json:"response,omitempty"`
	IdempotencyKey       string                 `json:"idempotencyKey,omitempty"`
	Revision             int64                  `json:"revision"`
	CreatedAt            time.Time              `json:"createdAt"`
	UpdatedAt            time.Time              `json:"updatedAt"`
	ResolvedAt           *time.Time             `json:"resolvedAt,omitempty"`
}

func (r *AgentRequest) Validate() error {
	if r == nil {
		return errors.New("agent request is required")
	}
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.SourceRunID) == "" || strings.TrimSpace(r.Goal) == "" {
		return errors.New("agent request id, source run, and goal are required")
	}
	if r.Kind != AgentRequestKindRequest && r.Kind != AgentRequestKindHandoff {
		return errors.New("agent request kind must be request or handoff")
	}
	if err := r.Requester.Validate(); err != nil {
		return fmt.Errorf("requester: %w", err)
	}
	if err := r.Recipient.Validate(); err != nil {
		return fmt.Errorf("recipient: %w", err)
	}
	if !validAgentRequestStatus(r.Status) || r.Revision <= 0 {
		return errors.New("agent request status and positive revision are required")
	}
	if err := validateCredentialFreeContext(r.SharedContext); err != nil {
		return err
	}
	return nil
}

type AgentRequestFilter struct {
	Scope       Scope
	SourceRunID string
	Requester   *CollaborationParty
	Recipient   *CollaborationParty
	Kinds       []AgentRequestKind
	Statuses    []AgentRequestStatus
	Limit       int
	Offset      int
}

type CreateAgentRequestRequest struct {
	Scope                Scope
	Kind                 AgentRequestKind
	Requester            CollaborationParty
	Recipient            CollaborationParty
	SourceRunID          string
	Goal                 string
	Instructions         string
	SemanticRole         string
	AcceptanceCriteria   map[string]interface{}
	ArtifactRequirements []ArtifactRequirement
	SharedContext        map[string]interface{}
	ConversationRefs     []string
	IdempotencyKey       string
}

type RespondAgentRequestRequest struct {
	Scope            Scope
	RequestID        string
	ExpectedRevision int64
	Decision         AgentRequestDecision
	Principal        CollaborationParty
	Message          string
}

type AgentRequestResult struct {
	Request *AgentRequest    `json:"request"`
	Child   *AgentRun        `json:"childRun,omitempty"`
	Events  []*ActivityEvent `json:"events,omitempty"`
}

type AgentRequestCreateRecord struct {
	Request *AgentRequest
	Event   *ActivityEvent
}

type AgentRequestResponseRecord struct {
	Request                 *AgentRequest
	ExpectedRequestRevision int64
	SourceRun               *AgentRun
	ExpectedSourceRevision  int64
	ChildRun                *AgentRun
	SourceEvent             *ActivityEvent
	ChildEvent              *ActivityEvent
}

type CollaborationStore interface {
	CreateAgentRequest(ctx context.Context, record AgentRequestCreateRecord) (*ActivityEvent, error)
	GetAgentRequest(ctx context.Context, scope Scope, requestID string) (*AgentRequest, error)
	FindAgentRequestByIdempotencyKey(ctx context.Context, scope Scope, key string) (*AgentRequest, error)
	ListAgentRequests(ctx context.Context, filter AgentRequestFilter) ([]*AgentRequest, error)
	RespondAgentRequest(ctx context.Context, record AgentRequestResponseRecord) ([]*ActivityEvent, error)
}

type CollaborationKernelStore interface {
	PortfolioStore
	RunActivityStore
	CollaborationStore
}

type CollaborationService struct {
	store CollaborationStore
	runs  PortfolioStore
	now   func() time.Time
	newID func() string
}

func NewCollaborationService(store CollaborationKernelStore) *CollaborationService {
	return &CollaborationService{store: store, runs: store, now: time.Now, newID: uuid.NewString}
}

func (s *CollaborationService) CreateAgentRequest(ctx context.Context, req CreateAgentRequestRequest) (*AgentRequestResult, error) {
	if s == nil || s.store == nil || s.runs == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	if req.Kind != AgentRequestKindRequest && req.Kind != AgentRequestKindHandoff {
		return nil, errors.New("agent request kind must be request or handoff")
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
	if err := validateCredentialFreeContext(req.SharedContext); err != nil {
		return nil, err
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
			return nil, ErrAgentRequestIdempotency
		}
	}
	source, err := s.runs.GetAgentRun(ctx, req.Scope, req.SourceRunID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrRunNotFound
	}
	if !requesterControlsRun(req.Requester, source) {
		return nil, ErrAgentRequestUnauthorized
	}
	now := s.now()
	request := &AgentRequest{
		ID: s.newID(), Scope: req.Scope, Kind: req.Kind, Status: AgentRequestStatusPending,
		Requester: req.Requester, Recipient: req.Recipient, SourceRunID: source.ID, ObjectiveID: source.ObjectiveID,
		Goal: strings.TrimSpace(req.Goal), Instructions: strings.TrimSpace(req.Instructions), SemanticRole: strings.TrimSpace(req.SemanticRole),
		AcceptanceCriteria: cloneMap(req.AcceptanceCriteria), ArtifactRequirements: cloneArtifactRequirements(req.ArtifactRequirements),
		SharedContext: cloneMap(req.SharedContext), ConversationRefs: append([]string(nil), req.ConversationRefs...),
		IdempotencyKey: strings.TrimSpace(req.IdempotencyKey), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	eventType := "collaboration.requested"
	summary := fmt.Sprintf("Requested work from %s %s", request.Recipient.Type, request.Recipient.ID)
	if request.Kind == AgentRequestKindHandoff {
		eventType = "handoff.proposed"
		summary = fmt.Sprintf("Proposed handoff to %s %s", request.Recipient.Type, request.Recipient.ID)
	}
	event := collaborationEvent(source, request, eventType, summary, req.Requester, now)
	persisted, err := s.store.CreateAgentRequest(ctx, AgentRequestCreateRecord{Request: request, Event: event})
	if err != nil {
		return nil, err
	}
	return &AgentRequestResult{Request: cloneAgentRequest(request), Events: []*ActivityEvent{persisted}}, nil
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
		return nil, ErrAgentRequestNotFound
	}
	if request.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if request.Status != AgentRequestStatusPending && request.Status != AgentRequestStatusClarificationRequested {
		return nil, ErrInvalidAgentRequestState
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
		return nil, ErrRunNotFound
	}
	now := s.now()
	updated := cloneAgentRequest(request)
	updated.Revision++
	updated.UpdatedAt = now
	updated.Response = strings.TrimSpace(req.Message)
	record := AgentRequestResponseRecord{Request: updated, ExpectedRequestRevision: request.Revision}
	eventType := ""
	summary := ""
	switch req.Decision {
	case AgentRequestDecisionProvideClarification:
		if request.Status != AgentRequestStatusClarificationRequested || strings.TrimSpace(req.Message) == "" {
			return nil, errors.New("clarification response is required for a request awaiting clarification")
		}
		updated.Status = AgentRequestStatusPending
		eventType = "collaboration.clarification_provided"
		summary = fmt.Sprintf("%s %s provided clarification", req.Principal.Type, req.Principal.ID)
	case AgentRequestDecisionRequestClarification:
		if request.Status != AgentRequestStatusPending {
			return nil, ErrInvalidAgentRequestState
		}
		if strings.TrimSpace(req.Message) == "" {
			return nil, errors.New("clarification question is required")
		}
		updated.Status = AgentRequestStatusClarificationRequested
		updated.Clarification = strings.TrimSpace(req.Message)
		eventType = "collaboration.clarification_requested"
		summary = fmt.Sprintf("%s %s requested clarification", req.Principal.Type, req.Principal.ID)
	case AgentRequestDecisionReject:
		updated.Status = AgentRequestStatusRejected
		updated.ResolvedAt = &now
		eventType = "collaboration.rejected"
		summary = fmt.Sprintf("%s %s rejected the request", req.Principal.Type, req.Principal.ID)
	case AgentRequestDecisionAccept:
		updated.Status = AgentRequestStatusAccepted
		updated.ResolvedAt = &now
		child := buildCollaborationChildRun(source, updated, now, s.newID())
		if err := child.Validate(); err != nil {
			return nil, err
		}
		updated.ChildRunID = child.ID
		record.ChildRun = child
		record.SourceRun = acceptedSourceRun(source, updated, now)
		record.ExpectedSourceRevision = source.Revision
		eventType = "collaboration.accepted"
		if updated.Kind == AgentRequestKindHandoff {
			eventType = "handoff.accepted"
		}
		summary = fmt.Sprintf("%s %s accepted the work", req.Principal.Type, req.Principal.ID)
		record.ChildEvent = collaborationEvent(child, updated, eventType, summary, req.Principal, now)
	default:
		return nil, errors.New("agent request decision is invalid")
	}
	record.SourceEvent = collaborationEvent(source, updated, eventType, summary, req.Principal, now)
	events, err := s.store.RespondAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	return &AgentRequestResult{Request: cloneAgentRequest(updated), Child: cloneAgentRun(record.ChildRun), Events: events}, nil
}

func (s *CollaborationService) GetAgentRequest(ctx context.Context, scope Scope, requestID string) (*AgentRequest, error) {
	request, err := s.store.GetAgentRequest(ctx, scope, requestID)
	if err != nil {
		return nil, err
	}
	if request == nil {
		return nil, ErrAgentRequestNotFound
	}
	return request, nil
}

func (s *CollaborationService) ListAgentRequests(ctx context.Context, filter AgentRequestFilter) ([]*AgentRequest, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("collaboration store is not configured")
	}
	return s.store.ListAgentRequests(ctx, filter)
}

func requesterControlsRun(requester CollaborationParty, run *AgentRun) bool {
	return requester == (CollaborationParty{Type: run.Owner.Type, ID: run.Owner.ID}) ||
		requester.Type == OwnerTypeAgent && requester.ID == run.AssignedAgentID
}

func buildCollaborationChildRun(source *AgentRun, request *AgentRequest, now time.Time, id string) *AgentRun {
	owner := source.Owner
	if request.Kind == AgentRequestKindHandoff {
		owner = ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID}
	}
	assignedAgent := ""
	if request.Recipient.Type == OwnerTypeAgent {
		assignedAgent = request.Recipient.ID
	}
	context := cloneMap(request.SharedContext)
	if context == nil {
		context = make(map[string]interface{})
	}
	context["collaboration"] = map[string]interface{}{
		"requestId": request.ID, "kind": request.Kind, "requester": request.Requester,
		"recipient": request.Recipient, "semanticRole": request.SemanticRole,
		"acceptanceCriteria": cloneMap(request.AcceptanceCriteria), "artifactRequirements": cloneArtifactRequirements(request.ArtifactRequirements),
	}
	sourceKind := RunSourceRequest
	if request.Kind == AgentRequestKindHandoff {
		sourceKind = RunSourceHandoff
	}
	return &AgentRun{
		ID: id, Scope: source.Scope, ObjectiveID: source.ObjectiveID, ParentRunID: source.ID, RootRunID: source.RootRunID,
		Owner: owner, AssignedAgentID: assignedAgent, Goal: request.Goal, Source: sourceKind, Status: AgentRunStatusQueued,
		Priority: source.Priority, AvailableAt: now, QueueEnteredAt: now, Context: context,
		Budget: cloneMap(source.Budget), Policy: cloneMap(source.Policy), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func acceptedSourceRun(source *AgentRun, request *AgentRequest, now time.Time) *AgentRun {
	updated := cloneAgentRun(source)
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	if request.Kind == AgentRequestKindHandoff {
		updated.Status = AgentRunStatusCompleted
		updated.WakeCondition = nil
		updated.Output = map[string]interface{}{"handoffRequestId": request.ID, "childRunId": request.ChildRunID}
		updated.CompletedAt = &now
	} else {
		updated.Status = AgentRunStatusWaitingForDependency
		updated.WakeCondition = &WakeCondition{Type: "agent_request", Reference: request.ID}
	}
	return updated
}

func collaborationEvent(run *AgentRun, request *AgentRequest, eventType, summary string, actor CollaborationParty, now time.Time) *ActivityEvent {
	teamID := teamIDForRun(run)
	if teamID == "" && request.Recipient.Type == OwnerTypeTeam {
		teamID = request.Recipient.ID
	}
	return &ActivityEvent{
		ID: uuid.NewString(), Scope: run.Scope, EventType: eventType, Severity: ActivitySeverityInfo,
		AgentID: run.AssignedAgentID, ObjectiveID: run.ObjectiveID, RunID: run.ID, ParentRunID: run.ParentRunID,
		TeamID: teamID, ConversationRefs: append([]string(nil), request.ConversationRefs...),
		Actor: ActivityActor{Type: string(actor.Type), ID: actor.ID}, Summary: summary, Visibility: ActivityVisibilityTeam,
		CorrelationID: request.ID, CausationID: request.SourceRunID, CreatedAt: now,
		Payload: map[string]interface{}{"requestId": request.ID, "kind": request.Kind, "status": request.Status, "recipient": request.Recipient, "semanticRole": request.SemanticRole, "childRunId": request.ChildRunID},
	}
}

func validateCredentialFreeContext(value interface{}) error {
	var walk func(interface{}) error
	walk = func(candidate interface{}) error {
		switch typed := candidate.(type) {
		case map[string]interface{}:
			for key, child := range typed {
				normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
				for _, forbidden := range []string{"credential", "secret", "password", "token", "apikey", "privatekey"} {
					if strings.Contains(normalized, forbidden) {
						return ErrUnsafeSharedContext
					}
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []interface{}:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func sameAgentRequestIntent(existing *AgentRequest, req CreateAgentRequestRequest) bool {
	return existing.Kind == req.Kind && existing.Requester == req.Requester && existing.Recipient == req.Recipient &&
		existing.SourceRunID == strings.TrimSpace(req.SourceRunID) && existing.Goal == strings.TrimSpace(req.Goal)
}

func validAgentRequestStatus(status AgentRequestStatus) bool {
	switch status {
	case AgentRequestStatusPending, AgentRequestStatusClarificationRequested, AgentRequestStatusAccepted, AgentRequestStatusRejected, AgentRequestStatusCanceled:
		return true
	default:
		return false
	}
}

func cloneAgentRequest(in *AgentRequest) *AgentRequest {
	if in == nil {
		return nil
	}
	out := *in
	out.AcceptanceCriteria = cloneMap(in.AcceptanceCriteria)
	out.ArtifactRequirements = cloneArtifactRequirements(in.ArtifactRequirements)
	out.SharedContext = cloneMap(in.SharedContext)
	out.ConversationRefs = append([]string(nil), in.ConversationRefs...)
	if in.ResolvedAt != nil {
		resolved := *in.ResolvedAt
		out.ResolvedAt = &resolved
	}
	return &out
}

func cloneArtifactRequirements(in []ArtifactRequirement) []ArtifactRequirement {
	result := make([]ArtifactRequirement, len(in))
	for index, requirement := range in {
		result[index] = requirement
		result[index].Schema = cloneMap(requirement.Schema)
	}
	return result
}
