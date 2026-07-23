package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
)

var (
	ErrDefinitionNotFound  = errors.New("team definition not found")
	ErrDeploymentNotFound  = errors.New("team deployment not found")
	ErrRevisionConflict    = errors.New("team deployment revision conflict")
	ErrAmendmentNotFound   = errors.New("team definition amendment not found")
	ErrIdempotencyConflict = errors.New("team amendment idempotency key was already used for a different proposal")
)

type AgentResolver interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelagent.AgentDeployment, error)
	GetDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error)
}

type Registry struct {
	store  Store
	agents AgentResolver
	now    func() time.Time
	newID  func() string
}

func NewRegistry(agents AgentResolver) *Registry {
	return NewRegistryWithStore(NewMemoryStore(), agents)
}

func NewRegistryWithStore(store Store, agents AgentResolver) *Registry {
	return &Registry{store: store, agents: agents, now: time.Now, newID: uuid.NewString}
}

func (r *Registry) RegisterDefinition(ctx context.Context, definition *Definition) (*Definition, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("team registry is not configured")
	}
	candidate, err := PrepareDefinition(definition, r.now().UTC())
	if err != nil {
		return nil, err
	}
	if err := r.store.CreateTeamDefinition(ctx, candidate); err != nil {
		return nil, err
	}
	return cloneDefinition(candidate), nil
}

func (r *Registry) GetDefinition(ctx context.Context, id, version string) (*Definition, error) {
	return r.store.GetTeamDefinition(ctx, id, version)
}

func (r *Registry) ListDefinitionVersions(ctx context.Context, id string) ([]*Definition, error) {
	return r.store.ListTeamDefinitionVersions(ctx, id)
}

func (r *Registry) CreateDeployment(ctx context.Context, deployment *Deployment, actorType, actorID, reason string) (*Deployment, *workforce.DefinitionActivation, error) {
	if r == nil || r.store == nil || r.agents == nil {
		return nil, nil, errors.New("team registry and Agent resolver are required")
	}
	candidate := cloneDeployment(deployment)
	if candidate == nil {
		return nil, nil, errors.New("team deployment is required")
	}
	if candidate.CreatedAt.IsZero() {
		candidate.CreatedAt = r.now().UTC()
	}
	candidate.UpdatedAt = candidate.CreatedAt
	if candidate.Revision == 0 {
		candidate.Revision = 1
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("team deployment activation actor is required")
	}
	definition, err := r.store.GetTeamDefinition(ctx, candidate.DefinitionID, candidate.ActiveVersion)
	if err != nil {
		return nil, nil, err
	}
	if err := candidate.Validate(definition); err != nil {
		return nil, nil, err
	}
	if err := r.validateRoster(ctx, definition, candidate); err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, candidate); err != nil {
		return nil, nil, err
	}
	activation := workforce.DefinitionActivation{
		ID: r.newID(), Scope: candidate.Scope, DeploymentID: candidate.ID, DefinitionID: candidate.DefinitionID,
		ToVersion: candidate.ActiveVersion, DeploymentRevision: candidate.Revision, Reason: strings.TrimSpace(reason),
		ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: candidate.CreatedAt,
	}
	if err := r.store.CreateTeamDeployment(ctx, candidate, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(candidate), &copyActivation, nil
}

func (r *Registry) GetDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*Deployment, error) {
	return r.store.GetTeamDeployment(ctx, scope, id)
}

func (r *Registry) ListDeployments(ctx context.Context, scope capability.ScopeReference) ([]*Deployment, error) {
	return r.store.ListTeamDeployments(ctx, scope)
}

// UpdateDeployment atomically reconciles a Team's active immutable definition,
// composition, restrictions, and operating state. This prevents role changes
// from requiring an impossible ordering between definition activation and
// roster mutation. The activation entry is the durable actor/reason audit.
func (r *Registry) UpdateDeployment(ctx context.Context, proposed *Deployment, expectedRevision int64, actorType, actorID, reason string) (*Deployment, *workforce.DefinitionActivation, error) {
	if r == nil || r.store == nil || r.agents == nil || proposed == nil {
		return nil, nil, errors.New("team registry, Agent resolver, and deployment are required")
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" || strings.TrimSpace(reason) == "" {
		return nil, nil, errors.New("team deployment update actor and reason are required")
	}
	current, err := r.store.GetTeamDeployment(ctx, proposed.Scope, proposed.ID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	if proposed.DefinitionID != current.DefinitionID {
		return nil, nil, errors.New("team deployment update cannot change definition identity")
	}
	updated := cloneDeployment(proposed)
	updated.CreatedAt = current.CreatedAt
	updated.UpdatedAt = r.now().UTC()
	updated.Revision = current.Revision + 1
	definition, err := r.store.GetTeamDefinition(ctx, updated.DefinitionID, updated.ActiveVersion)
	if err != nil {
		return nil, nil, err
	}
	if err := updated.Validate(definition); err != nil {
		return nil, nil, err
	}
	if err := r.validateRoster(ctx, definition, updated); err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, updated); err != nil {
		return nil, nil, err
	}
	activation := workforce.DefinitionActivation{
		ID: r.newID(), Scope: updated.Scope, DeploymentID: updated.ID, DefinitionID: updated.DefinitionID,
		FromVersion: current.ActiveVersion, ToVersion: updated.ActiveVersion, DeploymentRevision: updated.Revision,
		Reason: strings.TrimSpace(reason), ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: updated.UpdatedAt,
	}
	if err := r.store.UpdateTeamDeployment(ctx, updated, expectedRevision, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(updated), &copyActivation, nil
}

func (r *Registry) ActivateDefinition(ctx context.Context, scope capability.ScopeReference, deploymentID, version string, expectedRevision int64, actorType, actorID, reason string) (*Deployment, *workforce.DefinitionActivation, error) {
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, errors.New("team deployment activation actor is required")
	}
	if strings.EqualFold(strings.TrimSpace(actorType), "agent") {
		return nil, nil, errors.New("agents must activate Team behavior changes through the amendment workflow")
	}
	current, err := r.store.GetTeamDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, nil, err
	}
	if current.Revision != expectedRevision {
		return nil, nil, ErrRevisionConflict
	}
	definition, err := r.store.GetTeamDefinition(ctx, current.DefinitionID, version)
	if err != nil {
		return nil, nil, err
	}
	updated := cloneDeployment(current)
	updated.ActiveVersion = version
	updated.Status = DeploymentActive
	updated.Revision++
	updated.UpdatedAt = r.now().UTC()
	if err := updated.Validate(definition); err != nil {
		return nil, nil, err
	}
	if err := r.validateRoster(ctx, definition, updated); err != nil {
		return nil, nil, err
	}
	if err := validateNarrowing(definition, updated); err != nil {
		return nil, nil, err
	}
	activation := workforce.DefinitionActivation{
		ID: r.newID(), Scope: scope, DeploymentID: deploymentID, DefinitionID: updated.DefinitionID,
		FromVersion: current.ActiveVersion, ToVersion: version, DeploymentRevision: updated.Revision,
		Reason: strings.TrimSpace(reason), ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: updated.UpdatedAt,
	}
	if err := r.store.UpdateTeamDeployment(ctx, updated, expectedRevision, activation); err != nil {
		return nil, nil, err
	}
	copyActivation := activation
	return cloneDeployment(updated), &copyActivation, nil
}

func (r *Registry) ListActivations(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]workforce.DefinitionActivation, error) {
	return r.store.ListTeamDefinitionActivations(ctx, scope, deploymentID)
}

func (r *Registry) ProposeAmendment(ctx context.Context, req ProposeAmendmentRequest) (*DefinitionAmendment, error) {
	deployment, err := r.store.GetTeamDeployment(ctx, req.Scope, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	if req.ExpectedDeploymentRevision > 0 && deployment.Revision != req.ExpectedDeploymentRevision {
		return nil, ErrRevisionConflict
	}
	base, err := r.store.GetTeamDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(req.ProposerType), "agent") && !base.Amendments.AgentMayPropose {
		return nil, errors.New("team definition policy does not allow agent-proposed amendments")
	}
	if strings.TrimSpace(req.ProposerType) == "" || strings.TrimSpace(req.ProposerID) == "" || strings.TrimSpace(req.Rationale) == "" {
		return nil, errors.New("team amendment proposer and rationale are required")
	}
	candidate, err := PrepareDefinition(req.Candidate, r.now().UTC())
	if err != nil {
		return nil, err
	}
	if candidate.ID != base.ID || candidate.Version == base.Version {
		return nil, errors.New("team amendment candidate must use the same definition id and a new version")
	}
	candidate.Provenance.DerivedFrom = base.Digest
	candidate.Provenance.CreatedBy = strings.TrimSpace(req.ProposerType) + ":" + strings.TrimSpace(req.ProposerID)
	candidate.Digest = teamDefinitionDigest(candidate)
	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	evidenceRefs := normalizedStrings(req.EvidenceRefs)
	requestDigest := AmendmentRequestDigest(deployment.ID, base.Digest, candidate.Digest, req.ProposerType, req.ProposerID, req.Rationale, evidenceRefs)
	amendmentID := r.newID()
	if idempotencyKey != "" {
		amendmentID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(req.Scope.Kind+"\x00"+req.Scope.ID+"\x00"+deployment.ID+"\x00"+idempotencyKey)).String()
		if existing, lookupErr := r.store.GetTeamAmendment(ctx, req.Scope, amendmentID); lookupErr == nil {
			if existing.IdempotencyKey != idempotencyKey || existing.RequestDigest != requestDigest {
				return nil, ErrIdempotencyConflict
			}
			return existing, nil
		} else if !errors.Is(lookupErr, ErrAmendmentNotFound) {
			return nil, lookupErr
		}
	}
	if existing, lookupErr := r.store.GetTeamDefinition(ctx, candidate.ID, candidate.Version); lookupErr == nil && existing != nil {
		return nil, errors.New("team amendment candidate version already exists")
	} else if lookupErr != nil && !errors.Is(lookupErr, ErrDefinitionNotFound) {
		return nil, lookupErr
	}
	changes := teamDefinitionChanges(base, candidate)
	if len(changes) == 0 {
		return nil, errors.New("team amendment candidate does not change behavior")
	}
	changedFields := make([]string, len(changes))
	for index := range changes {
		changedFields[index] = changes[index].Field
	}
	if !stringSubset(changedFields, base.Amendments.AllowedFields) {
		return nil, errors.New("team amendment changes fields outside the definition policy")
	}
	riskWidening := riskRank(candidate.Approvals.MaximumRisk) > riskRank(base.Approvals.MaximumRisk) ||
		candidate.Delegation.MaximumConcurrent > base.Delegation.MaximumConcurrent || candidate.Delegation.MaximumDepth > base.Delegation.MaximumDepth ||
		candidate.SharedContext.AllowMemberWrite && !base.SharedContext.AllowMemberWrite
	if riskWidening && len(base.Amendments.ApproverPrincipals) == 0 {
		return nil, errors.New("risk-widening Team amendments require eligible approver principals")
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
	if err := r.store.CreateTeamAmendment(ctx, amendment); err != nil {
		if idempotencyKey != "" {
			if existing, lookupErr := r.store.GetTeamAmendment(ctx, req.Scope, amendmentID); lookupErr == nil && existing.IdempotencyKey == idempotencyKey && existing.RequestDigest == requestDigest {
				return existing, nil
			}
		}
		return nil, err
	}
	return cloneAmendment(amendment), nil
}

// AmendmentRequestDigest returns the stable fingerprint used to make Team
// amendment proposals idempotent. Importers that rewrite definition identity
// or digest metadata must recompute this value rather than preserving a stale
// fingerprint.
func AmendmentRequestDigest(deploymentID, baseDigest, candidateDigest, proposerType, proposerID, rationale string, evidenceRefs []string) string {
	payload, _ := json.Marshal(struct {
		DeploymentID, BaseDigest, CandidateDigest, ProposerType, ProposerID, Rationale string
		EvidenceRefs                                                                   []string
	}{strings.TrimSpace(deploymentID), strings.TrimSpace(baseDigest), strings.TrimSpace(candidateDigest), strings.TrimSpace(proposerType), strings.TrimSpace(proposerID), strings.TrimSpace(rationale), normalizedStrings(evidenceRefs)})
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (r *Registry) GetAmendment(ctx context.Context, scope capability.ScopeReference, amendmentID string) (*DefinitionAmendment, error) {
	return r.store.GetTeamAmendment(ctx, scope, amendmentID)
}

// ListAmendments returns the scoped durable governance history for a Team
// deployment, newest first. An empty deployment id lists the scope-wide
// history for administrative clients without weakening scope isolation.
func (r *Registry) ListAmendments(ctx context.Context, scope capability.ScopeReference, deploymentID string) ([]*DefinitionAmendment, error) {
	return r.store.ListTeamAmendments(ctx, scope, strings.TrimSpace(deploymentID))
}

func (r *Registry) SubmitAmendmentEvaluation(ctx context.Context, req SubmitAmendmentEvaluationRequest) (*DefinitionAmendment, error) {
	current, err := r.store.GetTeamAmendment(ctx, req.Scope, req.AmendmentID)
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if current.Status != AmendmentEvaluating {
		return nil, errors.New("team amendment is not awaiting evaluation")
	}
	base, err := r.store.GetTeamDefinition(ctx, current.DefinitionID, current.BaseVersion)
	if err != nil {
		return nil, err
	}
	results := make(map[string]AmendmentEvaluation)
	for _, result := range req.Evaluations {
		if strings.TrimSpace(result.CriterionID) == "" || strings.TrimSpace(result.Summary) == "" || results[result.CriterionID].CriterionID != "" {
			return nil, errors.New("Team evaluation results require unique criterion ids and summaries")
		}
		result.EvidenceRefs = normalizedStrings(result.EvidenceRefs)
		results[result.CriterionID] = result
	}
	passed := true
	ordered := make([]AmendmentEvaluation, 0, len(base.Evaluations))
	for _, criterion := range base.Evaluations {
		result, ok := results[criterion.ID]
		if !ok {
			return nil, errors.New("Team evaluation result is missing a definition criterion")
		}
		if criterion.Required && !result.Passed {
			passed = false
		}
		ordered = append(ordered, result)
	}
	if len(results) != len(base.Evaluations) {
		return nil, errors.New("Team evaluation results contain unknown criteria")
	}
	updated := cloneAmendment(current)
	updated.Evaluations, updated.Revision, updated.UpdatedAt = ordered, current.Revision+1, r.now().UTC()
	if !passed {
		updated.Status = AmendmentEvaluationFailed
	} else if base.Amendments.RequiresApproval || updated.RiskWidening {
		updated.Status = AmendmentAwaitingApproval
	} else {
		updated.Status = AmendmentReady
	}
	if err := r.store.UpdateTeamAmendment(ctx, updated, current.Revision); err != nil {
		return nil, err
	}
	return cloneAmendment(updated), nil
}

func (r *Registry) ResolveAmendment(ctx context.Context, req ResolveAmendmentRequest) (*DefinitionAmendment, error) {
	current, err := r.store.GetTeamAmendment(ctx, req.Scope, req.AmendmentID)
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, ErrRevisionConflict
	}
	if current.Status != AmendmentAwaitingApproval || strings.TrimSpace(req.ActorType) == "" || strings.TrimSpace(req.ActorID) == "" {
		return nil, errors.New("team amendment is not awaiting a valid approval decision")
	}
	base, err := r.store.GetTeamDefinition(ctx, current.DefinitionID, current.BaseVersion)
	if err != nil {
		return nil, err
	}
	principal := strings.TrimSpace(req.ActorType) + ":" + strings.TrimSpace(req.ActorID)
	if !stringSubset([]string{principal}, base.Amendments.ApproverPrincipals) {
		return nil, errors.New("principal is not eligible to approve Team amendment")
	}
	updated := cloneAmendment(current)
	now := r.now().UTC()
	updated.Decision = &AmendmentDecision{Approved: req.Approved, ActorType: strings.TrimSpace(req.ActorType), ActorID: strings.TrimSpace(req.ActorID), Reason: strings.TrimSpace(req.Reason), DecidedAt: now}
	if req.Approved {
		updated.Status = AmendmentApproved
	} else {
		updated.Status = AmendmentRejected
	}
	updated.Revision, updated.UpdatedAt = current.Revision+1, now
	if err := r.store.UpdateTeamAmendment(ctx, updated, current.Revision); err != nil {
		return nil, err
	}
	return cloneAmendment(updated), nil
}

func (r *Registry) ActivateAmendment(ctx context.Context, scope capability.ScopeReference, amendmentID string, expectedRevision int64, actorType, actorID, reason string) (*DefinitionAmendment, *Deployment, *workforce.DefinitionActivation, error) {
	current, err := r.store.GetTeamAmendment(ctx, scope, amendmentID)
	if err != nil {
		return nil, nil, nil, err
	}
	if current.Revision != expectedRevision {
		return nil, nil, nil, ErrRevisionConflict
	}
	if current.Status != AmendmentReady && current.Status != AmendmentApproved {
		return nil, nil, nil, errors.New("team amendment is not ready for activation")
	}
	if strings.TrimSpace(actorType) == "" || strings.TrimSpace(actorID) == "" {
		return nil, nil, nil, errors.New("team amendment activation actor is required")
	}
	deployment, err := r.store.GetTeamDeployment(ctx, scope, current.DeploymentID)
	if err != nil {
		return nil, nil, nil, err
	}
	if deployment.ActiveVersion != current.BaseVersion {
		return nil, nil, nil, errors.New("team amendment base version is no longer active")
	}
	base, err := r.store.GetTeamDefinition(ctx, current.DefinitionID, current.BaseVersion)
	if err != nil || base.Digest != current.BaseDigest {
		return nil, nil, nil, errors.New("team amendment base no longer matches")
	}
	definition := cloneDefinition(&current.Candidate)
	updatedDeployment := cloneDeployment(deployment)
	updatedDeployment.ActiveVersion = definition.Version
	updatedDeployment.Status = DeploymentActive
	updatedDeployment.Revision++
	updatedDeployment.UpdatedAt = r.now().UTC()
	if err := updatedDeployment.Validate(definition); err != nil {
		return nil, nil, nil, err
	}
	if err := r.validateRoster(ctx, definition, updatedDeployment); err != nil {
		return nil, nil, nil, err
	}
	if err := validateNarrowing(definition, updatedDeployment); err != nil {
		return nil, nil, nil, err
	}
	activation := workforce.DefinitionActivation{
		ID: r.newID(), Scope: scope, DeploymentID: deployment.ID, DefinitionID: definition.ID,
		FromVersion: deployment.ActiveVersion, ToVersion: definition.Version, DeploymentRevision: updatedDeployment.Revision,
		Reason: strings.TrimSpace(reason), ActorType: strings.TrimSpace(actorType), ActorID: strings.TrimSpace(actorID), CreatedAt: updatedDeployment.UpdatedAt,
	}
	updatedAmendment := cloneAmendment(current)
	updatedAmendment.Status, updatedAmendment.ActivationID = AmendmentActivated, activation.ID
	updatedAmendment.Revision, updatedAmendment.UpdatedAt = current.Revision+1, updatedDeployment.UpdatedAt
	if err := r.store.ActivateTeamAmendment(ctx, updatedAmendment, current.Revision, definition, updatedDeployment, deployment.Revision, activation); err != nil {
		return nil, nil, nil, err
	}
	copyActivation := activation
	return cloneAmendment(updatedAmendment), cloneDeployment(updatedDeployment), &copyActivation, nil
}

func (r *Registry) validateRoster(ctx context.Context, definition *Definition, deployment *Deployment) error {
	roles := make(map[string]RoleSlot, len(definition.Roles))
	for _, role := range definition.Roles {
		roles[role.ID] = role
	}
	for _, assignment := range deployment.Roster {
		agentDeployment, err := r.agents.GetDeployment(ctx, deployment.Scope, assignment.AgentDeploymentID)
		if err != nil {
			return errors.New("team roster Agent deployment is unavailable in scope")
		}
		agentDefinition, err := r.agents.GetDefinition(ctx, agentDeployment.DefinitionID, agentDeployment.ActiveVersion)
		if err != nil {
			return errors.New("team roster Agent definition is unavailable")
		}
		role := roles[assignment.RoleID]
		if len(role.RequiredDefinitionIDs) > 0 && !contains(role.RequiredDefinitionIDs, agentDefinition.ID) {
			return errors.New("team roster Agent definition does not satisfy its role")
		}
		availableSkills := make(map[string]bool, len(agentDefinition.SkillRequirements))
		for _, requirement := range agentDefinition.SkillRequirements {
			availableSkills[requirement.SkillID] = true
		}
		for _, skillID := range role.RequiredSkillIDs {
			if !availableSkills[skillID] {
				return errors.New("team roster Agent skills do not satisfy its role")
			}
		}
	}
	return nil
}

// PrepareDefinition returns the canonical immutable representation used by the
// Team registry without persisting it. Importers and storage migrations should
// use this function instead of duplicating normalization or digest rules.
//
// createdAt is part of the immutable record but is deliberately excluded from
// the digest. Callers migrating an existing definition should pass its original
// creation time; new registrations normally pass the current time.
func PrepareDefinition(definition *Definition, createdAt time.Time) (*Definition, error) {
	candidate := cloneDefinition(definition)
	if candidate == nil {
		return nil, errors.New("team definition is required")
	}
	candidate.ID = strings.TrimSpace(candidate.ID)
	candidate.Version = strings.TrimSpace(candidate.Version)
	candidate.DisplayName = strings.TrimSpace(candidate.DisplayName)
	candidate.Purpose = strings.TrimSpace(candidate.Purpose)
	candidate.Digest = ""
	candidate.CreatedAt = time.Time{}
	for index := range candidate.Roles {
		candidate.Roles[index].ID = strings.TrimSpace(candidate.Roles[index].ID)
		candidate.Roles[index].RequiredSkillIDs = normalizedStrings(candidate.Roles[index].RequiredSkillIDs)
		candidate.Roles[index].RequiredDefinitionIDs = normalizedStrings(candidate.Roles[index].RequiredDefinitionIDs)
		for grantIndex := range candidate.Roles[index].SkillGrants {
			grant := &candidate.Roles[index].SkillGrants[grantIndex]
			grant.SkillID = strings.TrimSpace(grant.SkillID)
			grant.SkillVersion = strings.TrimSpace(grant.SkillVersion)
			grant.CatalogID = strings.TrimSpace(grant.CatalogID)
			if grant.RuntimeIdentity != nil {
				identity := grant.RuntimeIdentity.Normalized()
				grant.RuntimeIdentity = &identity
			}
			grant.AllowedActions = normalizedStrings(grant.AllowedActions)
		}
		sort.Slice(candidate.Roles[index].SkillGrants, func(left, right int) bool {
			leftGrant, rightGrant := candidate.Roles[index].SkillGrants[left], candidate.Roles[index].SkillGrants[right]
			return leftGrant.ExactIdentity().Key() < rightGrant.ExactIdentity().Key()
		})
	}
	candidate.OperatingPrinciples = normalizedStrings(candidate.OperatingPrinciples)
	candidate.Approvals.ApproverRoleIDs = normalizedStrings(candidate.Approvals.ApproverRoleIDs)
	candidate.Approvals.ApproverPrincipals = normalizedStrings(candidate.Approvals.ApproverPrincipals)
	candidate.Amendments.AllowedFields = normalizedStrings(candidate.Amendments.AllowedFields)
	candidate.Amendments.ApproverPrincipals = normalizedStrings(candidate.Amendments.ApproverPrincipals)
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	candidate.Digest = "sha256:" + hex.EncodeToString(digest[:])
	candidate.CreatedAt = createdAt
	return candidate, nil
}

func validateNarrowing(definition *Definition, deployment *Deployment) error {
	if deployment.Restrictions.MaximumRisk != "" && riskRank(deployment.Restrictions.MaximumRisk) > riskRank(definition.Approvals.MaximumRisk) {
		return errors.New("team deployment risk cannot widen definition authority")
	}
	if deployment.Restrictions.MaximumConcurrency > 0 && definition.Delegation.MaximumConcurrent > 0 &&
		deployment.Restrictions.MaximumConcurrency > definition.Delegation.MaximumConcurrent {
		return errors.New("team deployment concurrency cannot widen definition authority")
	}
	return nil
}

func riskRank(risk capability.RiskLevel) int {
	switch risk {
	case capability.RiskLevelRead:
		return 0
	case capability.RiskLevelWrite:
		return 1
	case capability.RiskLevelExternal:
		return 2
	case capability.RiskLevelProduction:
		return 3
	case capability.RiskLevelDestructive:
		return 4
	default:
		return -1
	}
}

func normalizedStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func stringSubset(values, allowed []string) bool {
	set := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		set[value] = true
	}
	for _, value := range values {
		if !set[value] {
			return false
		}
	}
	return true
}

func teamDefinitionChanges(base, candidate *Definition) []DefinitionFieldChange {
	basePayload, _ := json.Marshal(base)
	candidatePayload, _ := json.Marshal(candidate)
	var baseMap, candidateMap map[string]interface{}
	_ = json.Unmarshal(basePayload, &baseMap)
	_ = json.Unmarshal(candidatePayload, &candidateMap)
	for _, field := range []string{"version", "digest", "createdAt", "provenance"} {
		delete(baseMap, field)
		delete(candidateMap, field)
	}
	fieldSet := make(map[string]bool, len(baseMap)+len(candidateMap))
	for field := range baseMap {
		fieldSet[field] = true
	}
	for field := range candidateMap {
		fieldSet[field] = true
	}
	fields := make([]string, 0, len(fieldSet))
	for field := range fieldSet {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	changes := make([]DefinitionFieldChange, 0)
	for _, field := range fields {
		before, _ := json.Marshal(baseMap[field])
		after, _ := json.Marshal(candidateMap[field])
		if string(before) == string(after) {
			continue
		}
		beforeDigest, afterDigest := sha256.Sum256(before), sha256.Sum256(after)
		changes = append(changes, DefinitionFieldChange{Field: field, BeforeDigest: hex.EncodeToString(beforeDigest[:]), AfterDigest: hex.EncodeToString(afterDigest[:])})
	}
	return changes
}

func teamDefinitionDigest(value *Definition) string {
	copyDefinition := cloneDefinition(value)
	copyDefinition.Digest = ""
	copyDefinition.CreatedAt = time.Time{}
	encoded, _ := json.Marshal(copyDefinition)
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func definitionKey(id, version string) string { return id + "\x00" + version }

func deploymentKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + "\x00" + scope.ID + "\x00" + id
}

func amendmentKey(scope capability.ScopeReference, id string) string {
	return scope.Kind + ":" + scope.ID + ":" + strings.TrimSpace(id)
}

func cloneDefinition(value *Definition) *Definition {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result Definition
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneDeployment(value *Deployment) *Deployment {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result Deployment
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
