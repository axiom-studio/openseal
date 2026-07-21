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

	"github.com/axiom-studio/openseal/pkg/agent"
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
	PolicyID        string `json:"policyId"`
	Role            string `json:"role"`
	Count           int    `json:"count"`
	SeparationGroup string `json:"separationGroup,omitempty"`
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

// ChangeSetPlacementUpdate is an immutable audit record for deterministic
// host-owned placement changes. Placement never passes through the model.
type ChangeSetPlacementUpdate struct {
	ID              string         `json:"id"`
	IdempotencyKey  string         `json:"idempotencyKey"`
	PlacementDigest string         `json:"placementDigest"`
	Reason          string         `json:"reason"`
	Actor           ChangeSetActor `json:"actor"`
	UpdatedAt       time.Time      `json:"updatedAt"`
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
	InitiativeID       string            `json:"initiativeId,omitempty"`
	// Expected revisions select compare-and-swap updates per resource. Zero means
	// create, allowing one amendment to preserve existing resources and add new ones.
	TeamExpectedRevision       int64                                                `json:"teamExpectedRevision,omitempty"`
	AgentExpectedRevisions     map[string]int64                                     `json:"agentExpectedRevisions,omitempty"`
	InitiativeExpectedRevision int64                                                `json:"initiativeExpectedRevision,omitempty"`
	CredentialReferences       map[string]map[string]capability.CredentialReference `json:"credentialReferences,omitempty"`
	// BindingConfigs selects reviewed non-secret Skill configuration by Agent
	// definition and catalog Skill id. Models cannot place values here; hosts
	// materialize and validate this authority before apply.
	BindingConfigs        map[string]map[string]map[string]interface{} `json:"bindingConfigs,omitempty"`
	SkillSourceIdentities map[string]map[string]string                 `json:"skillSourceIdentities,omitempty"`
	// SkillSourceVersions pins the immutable compiled version selected by the
	// host for each source-qualified Skill. The declared catalog version remains
	// model-visible; this version is deterministic placement and audit metadata.
	SkillSourceVersions map[string]map[string]string `json:"skillSourceVersions,omitempty"`
	// SkillRuntimeIdentities is the canonical host-resolved authority mapping
	// from each model-visible catalog Skill id to one immutable installed
	// definition variant. It is placement metadata: models select catalog ids,
	// while apply and runtime authority consume only these exact identities.
	SkillRuntimeIdentities map[string]map[string]capability.SkillIdentity `json:"skillRuntimeIdentities,omitempty"`
	Objectives             map[string]ObjectivePlacement                  `json:"objectives,omitempty"`
	Environment            string                                         `json:"environment,omitempty"`
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
	Activation      WorkforceActivationIntent  `json:"activation"`
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
	Refinement          ChangeSetRefinement         `json:"refinement,omitempty"`
	Evaluations         []ChangeSetEvaluation       `json:"evaluations,omitempty"`
	ApprovalDecisions   []ChangeSetApprovalDecision `json:"approvalDecisions,omitempty"`
	PlacementUpdates    []ChangeSetPlacementUpdate  `json:"placementUpdates,omitempty"`
	ApplyReceipt        *ChangeSetApplyReceipt      `json:"applyReceipt,omitempty"`
	Lifecycle           []ChangeSetLifecycleEvent   `json:"lifecycle"`
	Revision            int64                       `json:"revision"`
	CreatedAt           time.Time                   `json:"createdAt"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
}

// EffectiveChangeSetActivationIntent reads the digest-bound candidate field
// for current ChangeSets and the persisted typed commitment for pre-v8
// ChangeSets. It never derives lifecycle state from prompt text.
func EffectiveChangeSetActivationIntent(value *ChangeSet) (WorkforceActivationIntent, error) {
	if value == nil {
		return "", errors.New("change set is required")
	}
	if value.Result.Candidate.Activation == "" {
		if value.Result.Commitments.Activation == ActivationCommitmentInactive {
			return WorkforceActivationInactive, nil
		}
		return WorkforceActivationActive, nil
	}
	intent, err := EffectiveWorkforceActivationIntent(value.Result.Candidate.Activation)
	if err != nil {
		return "", err
	}
	if value.Result.Commitments.Activation == ActivationCommitmentInactive && intent != WorkforceActivationInactive {
		return "", errors.New("candidate activation conflicts with its inactive commitment")
	}
	return intent, nil
}

// ChangeSetGeneration is the durable, credential-free input and progress for
// probabilistic candidate generation. Hosts may enqueue it into their canonical
// Run scheduler after Prepare returns; the prompt request does not need to stay
// connected while generation is in flight.
type ChangeSetGeneration struct {
	Request GenerateRequest `json:"request"`
	RunID   string          `json:"runId,omitempty"`
	// PreviousCandidateDigest is empty for initial generation and pins the
	// reviewed candidate being refined on subsequent generations.
	PreviousCandidateDigest string                     `json:"previousCandidateDigest,omitempty"`
	Attempt                 int                        `json:"attempt"`
	FailureCode             string                     `json:"failureCode,omitempty"`
	LastError               string                     `json:"lastError,omitempty"`
	CompletedAt             *time.Time                 `json:"completedAt,omitempty"`
	Retries                 []ChangeSetGenerationRetry `json:"retries,omitempty"`
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

type AnswerChangeSetRefinementRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ChangeSetID      string                    `json:"changeSetId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	QuestionID       string                    `json:"questionId"`
	Value            RefinementAnswerValue     `json:"value"`
	Source           RefinementAnswerSource    `json:"source"`
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

type UpdateChangeSetPlacementRequest struct {
	Scope            capability.ScopeReference `json:"scope"`
	ChangeSetID      string                    `json:"changeSetId"`
	ExpectedRevision int64                     `json:"expectedRevision"`
	Placement        ChangeSetPlacement        `json:"placement"`
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

// ChangeSetReadinessValidator is an optional host boundary for deterministic
// checks that require installed, source-qualified resources. The compiler only
// sees a credential-free catalog projection; stores implementing this contract
// verify the exact resources selected by placement before a ChangeSet can be
// advertised as ready. Atomic apply repeats the checks as defense in depth.
type ChangeSetReadinessValidator interface {
	ValidateChangeSetReadiness(context.Context, *ChangeSet) ([]ValidationIssue, error)
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

// PendingChangeSetEvaluationStore is the durable recovery index used by hosts
// that delegate policy evaluation to an external authority. Notifications may
// be lossy; ChangeSets in review remain the source of truth until an evaluation
// advances their lifecycle.
type PendingChangeSetEvaluationStore interface {
	ListPendingChangeSetEvaluations(context.Context, capability.ScopeReference, int) ([]*ChangeSet, error)
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
	if err := ValidateCapabilityCatalog(request.Catalog); err != nil {
		return nil, false, fmt.Errorf("authoring capability catalog: %w", err)
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
		inheritParentPlacement(&request.Placement, parent)
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
	canonicalizePlacement(&request.Placement, request.Scope, &result.Candidate)
	materializationIssues := materializeAnsweredCapabilitySourceScopes(&result.Candidate, compileRequest)
	result.Validation = validateCandidate(&result.Candidate, existing)
	result.Validation = append(result.Validation, materializationIssues...)
	result.Validation = append(result.Validation, validateAnsweredCapabilityNeeds(&result.Candidate, compileRequest)...)
	if len(materializationIssues) == 0 {
		result.Validation = append(result.Validation, validateCapabilitySourceScopeFulfillment(&result.Candidate, compileRequest)...)
	}
	result.MissingRequirements = missingRequirements(&result.Candidate, request.Catalog)
	result.RiskChanges = riskChanges(existing, &result.Candidate)
	result.Diff = workforceDiff(existing, &result.Candidate)
	if err := validateRefinementQuestions(result.UnresolvedQuestions); err != nil {
		result.Validation = append(result.Validation, issue("unresolvedQuestions", "invalid_refinement_question", err.Error()))
	}
	if err := validateRefinementCatalog(result.UnresolvedQuestions, request.Catalog); err != nil {
		result.Validation = append(result.Validation, issue("unresolvedQuestions", "invalid_refinement_catalog", err.Error()))
	}
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.Questions) == 0 && len(result.UnresolvedQuestions) == 0
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
	changeSet.Refinement = reconcileRefinement(ChangeSetRefinement{}, result)
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
	if err := ValidateCapabilityCatalog(request.Catalog); err != nil {
		return nil, false, fmt.Errorf("authoring capability catalog: %w", err)
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
		inheritParentPlacement(&request.Placement, parent)
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
	canonicalizePlacement(&changeSet.Placement, changeSet.Scope, &result.Candidate)
	materializationIssues := materializeAnsweredCapabilitySourceScopes(&result.Candidate, changeSet.Generation.Request)
	result.Validation = validateCandidate(&result.Candidate, existing)
	result.Validation = append(result.Validation, materializationIssues...)
	result.Validation = append(result.Validation, validateAnsweredCapabilityNeeds(&result.Candidate, changeSet.Generation.Request)...)
	if len(materializationIssues) == 0 {
		result.Validation = append(result.Validation, validateCapabilitySourceScopeFulfillment(&result.Candidate, changeSet.Generation.Request)...)
	}
	result.MissingRequirements = missingRequirements(&result.Candidate, changeSet.Catalog)
	result.RiskChanges = riskChanges(existing, &result.Candidate)
	result.Diff = workforceDiff(existing, &result.Candidate)
	if err := validateRefinementQuestions(result.UnresolvedQuestions); err != nil {
		result.Validation = append(result.Validation, issue("unresolvedQuestions", "invalid_refinement_question", err.Error()))
	}
	if err := validateRefinementCatalog(result.UnresolvedQuestions, changeSet.Catalog); err != nil {
		result.Validation = append(result.Validation, issue("unresolvedQuestions", "invalid_refinement_catalog", err.Error()))
	}
	result.Valid = len(result.Validation) == 0 && len(result.MissingRequirements) == 0 && len(result.Questions) == 0 && len(result.UnresolvedQuestions) == 0
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
	changeSet.Refinement = reconcileRefinement(changeSet.Refinement, result)
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

// AnswerRefinement appends an immutable answer event and requeues generation
// against the last reviewed candidate. A first answer must target NextQuestion;
// an earlier answer may be revised explicitly without losing history.
func (s *ChangeSetService) AnswerRefinement(ctx context.Context, request AnswerChangeSetRefinementRequest) (*ChangeSet, bool, error) {
	request.ChangeSetID = strings.TrimSpace(request.ChangeSetID)
	request.QuestionID = strings.TrimSpace(request.QuestionID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Actor.Type, request.Actor.ID = strings.TrimSpace(request.Actor.Type), strings.TrimSpace(request.Actor.ID)
	if request.Source == "" {
		request.Source = RefinementAnswerSourceUser
	}
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || request.ChangeSetID == "" ||
		request.ExpectedRevision < 1 || request.QuestionID == "" || request.Actor.Type == "" || request.Actor.ID == "" || request.IdempotencyKey == "" {
		return nil, false, errors.New("refinement scope, change set, revision, question, actor, and idempotency key are required")
	}
	if request.Source != RefinementAnswerSourceUser && request.Source != RefinementAnswerSourceRuntime {
		return nil, false, errors.New("refinement answer source must be user or runtime")
	}
	current, err := s.store.GetChangeSet(ctx, request.Scope, request.ChangeSetID)
	if err != nil {
		return nil, false, err
	}
	request.Value = normalizeRefinementAnswerValue(request.Value)
	requestDigest, err := digestJSON(struct {
		QuestionID string
		Value      RefinementAnswerValue
		Source     RefinementAnswerSource
		Actor      ChangeSetActor
	}{request.QuestionID, request.Value, request.Source, request.Actor})
	if err != nil {
		return nil, false, err
	}
	for _, answer := range current.Refinement.Answers {
		if answer.IdempotencyKey != request.IdempotencyKey {
			continue
		}
		if answer.RequestDigest != requestDigest {
			return nil, false, ErrChangeSetIdempotency
		}
		return current, true, nil
	}
	if current.Revision != request.ExpectedRevision {
		return nil, false, ErrChangeSetRevision
	}
	if current.Status == ChangeSetEvaluating || current.Status == ChangeSetApplied || current.Status == ChangeSetFailed {
		return nil, false, fmt.Errorf("%w: cannot answer refinement in status %s", ErrChangeSetTransition, current.Status)
	}
	var question *RefinementQuestion
	for i := range current.Refinement.Questions {
		if current.Refinement.Questions[i].ID == request.QuestionID {
			question = &current.Refinement.Questions[i]
			break
		}
	}
	if question == nil {
		return nil, false, errors.New("refinement question not found")
	}
	prior := current.Refinement.CurrentAnswer(request.QuestionID)
	if prior == nil {
		next := current.Refinement.NextQuestion()
		if next == nil || next.ID != request.QuestionID {
			return nil, false, errors.New("refinement question is not currently answerable")
		}
	}
	if request.Source == RefinementAnswerSourceRuntime && !question.AutoResolvable {
		return nil, false, errors.New("refinement question requires a user answer")
	}
	if err := validateRefinementAnswer(*question, request.Value); err != nil {
		return nil, false, err
	}
	if question.Answer.Kind == RefinementAnswerSkillSelection {
		for _, skillID := range request.Value.SkillIDs {
			if _, available := current.Catalog.Skills[skillID]; !available {
				return nil, false, fmt.Errorf("selected Skill %s is not present in the authorized catalog", skillID)
			}
		}
	}
	next := cloneChangeSet(current)
	now := s.now().UTC()
	next.Refinement.Answers = append(next.Refinement.Answers, RefinementAnswerEvent{
		ID: uuid.NewString(), IdempotencyKey: request.IdempotencyKey, RequestDigest: requestDigest,
		QuestionID: request.QuestionID, QuestionRevision: current.Revision, Value: request.Value,
		Source: request.Source, Actor: request.Actor, AnsweredAt: now,
	})
	next.Status, next.Revision, next.UpdatedAt = ChangeSetEvaluating, current.Revision+1, now
	next.Generation = &ChangeSetGeneration{
		Request: GenerateRequest{Mode: ModeAmend, Prompt: current.Prompt, Existing: &current.Result.Candidate,
			Catalog: cloneCapabilityCatalog(current.Catalog), Refinement: providerRefinementContext(next.Refinement)},
		Attempt: generationAttempt(current), PreviousCandidateDigest: current.CandidateDigest,
	}
	next.Generation.Request.InvocationKey = generationInvocationKey(next.ID, next.Generation.Attempt)
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: ChangeSetEvaluating, Reason: "refinement_answered", Actor: request.Actor, At: now})
	updated, err := s.store.UpdateChangeSet(ctx, next, current.Revision)
	if errors.Is(err, ErrChangeSetRevision) {
		return s.AnswerRefinement(ctx, request)
	}
	return updated, false, err
}

func generationAttempt(value *ChangeSet) int {
	if value.Generation == nil {
		return 1
	}
	return value.Generation.Attempt
}

func (s *ChangeSetService) Get(ctx context.Context, scope capability.ScopeReference, id string) (*ChangeSet, error) {
	return s.store.GetChangeSet(ctx, scope, strings.TrimSpace(id))
}

func (s *ChangeSetService) ListPendingEvaluations(ctx context.Context, scope capability.ScopeReference, limit int) ([]*ChangeSet, error) {
	if strings.TrimSpace(scope.Kind) == "" || strings.TrimSpace(scope.ID) == "" {
		return nil, errors.New("workforce evaluation scope is required")
	}
	store, ok := s.store.(PendingChangeSetEvaluationStore)
	if !ok {
		return nil, errors.New("workforce evaluation recovery is unavailable")
	}
	return store.ListPendingChangeSetEvaluations(ctx, scope, limit)
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
	var schemaError *SchemaGenerationError
	var contractError *ContractGenerationError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "Workforce generation timed out"
	case errors.Is(err, context.Canceled):
		return "canceled", "Workforce generation was canceled"
	case errors.As(err, &schemaError):
		return "schema_failed", fmt.Sprintf("The provider response remained invalid after %d bounded schema repairs: %s", schemaError.RepairAttempts, schemaError.Diagnostic)
	case errors.As(err, &contractError):
		return "contract_failed", fmt.Sprintf("The provider response remained semantically invalid after %d bounded contract repairs: %s", contractError.RepairAttempts, contractError.Diagnostic)
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

// UpdatePlacement deterministically updates host-owned resource, credential,
// and Skill-source placement without invoking the probabilistic generator. Any
// prior policy decision is made inactive by returning the aggregate to review.
func (s *ChangeSetService) UpdatePlacement(ctx context.Context, request UpdateChangeSetPlacementRequest) (*ChangeSet, bool, error) {
	request.ChangeSetID = strings.TrimSpace(request.ChangeSetID)
	request.Reason = strings.TrimSpace(request.Reason)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.Actor.Type, request.Actor.ID = strings.TrimSpace(request.Actor.Type), strings.TrimSpace(request.Actor.ID)
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" || request.ChangeSetID == "" ||
		request.ExpectedRevision < 1 || request.Reason == "" || request.Actor.Type == "" || request.Actor.ID == "" || request.IdempotencyKey == "" {
		return nil, false, errors.New("placement scope, change set, revision, reason, actor, and idempotency key are required")
	}
	current, err := s.store.GetChangeSet(ctx, request.Scope, request.ChangeSetID)
	if err != nil {
		return nil, false, err
	}
	canonicalizePlacement(&request.Placement, current.Scope, &current.Result.Candidate)
	if err := validatePlacementReferences(request.Placement, &current.Result.Candidate); err != nil {
		return nil, false, err
	}
	placementDigest, err := digestJSON(request.Placement)
	if err != nil {
		return nil, false, fmt.Errorf("digest workforce placement: %w", err)
	}
	requestDigest, err := digestJSON(struct {
		PlacementDigest string
		Reason          string
		Actor           ChangeSetActor
	}{placementDigest, request.Reason, request.Actor})
	if err != nil {
		return nil, false, err
	}
	for _, update := range current.PlacementUpdates {
		if update.IdempotencyKey != request.IdempotencyKey {
			continue
		}
		storedDigest, _ := digestJSON(struct {
			PlacementDigest string
			Reason          string
			Actor           ChangeSetActor
		}{update.PlacementDigest, update.Reason, update.Actor})
		if storedDigest != requestDigest {
			return nil, false, ErrChangeSetIdempotency
		}
		return current, true, nil
	}
	if current.Revision != request.ExpectedRevision {
		return nil, false, ErrChangeSetRevision
	}
	if current.CandidateDigest == "" || current.Status == ChangeSetEvaluating || current.Status == ChangeSetApplied || current.Status == ChangeSetFailed {
		return nil, false, fmt.Errorf("%w: cannot update placement in status %s", ErrChangeSetTransition, current.Status)
	}
	now := s.now().UTC()
	next := cloneChangeSet(current)
	next.Placement = clonePlacement(request.Placement)
	// Readiness findings are derived from the exact placement. Clear the stale
	// projection when placement changes; the next governed ready transition
	// recomputes it against the newly selected immutable resources.
	next.Result.Validation = replaceReadinessValidation(next.Result.Validation, nil)
	next.Result.Valid = len(next.Result.Validation) == 0 && len(next.Result.MissingRequirements) == 0 && len(next.Result.Questions) == 0 && len(next.Result.UnresolvedQuestions) == 0
	next.Status = ChangeSetReview
	if !next.Result.Valid {
		next.Status = ChangeSetBlocked
	}
	next.Revision, next.UpdatedAt = current.Revision+1, now
	next.PlacementUpdates = append(next.PlacementUpdates, ChangeSetPlacementUpdate{
		ID: uuid.NewString(), IdempotencyKey: request.IdempotencyKey, PlacementDigest: placementDigest,
		Reason: request.Reason, Actor: request.Actor, UpdatedAt: now,
	})
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{
		Revision: next.Revision, From: current.Status, To: next.Status, Reason: "placement_updated", Actor: request.Actor, At: now,
	})
	updated, err := s.store.UpdateChangeSet(ctx, next, current.Revision)
	if errors.Is(err, ErrChangeSetRevision) {
		return s.UpdatePlacement(ctx, request)
	}
	return updated, false, err
}

func validatePlacementReferences(placement ChangeSetPlacement, candidate *WorkforceCandidate) error {
	agents := make(map[string]*agent.AgentDefinition, len(candidate.Agents))
	for _, definition := range candidate.Agents {
		if definition != nil {
			agents[definition.ID] = definition
		}
	}
	for agentID, sources := range placement.SkillSourceIdentities {
		definition := agents[agentID]
		if definition == nil {
			return fmt.Errorf("Skill source placement references unknown Agent %s", agentID)
		}
		required := make(map[string]struct{}, len(definition.SkillRequirements))
		for _, requirement := range definition.SkillRequirements {
			required[strings.TrimSpace(requirement.SkillID)] = struct{}{}
		}
		for skillID, identity := range sources {
			if _, ok := required[strings.TrimSpace(skillID)]; !ok {
				return fmt.Errorf("Skill source placement references undeclared Skill %s for Agent %s", skillID, agentID)
			}
			if strings.TrimSpace(identity) == "" {
				return fmt.Errorf("Skill source placement for Agent %s Skill %s is empty", agentID, skillID)
			}
			if strings.TrimSpace(placement.SkillSourceVersions[agentID][strings.TrimSpace(skillID)]) == "" {
				return fmt.Errorf("Skill source placement for Agent %s Skill %s requires an immutable version", agentID, skillID)
			}
		}
	}
	for agentID, versions := range placement.SkillSourceVersions {
		for skillID, version := range versions {
			if strings.TrimSpace(version) == "" || strings.TrimSpace(placement.SkillSourceIdentities[agentID][strings.TrimSpace(skillID)]) == "" {
				return fmt.Errorf("Skill source version for Agent %s Skill %s has no selected source", agentID, skillID)
			}
		}
	}
	for agentID, identities := range placement.SkillRuntimeIdentities {
		definition := agents[agentID]
		if definition == nil {
			return fmt.Errorf("Skill runtime identity placement references unknown Agent %s", agentID)
		}
		required := make(map[string]struct{}, len(definition.SkillRequirements))
		for _, requirement := range definition.SkillRequirements {
			required[strings.TrimSpace(requirement.SkillID)] = struct{}{}
		}
		for catalogID, identity := range identities {
			catalogID = strings.TrimSpace(catalogID)
			if _, ok := required[catalogID]; !ok {
				return fmt.Errorf("Skill runtime identity placement references undeclared Skill %s for Agent %s", catalogID, agentID)
			}
			if identity != identity.Normalized() || !identity.Valid() {
				return fmt.Errorf("Skill runtime identity for Agent %s Skill %s is invalid", agentID, catalogID)
			}
			if source := strings.TrimSpace(placement.SkillSourceIdentities[agentID][catalogID]); source != "" && source != identity.SourceIdentity {
				return fmt.Errorf("Skill runtime identity for Agent %s Skill %s conflicts with selected source", agentID, catalogID)
			}
			if version := strings.TrimSpace(placement.SkillSourceVersions[agentID][catalogID]); version != "" && version != identity.Version {
				return fmt.Errorf("Skill runtime identity for Agent %s Skill %s conflicts with immutable version", agentID, catalogID)
			}
		}
	}
	return nil
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
	activation, err := EffectiveChangeSetActivationIntent(current)
	if err != nil {
		return nil, false, err
	}
	now := s.now().UTC()
	next := cloneChangeSet(current)
	next.Status, next.Revision, next.UpdatedAt = ChangeSetApplied, current.Revision+1, now
	next.ApplyReceipt = &ChangeSetApplyReceipt{ID: uuid.NewString(), IdempotencyKey: request.IdempotencyKey, CandidateDigest: current.CandidateDigest, Activation: activation, Reason: request.Reason, Actor: request.Actor, AppliedAt: now}
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: ChangeSetApplied, Reason: request.Reason, Actor: request.Actor, At: now})
	applied, err := store.ApplyChangeSet(ctx, next, current.Revision)
	return applied, false, err
}

func validateApplyPlacement(value *ChangeSet) error {
	if !value.Result.Valid || len(value.Result.MissingRequirements) > 0 {
		return errors.New("workforce candidate has unresolved requirements")
	}
	if err := validatePlacementReferences(value.Placement, &value.Result.Candidate); err != nil {
		return err
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
		if value.Placement.AgentExpectedRevisions[definition.ID] < 0 {
			return errors.New("Agent placement expected revisions cannot be negative")
		}
		for _, kind := range value.RequiredCredentials[definition.ID] {
			reference := value.Placement.CredentialReferences[definition.ID][kind]
			if strings.TrimSpace(reference.Kind) == "" || strings.TrimSpace(reference.ID) == "" {
				return fmt.Errorf("Agent %s requires an opaque %s credential reference", definition.ID, kind)
			}
		}
	}
	if value.Placement.TeamExpectedRevision < 0 {
		return errors.New("Team placement expected revision cannot be negative")
	}
	if value.Result.Candidate.Initiative != nil {
		if strings.TrimSpace(value.Placement.InitiativeID) == "" {
			return errors.New("Initiative placement is required")
		}
		if value.Placement.InitiativeExpectedRevision < 0 {
			return errors.New("Initiative placement expected revision cannot be negative")
		}
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
	objectiveIDs := map[string]string{}
	for _, definition := range candidate.Agents {
		if definition != nil {
			old := definition.ID
			qualified := canonicalIdentity(scope, old)
			ids[old] = qualified
			for _, template := range definition.ObjectiveTemplates {
				objectiveIDs[WorkforceObjectiveKey(InitiativeOwnerAgent, old, template.ID)] = WorkforceObjectiveKey(InitiativeOwnerAgent, qualified, template.ID)
			}
		}
	}
	teamID := ""
	if candidate.Team != nil {
		teamID = candidate.Team.ID
		qualified := canonicalIdentity(scope, teamID)
		for _, template := range candidate.Team.ObjectiveTemplates {
			objectiveIDs[WorkforceObjectiveKey(InitiativeOwnerTeam, teamID, template.ID)] = WorkforceObjectiveKey(InitiativeOwnerTeam, qualified, template.ID)
		}
	}
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		definition.ID = ids[definition.ID]
		canonicalizeObjectiveTemplateAgents(definition.ObjectiveTemplates, ids)
	}
	for i := range candidate.Assignments {
		if qualified := ids[candidate.Assignments[i].AgentDefinitionID]; qualified != "" {
			candidate.Assignments[i].AgentDefinitionID = qualified
		}
	}
	if candidate.Team != nil {
		candidate.Team.ID = canonicalIdentity(scope, teamID)
		for i := range candidate.Team.Roles {
			for j, id := range candidate.Team.Roles[i].RequiredDefinitionIDs {
				if qualified := ids[id]; qualified != "" {
					candidate.Team.Roles[i].RequiredDefinitionIDs[j] = qualified
				}
			}
		}
		canonicalizeObjectiveTemplateAgents(candidate.Team.ObjectiveTemplates, ids)
	}
	if candidate.Initiative != nil {
		blueprint := candidate.Initiative
		switch blueprint.Owner.Type {
		case InitiativeOwnerAgent:
			if qualified := ids[blueprint.Owner.DefinitionID]; qualified != "" {
				blueprint.Owner.DefinitionID = qualified
			}
		case InitiativeOwnerTeam:
			if candidate.Team != nil && blueprint.Owner.DefinitionID == teamID {
				blueprint.Owner.DefinitionID = candidate.Team.ID
			}
		}
		canonicalizeBlueprintObjectiveRefs(blueprint.ObjectiveRefs, objectiveIDs)
		for index := range blueprint.Milestones {
			canonicalizeBlueprintObjectiveRefs(blueprint.Milestones[index].ObjectiveRefs, objectiveIDs)
		}
		for index := range blueprint.Deliverables {
			canonicalizeBlueprintObjectiveRefs(blueprint.Deliverables[index].ObjectiveRefs, objectiveIDs)
		}
		for index := range blueprint.SourceMonitors {
			monitor := &blueprint.SourceMonitors[index]
			if qualified := objectiveIDs[monitor.ObjectiveRef]; qualified != "" {
				monitor.ObjectiveRef = qualified
			}
			if qualified := ids[monitor.AssignedAgentDefinitionID]; qualified != "" {
				monitor.AssignedAgentDefinitionID = qualified
			}
		}
	}
}

func canonicalizeObjectiveTemplateAgents(templates []workforce.ObjectiveTemplate, ids map[string]string) {
	for index := range templates {
		assigned, _ := templates[index].Cadence["assignedAgentId"].(string)
		if qualified := ids[assigned]; qualified != "" {
			templates[index].Cadence["assignedAgentId"] = qualified
		}
	}
}

func canonicalizeBlueprintObjectiveRefs(refs []string, ids map[string]string) {
	for index, reference := range refs {
		if qualified := ids[reference]; qualified != "" {
			refs[index] = qualified
		}
	}
}

func canonicalizePlacement(placement *ChangeSetPlacement, scope capability.ScopeReference, candidate *WorkforceCandidate) {
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
	bindingConfigs := map[string]map[string]map[string]interface{}{}
	for id, values := range placement.BindingConfigs {
		qualified := canonicalIdentity(scope, id)
		bindingConfigs[qualified] = make(map[string]map[string]interface{}, len(values))
		for skillID, config := range values {
			bindingConfigs[qualified][strings.TrimSpace(skillID)] = cloneAuthoringMap(config)
		}
	}
	placement.BindingConfigs = bindingConfigs
	skillSources := map[string]map[string]string{}
	for id, values := range placement.SkillSourceIdentities {
		qualified := canonicalIdentity(scope, id)
		skillSources[qualified] = make(map[string]string, len(values))
		for skillID, identity := range values {
			skillSources[qualified][strings.TrimSpace(skillID)] = strings.TrimSpace(identity)
		}
	}
	placement.SkillSourceIdentities = skillSources
	skillVersions := map[string]map[string]string{}
	for id, values := range placement.SkillSourceVersions {
		qualified := canonicalIdentity(scope, id)
		skillVersions[qualified] = make(map[string]string, len(values))
		for skillID, version := range values {
			skillVersions[qualified][strings.TrimSpace(skillID)] = strings.TrimSpace(version)
		}
	}
	placement.SkillSourceVersions = skillVersions
	skillIdentities := map[string]map[string]capability.SkillIdentity{}
	for id, values := range placement.SkillRuntimeIdentities {
		qualified := canonicalIdentity(scope, id)
		skillIdentities[qualified] = make(map[string]capability.SkillIdentity, len(values))
		for skillID, identity := range values {
			skillIdentities[qualified][strings.TrimSpace(skillID)] = identity.Normalized()
		}
	}
	placement.SkillRuntimeIdentities = skillIdentities
	if strings.TrimSpace(placement.Environment) == "" {
		placement.Environment = "default"
	}
	for _, definition := range candidate.Agents {
		if definition != nil && strings.TrimSpace(placement.AgentDeploymentIDs[definition.ID]) == "" {
			placement.AgentDeploymentIDs[definition.ID] = "agent:" + digestString(scope.Kind + "\x00" + scope.ID + "\x00" + definition.ID)[:32]
		}
	}
	if candidate.Team != nil && strings.TrimSpace(placement.TeamDeploymentID) == "" {
		placement.TeamDeploymentID = "team:" + digestString(scope.Kind + "\x00" + scope.ID + "\x00" + candidate.Team.ID)[:32]
	}
	if candidate.Initiative != nil {
		if strings.TrimSpace(placement.InitiativeID) == "" {
			placement.InitiativeID = "initiative:" + digestString(scope.Kind + "\x00" + scope.ID + "\x00" + candidate.Initiative.ID)[:32]
		}
	}
	if placement.Objectives == nil {
		placement.Objectives = map[string]ObjectivePlacement{}
	}
	add := func(ownerType, definitionID string, templates []workforce.ObjectiveTemplate) {
		for _, template := range templates {
			key := WorkforceObjectiveKey(ownerType, definitionID, template.ID)
			p := placement.Objectives[key]
			if p.ID == "" {
				p.ID = "objective:" + digestString(scope.Kind + "\x00" + scope.ID + "\x00" + key)[:32]
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

// inheritParentPlacement preserves stable resource identity across refinements.
// Candidate presence is not evidence that a resource exists: only an immutable
// apply receipt may introduce a new expected revision. Positive revisions already
// carried by the parent remain authoritative across an unapplied refinement chain.
func inheritParentPlacement(placement *ChangeSetPlacement, parent *ChangeSet) {
	if parent == nil {
		return
	}
	parentPlacement := clonePlacement(parent.Placement)
	inheritAppliedRevisions(&parentPlacement, parent)

	if placement.AgentDeploymentIDs == nil {
		placement.AgentDeploymentIDs = map[string]string{}
	}
	if placement.AgentExpectedRevisions == nil {
		placement.AgentExpectedRevisions = map[string]int64{}
	}
	for definitionID, deploymentID := range parentPlacement.AgentDeploymentIDs {
		currentID := strings.TrimSpace(placement.AgentDeploymentIDs[definitionID])
		if currentID == "" {
			placement.AgentDeploymentIDs[definitionID] = deploymentID
			currentID = deploymentID
		}
		if currentID == deploymentID && parentPlacement.AgentExpectedRevisions[definitionID] > 0 {
			placement.AgentExpectedRevisions[definitionID] = parentPlacement.AgentExpectedRevisions[definitionID]
		}
	}
	currentTeamID := strings.TrimSpace(placement.TeamDeploymentID)
	if currentTeamID == "" {
		placement.TeamDeploymentID = parentPlacement.TeamDeploymentID
		currentTeamID = parentPlacement.TeamDeploymentID
	}
	if currentTeamID == parentPlacement.TeamDeploymentID && parentPlacement.TeamExpectedRevision > 0 {
		placement.TeamExpectedRevision = parentPlacement.TeamExpectedRevision
	}
	currentInitiativeID := strings.TrimSpace(placement.InitiativeID)
	if currentInitiativeID == "" {
		placement.InitiativeID = parentPlacement.InitiativeID
		currentInitiativeID = parentPlacement.InitiativeID
	}
	if currentInitiativeID == parentPlacement.InitiativeID && parentPlacement.InitiativeExpectedRevision > 0 {
		placement.InitiativeExpectedRevision = parentPlacement.InitiativeExpectedRevision
	}
	if strings.TrimSpace(placement.Environment) == "" {
		placement.Environment = parentPlacement.Environment
	}
	if placement.Objectives == nil {
		placement.Objectives = map[string]ObjectivePlacement{}
	}
	for key, inherited := range parentPlacement.Objectives {
		current := placement.Objectives[key]
		if strings.TrimSpace(current.ID) == "" {
			current.ID = inherited.ID
		}
		if current.ID == inherited.ID && inherited.ExpectedRevision > 0 {
			current.ExpectedRevision = inherited.ExpectedRevision
		}
		placement.Objectives[key] = current
	}
	if placement.CredentialReferences == nil {
		placement.CredentialReferences = map[string]map[string]capability.CredentialReference{}
	}
	for definitionID, inherited := range parentPlacement.CredentialReferences {
		if placement.CredentialReferences[definitionID] == nil {
			placement.CredentialReferences[definitionID] = map[string]capability.CredentialReference{}
		}
		for kind, reference := range inherited {
			if _, exists := placement.CredentialReferences[definitionID][kind]; !exists {
				placement.CredentialReferences[definitionID][kind] = reference
			}
		}
	}
	if placement.BindingConfigs == nil {
		placement.BindingConfigs = map[string]map[string]map[string]interface{}{}
	}
	for definitionID, inherited := range parentPlacement.BindingConfigs {
		if placement.BindingConfigs[definitionID] == nil {
			placement.BindingConfigs[definitionID] = map[string]map[string]interface{}{}
		}
		for skillID, config := range inherited {
			if _, exists := placement.BindingConfigs[definitionID][skillID]; !exists {
				placement.BindingConfigs[definitionID][skillID] = cloneAuthoringMap(config)
			}
		}
	}
	if placement.SkillSourceIdentities == nil {
		placement.SkillSourceIdentities = map[string]map[string]string{}
	}
	for definitionID, inherited := range parentPlacement.SkillSourceIdentities {
		if placement.SkillSourceIdentities[definitionID] == nil {
			placement.SkillSourceIdentities[definitionID] = map[string]string{}
		}
		for skillID, identity := range inherited {
			if _, exists := placement.SkillSourceIdentities[definitionID][skillID]; !exists {
				placement.SkillSourceIdentities[definitionID][skillID] = identity
			}
		}
	}
	if placement.SkillSourceVersions == nil {
		placement.SkillSourceVersions = map[string]map[string]string{}
	}
	if placement.SkillRuntimeIdentities == nil {
		placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{}
	}
	for definitionID, inherited := range parentPlacement.SkillRuntimeIdentities {
		if placement.SkillRuntimeIdentities[definitionID] == nil {
			placement.SkillRuntimeIdentities[definitionID] = map[string]capability.SkillIdentity{}
		}
		for skillID, identity := range inherited {
			if _, exists := placement.SkillRuntimeIdentities[definitionID][skillID]; !exists {
				placement.SkillRuntimeIdentities[definitionID][skillID] = identity
			}
		}
	}
	for definitionID, inherited := range parentPlacement.SkillSourceVersions {
		if placement.SkillSourceVersions[definitionID] == nil {
			placement.SkillSourceVersions[definitionID] = map[string]string{}
		}
		for skillID, version := range inherited {
			if _, exists := placement.SkillSourceVersions[definitionID][skillID]; !exists {
				placement.SkillSourceVersions[definitionID][skillID] = version
			}
		}
	}
}

func inheritAppliedRevisions(placement *ChangeSetPlacement, parent *ChangeSet) {
	if parent.ApplyReceipt == nil {
		return
	}
	resources := map[string]int64{}
	for _, resource := range parent.ApplyReceipt.Resources {
		if resource.Revision > 0 {
			resources[resource.Kind+"\x00"+resource.ID] = resource.Revision
		}
	}
	if placement.AgentExpectedRevisions == nil {
		placement.AgentExpectedRevisions = map[string]int64{}
	}
	for definitionID, deploymentID := range placement.AgentDeploymentIDs {
		if revision := resources["agent_deployment\x00"+deploymentID]; revision > 0 {
			placement.AgentExpectedRevisions[definitionID] = revision
		}
	}
	if revision := resources["team_deployment\x00"+placement.TeamDeploymentID]; revision > 0 {
		placement.TeamExpectedRevision = revision
	}
	if revision := resources["initiative\x00"+placement.InitiativeID]; revision > 0 {
		placement.InitiativeExpectedRevision = revision
	}
	for key, objective := range placement.Objectives {
		if revision := resources["objective\x00"+objective.ID]; revision > 0 {
			objective.ExpectedRevision = revision
			placement.Objectives[key] = objective
		}
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
		request.ApprovalRequirements[i].SeparationGroup = strings.TrimSpace(request.ApprovalRequirements[i].SeparationGroup)
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
	// Approval may proceed while deterministic placement is still being
	// configured. Validate only at the transition that would advertise ready;
	// ResolveApproval repeats this immediately before its final transition.
	readinessIssues, err := s.validateReadiness(ctx, current, request.Allowed && len(request.ApprovalRequirements) == 0)
	if err != nil {
		return nil, false, err
	}
	if request.Allowed && len(readinessIssues) > 0 {
		nextStatus, reason = ChangeSetBlocked, "binding_validation_failed"
	} else if request.Allowed && len(request.ApprovalRequirements) > 0 {
		nextStatus, reason = ChangeSetAwaitingApproval, "policy_requires_approval"
	} else if request.Allowed {
		nextStatus, reason = ChangeSetReady, "policy_allowed"
	}
	next := cloneChangeSet(current)
	next.Result.Validation = replaceReadinessValidation(next.Result.Validation, readinessIssues)
	next.Result.Valid = len(next.Result.Validation) == 0 && len(next.Result.MissingRequirements) == 0 && len(next.Result.Questions) == 0 && len(next.Result.UnresolvedQuestions) == 0
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

const readinessValidationCodePrefix = "skill_binding_"

func (s *ChangeSetService) validateReadiness(ctx context.Context, value *ChangeSet, enabled bool) ([]ValidationIssue, error) {
	if !enabled {
		return nil, nil
	}
	validator, ok := s.store.(ChangeSetReadinessValidator)
	if !ok {
		return nil, nil
	}
	issues, err := validator.ValidateChangeSetReadiness(ctx, value)
	if err != nil {
		return nil, fmt.Errorf("validate workforce readiness: %w", err)
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path != issues[j].Path {
			return issues[i].Path < issues[j].Path
		}
		if issues[i].Code != issues[j].Code {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Message < issues[j].Message
	})
	return issues, nil
}

func replaceReadinessValidation(existing, readiness []ValidationIssue) []ValidationIssue {
	result := make([]ValidationIssue, 0, len(existing)+len(readiness))
	for _, issue := range existing {
		if !strings.HasPrefix(issue.Code, readinessValidationCodePrefix) {
			result = append(result, issue)
		}
	}
	return append(result, readiness...)
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
	if count := len(current.Evaluations); count > 0 && current.Evaluations[count-1].ID == request.EvaluationID {
		evaluation = &current.Evaluations[count-1]
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
	if request.Approved && requirement.SeparationGroup != "" {
		for _, decision := range current.ApprovalDecisions {
			if !decision.Approved || decision.EvaluationID != request.EvaluationID ||
				decision.Actor.Type != request.Actor.Type || decision.Actor.ID != request.Actor.ID {
				continue
			}
			for _, decidedRequirement := range evaluation.ApprovalRequirements {
				if decidedRequirement.PolicyID == decision.PolicyID && decidedRequirement.Role == decision.Role &&
					decidedRequirement.SeparationGroup == requirement.SeparationGroup {
					return nil, false, fmt.Errorf("%w: principal already approved an incompatible requirement", ErrChangeSetTransition)
				}
			}
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
		readinessIssues, err := s.validateReadiness(ctx, next, true)
		if err != nil {
			return nil, false, err
		}
		next.Result.Validation = replaceReadinessValidation(next.Result.Validation, readinessIssues)
		next.Result.Valid = len(next.Result.Validation) == 0 && len(next.Result.MissingRequirements) == 0 && len(next.Result.Questions) == 0 && len(next.Result.UnresolvedQuestions) == 0
		if len(readinessIssues) > 0 {
			nextStatus, lifecycleReason = ChangeSetBlocked, "binding_validation_failed"
		} else {
			nextStatus, lifecycleReason = ChangeSetReady, "approvals_satisfied"
		}
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
	copy := ChangeSetPlacement{
		TeamDeploymentID: value.TeamDeploymentID, InitiativeID: value.InitiativeID,
		TeamExpectedRevision: value.TeamExpectedRevision, InitiativeExpectedRevision: value.InitiativeExpectedRevision,
		Environment: value.Environment,
	}
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
	if value.BindingConfigs != nil {
		copy.BindingConfigs = make(map[string]map[string]map[string]interface{}, len(value.BindingConfigs))
		for agentID, configs := range value.BindingConfigs {
			nested := make(map[string]map[string]interface{}, len(configs))
			for skillID, config := range configs {
				nested[skillID] = cloneAuthoringMap(config)
			}
			copy.BindingConfigs[agentID] = nested
		}
	}
	if value.SkillSourceIdentities != nil {
		copy.SkillSourceIdentities = make(map[string]map[string]string, len(value.SkillSourceIdentities))
		for agentID, identities := range value.SkillSourceIdentities {
			nested := make(map[string]string, len(identities))
			for skillID, identity := range identities {
				nested[skillID] = identity
			}
			copy.SkillSourceIdentities[agentID] = nested
		}
	}
	if value.SkillSourceVersions != nil {
		copy.SkillSourceVersions = make(map[string]map[string]string, len(value.SkillSourceVersions))
		for agentID, versions := range value.SkillSourceVersions {
			nested := make(map[string]string, len(versions))
			for skillID, version := range versions {
				nested[skillID] = version
			}
			copy.SkillSourceVersions[agentID] = nested
		}
	}
	if value.SkillRuntimeIdentities != nil {
		copy.SkillRuntimeIdentities = make(map[string]map[string]capability.SkillIdentity, len(value.SkillRuntimeIdentities))
		for agentID, identities := range value.SkillRuntimeIdentities {
			nested := make(map[string]capability.SkillIdentity, len(identities))
			for skillID, identity := range identities {
				nested[skillID] = identity
			}
			copy.SkillRuntimeIdentities[agentID] = nested
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

func cloneAuthoringMap(value map[string]interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result map[string]interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func cloneCapabilityCatalog(value CapabilityCatalog) CapabilityCatalog {
	payload, _ := json.Marshal(value)
	var copy CapabilityCatalog
	_ = json.Unmarshal(payload, &copy)
	return copy
}
