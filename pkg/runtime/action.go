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
)

type ActionCall struct {
	ID                      string                               `json:"id"`
	Scope                   Scope                                `json:"scope"`
	RunID                   string                               `json:"runId"`
	TurnID                  string                               `json:"turnId,omitempty"`
	DeploymentID            string                               `json:"deploymentId"`
	BindingID               string                               `json:"bindingId"`
	BindingRevision         int64                                `json:"bindingRevision"`
	SkillID                 string                               `json:"skillId"`
	SkillVersion            string                               `json:"skillVersion"`
	Action                  string                               `json:"action"`
	Status                  ActionCallStatus                     `json:"status"`
	Risk                    skill.RiskLevel                      `json:"risk"`
	SideEffect              skill.SideEffect                     `json:"sideEffect"`
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
	if c.DuplicateOfActionCallID != "" && c.ExternalOperationDigest == "" {
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

type ApprovalCheckpoint struct {
	ID                     string                  `json:"id"`
	Scope                  Scope                   `json:"scope"`
	RunID                  string                  `json:"runId"`
	ActionCallID           string                  `json:"actionCallId"`
	Status                 ApprovalStatus          `json:"status"`
	Risk                   skill.RiskLevel         `json:"risk"`
	Summary                string                  `json:"summary"`
	PolicyReason           string                  `json:"policyReason,omitempty"`
	ProposedAction         map[string]interface{}  `json:"proposedAction"`
	EvidenceRefs           []string                `json:"evidenceRefs,omitempty"`
	EligibleApprovers      []ApprovalPrincipal     `json:"eligibleApprovers"`
	Destinations           []ApprovalDestination   `json:"destinations,omitempty"`
	ContinuationCheckpoint map[string]interface{}  `json:"continuationCheckpoint,omitempty"`
	ExpiresAt              time.Time               `json:"expiresAt"`
	TimeoutDecision        ApprovalTimeoutDecision `json:"timeoutDecision,omitempty"`
	DecisionBy             *ApprovalPrincipal      `json:"decisionBy,omitempty"`
	DecisionID             string                  `json:"decisionId,omitempty"`
	DecisionReason         string                  `json:"decisionReason,omitempty"`
	Revision               int64                   `json:"revision"`
	CreatedAt              time.Time               `json:"createdAt"`
	UpdatedAt              time.Time               `json:"updatedAt"`
	DecidedAt              *time.Time              `json:"decidedAt,omitempty"`
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
}

type ActionProposalResult struct {
	Call     *ActionCall         `json:"call"`
	Approval *ApprovalCheckpoint `json:"approval,omitempty"`
	Run      *AgentRun           `json:"run"`
	Event    *ActivityEvent      `json:"event,omitempty"`
	Created  bool                `json:"created"`
}

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
	Scope       Scope
	Owner       *ObjectiveOwner
	RunID       string
	Status      []ApprovalStatus
	Limit       int
	Offset      int
	NewestFirst bool
}

type ActionClaim struct {
	Scope         Scope
	WorkerID      string
	Now           time.Time
	LeaseDuration time.Duration
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

type ActionExecutionRecord struct {
	Call                 *ActionCall
	ExpectedCallRevision int64
	Run                  *AgentRun
	ExpectedRunRevision  int64
	WorkerID             string
	Now                  time.Time
	Event                *ActivityEvent
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
		ActionCallStatusCanceled, ActionCallStatusCompensating, ActionCallStatusCompensated:
		return true
	default:
		return false
	}
}

func validApprovalStatus(status ApprovalStatus) bool {
	return status == ApprovalStatusPending || status == ApprovalStatusApproved || status == ApprovalStatusRejected ||
		status == ApprovalStatusChangesRequested || status == ApprovalStatusExpired || status == ApprovalStatusCanceled
}
