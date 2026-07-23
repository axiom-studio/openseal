package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

var (
	ErrDefinitionNotFound   = errors.New("agent definition not found")
	ErrDeploymentNotFound   = errors.New("agent deployment not found")
	ErrAmendmentNotFound    = errors.New("agent definition amendment not found")
	ErrCompilationNotFound  = errors.New("agent definition compilation not found")
	ErrCompilationImmutable = errors.New("agent definition compilation is immutable")
	ErrRevisionConflict     = errors.New("agent deployment revision conflict")
	ErrIdempotencyConflict  = errors.New("agent amendment idempotency key was already used for a different proposal")
)

type Registry struct {
	store Store
	now   func() time.Time
	newID func() string
}

func (r *Registry) RecordCompilation(ctx context.Context, compilation *DefinitionCompilation) (*DefinitionCompilation, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("agent registry is not configured")
	}
	candidate := cloneCompilation(compilation)
	if candidate != nil && candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = r.now().UTC()
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	if err := r.store.CreateCompilation(ctx, candidate); err != nil {
		if !errors.Is(err, ErrCompilationImmutable) {
			return nil, err
		}
		existing, getErr := r.store.GetCompilation(ctx, candidate.Scope, candidate.ID)
		if getErr != nil {
			return nil, err
		}
		// CreatedAt is server-authored on first persistence and is not part of
		// the immutable compiler result used to recognize a safe retry.
		candidate.CreatedAt = existing.CreatedAt
		existingJSON, _ := json.Marshal(existing)
		candidateJSON, _ := json.Marshal(candidate)
		if string(existingJSON) != string(candidateJSON) {
			return nil, ErrCompilationImmutable
		}
		return existing, nil
	}
	return cloneCompilation(candidate), nil
}

func (r *Registry) GetCompilation(ctx context.Context, scope capability.ScopeReference, id string) (*DefinitionCompilation, error) {
	return r.store.GetCompilation(ctx, scope, id)
}

func (r *Registry) ListCompilations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]*DefinitionCompilation, error) {
	return r.store.ListCompilations(ctx, scope, deploymentID)
}

func NewRegistry() *Registry {
	return NewRegistryWithStore(NewMemoryStore())
}

func NewRegistryWithStore(store Store) *Registry {
	return &Registry{store: store, now: time.Now, newID: uuid.NewString}
}

func (r *Registry) RegisterDefinition(ctx context.Context, definition *AgentDefinition) (*AgentDefinition, error) {
	if r == nil {
		return nil, errors.New("agent registry is not configured")
	}
	candidate, err := prepareDefinition(definition, r.now().UTC())
	if err != nil {
		return nil, err
	}
	if err := r.store.CreateDefinition(ctx, candidate); err != nil {
		return nil, err
	}
	return cloneDefinition(candidate), nil
}

func (r *Registry) GetDefinition(ctx context.Context, id, version string) (*AgentDefinition, error) {
	return r.store.GetDefinition(ctx, id, version)
}

func (r *Registry) ListDefinitionVersions(ctx context.Context, id string) ([]*AgentDefinition, error) {
	return r.store.ListDefinitionVersions(ctx, id)
}

func (r *Registry) CreateDeployment(ctx context.Context, deployment *AgentDeployment, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	if r == nil {
		return nil, nil, errors.New("agent registry is not configured")
	}
	candidate := cloneDeployment(deployment)
	candidate.DisplayName = strings.TrimSpace(candidate.DisplayName)
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = r.now().UTC()
	}
	candidate.UpdatedAt = candidate.CreatedAt
	if candidate.Revision == 0 {
		candidate.Revision = 1
	}
	candidate.SkillBindingIDs = normalizedStrings(candidate.SkillBindingIDs)
	if err := candidate.Validate(); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("deployment activation actor is required")
	}
	definition, err := r.store.GetDefinition(ctx, candidate.DefinitionID, candidate.ActiveVersion)
	if err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, candidate); err != nil {
		return nil, nil, err
	}
	activation := DefinitionActivation{
		ID: r.newID(), Scope: candidate.Scope, DeploymentID: candidate.ID, DefinitionID: candidate.DefinitionID,
		ToVersion: candidate.ActiveVersion, DeploymentRevision: candidate.Revision, Reason: strings.TrimSpace(reason),
		ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: candidate.CreatedAt,
	}
	if err := r.store.CreateDeployment(ctx, candidate, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(candidate), &copyActivation, nil
}

func (r *Registry) GetDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*AgentDeployment, error) {
	return r.store.GetDeployment(ctx, scope, id)
}

func (r *Registry) ListDeployments(ctx context.Context, scope capability.ScopeReference) ([]*AgentDeployment, error) {
	return r.store.ListDeployments(ctx, scope)
}

// UpdateDeployment atomically changes only an Agent's deployment-local
// configuration. Immutable behavior identity and definition lineage cannot be
// changed through this command; those changes use definition activation or the
// governed amendment lifecycle.
func (r *Registry) UpdateDeployment(ctx context.Context, proposed *AgentDeployment, expectedRevision int64, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	if r == nil || r.store == nil || proposed == nil {
		return nil, nil, errors.New("agent registry and deployment are required")
	}
	actorType, actorID, reason = strings.TrimSpace(actorType), strings.TrimSpace(actorID), strings.TrimSpace(reason)
	if actorType == "" || actorID == "" || reason == "" {
		return nil, nil, errors.New("agent deployment update actor and reason are required")
	}
	current, err := r.store.GetDeployment(ctx, proposed.Scope, proposed.ID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expectedRevision || proposed.Revision != expectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	if proposed.ID != current.ID || proposed.Scope != current.Scope || proposed.DefinitionID != current.DefinitionID ||
		proposed.ActiveVersion != current.ActiveVersion || proposed.PreviousVersion != current.PreviousVersion ||
		!proposed.CreatedAt.Equal(current.CreatedAt) {
		return nil, nil, errors.New("agent deployment update cannot change identity or definition lineage")
	}
	if err := validateRolloutTransition(current.RolloutStatus, proposed.RolloutStatus); err != nil {
		return nil, nil, err
	}
	updated := cloneDeployment(proposed)
	updated.DisplayName = strings.TrimSpace(updated.DisplayName)
	updated.SkillBindingIDs = normalizedStrings(updated.SkillBindingIDs)
	updated.Revision = current.Revision + 1
	updated.UpdatedAt = r.now().UTC()
	definition, err := r.store.GetDefinition(ctx, updated.DefinitionID, updated.ActiveVersion)
	if err != nil {
		return nil, nil, err
	}
	if err := updated.Validate(); err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, updated); err != nil {
		return nil, nil, err
	}
	if deploymentConfigurationEqual(current, updated) {
		return nil, nil, errors.New("agent deployment update does not change configuration")
	}
	activation := DefinitionActivation{
		ID: r.newID(), Scope: updated.Scope, DeploymentID: updated.ID, DefinitionID: updated.DefinitionID,
		FromVersion: current.ActiveVersion, ToVersion: current.ActiveVersion, DeploymentRevision: updated.Revision,
		ChangeKind: workforce.DeploymentChangeConfigurationUpdated,
		Reason:     reason, ActorType: actorType, ActorID: actorID, CreatedAt: updated.UpdatedAt,
	}
	if err := r.store.UpdateDeployment(ctx, updated, expectedRevision, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(updated), &copyActivation, nil
}

// validateRolloutTransition keeps lifecycle changes explicit and irreversible.
// A retired deployment is an auditable terminal record; callers create a new
// deployment instead of silently resurrecting or rewriting it.
func validateRolloutTransition(current, proposed RolloutStatus) error {
	if current == RolloutRetired {
		return errors.New("retired agent deployments cannot be changed")
	}
	if current == proposed {
		return nil
	}
	allowed := map[RolloutStatus]map[RolloutStatus]bool{
		RolloutPending:  {RolloutActive: true, RolloutPaused: true, RolloutRetired: true},
		RolloutActive:   {RolloutPaused: true, RolloutRetired: true},
		RolloutDegraded: {RolloutPaused: true, RolloutRetired: true},
		RolloutPaused:   {RolloutActive: true, RolloutRetired: true},
	}
	if allowed[current][proposed] {
		return nil
	}
	return errors.New("invalid agent deployment rollout transition")
}

func deploymentConfigurationEqual(left, right *AgentDeployment) bool {
	leftCopy, rightCopy := cloneDeployment(left), cloneDeployment(right)
	if leftCopy == nil || rightCopy == nil {
		return leftCopy == nil && rightCopy == nil
	}
	leftCopy.Revision, rightCopy.Revision = 0, 0
	leftCopy.UpdatedAt, rightCopy.UpdatedAt = time.Time{}, time.Time{}
	leftJSON, _ := json.Marshal(leftCopy)
	rightJSON, _ := json.Marshal(rightCopy)
	return string(leftJSON) == string(rightJSON)
}

func (r *Registry) ActivateDefinition(ctx context.Context, scope capability.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("deployment activation actor is required")
	}
	if strings.EqualFold(strings.TrimSpace(actorType), "agent") {
		return nil, nil, errors.New("agents must activate behavior changes through the amendment workflow")
	}
	current, err := r.store.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	sameVersion := current.ActiveVersion == version
	if sameVersion && current.RolloutStatus == RolloutActive {
		return nil, nil, errors.New("agent deployment already uses the requested definition version")
	}
	definition, err := r.store.GetDefinition(ctx, current.DefinitionID, version)
	if err != nil {
		return nil, nil, err
	}
	updated := cloneDeployment(current)
	if !sameVersion {
		updated.PreviousVersion = current.ActiveVersion
	}
	updated.ActiveVersion = version
	if err := validateRolloutTransition(current.RolloutStatus, RolloutActive); err != nil {
		return nil, nil, err
	}
	updated.RolloutStatus = RolloutActive
	updated.Revision++
	updated.UpdatedAt = r.now().UTC()
	if err := validateNarrowing(definition, updated); err != nil {
		return nil, nil, err
	}
	activation := DefinitionActivation{
		ID: r.newID(), Scope: scope, DeploymentID: deploymentID, DefinitionID: updated.DefinitionID,
		FromVersion: current.ActiveVersion, ToVersion: version, DeploymentRevision: updated.Revision,
		Reason: strings.TrimSpace(reason), ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: updated.UpdatedAt,
	}
	if err := r.store.UpdateDeployment(ctx, updated, expectedRevision, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(updated), &copyActivation, nil
}

func (r *Registry) RollbackDefinition(ctx context.Context, scope capability.ScopeReference, deploymentID string, expectedRevision int64, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	current, err := r.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(current.PreviousVersion) == "" {
		return nil, nil, errors.New("agent deployment has no previous definition version")
	}
	return r.ActivateDefinition(ctx, scope, deploymentID, current.PreviousVersion, expectedRevision, actorType, actorID, reason)
}

func (r *Registry) ListActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]DefinitionActivation, error) {
	return r.store.ListActivations(ctx, scope, deploymentID)
}

func (r *Registry) ProposeAmendment(ctx context.Context, req ProposeAmendmentRequest) (*DefinitionAmendment, error) {
	deployment, err := r.store.GetDeployment(ctx, req.Scope, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	if req.ExpectedDeploymentRevision > 0 && deployment.Revision != req.ExpectedDeploymentRevision {
		return nil, ErrRevisionConflict
	}
	base, err := r.store.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(req.ProposerType), "agent") && !base.Amendments.AgentMayPropose {
		return nil, errors.New("agent definition policy does not allow agent-proposed amendments")
	}
	if strings.TrimSpace(req.ProposerType) == "" || strings.TrimSpace(req.ProposerID) == "" || strings.TrimSpace(req.Rationale) == "" {
		return nil, errors.New("amendment proposer and rationale are required")
	}
	candidate, err := prepareDefinition(req.Candidate, r.now().UTC())
	if err != nil {
		return nil, err
	}
	if candidate.ID != base.ID || candidate.Version == base.Version {
		return nil, errors.New("amendment candidate must use the same definition id and a new version")
	}
	candidate.Provenance.DerivedFrom = base.Digest
	candidate.Provenance.CreatedBy = strings.TrimSpace(req.ProposerType) + ":" + strings.TrimSpace(req.ProposerID)
	candidate.Digest = definitionDigest(candidate)
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	evidenceRefs := normalizedStrings(req.EvidenceRefs)
	requestDigest := amendmentRequestDigest(deployment.ID, base.Digest, candidate.Digest, req.ProposerType, req.ProposerID, req.Rationale, evidenceRefs)
	amendmentID := r.newID()
	if idempotencyKey != "" {
		amendmentID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(req.Scope.Kind+"\x00"+req.Scope.ID+"\x00"+deployment.ID+"\x00"+idempotencyKey)).String()
		if existing, lookupErr := r.store.GetAmendment(ctx, req.Scope, amendmentID); lookupErr == nil {
			if existing.IdempotencyKey != idempotencyKey || existing.RequestDigest != requestDigest {
				return nil, ErrIdempotencyConflict
			}
			return existing, nil
		} else if !errors.Is(lookupErr, ErrAmendmentNotFound) {
			return nil, lookupErr
		}
	}
	if existing, lookupErr := r.store.GetDefinition(ctx, candidate.ID, candidate.Version); lookupErr == nil && existing != nil {
		return nil, errors.New("amendment candidate version already exists")
	} else if lookupErr != nil && !errors.Is(lookupErr, ErrDefinitionNotFound) {
		return nil, lookupErr
	}
	changes := definitionChanges(base, candidate)
	if len(changes) == 0 {
		return nil, errors.New("amendment candidate does not change behavior")
	}
	changedFields := make([]string, len(changes))
	for index := range changes {
		changedFields[index] = changes[index].Field
	}
	if !isSubset(changedFields, base.Amendments.AllowedFields) {
		return nil, errors.New("amendment changes fields outside the definition policy")
	}
	riskWidening := riskRank(candidate.Authority.MaximumRisk) > riskRank(base.Authority.MaximumRisk) || candidate.Authority.MaxConcurrentRuns > base.Authority.MaxConcurrentRuns || !isSubset(candidate.Authority.AllowedSkillIDs, base.Authority.AllowedSkillIDs)
	if riskWidening && len(base.Amendments.ApproverPrincipals) == 0 {
		return nil, errors.New("risk-widening amendments require eligible approver principals")
	}
	status := AmendmentReady
	if len(base.Evaluations) > 0 {
		status = AmendmentEvaluating
	} else if base.Amendments.RequiresApproval || riskWidening {
		status = AmendmentAwaitingApproval
	}
	now := r.now().UTC()
	amendment := &DefinitionAmendment{
		ID: amendmentID, Scope: req.Scope, DeploymentID: deployment.ID, DefinitionID: base.ID,
		BaseVersion: base.Version, BaseDigest: base.Digest, Candidate: *candidate, Changes: changes, RiskWidening: riskWidening,
		ProposerType: strings.TrimSpace(req.ProposerType), ProposerID: strings.TrimSpace(req.ProposerID), Rationale: strings.TrimSpace(req.Rationale),
		EvidenceRefs: evidenceRefs, IdempotencyKey: idempotencyKey, RequestDigest: requestDigest, Status: status, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if err := amendment.Validate(); err != nil {
		return nil, err
	}
	if err := r.store.CreateAmendment(ctx, amendment); err != nil {
		if idempotencyKey != "" {
			if existing, lookupErr := r.store.GetAmendment(ctx, req.Scope, amendmentID); lookupErr == nil && existing.IdempotencyKey == idempotencyKey && existing.RequestDigest == requestDigest {
				return existing, nil
			}
		}
		return nil, err
	}
	return cloneAmendment(amendment), nil
}

func amendmentRequestDigest(deploymentID, baseDigest, candidateDigest, proposerType, proposerID, rationale string, evidenceRefs []string) string {
	payload, _ := json.Marshal(struct {
		DeploymentID, BaseDigest, CandidateDigest, ProposerType, ProposerID, Rationale string
		EvidenceRefs                                                                   []string
	}{strings.TrimSpace(deploymentID), strings.TrimSpace(baseDigest), strings.TrimSpace(candidateDigest), strings.TrimSpace(proposerType), strings.TrimSpace(proposerID), strings.TrimSpace(rationale), evidenceRefs})
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (r *Registry) GetAmendment(ctx context.Context, scope capability.ScopeReference, amendmentID string) (*DefinitionAmendment, error) {
	return r.store.GetAmendment(ctx, scope, amendmentID)
}

// ListAmendments returns the complete scoped governance history for one Agent
// deployment, newest first. The deployment selector is required so callers do
// not accidentally turn tenant-wide amendment history into an unbounded UI.
func (r *Registry) ListAmendments(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]*DefinitionAmendment, error) {
	if strings.TrimSpace(scope.Kind) == "" || strings.TrimSpace(scope.ID) == "" || strings.TrimSpace(deploymentID) == "" {
		return nil, errors.New("amendment scope and deployment id are required")
	}
	if _, err := r.store.GetDeployment(ctx, scope, strings.TrimSpace(deploymentID)); err != nil {
		return nil, err
	}
	return r.store.ListAmendments(ctx, scope, strings.TrimSpace(deploymentID))
}

func (r *Registry) SubmitAmendmentEvaluation(ctx context.Context, req SubmitAmendmentEvaluationRequest) (*DefinitionAmendment, error) {
	current, err := r.store.GetAmendment(ctx, req.Scope, req.AmendmentID)
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if current.Status != AmendmentEvaluating {
		return nil, errors.New("amendment is not awaiting evaluation")
	}
	base, err := r.store.GetDefinition(ctx, current.DefinitionID, current.BaseVersion)
	if err != nil {
		return nil, err
	}
	results := make(map[string]AmendmentEvaluation)
	for _, result := range req.Evaluations {
		if strings.TrimSpace(result.CriterionID) == "" || strings.TrimSpace(result.Summary) == "" || results[result.CriterionID].CriterionID != "" {
			return nil, errors.New("evaluation results require unique criterion ids and summaries")
		}
		result.EvidenceRefs = normalizedStrings(result.EvidenceRefs)
		results[result.CriterionID] = result
	}
	passed := true
	ordered := make([]AmendmentEvaluation, 0, len(base.Evaluations))
	for _, criterion := range base.Evaluations {
		result, ok := results[criterion.ID]
		if !ok {
			return nil, errors.New("evaluation result is missing a definition criterion")
		}
		if criterion.Required && !result.Passed {
			passed = false
		}
		ordered = append(ordered, result)
	}
	if len(results) != len(base.Evaluations) {
		return nil, errors.New("evaluation results contain unknown criteria")
	}
	updated := cloneAmendment(current)
	updated.Evaluations = ordered
	updated.Revision++
	updated.UpdatedAt = r.now().UTC()
	if !passed {
		updated.Status = AmendmentEvaluationFailed
	} else if base.Amendments.RequiresApproval || updated.RiskWidening {
		updated.Status = AmendmentAwaitingApproval
	} else {
		updated.Status = AmendmentReady
	}
	if err := r.store.UpdateAmendment(ctx, updated, current.Revision); err != nil {
		return nil, err
	}
	return cloneAmendment(updated), nil
}

func (r *Registry) ResolveAmendment(ctx context.Context, req ResolveAmendmentRequest) (*DefinitionAmendment, error) {
	current, err := r.store.GetAmendment(ctx, req.Scope, req.AmendmentID)
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if current.Status != AmendmentAwaitingApproval || strings.TrimSpace(req.ActorType) == "" || strings.TrimSpace(req.ActorID) == "" {
		return nil, errors.New("amendment is not awaiting a valid approval decision")
	}
	base, err := r.store.GetDefinition(ctx, current.DefinitionID, current.BaseVersion)
	if err != nil {
		return nil, err
	}
	principal := strings.TrimSpace(req.ActorType) + ":" + strings.TrimSpace(req.ActorID)
	if !isSubset([]string{principal}, base.Amendments.ApproverPrincipals) {
		return nil, errors.New("principal is not eligible to approve this amendment")
	}
	updated := cloneAmendment(current)
	now := r.now().UTC()
	updated.Decision = &AmendmentDecision{Approved: req.Approved, ActorType: strings.TrimSpace(req.ActorType), ActorID: strings.TrimSpace(req.ActorID), Reason: strings.TrimSpace(req.Reason), DecidedAt: now}
	if req.Approved {
		updated.Status = AmendmentApproved
	} else {
		updated.Status = AmendmentRejected
	}
	updated.Revision++
	updated.UpdatedAt = now
	if err := r.store.UpdateAmendment(ctx, updated, current.Revision); err != nil {
		return nil, err
	}
	return cloneAmendment(updated), nil
}

func (r *Registry) ActivateAmendment(ctx context.Context, scope capability.ScopeReference, amendmentID string, expectedRevision int64, actorType, actorID, reason string) (*DefinitionAmendment, *AgentDeployment, *DefinitionActivation, error) {
	current, err := r.store.GetAmendment(ctx, scope, amendmentID)
	if err != nil {
		return nil, nil, nil, err
	}
	if current.Revision != expectedRevision {
		return nil, nil, nil, ErrRevisionConflict
	}
	if current.Status != AmendmentReady && current.Status != AmendmentApproved {
		return nil, nil, nil, errors.New("amendment is not ready for activation")
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, nil, errors.New("amendment activation actor is required")
	}
	deployment, err := r.store.GetDeployment(ctx, scope, current.DeploymentID)
	if err != nil {
		return nil, nil, nil, err
	}
	if deployment.ActiveVersion != current.BaseVersion {
		return nil, nil, nil, errors.New("amendment base version is no longer active")
	}
	base, err := r.store.GetDefinition(ctx, current.DefinitionID, current.BaseVersion)
	if err != nil {
		return nil, nil, nil, err
	}
	if base.Digest != current.BaseDigest {
		return nil, nil, nil, errors.New("amendment base digest no longer matches")
	}
	definition := cloneDefinition(&current.Candidate)
	updatedDeployment := cloneDeployment(deployment)
	updatedDeployment.PreviousVersion = deployment.ActiveVersion
	updatedDeployment.ActiveVersion = definition.Version
	updatedDeployment.RolloutStatus = RolloutActive
	updatedDeployment.Revision++
	updatedDeployment.UpdatedAt = r.now().UTC()
	if err := validateNarrowing(definition, updatedDeployment); err != nil {
		return nil, nil, nil, err
	}
	activation := DefinitionActivation{
		ID: r.newID(), Scope: scope, DeploymentID: deployment.ID, DefinitionID: definition.ID,
		FromVersion: deployment.ActiveVersion, ToVersion: definition.Version, DeploymentRevision: updatedDeployment.Revision,
		Reason: strings.TrimSpace(reason), ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: updatedDeployment.UpdatedAt,
	}
	updatedAmendment := cloneAmendment(current)
	updatedAmendment.Status = AmendmentActivated
	updatedAmendment.ActivationID = activation.ID
	updatedAmendment.Revision++
	updatedAmendment.UpdatedAt = updatedDeployment.UpdatedAt
	if err := r.store.ActivateAmendment(ctx, updatedAmendment, current.Revision, definition, updatedDeployment, deployment.Revision, activation); err != nil {
		return nil, nil, nil, err
	}
	copyActivation := activation
	return cloneAmendment(updatedAmendment), cloneDeployment(updatedDeployment), &copyActivation, nil
}

func canonicalizeDefinition(value *AgentDefinition) {
	if value == nil {
		return
	}
	value.ID = strings.TrimSpace(value.ID)
	value.Version = strings.TrimSpace(value.Version)
	value.OperatingPrinciples = normalizedStrings(value.OperatingPrinciples)
	value.Authority.AllowedSkillIDs = normalizedStrings(value.Authority.AllowedSkillIDs)
	value.Amendments.AllowedFields = normalizedStrings(value.Amendments.AllowedFields)
	value.Amendments.ApproverPrincipals = normalizedStrings(value.Amendments.ApproverPrincipals)
	for index := range value.SkillRequirements {
		value.SkillRequirements[index].RequiredActions = normalizedStrings(value.SkillRequirements[index].RequiredActions)
	}
	sort.Slice(value.SkillRequirements, func(i, j int) bool { return value.SkillRequirements[i].SkillID < value.SkillRequirements[j].SkillID })
	sort.Slice(value.ObjectiveTemplates, func(i, j int) bool { return value.ObjectiveTemplates[i].ID < value.ObjectiveTemplates[j].ID })
	sort.Slice(value.Evaluations, func(i, j int) bool { return value.Evaluations[i].ID < value.Evaluations[j].ID })
}

func prepareDefinition(definition *AgentDefinition, now time.Time) (*AgentDefinition, error) {
	candidate := cloneDefinition(definition)
	canonicalizeDefinition(candidate)
	if candidate == nil {
		return nil, errors.New("agent definition is required")
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = now
	}
	providedDigest := candidate.Digest
	candidate.Digest = ""
	digest := definitionDigest(candidate)
	if providedDigest != "" && providedDigest != digest {
		return nil, errors.New("agent definition digest does not match content")
	}
	candidate.Digest = digest
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	return candidate, nil
}

func definitionChanges(base, candidate *AgentDefinition) []DefinitionFieldChange {
	base = cloneDefinition(base)
	candidate = cloneDefinition(candidate)
	canonicalizeDefinition(base)
	canonicalizeDefinition(candidate)
	baseMap, candidateMap := make(map[string]interface{}), make(map[string]interface{})
	baseJSON, _ := json.Marshal(base)
	candidateJSON, _ := json.Marshal(candidate)
	_ = json.Unmarshal(baseJSON, &baseMap)
	_ = json.Unmarshal(candidateJSON, &candidateMap)
	ignored := map[string]bool{"version": true, "digest": true, "createdAt": true, "provenance": true}
	fieldSet := make(map[string]bool, len(baseMap)+len(candidateMap))
	for field := range baseMap {
		if !ignored[field] {
			fieldSet[field] = true
		}
	}
	for field := range candidateMap {
		if !ignored[field] {
			fieldSet[field] = true
		}
	}
	fields := mapKeys(fieldSet)
	sort.Strings(fields)
	changes := make([]DefinitionFieldChange, 0)
	for _, field := range fields {
		before, _ := json.Marshal(baseMap[field])
		after, _ := json.Marshal(candidateMap[field])
		if string(before) == string(after) {
			continue
		}
		beforeDigest := sha256.Sum256(before)
		afterDigest := sha256.Sum256(after)
		changes = append(changes, DefinitionFieldChange{Field: field, BeforeDigest: hex.EncodeToString(beforeDigest[:]), AfterDigest: hex.EncodeToString(afterDigest[:])})
	}
	return changes
}

func definitionDigest(value *AgentDefinition) string {
	copy := cloneDefinition(value)
	copy.Digest = ""
	copy.CreatedAt = time.Time{}
	encoded, _ := json.Marshal(copy)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func definitionKey(id, version string) string {
	return strings.TrimSpace(id) + "@" + strings.TrimSpace(version)
}
func deploymentKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + ":" + scope.ID + ":" + strings.TrimSpace(id)
}
func amendmentKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + ":" + scope.ID + ":" + strings.TrimSpace(id)
}
func compilationKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + ":" + scope.ID + ":" + strings.TrimSpace(id)
}

func cloneDefinition(value *AgentDefinition) *AgentDefinition {
	if value == nil {
		return nil
	}
	var result AgentDefinition
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneDeployment(value *AgentDeployment) *AgentDeployment {
	if value == nil {
		return nil
	}
	var result AgentDeployment
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneAmendment(value *DefinitionAmendment) *DefinitionAmendment {
	if value == nil {
		return nil
	}
	var result DefinitionAmendment
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}
