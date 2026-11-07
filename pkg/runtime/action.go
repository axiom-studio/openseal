package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

var (
	ErrActionNotFound      = errors.New("action call not found")
	ErrApprovalNotFound    = errors.New("approval checkpoint not found")
	ErrIdempotencyConflict = errors.New("idempotency key was already used for a different action invocation")
	ErrApprovalResolved    = errors.New("approval checkpoint is already resolved")
)

type ActionCallStatus string

const (
	ActionCallStatusReady           ActionCallStatus = "ready"
	ActionCallStatusWaitingApproval ActionCallStatus = "waiting_for_approval"
	ActionCallStatusDenied          ActionCallStatus = "denied"
	ActionCallStatusRunning         ActionCallStatus = "running"
	ActionCallStatusSucceeded       ActionCallStatus = "succeeded"
	ActionCallStatusFailed          ActionCallStatus = "failed"
	ActionCallStatusCanceled        ActionCallStatus = "canceled"
	ActionCallStatusCompensating    ActionCallStatus = "compensating"
	ActionCallStatusCompensated     ActionCallStatus = "compensated"
	// ActionCallStatusRefused is the terminal status of a governed call the
	// kernel refused without running it, such as a repeat of an identical
	// call earlier in the Run. It is never claimable and never protects an
	// external operation.
	ActionCallStatusRefused ActionCallStatus = "refused"
)

// ActionOriginLoop marks a governed call the kernel's run loop proposed for
// one tool call of a step (TurnID); the loop executes it inline.
const ActionOriginLoop ActionOrigin = "loop"

// ActionExecutionLoop marks a call the run loop that proposed it executes
// inline in the claiming replica. Action workers never dispatch it; they only
// close it once its Run ended.
const ActionExecutionLoop ActionExecution = "loop"

const (
	// LoopActionLease is the call lease the loop holds while it executes a
	// call inline; the loop renews it every LoopActionLease/3.
	LoopActionLease = time.Minute
	// GovernedMaxConcurrentReads bounds concurrent inline reads of one step.
	GovernedMaxConcurrentReads = 4
)

// ActionErrorCodeInterrupted is the ErrorCode of a write recovered from a
// lost execution: its outcome is unknown, so it fails without redispatch.
const ActionErrorCodeInterrupted = "interrupted"

// ActionFailurePhase is trusted execution provenance retained in the durable
// ActionCall payload. Missing legacy provenance never proves a mutation safe.
type ActionFailurePhase string

const (
	ActionFailureBeforeDispatch ActionFailurePhase = "before_dispatch"
	ActionFailureAfterDispatch  ActionFailurePhase = "after_dispatch"
)

type ActionCall struct {
	ID              string `json:"id"`
	Scope           Scope  `json:"scope"`
	RunID           string `json:"runId"`
	TurnID          string `json:"turnId,omitempty"`
	DeploymentID    string `json:"deploymentId"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	SkillID         string `json:"skillId"`
	SkillVersion    string `json:"skillVersion"`
	Action          string `json:"action"`
	// Origin is ActionOriginLoop for a governed tool call of a run loop step
	// and ActionOriginVM for a nested call made during a step by code in the
	// chat's VM. Neither changes its Run's lifecycle by itself.
	Origin ActionOrigin `json:"origin,omitempty"`
	// OriginRef is the host's grouping reference for a nested call, such as
	// the workspace command it ran under.
	OriginRef string `json:"originRef,omitempty"`
	// Execution is empty for a call an action worker dispatches to its
	// Skill. ActionExecutionHost marks a nested call the host that proposed it
	// performs itself under a credential lease (see ProposeNestedHost);
	// ActionExecutionLoop marks a loop call its run loop executes inline.
	// Action workers dispatch neither.
	Execution  ActionExecution  `json:"execution,omitempty"`
	Status     ActionCallStatus `json:"status"`
	Risk       skill.RiskLevel  `json:"risk"`
	SideEffect skill.SideEffect `json:"sideEffect"`
	// Review is the action's manifest review floor captured at proposal time,
	// so later approval-mode sweeps cannot waive an always-reviewed action.
	Review                  skill.ActionReview                   `json:"review,omitempty"`
	Arguments               map[string]interface{}               `json:"arguments"`
	ResolvedArguments       map[string]ResolvedActionArgument    `json:"resolvedArguments,omitempty"`
	PreparedRuntime         *skill.PreparedRuntime               `json:"preparedRuntime,omitempty"`
	CredentialRefs          map[string]skill.CredentialReference `json:"credentialRefs,omitempty"`
	EvidenceRefs            []string                             `json:"evidenceRefs,omitempty"`
	IdempotencyKey          string                               `json:"idempotencyKey,omitempty"`
	InvocationDigest        string                               `json:"invocationDigest,omitempty"`
	SemanticDigest          string                               `json:"semanticDigest,omitempty"`
	ExternalOperationDigest string                               `json:"externalOperationDigest,omitempty"`
	DuplicateOfActionCallID string                               `json:"duplicateOfActionCallId,omitempty"`
	ApprovalID              string                               `json:"approvalId,omitempty"`
	Attempt                 int                                  `json:"attempt"`
	MaxAttempts             int                                  `json:"maxAttempts"`
	AvailableAt             time.Time                            `json:"availableAt"`
	LeaseOwner              string                               `json:"leaseOwner,omitempty"`
	LeaseExpiresAt          *time.Time                           `json:"leaseExpiresAt,omitempty"`
	Output                  map[string]interface{}               `json:"output,omitempty"`
	Error                   string                               `json:"error,omitempty"`
	ErrorCode               string                               `json:"errorCode,omitempty"`
	ErrorDetails            map[string]string                    `json:"errorDetails,omitempty"`
	FailurePhase            ActionFailurePhase                   `json:"failurePhase,omitempty"`
	RecoveredRunning        bool                                 `json:"recoveredRunning,omitempty"`
	Revision                int64                                `json:"revision"`
	CreatedAt               time.Time                            `json:"createdAt"`
	UpdatedAt               time.Time                            `json:"updatedAt"`
	StartedAt               *time.Time                           `json:"startedAt,omitempty"`
	CompletedAt             *time.Time                           `json:"completedAt,omitempty"`
}

// ResolvedActionArgument records why a persisted argument was not selected by
// the model. It intentionally records provenance only; the resolved value is
// already governed by Arguments and no claim set is copied into activity.
type ResolvedActionArgument struct {
	Source skill.BindingArgumentSource `json:"source"`
	Claim  string                      `json:"claim,omitempty"`
}

func (c *ActionCall) Validate() error {
	if c == nil {
		return errors.New("action call is required")
	}
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.RunID) == "" || strings.TrimSpace(c.DeploymentID) == "" ||
		strings.TrimSpace(c.SkillID) == "" || strings.TrimSpace(c.SkillVersion) == "" || strings.TrimSpace(c.Action) == "" {
		return errors.New("action identity, run, deployment, skill, version, and action are required")
	}
	if (strings.TrimSpace(c.BindingID) == "") != (c.BindingRevision == 0) || c.BindingRevision < 0 {
		return errors.New("action binding id and revision must be provided together")
	}
	if !validActionCallStatus(c.Status) || c.Revision < 1 || c.MaxAttempts < 1 || c.Attempt < 0 {
		return errors.New("action call lifecycle metadata is invalid")
	}
	switch c.Origin {
	case ActionOriginVM:
		if strings.TrimSpace(c.TurnID) == "" {
			return errors.New("a nested action call requires its step")
		}
		if c.Execution != "" && c.Execution != ActionExecutionHost {
			return errors.New("a nested action call is performed by an action worker or its host")
		}
	case ActionOriginLoop:
		if strings.TrimSpace(c.TurnID) == "" || c.Execution != ActionExecutionLoop || c.OriginRef != "" {
			return errors.New("a loop action call requires its step, loop execution and no origin reference")
		}
	default:
		return errors.New("action origin is invalid")
	}
	if c.FailurePhase != "" && (c.Status != ActionCallStatusFailed || c.FailurePhase != ActionFailureBeforeDispatch && c.FailurePhase != ActionFailureAfterDispatch) {
		return errors.New("action failure phase is invalid")
	}
	if c.IdempotencyKey != "" && c.InvocationDigest == "" {
		return errors.New("idempotent action calls require an invocation digest")
	}
	if c.SemanticDigest != "" && c.SemanticDigest != ComputeActionSemanticDigest(c) {
		return errors.New("action semantic digest does not match its invocation")
	}
	if c.ExternalOperationDigest != "" {
		if err := validateSHA256Digest(c.ExternalOperationDigest); err != nil {
			return errors.New("external operation digest is invalid")
		}
	}
	// A refused call names the earlier identical call of its Run; any other
	// duplicate is a cross-Run external operation receipt.
	if c.DuplicateOfActionCallID != "" && c.ExternalOperationDigest == "" && c.Status != ActionCallStatusRefused {
		return errors.New("duplicate action calls require an external operation digest")
	}
	if c.PreparedRuntime != nil {
		if err := skill.ValidatePreparedRuntimeReference(c.PreparedRuntime); err != nil {
			return err
		}
	}
	if err := uniqueIDs(c.EvidenceRefs, "action evidence"); err != nil {
		return err
	}
	for name, resolved := range c.ResolvedArguments {
		if strings.TrimSpace(name) == "" {
			return errors.New("resolved action argument name is required")
		}
		if _, ok := c.Arguments[name]; !ok {
			return errors.New("resolved action argument must exist in persisted arguments")
		}
		switch resolved.Source {
		case skill.BindingArgumentLiteral, skill.BindingArgumentSessionID:
			if resolved.Claim != "" {
				return errors.New("resolved literal and session arguments cannot name a claim")
			}
		case skill.BindingArgumentVerifiedClaim:
			if strings.TrimSpace(resolved.Claim) == "" {
				return errors.New("resolved verified claim argument requires its claim name")
			}
		default:
			return errors.New("resolved action argument source is invalid")
		}
	}
	return nil
}

func ComputeActionInvocationDigest(call *ActionCall) string {
	if call == nil {
		return ""
	}
	canonical := struct {
		DeploymentID      string                               `json:"deploymentId"`
		BindingID         string                               `json:"bindingId"`
		BindingRevision   int64                                `json:"bindingRevision"`
		SkillID           string                               `json:"skillId"`
		SkillVersion      string                               `json:"skillVersion"`
		Action            string                               `json:"action"`
		Arguments         map[string]interface{}               `json:"arguments,omitempty"`
		ResolvedArguments map[string]ResolvedActionArgument    `json:"resolvedArguments,omitempty"`
		PreparedRuntime   *skill.PreparedRuntime               `json:"preparedRuntime,omitempty"`
		CredentialRefs    map[string]skill.CredentialReference `json:"credentialRefs,omitempty"`
		EvidenceRefs      []string                             `json:"evidenceRefs,omitempty"`
	}{call.DeploymentID, call.BindingID, call.BindingRevision, call.SkillID, call.SkillVersion, call.Action, call.Arguments, call.ResolvedArguments, call.PreparedRuntime, call.CredentialRefs, call.EvidenceRefs}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// ComputeActionSemanticDigest identifies the authorized capability invocation
// independently of a model-selected idempotency key and mutable evidence refs.
// It is used only to reuse an already-succeeded action inside the same Run.
func ComputeActionSemanticDigest(call *ActionCall) string {
	if call == nil {
		return ""
	}
	canonical := struct {
		DeploymentID      string                            `json:"deploymentId"`
		BindingID         string                            `json:"bindingId"`
		BindingRevision   int64                             `json:"bindingRevision"`
		SkillID           string                            `json:"skillId"`
		SkillVersion      string                            `json:"skillVersion"`
		Action            string                            `json:"action"`
		Arguments         map[string]interface{}            `json:"arguments,omitempty"`
		ResolvedArguments map[string]ResolvedActionArgument `json:"resolvedArguments,omitempty"`
		PreparedRuntime   *skill.PreparedRuntime            `json:"preparedRuntime,omitempty"`
	}{call.DeploymentID, call.BindingID, call.BindingRevision, call.SkillID, call.SkillVersion, call.Action, call.Arguments, call.ResolvedArguments, call.PreparedRuntime}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func clonePreparedRuntime(value *skill.PreparedRuntime) *skill.PreparedRuntime {
	if value == nil {
		return nil
	}
	result := *value
	result.Executables = append([]string(nil), value.Executables...)
	return &result
}

type ApprovalStatus string

const (
	ApprovalStatusPending          ApprovalStatus = "pending"
	ApprovalStatusApproved         ApprovalStatus = "approved"
	ApprovalStatusRejected         ApprovalStatus = "rejected"
	ApprovalStatusChangesRequested ApprovalStatus = "changes_requested"
	ApprovalStatusExpired          ApprovalStatus = "expired"
	ApprovalStatusCanceled         ApprovalStatus = "canceled"
	// ApprovalStatusSuperseded closes a routine brief the person answered
	// with a chat message instead of a decision. Like changes_requested it
	// resumes the Run, which revises the proposal from that message.
	ApprovalStatusSuperseded ApprovalStatus = "superseded"
)

// ApprovalDecision is the reviewer's explicit decision on one immutable
// proposal revision. Requesting changes closes that proposal and resumes its
// Run with reviewer guidance; a revised proposal requires a new approval.
type ApprovalDecision string

const (
	ApprovalDecisionApprove        ApprovalDecision = "approve"
	ApprovalDecisionReject         ApprovalDecision = "reject"
	ApprovalDecisionRequestChanges ApprovalDecision = "request_changes"
)

func (d ApprovalDecision) Validate() error {
	switch d {
	case ApprovalDecisionApprove, ApprovalDecisionReject, ApprovalDecisionRequestChanges:
		return nil
	default:
		return errors.New("approval decision is invalid")
	}
}

// ApprovalTimeoutDecision is the reviewed outcome applied when an approval
// remains pending through its deadline. The zero value is deliberately the
// safe expire behavior for approvals created before this policy existed.
type ApprovalTimeoutDecision string

const (
	ApprovalTimeoutExpire  ApprovalTimeoutDecision = "expire"
	ApprovalTimeoutApprove ApprovalTimeoutDecision = "approve"
)

type ApprovalPrincipal struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type ApprovalDestination struct {
	EndpointID string `json:"endpointId"`
}

// ApprovalConversationContext is a list projection, never an authority for decisions.
type ApprovalConversationContext struct {
	ConversationID   string `json:"conversationId,omitempty"`
	TriggerMessageID string `json:"triggerMessageId,omitempty"`
	// ThreadRootMessageID is set by conversation change projections so a
	// thread view can place the card without reading the Run lineage.
	ThreadRootMessageID string `json:"threadRootMessageId,omitempty"`
}

type ApprovalCheckpoint struct {
	ConversationContext *ApprovalConversationContext `json:"conversationContext,omitempty"`
	ID                  string                       `json:"id"`
	Scope               Scope                        `json:"scope"`
	RunID               string                       `json:"runId"`
	ActionCallID        string                       `json:"actionCallId"`
	Status              ApprovalStatus               `json:"status"`
	Risk                skill.RiskLevel              `json:"risk"`
	Summary             string                       `json:"summary"`
	PolicyReason        string                       `json:"policyReason,omitempty"`
	ProposedAction      map[string]interface{}       `json:"proposedAction"`
	EvidenceRefs        []string                     `json:"evidenceRefs,omitempty"`
	EligibleApprovers   []ApprovalPrincipal          `json:"eligibleApprovers"`
	Destinations        []ApprovalDestination        `json:"destinations,omitempty"`
	ExpiresAt           time.Time                    `json:"expiresAt"`
	TimeoutDecision     ApprovalTimeoutDecision      `json:"timeoutDecision,omitempty"`
	DecisionBy          *ApprovalPrincipal           `json:"decisionBy,omitempty"`
	DecisionID          string                       `json:"decisionId,omitempty"`
	DecisionReason      string                       `json:"decisionReason,omitempty"`
	Revision            int64                        `json:"revision"`
	CreatedAt           time.Time                    `json:"createdAt"`
	UpdatedAt           time.Time                    `json:"updatedAt"`
	DecidedAt           *time.Time                   `json:"decidedAt,omitempty"`
	// SupersededByMessageID is the newer message that superseded this
	// approval, set when it is resolved as superseded.
	SupersededByMessageID string `json:"supersededByMessageId,omitempty"`
}

func (a *ApprovalCheckpoint) Validate() error {
	if a == nil {
		return errors.New("approval checkpoint is required")
	}
	if err := a.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.ID) == "" || strings.TrimSpace(a.RunID) == "" || strings.TrimSpace(a.ActionCallID) == "" ||
		strings.TrimSpace(a.Summary) == "" || a.Revision < 1 || a.ExpiresAt.IsZero() || len(a.EligibleApprovers) == 0 {
		return errors.New("approval identity, run, action, summary, expiry, approvers, and revision are required")
	}
	if !validApprovalStatus(a.Status) {
		return errors.New("approval status is invalid")
	}
	if a.TimeoutDecision != "" && a.TimeoutDecision != ApprovalTimeoutExpire && a.TimeoutDecision != ApprovalTimeoutApprove {
		return errors.New("approval timeout decision is invalid")
	}
	for _, principal := range a.EligibleApprovers {
		if strings.TrimSpace(principal.Type) == "" || strings.TrimSpace(principal.ID) == "" {
			return errors.New("approval principals require type and id")
		}
	}
	seenDestinations := make(map[string]bool, len(a.Destinations))
	for _, destination := range a.Destinations {
		id := strings.TrimSpace(destination.EndpointID)
		if id == "" || len(id) > 256 || seenDestinations[id] {
			return errors.New("approval destinations require unique portable endpoint ids")
		}
		seenDestinations[id] = true
	}
	return nil
}

type ActionProposalRecord struct {
	Call                *ActionCall
	Approval            *ApprovalCheckpoint
	Run                 *AgentRun
	ExpectedRunRevision int64
	Lease               *AgentRunLeaseGuard
	Event               *ActivityEvent
	// RequireBindingFence is supplied by the kernel's catalog configuration,
	// never by model arguments. Durable catalogs share submission authority
	// with the store and must serialize it against binding changes.
	RequireBindingFence bool
	// HostBinding is the host-supplied binding the call was resolved
	// through (see skill.HostBindingProvider). It has no stored row: the
	// fence requires that none exists and checks the call against it.
	HostBinding *skill.Binding
	// Nested is set, with Run nil, for a nested call. The store then checks
	// the fence instead of updating the Run.
	Nested *NestedActionFence
}

type ActionProposalResult struct {
	Call     *ActionCall         `json:"call"`
	Approval *ApprovalCheckpoint `json:"approval,omitempty"`
	Run      *AgentRun           `json:"run"`
	Event    *ActivityEvent      `json:"event,omitempty"`
	Created  bool                `json:"created"`
}

// ApprovalResolutionRecord updates its Run, except for a nested call's
// approval (Run nil), which leaves the Run unchanged.
type ApprovalResolutionRecord struct {
	Approval                 *ApprovalCheckpoint
	ExpectedApprovalRevision int64
	Call                     *ActionCall
	ExpectedCallRevision     int64
	Run                      *AgentRun
	ExpectedRunRevision      int64
	Event                    *ActivityEvent
}

type ApprovalResolutionResult struct {
	Approval *ApprovalCheckpoint `json:"approval"`
	Call     *ActionCall         `json:"call"`
	Run      *AgentRun           `json:"run"`
	Event    *ActivityEvent      `json:"event,omitempty"`
	Resolved bool                `json:"resolved"`
}

type ActionFilter struct {
	Scope  Scope
	RunID  string
	Status []ActionCallStatus
	Limit  int
	Offset int
}

type ApprovalFilter struct {
	ConversationID  string
	IncludeUnscoped bool // Only the owner's legacy control conversation may include unscoped work.
	Scope           Scope
	Owner           *ObjectiveOwner
	RunID           string
	RunIDs          []string // When set, only approvals for these exact Runs.
	Status          []ApprovalStatus
	Limit           int
	Offset          int
	NewestFirst     bool
}

type ActionClaim struct {
	Scope         Scope
	WorkerID      string
	Now           time.Time
	LeaseDuration time.Duration
	// OwnActionID claims exactly this host- or loop-executed call for the
	// host or loop that proposed it (see ClaimNestedHostAction and the run
	// loop's inline execution). It may take a ready call or a running call
	// whose lease expired; callers that need a ready call check that first.
	// Without it a claim takes the next call an action worker may run: never
	// a host call that became ready within NestedHostActionReadyGrace, which
	// its host claims itself, and never a loop call of a Run that has not
	// ended, which only its loop runs.
	OwnActionID string
}

func (c ActionClaim) Validate() error {
	if err := c.Scope.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.WorkerID) == "" || c.Now.IsZero() || c.LeaseDuration <= 0 {
		return errors.New("action worker, current time, and lease duration are required")
	}
	return nil
}

// claimSelects reports whether a claim may take call, apart from its status,
// availability, paused dependency and maintenance, which every store checks.
// run is the call's Run row (nil when missing).
func (c ActionClaim) claimSelects(call *ActionCall, run *AgentRun) bool {
	if c.OwnActionID != "" {
		return call.ID == c.OwnActionID && (call.Execution == ActionExecutionHost || call.Execution == ActionExecutionLoop)
	}
	switch call.Execution {
	case ActionExecutionHost:
		return call.Status != ActionCallStatusReady || !call.AvailableAt.After(c.hostReadyCutoff())
	case ActionExecutionLoop:
		// Only the loop runs its calls. A worker takes one only after its Run
		// ended, to close it without dispatch.
		return run == nil || isTerminalAgentRunStatus(run.Status)
	}
	return true
}

// hostReadyCutoff is the latest time a ready host call became available for
// an action worker to recover it.
func (c ActionClaim) hostReadyCutoff() time.Time {
	return c.Now.Add(-NestedHostActionReadyGrace)
}

type ActionExecutionRecord struct {
	Call                 *ActionCall
	ExpectedCallRevision int64
	Run                  *AgentRun
	// ExpectedRunRevision fences the observed owner even when Run is nil and
	// execution only finishes a stale ActionCall without changing its owner.
	ExpectedRunRevision int64
	WorkerID            string
	Now                 time.Time
	Event               *ActivityEvent
}

type ActionExecutionResult struct {
	Call  *ActionCall
	Run   *AgentRun
	Event *ActivityEvent
}

type ActionStore interface {
	CreateActionProposal(ctx context.Context, proposal ActionProposalRecord) (*ActionProposalResult, error)
	GetActionCall(ctx context.Context, scope Scope, actionID string) (*ActionCall, error)
	ListActionCalls(ctx context.Context, filter ActionFilter) ([]*ActionCall, error)
	GetActionCallByExternalOperation(ctx context.Context, scope Scope, digest string) (*ActionCall, error)
	// GetActionCallByIdempotencyKey returns the Run's call with this
	// idempotency key, or ErrActionNotFound.
	GetActionCallByIdempotencyKey(ctx context.Context, scope Scope, runID, key string) (*ActionCall, error)
	GetApproval(ctx context.Context, scope Scope, approvalID string) (*ApprovalCheckpoint, error)
	ListApprovals(ctx context.Context, filter ApprovalFilter) ([]*ApprovalCheckpoint, error)
	ResolveApproval(ctx context.Context, resolution ApprovalResolutionRecord) (*ApprovalResolutionResult, error)
	ClaimNextAction(ctx context.Context, claim ActionClaim) (*ActionCall, error)
	RenewActionLease(ctx context.Context, scope Scope, actionID, workerID string, now time.Time, leaseDuration time.Duration) (*ActionCall, error)
	PersistActionExecution(ctx context.Context, execution ActionExecutionRecord) (*ActionExecutionResult, error)
}

func validActionCallStatus(status ActionCallStatus) bool {
	switch status {
	case ActionCallStatusReady, ActionCallStatusWaitingApproval, ActionCallStatusDenied,
		ActionCallStatusRunning, ActionCallStatusSucceeded, ActionCallStatusFailed,
		ActionCallStatusCanceled, ActionCallStatusCompensating, ActionCallStatusCompensated, ActionCallStatusRefused:
		return true
	default:
		return false
	}
}

func validApprovalStatus(status ApprovalStatus) bool {
	return status == ApprovalStatusPending || status == ApprovalStatusApproved || status == ApprovalStatusRejected ||
		status == ApprovalStatusChangesRequested || status == ApprovalStatusExpired || status == ApprovalStatusCanceled ||
		status == ApprovalStatusSuperseded
}

// approvalRevisesProposal reports a decision that closes the proposal and
// asks the Run for a revised one rather than ending the attempt.
func approvalRevisesProposal(status string) bool {
	return status == string(ApprovalStatusChangesRequested) || status == string(ApprovalStatusSuperseded)
}
