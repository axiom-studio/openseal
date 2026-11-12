package models

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

type AgentRequestInboxStore interface {
	CollaborationKernelStore
	RunCommandStore
}

var (
	ErrAgentRequestNotFound     = errors.New("agent request not found")
	ErrInvalidAgentRequestState = errors.New("invalid agent request state")
	ErrAgentRequestAssignment   = errors.New("agent request assignment is invalid")
	ErrAgentRequestIdempotency  = errors.New("agent request idempotency conflict")
)

type AgentRequestKind string

const (
	AgentRequestKindRequest    AgentRequestKind = "request"
	AgentRequestKindHandoff    AgentRequestKind = "handoff"
	AgentRequestKindEscalation AgentRequestKind = "escalation"
)

type AgentRequestStatus string

const (
	AgentRequestStatusPending                AgentRequestStatus = "pending"
	AgentRequestStatusClarificationRequested AgentRequestStatus = "clarification_requested"
	AgentRequestStatusAccepted               AgentRequestStatus = "accepted"
	AgentRequestStatusCompletionReview       AgentRequestStatus = "completion_review"
	AgentRequestStatusCompleted              AgentRequestStatus = "completed"
	AgentRequestStatusFailed                 AgentRequestStatus = "failed"
	AgentRequestStatusRejected               AgentRequestStatus = "rejected"
	AgentRequestStatusCanceled               AgentRequestStatus = "canceled"
)

type AgentRequestAcceptancePolicy string

const (
	// AgentRequestAcceptanceRecipientReview asks the recipient Agent, or one
	// deterministic eligible member of the recipient Team, to evaluate the
	// request before work starts.
	AgentRequestAcceptanceRecipientReview AgentRequestAcceptancePolicy = "recipient_review"
	// AgentRequestAcceptancePreauthorized is reserved for kernel-authored
	// delegation intents whose originating bounded Turn already selected the
	// exact recipient. The inbox reconciler completes acceptance after a crash
	// between durable request creation and response.
	AgentRequestAcceptancePreauthorized AgentRequestAcceptancePolicy = "preauthorized"
)

// AgentRequestBidDecision is an eligible Team member's explicit response to a
// work offer. Bids are durable facts; assignment is a separate deterministic
// decision so a retry or another replica cannot silently choose a different
// worker.
type AgentRequestBidDecision string

const (
	AgentRequestBidAccept  AgentRequestBidDecision = "accept"
	AgentRequestBidDecline AgentRequestBidDecision = "decline"
)

// AgentRequestCandidate records why one roster member was eligible when a Team
// request was offered and how much capacity was available at that instant.
type AgentRequestCandidate struct {
	AgentDeploymentID  string   `json:"agentDeploymentId"`
	RosterAssignmentID string   `json:"rosterAssignmentId"`
	RequiredSkillIDs   []string `json:"requiredSkillIds,omitempty"`
	CapacityLimit      int      `json:"capacityLimit"`
	ActiveRuns         int      `json:"activeRuns"`
	AvailableSlots     int      `json:"availableSlots"`
}

type AgentRequestBid struct {
	AgentDeploymentID string                  `json:"agentDeploymentId"`
	Decision          AgentRequestBidDecision `json:"decision"`
	Reason            string                  `json:"reason,omitempty"`
	IdempotencyKey    string                  `json:"idempotencyKey"`
	SubmittedAt       time.Time               `json:"submittedAt"`
}

type AgentRequestAssignmentDecision struct {
	AgentDeploymentID string    `json:"agentDeploymentId"`
	Reason            string    `json:"reason"`
	DecidedAt         time.Time `json:"decidedAt"`
}

type AgentRequestCompletionReviewDecision struct {
	Reviewer       CollaborationParty `json:"reviewer"`
	Approve        bool               `json:"approve"`
	Summary        string             `json:"summary"`
	IdempotencyKey string             `json:"idempotencyKey"`
	DecidedAt      time.Time          `json:"decidedAt"`
}

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

// AgentRequest is the durable collaboration fact shared by agent, Team, chat,
// and activity projections. It contains references and explicitly shared
// context only; credential values and bindings are never transferable.
type AgentRequest struct {
	ID                   string                          `json:"id"`
	Scope                Scope                           `json:"scope"`
	Kind                 AgentRequestKind                `json:"kind"`
	Status               AgentRequestStatus              `json:"status"`
	Requester            CollaborationParty              `json:"requester"`
	Recipient            CollaborationParty              `json:"recipient"`
	SourceRunID          string                          `json:"sourceRunId"`
	ChildRunID           string                          `json:"childRunId,omitempty"`
	AssignedAgentID      string                          `json:"assignedAgentId,omitempty"`
	Candidates           []AgentRequestCandidate         `json:"candidates,omitempty"`
	Bids                 []AgentRequestBid               `json:"bids,omitempty"`
	AssignmentDecision   *AgentRequestAssignmentDecision `json:"assignmentDecision,omitempty"`
	DependencyGroupID    string                          `json:"dependencyGroupId,omitempty"`
	DependencyID         string                          `json:"dependencyId,omitempty"`
	ObjectiveID          string                          `json:"objectiveId,omitempty"`
	Goal                 string                          `json:"goal"`
	Instructions         string                          `json:"instructions,omitempty"`
	SemanticRole         string                          `json:"semanticRole,omitempty"`
	AcceptanceCriteria   map[string]interface{}          `json:"acceptanceCriteria,omitempty"`
	ArtifactRequirements []ArtifactRequirement           `json:"artifactRequirements,omitempty"`
	SharedContext        map[string]interface{}          `json:"sharedContext,omitempty"`
	ChildCheckpoint      map[string]interface{}          `json:"childCheckpoint,omitempty"`
	AcceptancePolicy     AgentRequestAcceptancePolicy    `json:"acceptancePolicy"`
	// DelegationPolicy is the immutable Team policy snapshot that governed
	// creation of this request. A nil value means the source Run was not owned
	// by a deployed Team. Keeping the snapshot on the collaboration fact makes
	// acceptance, depth, concurrency, and review decisions explainable after a
	// Team definition is amended.
	DelegationPolicy        *AgentRequestDelegationPolicy          `json:"delegationPolicy,omitempty"`
	ConversationRefs        []string                               `json:"conversationRefs,omitempty"`
	BudgetAllocation        *BudgetPolicy                          `json:"budgetAllocation,omitempty"`
	Clarification           string                                 `json:"clarification,omitempty"`
	Response                string                                 `json:"response,omitempty"`
	CompletionSummary       string                                 `json:"completionSummary,omitempty"`
	ResolutionReason        string                                 `json:"resolutionReason,omitempty"`
	AcceptanceEvidence      map[string]interface{}                 `json:"acceptanceEvidence,omitempty"`
	Artifacts               []ArtifactReference                    `json:"artifacts,omitempty"`
	IdempotencyKey          string                                 `json:"idempotencyKey,omitempty"`
	CompletionKey           string                                 `json:"completionKey,omitempty"`
	CompletionSubmittedAt   *time.Time                             `json:"completionSubmittedAt,omitempty"`
	CompletionReviewer      *CollaborationParty                    `json:"completionReviewer,omitempty"`
	CompletionReviewSummary string                                 `json:"completionReviewSummary,omitempty"`
	CompletionReviewKey     string                                 `json:"completionReviewKey,omitempty"`
	CompletionReviews       []AgentRequestCompletionReviewDecision `json:"completionReviews,omitempty"`
	ReviewDisagreement      bool                                   `json:"reviewDisagreement,omitempty"`
	Revision                int64                                  `json:"revision"`
	CreatedAt               time.Time                              `json:"createdAt"`
	UpdatedAt               time.Time                              `json:"updatedAt"`
	AcceptedAt              *time.Time                             `json:"acceptedAt,omitempty"`
	ReviewedAt              *time.Time                             `json:"reviewedAt,omitempty"`
	CompletedAt             *time.Time                             `json:"completedAt,omitempty"`
	ResolvedAt              *time.Time                             `json:"resolvedAt,omitempty"`
}

type AgentRequestDelegationPolicy struct {
	TeamDeploymentID        string `json:"teamDeploymentId"`
	TeamDefinitionID        string `json:"teamDefinitionId"`
	TeamDefinitionVersion   string `json:"teamDefinitionVersion"`
	DelegationDepth         int    `json:"delegationDepth"`
	MaximumDepth            int    `json:"maximumDepth,omitempty"`
	MaximumConcurrent       int    `json:"maximumConcurrent,omitempty"`
	AllowPeerDelegation     bool   `json:"allowPeerDelegation,omitempty"`
	RequireAcceptance       bool   `json:"requireAcceptance,omitempty"`
	RequireCompletionReview bool   `json:"requireCompletionReview,omitempty"`
	CompletionReviewQuorum  int    `json:"completionReviewQuorum,omitempty"`
	EscalateOnDisagreement  bool   `json:"escalateOnDisagreement,omitempty"`
}

func (p *AgentRequestDelegationPolicy) Validate() error {
	if p == nil {
		return nil
	}
	if !IsValidOpaqueIdentifier(p.TeamDeploymentID, 128) || strings.TrimSpace(p.TeamDefinitionID) == "" ||
		strings.TrimSpace(p.TeamDefinitionVersion) == "" || p.DelegationDepth <= 0 || p.MaximumDepth < 0 || p.MaximumConcurrent < 0 || p.CompletionReviewQuorum < 0 {
		return errors.New("agent request delegation policy snapshot is invalid")
	}
	if !p.RequireCompletionReview && (p.CompletionReviewQuorum > 0 || p.EscalateOnDisagreement) {
		return errors.New("agent request review quorum and disagreement escalation require completion review")
	}
	return nil
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
	if !IsValidAgentRequestKind(r.Kind) {
		return errors.New("agent request kind must be request, handoff, or escalation")
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
	if !validAgentRequestAcceptancePolicy(r.AcceptancePolicy) {
		return errors.New("agent request acceptance policy is invalid")
	}
	if err := r.DelegationPolicy.Validate(); err != nil {
		return err
	}
	if err := validateAgentRequestCoordination(r); err != nil {
		return err
	}
	if r.DelegationPolicy != nil && r.DelegationPolicy.RequireAcceptance && r.AcceptancePolicy != AgentRequestAcceptanceRecipientReview {
		return errors.New("Team delegation policy requires recipient acceptance")
	}
	if (r.DependencyGroupID == "") != (r.DependencyID == "") {
		return errors.New("agent request dependency group and edge must be set together")
	}
	if r.DependencyGroupID != "" && (!IsValidOpaqueIdentifier(r.DependencyGroupID, 128) || !IsValidOpaqueIdentifier(r.DependencyID, 128)) {
		return errors.New("agent request dependency identifiers must be portable opaque identifiers")
	}
	if err := ValidateCredentialFreeContext(r.SharedContext); err != nil {
		return err
	}
	if err := ValidateCredentialFreeContext(r.ChildCheckpoint); err != nil {
		return err
	}
	if r.BudgetAllocation != nil {
		if err := r.BudgetAllocation.Validate(); err != nil {
			return fmt.Errorf("budget allocation: %w", err)
		}
	}
	if err := ValidateArtifactRequirements(r.ArtifactRequirements); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
	}
	completionSubmitted := r.Status == AgentRequestStatusCompletionReview || r.Status == AgentRequestStatusCompleted || r.CompletionReviewKey != ""
	if completionSubmitted {
		if err := ValidateArtifactReferences(r.ArtifactRequirements, r.Artifacts); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidArtifact, err)
		}
	} else if len(r.Artifacts) > 0 || len(r.AcceptanceEvidence) > 0 || strings.TrimSpace(r.CompletionSummary) != "" {
		return errors.New("completion output is only valid for a completed agent request")
	}
	if completionSubmitted && (r.CompletionSubmittedAt == nil || strings.TrimSpace(r.CompletionKey) == "") {
		return errors.New("submitted completion requires a timestamp and idempotency key")
	}
	if r.Status == AgentRequestStatusCompletionReview && (r.DelegationPolicy == nil || !r.DelegationPolicy.RequireCompletionReview) {
		return errors.New("completion review state requires snapshotted Team review policy")
	}
	if r.CompletionReviewKey != "" {
		if r.CompletionReviewer == nil || r.CompletionReviewer.Validate() != nil || r.ReviewedAt == nil || strings.TrimSpace(r.CompletionReviewSummary) == "" {
			return errors.New("completion review decision requires reviewer, summary, key, and timestamp")
		}
	}
	if r.Status != AgentRequestStatusFailed && r.Status != AgentRequestStatusCanceled && strings.TrimSpace(r.ResolutionReason) != "" {
		return errors.New("resolution reason is only valid for a failed or canceled agent request")
	}
	if err := ValidateCredentialFreeContext(r.AcceptanceEvidence); err != nil {
		return err
	}
	return nil
}

func validateAgentRequestCoordination(request *AgentRequest) error {
	seenCandidates := make(map[string]struct{}, len(request.Candidates))
	for _, candidate := range request.Candidates {
		if strings.TrimSpace(candidate.AgentDeploymentID) == "" || strings.TrimSpace(candidate.RosterAssignmentID) == "" ||
			candidate.CapacityLimit < 1 || candidate.ActiveRuns < 0 ||
			candidate.AvailableSlots < 0 || candidate.AvailableSlots != max(0, candidate.CapacityLimit-candidate.ActiveRuns) {
			return errors.New("Team work offer candidate has invalid identity or capacity evidence")
		}
		if _, duplicate := seenCandidates[candidate.AgentDeploymentID]; duplicate {
			return errors.New("Team work offer candidates must be unique")
		}
		seenCandidates[candidate.AgentDeploymentID] = struct{}{}
	}
	seenBids := make(map[string]struct{}, len(request.Bids))
	for _, bid := range request.Bids {
		if _, eligible := seenCandidates[bid.AgentDeploymentID]; !eligible || strings.TrimSpace(bid.IdempotencyKey) == "" || bid.SubmittedAt.IsZero() ||
			bid.Decision != AgentRequestBidAccept && bid.Decision != AgentRequestBidDecline {
			return errors.New("Team work bid must reference one eligible candidate with a valid decision")
		}
		if _, duplicate := seenBids[bid.AgentDeploymentID]; duplicate {
			return errors.New("Team work bids must be unique per candidate")
		}
		seenBids[bid.AgentDeploymentID] = struct{}{}
	}
	if request.AssignmentDecision != nil {
		decision := request.AssignmentDecision
		if decision.AgentDeploymentID == "" || strings.TrimSpace(decision.Reason) == "" || decision.DecidedAt.IsZero() || decision.AgentDeploymentID != request.AssignedAgentID {
			return errors.New("Team assignment decision requires the selected Agent, reason, and timestamp")
		}
		if _, eligible := seenCandidates[decision.AgentDeploymentID]; !eligible {
			return errors.New("Team assignment decision must select an eligible candidate")
		}
	}
	if request.Recipient.Type == OwnerTypeTeam && request.Status != AgentRequestStatusPending && request.Status != AgentRequestStatusClarificationRequested &&
		request.Status != AgentRequestStatusRejected && request.Status != AgentRequestStatusCanceled && request.AssignmentDecision == nil {
		return errors.New("accepted Team work requires a durable assignment decision")
	}
	seenReviewers := make(map[CollaborationParty]struct{}, len(request.CompletionReviews))
	seenReviewKeys := make(map[string]struct{}, len(request.CompletionReviews))
	for _, decision := range request.CompletionReviews {
		if decision.Reviewer.Validate() != nil || decision.Reviewer.Type != OwnerTypeAgent || decision.Reviewer.ID == request.AssignedAgentID ||
			strings.TrimSpace(decision.Summary) == "" || strings.TrimSpace(decision.IdempotencyKey) == "" || decision.DecidedAt.IsZero() {
			return errors.New("completion review decision requires an independent Agent, summary, key, and timestamp")
		}
		if _, duplicate := seenReviewers[decision.Reviewer]; duplicate {
			return errors.New("completion review decisions must be unique per reviewer")
		}
		if _, duplicate := seenReviewKeys[decision.IdempotencyKey]; duplicate {
			return errors.New("completion review decision keys must be unique")
		}
		seenReviewers[decision.Reviewer] = struct{}{}
		seenReviewKeys[decision.IdempotencyKey] = struct{}{}
	}
	if request.ReviewDisagreement {
		approvals, rejections := CompletionReviewCounts(request.CompletionReviews)
		if request.Status != AgentRequestStatusCompletionReview || request.DelegationPolicy == nil ||
			!request.DelegationPolicy.EscalateOnDisagreement || approvals == 0 || rejections == 0 {
			return errors.New("completion review disagreement requires conflicting decisions and escalation policy")
		}
	}
	return nil
}

func CompletionReviewCounts(decisions []AgentRequestCompletionReviewDecision) (approvals, rejections int) {
	for _, decision := range decisions {
		if decision.Approve {
			approvals++
		} else {
			rejections++
		}
	}
	return approvals, rejections
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

type AgentRequestCreateRecord struct {
	Request                *AgentRequest
	SourceRun              *AgentRun
	ExpectedSourceRevision int64
	DelegationTeamID       string
	MaximumConcurrent      int
	Event                  *ActivityEvent
	// ToolTransition, when set, moves the source Run's delegating tool entry
	// (typically pending -> awaiting_child) in the same transaction. Its
	// lease is checked against the source Run as stored before this write.
	ToolTransition *ToolEntryTransitionRequest
	// RequireInterventionCount, when set, refuses the create with
	// ErrSteeringArrived unless the stored source Run has exactly this many
	// PendingInterventions.
	RequireInterventionCount *int
}

type AgentRequestResponseRecord struct {
	Request                 *AgentRequest
	ExpectedRequestRevision int64
	SourceRun               *AgentRun
	ExpectedSourceRevision  int64
	ChildRun                *AgentRun
	SourceEvent             *ActivityEvent
	ChildEvent              *ActivityEvent
	DependencyResolution    *RunDependencyResolutionRecord
}

type AgentRequestCoordinationRecord struct {
	Request                 *AgentRequest
	ExpectedRequestRevision int64
	Event                   *ActivityEvent
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
	DependencyResolution    *RunDependencyResolutionRecord
}

type CollaborationStore interface {
	CreateAgentRequest(ctx context.Context, record AgentRequestCreateRecord) (*ActivityEvent, error)
	GetAgentRequest(ctx context.Context, scope Scope, requestID string) (*AgentRequest, error)
	FindAgentRequestByIdempotencyKey(ctx context.Context, scope Scope, key string) (*AgentRequest, error)
	ListAgentRequests(ctx context.Context, filter AgentRequestFilter) ([]*AgentRequest, error)
	CoordinateAgentRequest(ctx context.Context, record AgentRequestCoordinationRecord) (*ActivityEvent, error)
	RespondAgentRequest(ctx context.Context, record AgentRequestResponseRecord) ([]*ActivityEvent, error)
	CompleteAgentRequest(ctx context.Context, record AgentRequestCompletionRecord) ([]*ActivityEvent, error)
}

type CollaborationKernelStore interface {
	PortfolioStore
	RunActivityStore
	CollaborationStore
	ArtifactStore
	RunDependencyStore
}

func RequesterControlsRun(requester CollaborationParty, run *AgentRun) bool {
	return requester == (CollaborationParty{Type: run.Owner.Type, ID: run.Owner.ID}) ||
		requester.Type == OwnerTypeAgent && requester.ID == run.AssignedAgentID
}

// CollaborationChildOwner is who owns delegated work: the source's owner, or
// the recipient of a hand-off. A hand-off inside a Team chat stays in that
// chat, so the Team keeps it and the recipient member is its acting Agent.
func CollaborationChildOwner(source *AgentRun, request *AgentRequest) ObjectiveOwner {
	teamChat := NormalizeRunKind(source.Kind) == RunKindConversation && source.Owner.Type == OwnerTypeTeam
	if request.Kind == AgentRequestKindHandoff && !teamChat {
		return ObjectiveOwner{Type: request.Recipient.Type, ID: request.Recipient.ID}
	}
	return source.Owner
}

func CloneBudgetPolicy(policy *BudgetPolicy) *BudgetPolicy {
	if policy == nil {
		return nil
	}
	cloned := *policy
	return &cloned
}

func validAgentRequestStatus(status AgentRequestStatus) bool {
	switch status {
	case AgentRequestStatusPending, AgentRequestStatusClarificationRequested, AgentRequestStatusAccepted, AgentRequestStatusCompletionReview,
		AgentRequestStatusCompleted, AgentRequestStatusFailed, AgentRequestStatusRejected, AgentRequestStatusCanceled:
		return true
	default:
		return false
	}
}

func IsValidAgentRequestKind(kind AgentRequestKind) bool {
	switch kind {
	case AgentRequestKindRequest, AgentRequestKindHandoff, AgentRequestKindEscalation:
		return true
	default:
		return false
	}
}

func NormalizeAgentRequestAcceptancePolicy(policy AgentRequestAcceptancePolicy) AgentRequestAcceptancePolicy {
	if policy == "" {
		return AgentRequestAcceptanceRecipientReview
	}
	return policy
}

func validAgentRequestAcceptancePolicy(policy AgentRequestAcceptancePolicy) bool {
	switch NormalizeAgentRequestAcceptancePolicy(policy) {
	case AgentRequestAcceptanceRecipientReview, AgentRequestAcceptancePreauthorized:
		return true
	default:
		return false
	}
}

func CloneAgentRequest(in *AgentRequest) *AgentRequest {
	if in == nil {
		return nil
	}
	out := *in
	out.DelegationPolicy = CloneAgentRequestDelegationPolicy(in.DelegationPolicy)
	out.Candidates = cloneAgentRequestCandidates(in.Candidates)
	out.Bids = append([]AgentRequestBid(nil), in.Bids...)
	out.CompletionReviews = append([]AgentRequestCompletionReviewDecision(nil), in.CompletionReviews...)
	if in.AssignmentDecision != nil {
		decision := *in.AssignmentDecision
		out.AssignmentDecision = &decision
	}
	out.AcceptanceCriteria = maps.Clone(in.AcceptanceCriteria)
	out.ArtifactRequirements = CloneArtifactRequirements(in.ArtifactRequirements)
	out.AcceptanceEvidence = maps.Clone(in.AcceptanceEvidence)
	out.Artifacts = CloneArtifactReferences(in.Artifacts)
	out.SharedContext = maps.Clone(in.SharedContext)
	out.ChildCheckpoint = maps.Clone(in.ChildCheckpoint)
	out.ConversationRefs = append([]string(nil), in.ConversationRefs...)
	if in.ResolvedAt != nil {
		resolved := *in.ResolvedAt
		out.ResolvedAt = &resolved
	}
	if in.AcceptedAt != nil {
		accepted := *in.AcceptedAt
		out.AcceptedAt = &accepted
	}
	if in.CompletionSubmittedAt != nil {
		submitted := *in.CompletionSubmittedAt
		out.CompletionSubmittedAt = &submitted
	}
	if in.CompletionReviewer != nil {
		reviewer := *in.CompletionReviewer
		out.CompletionReviewer = &reviewer
	}
	if in.ReviewedAt != nil {
		reviewed := *in.ReviewedAt
		out.ReviewedAt = &reviewed
	}
	if in.CompletedAt != nil {
		completed := *in.CompletedAt
		out.CompletedAt = &completed
	}
	return &out
}

func cloneAgentRequestCandidates(in []AgentRequestCandidate) []AgentRequestCandidate {
	out := append([]AgentRequestCandidate(nil), in...)
	for index := range out {
		out[index].RequiredSkillIDs = append([]string(nil), in[index].RequiredSkillIDs...)
	}
	return out
}

func CloneAgentRequestDelegationPolicy(policy *AgentRequestDelegationPolicy) *AgentRequestDelegationPolicy {
	if policy == nil {
		return nil
	}
	out := *policy
	return &out
}
