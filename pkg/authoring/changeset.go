package authoring

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

type ChangeSetStatus string

const (
	ChangeSetBlocked          ChangeSetStatus = "blocked"
	ChangeSetReview           ChangeSetStatus = "review"
	ChangeSetEvaluating       ChangeSetStatus = "evaluating"
	ChangeSetAwaitingApproval ChangeSetStatus = "awaiting_approval"
	ChangeSetReady            ChangeSetStatus = "ready"
	ChangeSetApplied          ChangeSetStatus = "applied"
	ChangeSetRejected         ChangeSetStatus = "rejected"
	ChangeSetFailed           ChangeSetStatus = "failed"
)

type ChangeSetActor struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// ChangeSetPolicyFinding is a portable policy result. It intentionally carries
// references and human-readable evidence, never provider-specific policy state.
type ChangeSetPolicyFinding struct {
	PolicyID string `json:"policyId"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

type ChangeSetApprovalRequirement struct {
	PolicyID string `json:"policyId"`
	Role     string `json:"role"`
	Count    int    `json:"count"`
}

// ChangeSetEvaluation is an immutable audit record submitted by a trusted
// policy evaluator. Approval fulfillment is a separate lifecycle seam.
type ChangeSetEvaluation struct {
	ID                   string                         `json:"id"`
	IdempotencyKey       string                         `json:"idempotencyKey"`
	CandidateDigest      string                         `json:"candidateDigest"`
	Allowed              bool                           `json:"allowed"`
	Findings             []ChangeSetPolicyFinding       `json:"findings,omitempty"`
	ApprovalRequirements []ChangeSetApprovalRequirement `json:"approvalRequirements,omitempty"`
	Actor                ChangeSetActor                 `json:"actor"`
	SubmittedAt          time.Time                      `json:"submittedAt"`
}

type ChangeSetLifecycleEvent struct {
	Revision int64           `json:"revision"`
	From     ChangeSetStatus `json:"from,omitempty"`
	To       ChangeSetStatus `json:"to"`
	Reason   string          `json:"reason"`
	Actor    ChangeSetActor  `json:"actor"`
	At       time.Time       `json:"at"`
}

type ChangeSetPlacement struct {
	TeamDeploymentID   string            `json:"teamDeploymentId"`
	AgentDeploymentIDs map[string]string `json:"agentDeploymentIds"`
	Environment        string            `json:"environment,omitempty"`
}

// ChangeSet is a durable, immutable workforce candidate plus mutable governed
// lifecycle. Apply is deliberately a later transition, never a side effect of
// compilation or refinement.
type ChangeSet struct {
	ID              string                    `json:"id"`
	Scope           capability.ScopeReference `json:"scope"`
	ParentID        string                    `json:"parentId,omitempty"`
	Mode            Mode                      `json:"mode"`
	Prompt          string                    `json:"prompt"`
	PromptDigest    string                    `json:"promptDigest"`
	CandidateDigest string                    `json:"candidateDigest"`
	Result          CompileResult             `json:"result"`
	Placement       ChangeSetPlacement        `json:"placement"`
	Status          ChangeSetStatus           `json:"status"`
	Actor           ChangeSetActor            `json:"actor"`
	Evaluations     []ChangeSetEvaluation     `json:"evaluations,omitempty"`
	Lifecycle       []ChangeSetLifecycleEvent `json:"lifecycle"`
	Revision        int64                     `json:"revision"`
	CreatedAt       time.Time                 `json:"createdAt"`
	UpdatedAt       time.Time                 `json:"updatedAt"`
}

type SubmitChangeSetEvaluationRequest struct {
	Scope                capability.ScopeReference      `json:"scope"`
	ChangeSetID          string                         `json:"changeSetId"`
	ExpectedRevision     int64                          `json:"expectedRevision"`
	CandidateDigest      string                         `json:"candidateDigest"`
	Allowed              bool                           `json:"allowed"`
	Findings             []ChangeSetPolicyFinding       `json:"findings,omitempty"`
	ApprovalRequirements []ChangeSetApprovalRequirement `json:"approvalRequirements,omitempty"`
	Actor                ChangeSetActor                 `json:"actor"`
	IdempotencyKey       string                         `json:"idempotencyKey"`
}

type CreateChangeSetRequest struct {
	Scope          capability.ScopeReference `json:"scope"`
	ParentID       string                    `json:"parentId,omitempty"`
	Prompt         string                    `json:"prompt"`
	Catalog        CapabilityCatalog         `json:"catalog"`
	Placement      ChangeSetPlacement        `json:"placement"`
	Actor          ChangeSetActor            `json:"actor"`
	IdempotencyKey string                    `json:"idempotencyKey,omitempty"`
}

type ChangeSetStore interface {
	GetChangeSetByIdempotency(context.Context, capability.ScopeReference, string, string) (*ChangeSet, bool, error)
	CreateChangeSet(context.Context, *ChangeSet, string, string) (*ChangeSet, bool, error)
	GetChangeSet(context.Context, capability.ScopeReference, string) (*ChangeSet, error)
	UpdateChangeSet(context.Context, *ChangeSet, int64) (*ChangeSet, error)
}

var (
	ErrChangeSetNotFound    = errors.New("workforce change set not found")
	ErrChangeSetIdempotency = errors.New("workforce change set idempotency conflict")
	ErrChangeSetRevision    = errors.New("workforce change set revision conflict")
	ErrChangeSetTransition  = errors.New("invalid workforce change set transition")
)

type ChangeSetService struct {
	compiler *Compiler
	store    ChangeSetStore
	now      func() time.Time
}

func NewChangeSetService(compiler *Compiler, store ChangeSetStore) (*ChangeSetService, error) {
	if compiler == nil || store == nil {
		return nil, errors.New("workforce compiler and change set store are required")
	}
	return &ChangeSetService{compiler: compiler, store: store, now: time.Now}, nil
}

func (s *ChangeSetService) Create(ctx context.Context, request CreateChangeSetRequest) (*ChangeSet, bool, error) {
	request.Prompt = strings.TrimSpace(request.Prompt)
	request.ParentID = strings.TrimSpace(request.ParentID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || request.Prompt == "" ||
		strings.TrimSpace(request.Actor.Type) == "" || strings.TrimSpace(request.Actor.ID) == "" || request.IdempotencyKey == "" {
		return nil, false, errors.New("change set scope, prompt, actor, and idempotency key are required")
	}
	mode := ModeCreate
	var existing *WorkforceCandidate
	if request.ParentID != "" {
		parent, err := s.store.GetChangeSet(ctx, request.Scope, request.ParentID)
		if err != nil {
			return nil, false, err
		}
		mode = ModeAmend
		candidate := parent.Result.Candidate
		existing = &candidate
	}
	compileRequest := GenerateRequest{Mode: mode, Prompt: request.Prompt, Existing: existing, Catalog: request.Catalog}
	requestDigest, err := digestChangeSetRequest(request, mode, existing)
	if err != nil {
		return nil, false, err
	}
	if replay, found, err := s.store.GetChangeSetByIdempotency(ctx, request.Scope, request.IdempotencyKey, requestDigest); err != nil || found {
		return replay, found, err
	}
	result, err := s.compiler.Compile(ctx, compileRequest)
	if err != nil {
		return nil, false, err
	}
	candidateDigest, err := digestJSON(result.Candidate)
	if err != nil {
		return nil, false, fmt.Errorf("digest workforce candidate: %w", err)
	}
	now := s.now().UTC()
	status := ChangeSetReview
	if !result.Valid {
		status = ChangeSetBlocked
	}
	changeSet := &ChangeSet{
		ID: uuid.NewString(), Scope: request.Scope, ParentID: request.ParentID, Mode: mode,
		Prompt: request.Prompt, PromptDigest: digestString(request.Prompt), CandidateDigest: candidateDigest,
		Result: *result, Placement: clonePlacement(request.Placement), Status: status, Actor: request.Actor,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	changeSet.Lifecycle = []ChangeSetLifecycleEvent{{Revision: 1, To: status, Reason: "candidate_compiled", Actor: request.Actor, At: now}}
	return s.store.CreateChangeSet(ctx, changeSet, request.IdempotencyKey, requestDigest)
}

func (s *ChangeSetService) Get(ctx context.Context, scope capability.ScopeReference, id string) (*ChangeSet, error) {
	return s.store.GetChangeSet(ctx, scope, strings.TrimSpace(id))
}

func (s *ChangeSetService) SubmitEvaluation(ctx context.Context, request SubmitChangeSetEvaluationRequest) (*ChangeSet, bool, error) {
	request.ChangeSetID = strings.TrimSpace(request.ChangeSetID)
	request.CandidateDigest = strings.TrimSpace(request.CandidateDigest)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || request.ChangeSetID == "" ||
		request.ExpectedRevision < 1 || request.CandidateDigest == "" || request.IdempotencyKey == "" ||
		strings.TrimSpace(request.Actor.Type) == "" || strings.TrimSpace(request.Actor.ID) == "" {
		return nil, false, errors.New("evaluation scope, change set, revision, candidate digest, actor, and idempotency key are required")
	}
	current, err := s.store.GetChangeSet(ctx, request.Scope, request.ChangeSetID)
	if err != nil {
		return nil, false, err
	}
	evaluationDigest, err := digestJSON(struct {
		CandidateDigest string
		Allowed         bool
		Findings        []ChangeSetPolicyFinding
		Approvals       []ChangeSetApprovalRequirement
		Actor           ChangeSetActor
	}{request.CandidateDigest, request.Allowed, request.Findings, request.ApprovalRequirements, request.Actor})
	if err != nil {
		return nil, false, err
	}
	for _, evaluation := range current.Evaluations {
		if evaluation.IdempotencyKey != request.IdempotencyKey {
			continue
		}
		storedDigest, _ := digestJSON(struct {
			CandidateDigest string
			Allowed         bool
			Findings        []ChangeSetPolicyFinding
			Approvals       []ChangeSetApprovalRequirement
			Actor           ChangeSetActor
		}{evaluation.CandidateDigest, evaluation.Allowed, evaluation.Findings, evaluation.ApprovalRequirements, evaluation.Actor})
		if storedDigest != evaluationDigest {
			return nil, false, ErrChangeSetIdempotency
		}
		return current, true, nil
	}
	if current.Revision != request.ExpectedRevision {
		return nil, false, ErrChangeSetRevision
	}
	if current.CandidateDigest != request.CandidateDigest {
		return nil, false, ErrChangeSetRevision
	}
	if current.Status != ChangeSetReview && current.Status != ChangeSetEvaluating {
		return nil, false, fmt.Errorf("%w: cannot evaluate status %s", ErrChangeSetTransition, current.Status)
	}
	for i := range request.ApprovalRequirements {
		request.ApprovalRequirements[i].PolicyID = strings.TrimSpace(request.ApprovalRequirements[i].PolicyID)
		request.ApprovalRequirements[i].Role = strings.TrimSpace(request.ApprovalRequirements[i].Role)
		if request.ApprovalRequirements[i].PolicyID == "" || request.ApprovalRequirements[i].Role == "" || request.ApprovalRequirements[i].Count < 1 {
			return nil, false, errors.New("approval requirements need a policy, role, and positive count")
		}
	}
	if !request.Allowed && len(request.ApprovalRequirements) > 0 {
		return nil, false, errors.New("a denied evaluation cannot require approval")
	}
	now := s.now().UTC()
	nextStatus := ChangeSetRejected
	reason := "policy_denied"
	if request.Allowed && len(request.ApprovalRequirements) > 0 {
		nextStatus, reason = ChangeSetAwaitingApproval, "policy_requires_approval"
	} else if request.Allowed {
		nextStatus, reason = ChangeSetReady, "policy_allowed"
	}
	next := cloneChangeSet(current)
	next.Evaluations = append(next.Evaluations, ChangeSetEvaluation{ID: uuid.NewString(), IdempotencyKey: request.IdempotencyKey,
		CandidateDigest: request.CandidateDigest, Allowed: request.Allowed, Findings: append([]ChangeSetPolicyFinding(nil), request.Findings...),
		ApprovalRequirements: append([]ChangeSetApprovalRequirement(nil), request.ApprovalRequirements...), Actor: request.Actor, SubmittedAt: now})
	next.Status, next.Revision, next.UpdatedAt = nextStatus, current.Revision+1, now
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: nextStatus, Reason: reason, Actor: request.Actor, At: now})
	updated, err := s.store.UpdateChangeSet(ctx, next, current.Revision)
	if errors.Is(err, ErrChangeSetRevision) {
		// A concurrent identical retry may have won the compare-and-swap. Reload
		// once through the normal idempotency path so callers observe a replay,
		// while genuinely competing decisions still receive a revision conflict.
		return s.SubmitEvaluation(ctx, request)
	}
	return updated, false, err
}

func digestChangeSetRequest(request CreateChangeSetRequest, mode Mode, existing *WorkforceCandidate) (string, error) {
	value := struct {
		Scope     capability.ScopeReference
		ParentID  string
		Mode      Mode
		Prompt    string
		Catalog   CapabilityCatalog
		Placement ChangeSetPlacement
		Actor     ChangeSetActor
		Existing  *WorkforceCandidate
	}{request.Scope, request.ParentID, mode, request.Prompt, request.Catalog, request.Placement, request.Actor, existing}
	return digestJSON(value)
}

func digestJSON(value interface{}) (string, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digestString(string(payload)), nil
}

func digestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func clonePlacement(value ChangeSetPlacement) ChangeSetPlacement {
	copy := ChangeSetPlacement{TeamDeploymentID: value.TeamDeploymentID, Environment: value.Environment}
	if value.AgentDeploymentIDs != nil {
		copy.AgentDeploymentIDs = make(map[string]string, len(value.AgentDeploymentIDs))
		for definitionID, deploymentID := range value.AgentDeploymentIDs {
			copy.AgentDeploymentIDs[definitionID] = deploymentID
		}
	}
	return copy
}
