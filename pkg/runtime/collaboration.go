package runtime

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	ErrAgentRequestNotFound     = errors.New("agent request not found")
	ErrInvalidAgentRequestState = errors.New("invalid agent request state")
	ErrAgentRequestUnauthorized = errors.New("agent request principal is not authorized")
	ErrAgentRequestIdempotency  = errors.New("agent request idempotency conflict")
	ErrUnsafeSharedContext      = errors.New("shared context cannot contain credentials or secrets")
	ErrInvalidArtifact          = errors.New("invalid artifact contract")
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
	AgentRequestStatusCompleted              AgentRequestStatus = "completed"
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

// ArtifactReference is a portable, immutable link to output stored by a host
// artifact service. ContentRef is deliberately opaque: OpenSeal persists and
// audits identity and provenance without exposing signed URLs or credentials.
type ArtifactReference struct {
	ID              string                 `json:"id"`
	RequirementName string                 `json:"requirementName,omitempty"`
	Name            string                 `json:"name"`
	Type            string                 `json:"type,omitempty"`
	MediaType       string                 `json:"mediaType,omitempty"`
	ContentRef      string                 `json:"contentRef"`
	Digest          string                 `json:"digest,omitempty"`
	SizeBytes       int64                  `json:"sizeBytes,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
	EvidenceRefs    []string               `json:"evidenceRefs,omitempty"`
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
	CompletionSummary    string                 `json:"completionSummary,omitempty"`
	AcceptanceEvidence   map[string]interface{} `json:"acceptanceEvidence,omitempty"`
	Artifacts            []ArtifactReference    `json:"artifacts,omitempty"`
	IdempotencyKey       string                 `json:"idempotencyKey,omitempty"`
	CompletionKey        string                 `json:"completionKey,omitempty"`
	Revision             int64                  `json:"revision"`
	CreatedAt            time.Time              `json:"createdAt"`
	UpdatedAt            time.Time              `json:"updatedAt"`
	AcceptedAt           *time.Time             `json:"acceptedAt,omitempty"`
	CompletedAt          *time.Time             `json:"completedAt,omitempty"`
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
	if err := validateArtifactRequirements(r.ArtifactRequirements); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
	}
	if r.Status == AgentRequestStatusCompleted {
		if err := validateArtifactReferences(r.ArtifactRequirements, r.Artifacts); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
		}
	} else if len(r.Artifacts) > 0 || len(r.AcceptanceEvidence) > 0 || strings.TrimSpace(r.CompletionSummary) != "" {
		return errors.New("completion output is only valid for a completed agent request")
	}
	if err := validateCredentialFreeContext(r.AcceptanceEvidence); err != nil {
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

type CompleteAgentRequestRequest struct {
	Scope                 Scope
	RequestID             string
	ExpectedRevision      int64
	ExpectedChildRevision int64
	Principal             CollaborationParty
	Actor                 CollaborationParty
	Summary               string
	AcceptanceEvidence    map[string]interface{}
	Artifacts             []ArtifactReference
	CompletionKey         string
}

type AgentRequestResult struct {
	Request *AgentRequest    `json:"request"`
	Source  *AgentRun        `json:"sourceRun,omitempty"`
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

type AgentRequestCompletionRecord struct {
	Request                 *AgentRequest
	ExpectedRequestRevision int64
	SourceRun               *AgentRun
	ExpectedSourceRevision  int64
	ChildRun                *AgentRun
	ExpectedChildRevision   int64
	SourceEvent             *ActivityEvent
	ChildEvent              *ActivityEvent
}

type CollaborationStore interface {
	CreateAgentRequest(ctx context.Context, record AgentRequestCreateRecord) (*ActivityEvent, error)
	GetAgentRequest(ctx context.Context, scope Scope, requestID string) (*AgentRequest, error)
	FindAgentRequestByIdempotencyKey(ctx context.Context, scope Scope, key string) (*AgentRequest, error)
	ListAgentRequests(ctx context.Context, filter AgentRequestFilter) ([]*AgentRequest, error)
	RespondAgentRequest(ctx context.Context, record AgentRequestResponseRecord) ([]*ActivityEvent, error)
	CompleteAgentRequest(ctx context.Context, record AgentRequestCompletionRecord) ([]*ActivityEvent, error)
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
	if err := validateArtifactRequirements(req.ArtifactRequirements); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
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
		updated.AcceptedAt = &now
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

// CompleteAgentRequest atomically records the recipient's result, completes
// the child run, and wakes the source run that is waiting on the request. The
// Principal is the authorized recipient; Actor is the agent or Team member
// that actually performed the completion and is retained in the audit trail.
func (s *CollaborationService) CompleteAgentRequest(ctx context.Context, req CompleteAgentRequestRequest) (*AgentRequestResult, error) {
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
		return nil, ErrAgentRequestNotFound
	}
	if req.Principal != request.Recipient {
		return nil, ErrAgentRequestUnauthorized
	}
	if request.Status == AgentRequestStatusCompleted {
		if request.CompletionKey != strings.TrimSpace(req.CompletionKey) || request.CompletionKey == "" {
			return nil, ErrInvalidAgentRequestState
		}
		if !sameAgentRequestCompletion(request, req) {
			return nil, ErrAgentRequestIdempotency
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
		return nil, ErrRevisionConflict
	}
	if request.Status != AgentRequestStatusAccepted || strings.TrimSpace(request.ChildRunID) == "" {
		return nil, ErrInvalidAgentRequestState
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
	if err := validateCredentialFreeContext(req.AcceptanceEvidence); err != nil {
		return nil, err
	}
	if err := validateArtifactReferences(request.ArtifactRequirements, req.Artifacts); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
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
		return nil, ErrRunNotFound
	}
	if child.ParentRunID != source.ID || child.RootRunID != source.RootRunID || child.Status == AgentRunStatusFailed || child.Status == AgentRunStatusCanceled {
		return nil, ErrInvalidAgentRequestState
	}
	if req.ExpectedChildRevision != child.Revision {
		return nil, ErrRevisionConflict
	}
	now := s.now()
	updatedRequest := cloneAgentRequest(request)
	updatedRequest.Status = AgentRequestStatusCompleted
	updatedRequest.CompletionSummary = strings.TrimSpace(req.Summary)
	updatedRequest.AcceptanceEvidence = cloneMap(req.AcceptanceEvidence)
	updatedRequest.Artifacts = normalizeArtifactReferences(req.Artifacts)
	updatedRequest.CompletionKey = strings.TrimSpace(req.CompletionKey)
	updatedRequest.Revision++
	updatedRequest.UpdatedAt = now
	updatedRequest.CompletedAt = &now
	updatedRequest.ResolvedAt = &now

	updatedChild := completedCollaborationChildRun(child, updatedRequest, now)
	updatedSource, err := completedCollaborationSourceRun(source, updatedRequest, now)
	if err != nil {
		return nil, err
	}
	eventType := "collaboration.completed"
	if request.Kind == AgentRequestKindHandoff {
		eventType = "handoff.completed"
	}
	summary := fmt.Sprintf("%s %s completed the work", actor.Type, actor.ID)
	record := AgentRequestCompletionRecord{
		Request: updatedRequest, ExpectedRequestRevision: request.Revision,
		SourceRun: updatedSource, ExpectedSourceRevision: source.Revision,
		ChildRun: updatedChild, ExpectedChildRevision: child.Revision,
		SourceEvent: collaborationCompletionEvent(source, updatedRequest, eventType, summary, actor, now),
		ChildEvent:  collaborationCompletionEvent(child, updatedRequest, eventType, summary, actor, now),
	}
	events, err := s.store.CompleteAgentRequest(ctx, record)
	if err != nil {
		return nil, err
	}
	return &AgentRequestResult{
		Request: cloneAgentRequest(updatedRequest), Source: cloneAgentRun(updatedSource), Child: cloneAgentRun(updatedChild), Events: events,
	}, nil
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

func completedCollaborationChildRun(child *AgentRun, request *AgentRequest, now time.Time) *AgentRun {
	updated := cloneAgentRun(child)
	updated.Status = AgentRunStatusCompleted
	updated.Revision++
	updated.UpdatedAt = now
	updated.CompletedAt = &now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.WakeCondition = nil
	updated.Output = collaborationCompletionOutput(updated.Output, request)
	return updated
}

func completedCollaborationSourceRun(source *AgentRun, request *AgentRequest, now time.Time) (*AgentRun, error) {
	updated := cloneAgentRun(source)
	if request.Kind == AgentRequestKindRequest {
		if updated.Status != AgentRunStatusWaitingForDependency || updated.WakeCondition == nil ||
			updated.WakeCondition.Type != "agent_request" || updated.WakeCondition.Reference != request.ID {
			return nil, fmt.Errorf("%w: source run is not waiting on request %s", ErrInvalidAgentRequestState, request.ID)
		}
		updated.Status = AgentRunStatusQueued
		updated.AvailableAt = now
		updated.QueueEnteredAt = now
		updated.WakeCondition = nil
		updated.LastWakeSignalID = "agent_request:" + request.ID + ":" + fmt.Sprint(request.Revision)
	} else if updated.Status != AgentRunStatusCompleted {
		return nil, fmt.Errorf("%w: handoff source run is not completed", ErrInvalidAgentRequestState)
	}
	updated.Revision++
	updated.UpdatedAt = now
	updated.LeaseOwner = ""
	updated.LeaseExpiresAt = nil
	updated.Output = collaborationCompletionOutput(updated.Output, request)
	return updated, nil
}

func collaborationCompletionOutput(existing map[string]interface{}, request *AgentRequest) map[string]interface{} {
	result := cloneMap(existing)
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
		"summary": request.CompletionSummary, "acceptanceEvidence": cloneMap(request.AcceptanceEvidence),
		"artifacts": cloneArtifactReferences(request.Artifacts),
	}
	result["collaborationResults"] = results
	return result
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

func collaborationCompletionEvent(run *AgentRun, request *AgentRequest, eventType, summary string, actor CollaborationParty, now time.Time) *ActivityEvent {
	event := collaborationEvent(run, request, eventType, summary, actor, now)
	artifacts := make([]map[string]interface{}, 0, len(request.Artifacts))
	for _, artifact := range request.Artifacts {
		artifacts = append(artifacts, map[string]interface{}{
			"id": artifact.ID, "requirementName": artifact.RequirementName, "name": artifact.Name,
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

func validateCredentialFreeContext(value interface{}) error {
	var walk func(interface{}) error
	walk = func(candidate interface{}) error {
		switch typed := candidate.(type) {
		case map[string]interface{}:
			for key, child := range typed {
				normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
				for _, forbidden := range []string{"credential", "credentials", "secret", "secrets", "password", "passphrase", "apikey", "apitoken", "privatekey", "accesstoken", "authtoken", "refreshtoken", "bearertoken"} {
					if normalized == forbidden || strings.HasSuffix(normalized, forbidden) {
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

// ValidateCredentialFreeContext rejects resolved credentials and secret-shaped
// values before portable work state can reach persistence or model context.
// Opaque credential references belong in governed skill bindings instead.
func ValidateCredentialFreeContext(value interface{}) error {
	return validateCredentialFreeContext(value)
}

func validateArtifactRequirements(requirements []ArtifactRequirement) error {
	seen := make(map[string]struct{}, len(requirements))
	for index, requirement := range requirements {
		name := strings.TrimSpace(requirement.Name)
		if name == "" {
			return fmt.Errorf("artifact requirement %d name is required", index)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("artifact requirement %q is duplicated", name)
		}
		seen[name] = struct{}{}
		if requirement.Schema != nil {
			if _, err := compileArtifactSchema(name, requirement.Schema); err != nil {
				return fmt.Errorf("artifact requirement %q schema: %w", name, err)
			}
		}
	}
	return nil
}

func validateArtifactReferences(requirements []ArtifactRequirement, artifacts []ArtifactReference) error {
	requirementsByName := make(map[string]ArtifactRequirement, len(requirements))
	for _, requirement := range requirements {
		requirementsByName[strings.TrimSpace(requirement.Name)] = requirement
	}
	seenIDs := make(map[string]struct{}, len(artifacts))
	provided := make(map[string]struct{}, len(artifacts))
	for index, artifact := range artifacts {
		artifact.ID = strings.TrimSpace(artifact.ID)
		artifact.Name = strings.TrimSpace(artifact.Name)
		artifact.ContentRef = strings.TrimSpace(artifact.ContentRef)
		if artifact.ID == "" || artifact.Name == "" || artifact.ContentRef == "" {
			return fmt.Errorf("artifact %d id, name, and content reference are required", index)
		}
		if _, exists := seenIDs[artifact.ID]; exists {
			return fmt.Errorf("artifact id %q is duplicated", artifact.ID)
		}
		seenIDs[artifact.ID] = struct{}{}
		if err := validateOpaqueArtifactRef(artifact.ContentRef); err != nil {
			return fmt.Errorf("artifact %q content reference: %w", artifact.ID, err)
		}
		if artifact.SizeBytes < 0 {
			return fmt.Errorf("artifact %q size cannot be negative", artifact.ID)
		}
		if artifact.Digest != "" {
			parts := strings.SplitN(strings.ToLower(strings.TrimSpace(artifact.Digest)), ":", 2)
			if len(parts) != 2 || parts[0] != "sha256" || len(parts[1]) != 64 {
				return fmt.Errorf("artifact %q digest must be sha256:<64 hex characters>", artifact.ID)
			}
			if _, err := hex.DecodeString(parts[1]); err != nil {
				return fmt.Errorf("artifact %q digest is invalid", artifact.ID)
			}
		}
		if err := validateCredentialFreeContext(artifact.Metadata); err != nil {
			return fmt.Errorf("artifact %q metadata: %w", artifact.ID, err)
		}
		for _, evidenceRef := range artifact.EvidenceRefs {
			if err := validateOpaqueArtifactRef(evidenceRef); err != nil {
				return fmt.Errorf("artifact %q evidence reference: %w", artifact.ID, err)
			}
		}
		requirementName := strings.TrimSpace(artifact.RequirementName)
		if requirementName == "" {
			continue
		}
		requirement, exists := requirementsByName[requirementName]
		if !exists {
			return fmt.Errorf("artifact %q references unknown requirement %q", artifact.ID, requirementName)
		}
		if requirement.Type != "" && strings.TrimSpace(artifact.Type) != strings.TrimSpace(requirement.Type) {
			return fmt.Errorf("artifact %q type %q does not satisfy requirement %q type %q", artifact.ID, artifact.Type, requirementName, requirement.Type)
		}
		if requirement.Schema != nil {
			compiled, err := compileArtifactSchema(requirementName, requirement.Schema)
			if err != nil {
				return err
			}
			if err := validateArtifactSchema(compiled, artifact.Metadata); err != nil {
				return fmt.Errorf("artifact %q metadata: %w", artifact.ID, err)
			}
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

func validateOpaqueArtifactRef(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 1024 {
		return errors.New("opaque reference must be between 1 and 1024 characters")
	}
	if strings.ContainsAny(value, "\r\n?#@") || strings.Contains(value, "://") {
		return errors.New("opaque reference cannot contain a URL, query, fragment, user info, or line break")
	}
	return nil
}

func compileArtifactSchema(name string, schema map[string]interface{}) (*jsonschema.Schema, error) {
	if err := validateLocalSchemaReferences(schema); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	resource := "artifact-" + strings.TrimSpace(name) + ".json"
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

func validateArtifactSchema(schema *jsonschema.Schema, value interface{}) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("JSON schema validation failed: %w", err)
	}
	return nil
}

func validateLocalSchemaReferences(value interface{}) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return errors.New("external JSON Schema references are not allowed")
				}
			}
			if err := validateLocalSchemaReferences(child); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range typed {
			if err := validateLocalSchemaReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func sameAgentRequestIntent(existing *AgentRequest, req CreateAgentRequestRequest) bool {
	return existing.Kind == req.Kind && existing.Requester == req.Requester && existing.Recipient == req.Recipient &&
		existing.SourceRunID == strings.TrimSpace(req.SourceRunID) && existing.Goal == strings.TrimSpace(req.Goal)
}

func sameAgentRequestCompletion(existing *AgentRequest, req CompleteAgentRequestRequest) bool {
	left, _ := json.Marshal(struct {
		Summary   string
		Evidence  map[string]interface{}
		Artifacts []ArtifactReference
	}{existing.CompletionSummary, existing.AcceptanceEvidence, existing.Artifacts})
	right, _ := json.Marshal(struct {
		Summary   string
		Evidence  map[string]interface{}
		Artifacts []ArtifactReference
	}{strings.TrimSpace(req.Summary), req.AcceptanceEvidence, normalizeArtifactReferences(req.Artifacts)})
	return bytes.Equal(left, right)
}

func validAgentRequestStatus(status AgentRequestStatus) bool {
	switch status {
	case AgentRequestStatusPending, AgentRequestStatusClarificationRequested, AgentRequestStatusAccepted, AgentRequestStatusCompleted, AgentRequestStatusRejected, AgentRequestStatusCanceled:
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
	out.AcceptanceEvidence = cloneMap(in.AcceptanceEvidence)
	out.Artifacts = cloneArtifactReferences(in.Artifacts)
	out.SharedContext = cloneMap(in.SharedContext)
	out.ConversationRefs = append([]string(nil), in.ConversationRefs...)
	if in.ResolvedAt != nil {
		resolved := *in.ResolvedAt
		out.ResolvedAt = &resolved
	}
	if in.AcceptedAt != nil {
		accepted := *in.AcceptedAt
		out.AcceptedAt = &accepted
	}
	if in.CompletedAt != nil {
		completed := *in.CompletedAt
		out.CompletedAt = &completed
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

func cloneArtifactReferences(in []ArtifactReference) []ArtifactReference {
	result := make([]ArtifactReference, len(in))
	for index, artifact := range in {
		result[index] = artifact
		result[index].Metadata = cloneMap(artifact.Metadata)
		result[index].EvidenceRefs = append([]string(nil), artifact.EvidenceRefs...)
	}
	return result
}

func normalizeArtifactReferences(in []ArtifactReference) []ArtifactReference {
	result := cloneArtifactReferences(in)
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
