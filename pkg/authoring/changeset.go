package authoring

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
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

// ChangeSetApprovalDecision records one principal's immutable decision against
// one requirement emitted by a specific evaluation. Enterprise hosts own the
// identity and role authorization boundary before submitting this fact.
type ChangeSetApprovalDecision struct {
	ID             string         `json:"id"`
	IdempotencyKey string         `json:"idempotencyKey"`
	EvaluationID   string         `json:"evaluationId"`
	PolicyID       string         `json:"policyId"`
	Role           string         `json:"role"`
	Approved       bool           `json:"approved"`
	Reason         string         `json:"reason,omitempty"`
	Actor          ChangeSetActor `json:"actor"`
	DecidedAt      time.Time      `json:"decidedAt"`
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
	// Expected revisions are mandatory for amendments and make stale placement
	// fail before any definition, deployment, or objective is written.
	TeamExpectedRevision   int64                                                `json:"teamExpectedRevision,omitempty"`
	AgentExpectedRevisions map[string]int64                                     `json:"agentExpectedRevisions,omitempty"`
	CredentialReferences   map[string]map[string]capability.CredentialReference `json:"credentialReferences,omitempty"`
	Objectives             map[string]ObjectivePlacement                        `json:"objectives,omitempty"`
	Environment            string                                               `json:"environment,omitempty"`
}

type ObjectivePlacement struct {
	ID               string `json:"id"`
	ExpectedRevision int64  `json:"expectedRevision,omitempty"`
}

type AppliedResourceReference struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Version  string `json:"version,omitempty"`
	Revision int64  `json:"revision,omitempty"`
}

type ChangeSetApplyReceipt struct {
	ID              string                     `json:"id"`
	IdempotencyKey  string                     `json:"idempotencyKey"`
	CandidateDigest string                     `json:"candidateDigest"`
	Reason          string                     `json:"reason"`
	Resources       []AppliedResourceReference `json:"resources"`
	Actor           ChangeSetActor             `json:"actor"`
	AppliedAt       time.Time                  `json:"appliedAt"`
}

// ChangeSet is a durable, immutable workforce candidate plus mutable governed
// lifecycle. Apply is deliberately a later transition, never a side effect of
// compilation or refinement.
type ChangeSet struct {
	ID                  string                      `json:"id"`
	Scope               capability.ScopeReference   `json:"scope"`
	ParentID            string                      `json:"parentId,omitempty"`
	Mode                Mode                        `json:"mode"`
	Prompt              string                      `json:"prompt"`
	PromptDigest        string                      `json:"promptDigest"`
	CandidateDigest     string                      `json:"candidateDigest"`
	Result              CompileResult               `json:"result"`
	Catalog             CapabilityCatalog           `json:"catalog"`
	Placement           ChangeSetPlacement          `json:"placement"`
	RequiredCredentials map[string][]string         `json:"requiredCredentials,omitempty"`
	Status              ChangeSetStatus             `json:"status"`
	Actor               ChangeSetActor              `json:"actor"`
	Generation          *ChangeSetGeneration        `json:"generation,omitempty"`
	Evaluations         []ChangeSetEvaluation       `json:"evaluations,omitempty"`
	ApprovalDecisions   []ChangeSetApprovalDecision `json:"approvalDecisions,omitempty"`
	ApplyReceipt        *ChangeSetApplyReceipt      `json:"applyReceipt,omitempty"`
	Lifecycle           []ChangeSetLifecycleEvent   `json:"lifecycle"`
	Revision            int64                       `json:"revision"`
	CreatedAt           time.Time                   `json:"createdAt"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
}

// ChangeSetGeneration is the durable, credential-free input and progress for
// probabilistic candidate generation. Hosts may enqueue it into their canonical
// Run scheduler after Prepare returns; the prompt request does not need to stay
// connected while generation is in flight.
type ChangeSetGeneration struct {
	Request     GenerateRequest            `json:"request"`
	RunID       string                     `json:"runId,omitempty"`
	Attempt     int                        `json:"attempt"`
	FailureCode string                     `json:"failureCode,omitempty"`
	LastError   string                     `json:"lastError,omitempty"`
	CompletedAt *time.Time                 `json:"completedAt,omitempty"`
	Retries     []ChangeSetGenerationRetry `json:"retries,omitempty"`
}

type ChangeSetGenerationRetry struct {
	IdempotencyKey   string         `json:"idempotencyKey"`
	RequestDigest    string         `json:"requestDigest"`
	Attempt          int            `json:"attempt"`
	ExpectedRevision int64          `json:"expectedRevision"`
	Reason           string         `json:"reason"`
	Actor            ChangeSetActor `json:"actor"`
	RequestedAt      time.Time      `json:"requestedAt"`
}

type RetryChangeSetGenerationRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ChangeSetID      string                    `json:"changeSetId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	Reason           string                    `json:"reason"`
	Actor            ChangeSetActor            `json:"actor"`
	IdempotencyKey   string                    `json:"idempotencyKey"`
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

type ResolveChangeSetApprovalRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ChangeSetID      string                    `json:"changeSetId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	EvaluationID     string                    `json:"evaluationId"`
	PolicyID         string                    `json:"policyId"`
	Role             string                    `json:"role"`
	Approved         bool                      `json:"approved"`
	Reason           string                    `json:"reason,omitempty"`
	Actor            ChangeSetActor            `json:"actor"`
	IdempotencyKey   string                    `json:"idempotencyKey"`
}

type ApplyChangeSetRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ChangeSetID      string                    `json:"changeSetId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	CandidateDigest  string                    `json:"candidateDigest"`
	Reason           string                    `json:"reason"`
	Actor            ChangeSetActor            `json:"actor"`
	IdempotencyKey   string                    `json:"idempotencyKey"`
}

// AtomicChangeSetStore is deliberately stronger than ChangeSetStore. Hosts
// advertise Apply only when their single store owns every workforce table and
// can commit this aggregate as one transaction/CAS.
type AtomicChangeSetStore interface {
	ChangeSetStore
	ApplyChangeSet(context.Context, *ChangeSet, int64) (*ChangeSet, error)
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
	CompleteChangeSetGeneration(context.Context, *ChangeSet, int64) (*ChangeSet, error)
}

// PendingChangeSetGenerationStore is an optional recovery index implemented by
// durable stores that host asynchronous generation workers.
type PendingChangeSetGenerationStore interface {
	ListPendingChangeSetGenerations(context.Context, capability.ScopeReference, int) ([]*ChangeSet, error)
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
	canonicalizeCandidateScope(&result.Candidate, request.Scope)
	canonicalizePlacement(&request.Placement, request.Scope, &result.Candidate, existing)
	result.Validation = validateCandidate(&result.Candidate, existing)
	result.MissingRequirements = missingRequirements(&result.Candidate, request.Catalog)
	result.RiskChanges = riskChanges(existing, &result.Candidate)
	result.Diff = workforceDiff(existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.Questions) == 0
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
		Result: *result, Catalog: cloneCapabilityCatalog(request.Catalog), Placement: clonePlacement(request.Placement), RequiredCredentials: requiredCredentials(result.Candidate, request.Catalog), Status: status, Actor: request.Actor,
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	changeSet.Lifecycle = []ChangeSetLifecycleEvent{{Revision: 1, To: status, Reason: "candidate_compiled", Actor: request.Actor, At: now}}
	return s.store.CreateChangeSet(ctx, changeSet, request.IdempotencyKey, requestDigest)
}

// Prepare durably records generation intent before any model call. Replaying
// the same idempotency key returns the same aggregate without another call.
func (s *ChangeSetService) Prepare(ctx context.Context, request CreateChangeSetRequest) (*ChangeSet, bool, error) {
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
	now := s.now().UTC()
	changeSet := &ChangeSet{
		ID: uuid.NewString(), Scope: request.Scope, ParentID: request.ParentID, Mode: mode,
		Prompt: request.Prompt, PromptDigest: digestString(request.Prompt), Catalog: cloneCapabilityCatalog(request.Catalog), Placement: clonePlacement(request.Placement),
		Status: ChangeSetEvaluating, Actor: request.Actor, Generation: &ChangeSetGeneration{Request: compileRequest},
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	changeSet.Generation.Request.InvocationKey = generationInvocationKey(changeSet.ID, 0)
	changeSet.Lifecycle = []ChangeSetLifecycleEvent{{Revision: 1, To: ChangeSetEvaluating, Reason: "candidate_generation_queued", Actor: request.Actor, At: now}}
	return s.store.CreateChangeSet(ctx, changeSet, request.IdempotencyKey, requestDigest)
}

// GeneratePrepared compiles one previously persisted generation intent and
// commits the immutable candidate with revision CAS.
func (s *ChangeSetService) GeneratePrepared(ctx context.Context, scope capability.ScopeReference, id string, expectedRevision int64) (*ChangeSet, error) {
	changeSet, err := s.store.GetChangeSet(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if changeSet.Revision != expectedRevision || changeSet.Status != ChangeSetEvaluating || changeSet.Generation == nil {
		return nil, ErrChangeSetRevision
	}
	changeSet.Generation.Attempt++
	result, err := s.compiler.Compile(ctx, changeSet.Generation.Request)
	if err != nil {
		// Host shutdown is not a candidate failure. The leased Run worker yields
		// this unchanged evaluating intent so another process can resume it with
		// the same stable provider invocation key.
		if errors.Is(err, context.Canceled) {
			return nil, err
		}
		failureCode, publicMessage := classifyGenerationFailure(err)
		failed := cloneChangeSet(changeSet)
		failed.Status = ChangeSetFailed
		failed.Generation.FailureCode = failureCode
		failed.Generation.LastError = publicMessage
		failed.Revision++
		failed.UpdatedAt = s.now().UTC()
		failed.Lifecycle = append(failed.Lifecycle, ChangeSetLifecycleEvent{Revision: failed.Revision, From: ChangeSetEvaluating, To: ChangeSetFailed, Reason: "candidate_generation_failed", Actor: failed.Actor, At: failed.UpdatedAt})
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		persisted, updateErr := s.store.UpdateChangeSet(persistCtx, failed, expectedRevision)
		if updateErr != nil {
			return nil, updateErr
		}
		return persisted, err
	}
	existing := changeSet.Generation.Request.Existing
	canonicalizeCandidateScope(&result.Candidate, changeSet.Scope)
	canonicalizePlacement(&changeSet.Placement, changeSet.Scope, &result.Candidate, existing)
	result.Validation = validateCandidate(&result.Candidate, existing)
	result.MissingRequirements = missingRequirements(&result.Candidate, changeSet.Catalog)
	result.RiskChanges = riskChanges(existing, &result.Candidate)
	result.Diff = workforceDiff(existing, &result.Candidate)
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.Questions) == 0
	candidateDigest, err := digestJSON(result.Candidate)
	if err != nil {
		return nil, fmt.Errorf("digest workforce candidate: %w", err)
	}
	now := s.now().UTC()
	status := ChangeSetReview
	if !result.Valid {
		status = ChangeSetBlocked
	}
	changeSet.CandidateDigest = candidateDigest
	changeSet.Result = *result
	changeSet.RequiredCredentials = requiredCredentials(result.Candidate, changeSet.Catalog)
	changeSet.Status = status
	changeSet.Revision++
	changeSet.UpdatedAt = now
	changeSet.Generation.CompletedAt = &now
	changeSet.Generation.FailureCode = ""
	changeSet.Generation.LastError = ""
	changeSet.Lifecycle = append(changeSet.Lifecycle, ChangeSetLifecycleEvent{Revision: changeSet.Revision, From: ChangeSetEvaluating, To: status, Reason: "candidate_compiled", Actor: changeSet.Actor, At: now})
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return s.store.CompleteChangeSetGeneration(persistCtx, changeSet, expectedRevision)
}

func (s *ChangeSetService) Get(ctx context.Context, scope capability.ScopeReference, id string) (*ChangeSet, error) {
	return s.store.GetChangeSet(ctx, scope, strings.TrimSpace(id))
}

// RetryGeneration explicitly requeues a failed model attempt. The previous
// error and lifecycle remain auditable; a host schedules a new canonical Run.
func (s *ChangeSetService) RetryGeneration(ctx context.Context, request RetryChangeSetGenerationRequest) (*ChangeSet, bool, error) {
	request.ChangeSetID = strings.TrimSpace(request.ChangeSetID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.Actor.Type, request.Actor.ID = strings.TrimSpace(request.Actor.Type), strings.TrimSpace(request.Actor.ID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || request.ChangeSetID == "" ||
		request.ExpectedRevision < 1 || request.Reason == "" || request.Actor.Type == "" || request.Actor.ID == "" || request.IdempotencyKey == "" {
		return nil, false, errors.New("retry scope, change set, revision, reason, actor, and idempotency key are required")
	}
	current, err := s.store.GetChangeSet(ctx, request.Scope, request.ChangeSetID)
	if err != nil {
		return nil, false, err
	}
	retryDigest, err := digestJSON(struct {
		Scope            capability.ScopeReference
		ChangeSetID      string
		ExpectedRevision int64
		Reason           string
		Actor            ChangeSetActor
	}{request.Scope, request.ChangeSetID, request.ExpectedRevision, request.Reason, request.Actor})
	if err != nil {
		return nil, false, err
	}
	if current.Generation != nil {
		for _, retry := range current.Generation.Retries {
			if retry.IdempotencyKey != request.IdempotencyKey {
				continue
			}
			if retry.RequestDigest != retryDigest {
				return nil, false, ErrChangeSetIdempotency
			}
			return current, true, nil
		}
	}
	if current.Revision != request.ExpectedRevision {
		return nil, false, ErrChangeSetRevision
	}
	if current.Status != ChangeSetFailed || current.Generation == nil {
		return nil, false, fmt.Errorf("%w: cannot retry status %s", ErrChangeSetTransition, current.Status)
	}
	next := cloneChangeSet(current)
	now := s.now().UTC()
	next.Status, next.Revision, next.UpdatedAt = ChangeSetEvaluating, current.Revision+1, now
	next.Generation.RunID = ""
	next.Generation.Request.InvocationKey = generationInvocationKey(next.ID, next.Generation.Attempt)
	next.Generation.Retries = append(next.Generation.Retries, ChangeSetGenerationRetry{
		IdempotencyKey: request.IdempotencyKey, RequestDigest: retryDigest, Attempt: next.Generation.Attempt,
		ExpectedRevision: request.ExpectedRevision, Reason: request.Reason, Actor: request.Actor, RequestedAt: now,
	})
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: ChangeSetEvaluating, Reason: request.Reason, Actor: request.Actor, At: now})
	persisted, err := s.store.UpdateChangeSet(ctx, next, current.Revision)
	if errors.Is(err, ErrChangeSetRevision) {
		return s.RetryGeneration(ctx, request)
	}
	return persisted, false, err
}

func generationInvocationKey(changeSetID string, attempt int) string {
	return fmt.Sprintf("workforce-change-set:%s:%d", strings.TrimSpace(changeSetID), attempt)
}

func classifyGenerationFailure(err error) (string, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "Workforce generation timed out"
	case errors.Is(err, context.Canceled):
		return "canceled", "Workforce generation was canceled"
	case strings.Contains(err.Error(), "decode workforce candidate"), strings.Contains(err.Error(), "decode repaired workforce candidate"),
		strings.Contains(err.Error(), "generated workforce candidate must"), strings.Contains(err.Error(), "repaired workforce candidate must"):
		return "schema_failed", "The provider returned an invalid workforce candidate"
	default:
		return "provider_failed", "The workforce generation provider failed"
	}
}

func (s *ChangeSetService) ApplyAvailable() bool {
	_, ok := s.store.(AtomicChangeSetStore)
	return ok
}

func (s *ChangeSetService) Apply(ctx context.Context, request ApplyChangeSetRequest) (*ChangeSet, bool, error) {
	request.ChangeSetID = strings.TrimSpace(request.ChangeSetID)
	request.CandidateDigest = strings.TrimSpace(request.CandidateDigest)
	request.Reason = strings.TrimSpace(request.Reason)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Actor.Type, request.Actor.ID = strings.TrimSpace(request.Actor.Type), strings.TrimSpace(request.Actor.ID)
	store, ok := s.store.(AtomicChangeSetStore)
	if !ok {
		return nil, false, errors.New("atomic workforce apply is unavailable")
	}
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || request.ChangeSetID == "" || request.ExpectedRevision < 1 || request.CandidateDigest == "" || request.Reason == "" || request.IdempotencyKey == "" || request.Actor.Type == "" || request.Actor.ID == "" {
		return nil, false, errors.New("apply scope, change set, revision, candidate digest, reason, actor, and idempotency key are required")
	}
	current, err := store.GetChangeSet(ctx, request.Scope, request.ChangeSetID)
	if err != nil {
		return nil, false, err
	}
	if current.ApplyReceipt != nil {
		if current.ApplyReceipt.IdempotencyKey == request.IdempotencyKey && current.ApplyReceipt.CandidateDigest == request.CandidateDigest && current.ApplyReceipt.Reason == request.Reason && current.ApplyReceipt.Actor == request.Actor {
			return current, true, nil
		}
		return nil, false, ErrChangeSetIdempotency
	}
	if current.Revision != request.ExpectedRevision || current.CandidateDigest != request.CandidateDigest {
		return nil, false, ErrChangeSetRevision
	}
	if current.Status != ChangeSetReady {
		return nil, false, fmt.Errorf("%w: cannot apply status %s", ErrChangeSetTransition, current.Status)
	}
	if err := validateApplyPlacement(current); err != nil {
		return nil, false, err
	}
	now := s.now().UTC()
	next := cloneChangeSet(current)
	next.Status, next.Revision, next.UpdatedAt = ChangeSetApplied, current.Revision+1, now
	next.ApplyReceipt = &ChangeSetApplyReceipt{ID: uuid.NewString(), IdempotencyKey: request.IdempotencyKey, CandidateDigest: current.CandidateDigest, Reason: request.Reason, Actor: request.Actor, AppliedAt: now}
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: ChangeSetApplied, Reason: request.Reason, Actor: request.Actor, At: now})
	applied, err := store.ApplyChangeSet(ctx, next, current.Revision)
	if errors.Is(err, ErrChangeSetRevision) {
		return s.Apply(ctx, request)
	}
	return applied, false, err
}

func validateApplyPlacement(value *ChangeSet) error {
	if !value.Result.Valid || len(value.Result.MissingRequirements) > 0 {
		return errors.New("workforce candidate has unresolved requirements")
	}
	if strings.TrimSpace(value.Placement.Environment) == "" {
		return errors.New("deployment environment placement is required")
	}
	if value.Result.Candidate.Team != nil && strings.TrimSpace(value.Placement.TeamDeploymentID) == "" {
		return errors.New("Team deployment placement is required")
	}
	for _, definition := range value.Result.Candidate.Agents {
		if definition == nil || strings.TrimSpace(value.Placement.AgentDeploymentIDs[definition.ID]) == "" {
			return errors.New("every Agent requires a deployment placement")
		}
		if value.Mode == ModeAmend && value.Placement.AgentExpectedRevisions[definition.ID] < 1 {
			return errors.New("every amended Agent placement requires an expected revision")
		}
		for _, kind := range value.RequiredCredentials[definition.ID] {
			reference := value.Placement.CredentialReferences[definition.ID][kind]
			if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("Agent %s requires an opaque %s credential reference", definition.ID, kind)
			}
		}
	}
	if value.Mode == ModeAmend && value.Result.Candidate.Team != nil && value.Placement.TeamExpectedRevision < 1 {
		return errors.New("amended Team placement requires an expected revision")
	}
	for key, objective := range value.Placement.Objectives {
		if strings.TrimSpace(objective.ID) == "" || objective.ExpectedRevision < 0 {
			return fmt.Errorf("objective %s placement is invalid", key)
		}
	}
	return nil
}

func WorkforceObjectiveKey(ownerType, definitionID, templateID string) string {
	return ownerType + ":" + definitionID + ":" + templateID
}

func canonicalIdentity(scope capability.ScopeReference, id string) string {
	prefix := scope.Kind + "/" + scope.ID + "/"
	if strings.HasPrefix(id, prefix) {
		return id
	}
	return prefix + id
}

func canonicalizeCandidateScope(candidate *WorkforceCandidate, scope capability.ScopeReference) {
	ids := map[string]string{}
	for _, definition := range candidate.Agents {
		if definition != nil {
			old := definition.ID
			definition.ID = canonicalIdentity(scope, old)
			ids[old] = definition.ID
		}
	}
	for i := range candidate.Assignments {
		candidate.Assignments[i].AgentDefinitionID = ids[candidate.Assignments[i].AgentDefinitionID]
	}
	if candidate.Team != nil {
		candidate.Team.ID = canonicalIdentity(scope, candidate.Team.ID)
		for i := range candidate.Team.Roles {
			for j, id := range candidate.Team.Roles[i].RequiredDefinitionIDs {
				if qualified := ids[id]; qualified != "" {
					candidate.Team.Roles[i].RequiredDefinitionIDs[j] = qualified
				}
			}
		}
	}
}

func canonicalizePlacement(placement *ChangeSetPlacement, scope capability.ScopeReference, candidate *WorkforceCandidate, existing *WorkforceCandidate) {
	stringsByAgent := map[string]string{}
	for id, value := range placement.AgentDeploymentIDs {
		stringsByAgent[canonicalIdentity(scope, id)] = value
	}
	placement.AgentDeploymentIDs = stringsByAgent
	revisions := map[string]int64{}
	for id, value := range placement.AgentExpectedRevisions {
		revisions[canonicalIdentity(scope, id)] = value
	}
	placement.AgentExpectedRevisions = revisions
	credentials := map[string]map[string]capability.CredentialReference{}
	for id, value := range placement.CredentialReferences {
		credentials[canonicalIdentity(scope, id)] = value
	}
	placement.CredentialReferences = credentials
	if placement.Objectives == nil {
		placement.Objectives = map[string]ObjectivePlacement{}
	}
	existingKeys := map[string]bool{}
	if existing != nil {
		for _, definition := range existing.Agents {
			if definition != nil {
				for _, template := range definition.ObjectiveTemplates {
					existingKeys[WorkforceObjectiveKey("agent", definition.ID, template.ID)] = true
				}
			}
		}
		if existing.Team != nil {
			for _, template := range existing.Team.ObjectiveTemplates {
				existingKeys[WorkforceObjectiveKey("team", existing.Team.ID, template.ID)] = true
			}
		}
	}
	add := func(ownerType, definitionID string, templates []workforce.ObjectiveTemplate) {
		for _, template := range templates {
			key := WorkforceObjectiveKey(ownerType, definitionID, template.ID)
			p := placement.Objectives[key]
			if p.ID == "" {
				p.ID = "objective:" + key
			}
			if existingKeys[key] && p.ExpectedRevision < 1 {
				p.ExpectedRevision = 1
			}
			placement.Objectives[key] = p
		}
	}
	for _, definition := range candidate.Agents {
		if definition != nil {
			add("agent", definition.ID, definition.ObjectiveTemplates)
		}
	}
	if candidate.Team != nil {
		add("team", candidate.Team.ID, candidate.Team.ObjectiveTemplates)
	}
}

func requiredCredentials(candidate WorkforceCandidate, catalog CapabilityCatalog) map[string][]string {
	result := map[string][]string{}
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		seen := map[string]bool{}
		for _, requirement := range definition.SkillRequirements {
			for _, kind := range catalog.Skills[requirement.SkillID].CredentialKinds {
				kind = strings.TrimSpace(kind)
				if kind != "" {
					seen[kind] = true
				}
			}
		}
		for kind := range seen {
			result[definition.ID] = append(result[definition.ID], kind)
		}
		sort.Strings(result[definition.ID])
	}
	return result
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
	approvalKeys := make(map[string]struct{}, len(request.ApprovalRequirements))
	for i := range request.ApprovalRequirements {
		request.ApprovalRequirements[i].PolicyID = strings.TrimSpace(request.ApprovalRequirements[i].PolicyID)
		request.ApprovalRequirements[i].Role = strings.TrimSpace(request.ApprovalRequirements[i].Role)
		if request.ApprovalRequirements[i].PolicyID == "" || request.ApprovalRequirements[i].Role == "" || request.ApprovalRequirements[i].Count < 1 {
			return nil, false, errors.New("approval requirements need a policy, role, and positive count")
		}
		key := request.ApprovalRequirements[i].PolicyID + "\x00" + request.ApprovalRequirements[i].Role
		if _, duplicate := approvalKeys[key]; duplicate {
			return nil, false, errors.New("approval requirements must have unique policy and role pairs")
		}
		approvalKeys[key] = struct{}{}
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

func (s *ChangeSetService) ResolveApproval(ctx context.Context, request ResolveChangeSetApprovalRequest) (*ChangeSet, bool, error) {
	request.ChangeSetID = strings.TrimSpace(request.ChangeSetID)
	request.EvaluationID = strings.TrimSpace(request.EvaluationID)
	request.PolicyID = strings.TrimSpace(request.PolicyID)
	request.Role = strings.TrimSpace(request.Role)
	request.Reason = strings.TrimSpace(request.Reason)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Actor.Type = strings.TrimSpace(request.Actor.Type)
	request.Actor.ID = strings.TrimSpace(request.Actor.ID)
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || request.ChangeSetID == "" ||
		request.ExpectedRevision < 1 || request.EvaluationID == "" || request.PolicyID == "" || request.Role == "" ||
		request.Actor.Type == "" || request.Actor.ID == "" || request.IdempotencyKey == "" {
		return nil, false, errors.New("approval scope, change set, revision, evaluation, policy, role, actor, and idempotency key are required")
	}
	current, err := s.store.GetChangeSet(ctx, request.Scope, request.ChangeSetID)
	if err != nil {
		return nil, false, err
	}
	decisionDigest, err := digestJSON(struct {
		EvaluationID string
		PolicyID     string
		Role         string
		Approved     bool
		Reason       string
		Actor        ChangeSetActor
	}{request.EvaluationID, request.PolicyID, request.Role, request.Approved, request.Reason, request.Actor})
	if err != nil {
		return nil, false, err
	}
	for _, decision := range current.ApprovalDecisions {
		if decision.IdempotencyKey != request.IdempotencyKey {
			continue
		}
		storedDigest, _ := digestJSON(struct {
			EvaluationID string
			PolicyID     string
			Role         string
			Approved     bool
			Reason       string
			Actor        ChangeSetActor
		}{decision.EvaluationID, decision.PolicyID, decision.Role, decision.Approved, decision.Reason, decision.Actor})
		if storedDigest != decisionDigest {
			return nil, false, ErrChangeSetIdempotency
		}
		return current, true, nil
	}
	if current.Revision != request.ExpectedRevision {
		return nil, false, ErrChangeSetRevision
	}
	if current.Status != ChangeSetAwaitingApproval {
		return nil, false, fmt.Errorf("%w: cannot approve status %s", ErrChangeSetTransition, current.Status)
	}
	var evaluation *ChangeSetEvaluation
	for i := range current.Evaluations {
		if current.Evaluations[i].ID == request.EvaluationID {
			evaluation = &current.Evaluations[i]
			break
		}
	}
	if evaluation == nil || !evaluation.Allowed {
		return nil, false, fmt.Errorf("%w: approval evaluation is not active", ErrChangeSetTransition)
	}
	var requirement *ChangeSetApprovalRequirement
	for i := range evaluation.ApprovalRequirements {
		candidate := &evaluation.ApprovalRequirements[i]
		if candidate.PolicyID == request.PolicyID && candidate.Role == request.Role {
			requirement = candidate
			break
		}
	}
	if requirement == nil {
		return nil, false, fmt.Errorf("%w: approval requirement is not present", ErrChangeSetTransition)
	}
	for _, decision := range current.ApprovalDecisions {
		if decision.EvaluationID == request.EvaluationID && decision.PolicyID == request.PolicyID && decision.Role == request.Role &&
			decision.Actor.Type == request.Actor.Type && decision.Actor.ID == request.Actor.ID {
			return nil, false, fmt.Errorf("%w: principal already decided this requirement", ErrChangeSetTransition)
		}
	}
	now := s.now().UTC()
	next := cloneChangeSet(current)
	next.ApprovalDecisions = append(next.ApprovalDecisions, ChangeSetApprovalDecision{
		ID: uuid.NewString(), IdempotencyKey: request.IdempotencyKey, EvaluationID: request.EvaluationID,
		PolicyID: request.PolicyID, Role: request.Role, Approved: request.Approved, Reason: request.Reason,
		Actor: request.Actor, DecidedAt: now,
	})
	nextStatus, lifecycleReason := ChangeSetAwaitingApproval, "approval_recorded"
	if !request.Approved {
		nextStatus, lifecycleReason = ChangeSetRejected, "approval_rejected"
	} else if approvalRequirementsSatisfied(evaluation.ApprovalRequirements, next.ApprovalDecisions, request.EvaluationID) {
		nextStatus, lifecycleReason = ChangeSetReady, "approvals_satisfied"
	}
	next.Status, next.Revision, next.UpdatedAt = nextStatus, current.Revision+1, now
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: nextStatus, Reason: lifecycleReason, Actor: request.Actor, At: now})
	updated, err := s.store.UpdateChangeSet(ctx, next, current.Revision)
	if errors.Is(err, ErrChangeSetRevision) {
		return s.ResolveApproval(ctx, request)
	}
	return updated, false, err
}

func approvalRequirementsSatisfied(requirements []ChangeSetApprovalRequirement, decisions []ChangeSetApprovalDecision, evaluationID string) bool {
	for _, requirement := range requirements {
		approved := 0
		for _, decision := range decisions {
			if decision.EvaluationID == evaluationID && decision.PolicyID == requirement.PolicyID && decision.Role == requirement.Role && decision.Approved {
				approved++
			}
		}
		if approved < requirement.Count {
			return false
		}
	}
	return len(requirements) > 0
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
	copy := ChangeSetPlacement{TeamDeploymentID: value.TeamDeploymentID, TeamExpectedRevision: value.TeamExpectedRevision, Environment: value.Environment}
	if value.AgentDeploymentIDs != nil {
		copy.AgentDeploymentIDs = make(map[string]string, len(value.AgentDeploymentIDs))
		for definitionID, deploymentID := range value.AgentDeploymentIDs {
			copy.AgentDeploymentIDs[definitionID] = deploymentID
		}
	}
	if value.AgentExpectedRevisions != nil {
		copy.AgentExpectedRevisions = make(map[string]int64, len(value.AgentExpectedRevisions))
		for definitionID, revision := range value.AgentExpectedRevisions {
			copy.AgentExpectedRevisions[definitionID] = revision
		}
	}
	if value.CredentialReferences != nil {
		copy.CredentialReferences = make(map[string]map[string]capability.CredentialReference, len(value.CredentialReferences))
		for agentID, references := range value.CredentialReferences {
			nested := make(map[string]capability.CredentialReference, len(references))
			for kind, reference := range references {
				nested[kind] = reference
			}
			copy.CredentialReferences[agentID] = nested
		}
	}
	if value.Objectives != nil {
		copy.Objectives = make(map[string]ObjectivePlacement, len(value.Objectives))
		for key, placement := range value.Objectives {
			copy.Objectives[key] = placement
		}
	}
	return copy
}

func cloneCapabilityCatalog(value CapabilityCatalog) CapabilityCatalog {
	payload, _ := json.Marshal(value)
	var copy CapabilityCatalog
	_ = json.Unmarshal(payload, &copy)
	return copy
}
