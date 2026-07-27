package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const SkillReferenceUpgradeAPIVersion = "openseal.io/skill-reference-upgrade/v1"

var (
	ErrSkillReferenceUpgradeUnavailable = errors.New("skill reference upgrade service is not configured")
	ErrSkillReferenceUpgradeInvalid     = errors.New("skill reference upgrade is invalid")
	ErrSkillReferenceUpgradeConflict    = errors.New("skill reference upgrade revision conflict")
	ErrSkillReferenceUpgradeApproval    = errors.New("skill reference upgrade requires approval")
)

type SkillReferenceIdentity struct {
	ID             string `json:"id"`
	Version        string `json:"version"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
}

type SkillReferenceObjectiveReference struct {
	Kind   string `json:"kind"`
	ID     string `json:"id,omitempty"`
	Action string `json:"action"`
}

type SkillReferenceObjectiveImpact struct {
	ID               string                             `json:"id"`
	ExpectedRevision int64                              `json:"expectedRevision"`
	References       []SkillReferenceObjectiveReference `json:"references"`
}

type SkillReferenceInitiativeImpact struct {
	ID               string   `json:"id"`
	ExpectedRevision int64    `json:"expectedRevision"`
	MonitorIDs       []string `json:"monitorIds"`
}

type SkillReferenceTeamAuthorityImpact struct {
	DeploymentID      string   `json:"deploymentId"`
	ExpectedRevision  int64    `json:"expectedRevision"`
	DefinitionID      string   `json:"definitionId"`
	DefinitionVersion string   `json:"definitionVersion"`
	AuthorizedRoleIDs []string `json:"authorizedRoleIds"`
}

type SkillReferenceUpgradeFinding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SkillReferenceUpgradePlan is a portable, reviewable projection of every
// durable reference that must move together. Historical Runs and ActionCalls
// are deliberately absent and remain immutable.
type SkillReferenceUpgradePlan struct {
	APIVersion              string                             `json:"apiVersion"`
	Scope                   Scope                              `json:"scope"`
	DeploymentID            string                             `json:"deploymentId"`
	BindingID               string                             `json:"bindingId"`
	ExpectedBindingRevision int64                              `json:"expectedBindingRevision"`
	From                    SkillReferenceIdentity             `json:"from"`
	To                      SkillReferenceIdentity             `json:"to"`
	Objectives              []SkillReferenceObjectiveImpact    `json:"objectives,omitempty"`
	Initiatives             []SkillReferenceInitiativeImpact   `json:"initiatives,omitempty"`
	TeamAuthority           *SkillReferenceTeamAuthorityImpact `json:"teamAuthority,omitempty"`
	Findings                []SkillReferenceUpgradeFinding     `json:"findings,omitempty"`
	ApprovalRequired        bool                               `json:"approvalRequired"`
	Digest                  string                             `json:"digest"`
	GeneratedAt             time.Time                          `json:"generatedAt"`
}

type PlanSkillReferenceUpgradeRequest struct {
	Scope            Scope  `json:"scope"`
	DeploymentID     string `json:"deploymentId"`
	BindingID        string `json:"bindingId"`
	ToVersion        string `json:"toVersion"`
	ToSourceIdentity string `json:"toSourceIdentity,omitempty"`
}

type SkillReferenceUpgradeApproval struct {
	Principal ActivityActor `json:"principal"`
	Reason    string        `json:"reason"`
}

type ApplySkillReferenceUpgradeRequest struct {
	Plan     *SkillReferenceUpgradePlan     `json:"plan"`
	Actor    ActivityActor                  `json:"actor"`
	Reason   string                         `json:"reason"`
	Approval *SkillReferenceUpgradeApproval `json:"approval,omitempty"`
}

type SkillReferenceUpgradeReceipt struct {
	APIVersion      string                           `json:"apiVersion"`
	PlanDigest      string                           `json:"planDigest"`
	Scope           Scope                            `json:"scope"`
	DeploymentID    string                           `json:"deploymentId"`
	BindingID       string                           `json:"bindingId"`
	BindingRevision int64                            `json:"bindingRevision"`
	From            SkillReferenceIdentity           `json:"from"`
	To              SkillReferenceIdentity           `json:"to"`
	Objectives      []SkillReferenceObjectiveImpact  `json:"objectives,omitempty"`
	Initiatives     []SkillReferenceInitiativeImpact `json:"initiatives,omitempty"`
	Actor           ActivityActor                    `json:"actor"`
	Reason          string                           `json:"reason"`
	Approval        *SkillReferenceUpgradeApproval   `json:"approval,omitempty"`
	ActivityIDs     []string                         `json:"activityIds,omitempty"`
	AppliedAt       time.Time                        `json:"appliedAt"`
}

type SkillReferenceObjectiveMutation struct {
	Value            *Objective
	ExpectedRevision int64
	Event            *ActivityEvent
}

type SkillReferenceInitiativeMutation struct {
	Value            *Initiative
	ExpectedRevision int64
	Event            *ActivityEvent
}

// SkillReferenceUpgradeMutation is validated and materialized by the portable
// service. Store implementations must apply all records in one transaction or
// leave every record unchanged.
type SkillReferenceUpgradeMutation struct {
	Plan        *SkillReferenceUpgradePlan
	Binding     *skill.Binding
	Objectives  []SkillReferenceObjectiveMutation
	Initiatives []SkillReferenceInitiativeMutation
	Receipt     *SkillReferenceUpgradeReceipt
}

type SkillReferenceUpgradeStore interface {
	skill.CatalogStore
	PortfolioStore
	InitiativeStore
	ApplySkillReferenceUpgrade(context.Context, *SkillReferenceUpgradeMutation) error
}

type SkillReferenceUpgradeTeamAuthority interface {
	GetDeployment(context.Context, capability.ScopeReference, string) (*kernelteam.Deployment, error)
	GetDefinition(context.Context, string, string) (*kernelteam.Definition, error)
}

func validateSkillReferenceUpgradeMutation(mutation *SkillReferenceUpgradeMutation) error {
	if mutation == nil || mutation.Plan == nil || mutation.Binding == nil || mutation.Receipt == nil ||
		mutation.Plan.Digest == "" || mutation.Receipt.PlanDigest != mutation.Plan.Digest ||
		mutation.Binding.ID != mutation.Plan.BindingID ||
		mutation.Binding.DeploymentID != mutation.Plan.DeploymentID ||
		mutation.Binding.Revision != mutation.Plan.ExpectedBindingRevision+1 ||
		len(mutation.Objectives) != len(mutation.Plan.Objectives) ||
		len(mutation.Initiatives) != len(mutation.Plan.Initiatives) {
		return ErrSkillReferenceUpgradeInvalid
	}
	objectiveRevisions := make(map[string]int64, len(mutation.Plan.Objectives))
	for _, impact := range mutation.Plan.Objectives {
		objectiveRevisions[impact.ID] = impact.ExpectedRevision
	}
	for _, item := range mutation.Objectives {
		if item.Value == nil || item.Event == nil || item.ExpectedRevision != objectiveRevisions[item.Value.ID] ||
			item.Value.Revision != item.ExpectedRevision+1 || item.Value.Scope != mutation.Plan.Scope {
			return ErrSkillReferenceUpgradeInvalid
		}
		if err := item.Value.Validate(); err != nil {
			return fmt.Errorf("%w: objective %s: %v", ErrSkillReferenceUpgradeInvalid, item.Value.ID, err)
		}
		if err := item.Event.Validate(); err != nil {
			return fmt.Errorf("%w: objective activity: %v", ErrSkillReferenceUpgradeInvalid, err)
		}
	}
	initiativeRevisions := make(map[string]int64, len(mutation.Plan.Initiatives))
	for _, impact := range mutation.Plan.Initiatives {
		initiativeRevisions[impact.ID] = impact.ExpectedRevision
	}
	for _, item := range mutation.Initiatives {
		if item.Value == nil || item.Event == nil || item.ExpectedRevision != initiativeRevisions[item.Value.ID] ||
			item.Value.Revision != item.ExpectedRevision+1 || item.Value.Scope != mutation.Plan.Scope {
			return ErrSkillReferenceUpgradeInvalid
		}
		if err := item.Value.Validate(); err != nil {
			return fmt.Errorf("%w: initiative %s: %v", ErrSkillReferenceUpgradeInvalid, item.Value.ID, err)
		}
		if err := item.Event.Validate(); err != nil {
			return fmt.Errorf("%w: initiative activity: %v", ErrSkillReferenceUpgradeInvalid, err)
		}
	}
	return nil
}

type SkillReferenceUpgradeService struct {
	store   SkillReferenceUpgradeStore
	catalog *skill.Catalog
	teams   SkillReferenceUpgradeTeamAuthority
	now     func() time.Time
}

func NewSkillReferenceUpgradeService(store SkillReferenceUpgradeStore, catalog *skill.Catalog, teams ...SkillReferenceUpgradeTeamAuthority) *SkillReferenceUpgradeService {
	service := &SkillReferenceUpgradeService{store: store, catalog: catalog, now: time.Now}
	if len(teams) > 0 {
		service.teams = teams[0]
	}
	return service
}

func (s *SkillReferenceUpgradeService) Plan(ctx context.Context, req PlanSkillReferenceUpgradeRequest) (*SkillReferenceUpgradePlan, error) {
	if s == nil || s.store == nil || s.catalog == nil {
		return nil, ErrSkillReferenceUpgradeUnavailable
	}
	if err := req.Scope.Validate(); err != nil {
		return nil, err
	}
	req.DeploymentID = strings.TrimSpace(req.DeploymentID)
	req.BindingID = strings.TrimSpace(req.BindingID)
	req.ToVersion = strings.TrimSpace(req.ToVersion)
	req.ToSourceIdentity = strings.TrimSpace(req.ToSourceIdentity)
	if req.DeploymentID == "" || req.BindingID == "" || req.ToVersion == "" {
		return nil, fmt.Errorf("%w: deployment, binding, and target version are required", ErrSkillReferenceUpgradeInvalid)
	}
	scope := skill.ScopeReference{Kind: req.Scope.Kind, ID: req.Scope.ID}
	bindings, err := s.store.ListSkillBindings(ctx, scope, req.DeploymentID)
	if err != nil {
		return nil, err
	}
	var current *skill.Binding
	for _, candidate := range bindings {
		if candidate != nil && candidate.ID == req.BindingID {
			current = cloneUpgradeBinding(candidate)
			break
		}
	}
	if current == nil {
		return nil, skill.ErrBindingNotFound
	}
	if current.Disabled {
		return nil, fmt.Errorf("%w: disabled binding cannot be upgraded", ErrSkillReferenceUpgradeInvalid)
	}
	if current.SkillVersion == req.ToVersion && current.SourceIdentity == req.ToSourceIdentity {
		return nil, fmt.Errorf("%w: target identity is already active", ErrSkillReferenceUpgradeInvalid)
	}
	target, err := exactUpgradeDefinition(ctx, s.catalog, current.SkillID, req.ToVersion, req.ToSourceIdentity)
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, fmt.Errorf("%w: exact target Skill definition is not installed", ErrSkillReferenceUpgradeInvalid)
	}
	targetSource := skill.DefinitionSourceIdentity(target)
	for _, candidate := range bindings {
		if candidate == nil || candidate.Disabled || candidate.ID == current.ID || candidate.SkillID != current.SkillID {
			continue
		}
		if candidate.SkillVersion == current.SkillVersion || candidate.SkillVersion == target.Version {
			return nil, fmt.Errorf("%w: durable references are ambiguous across bindings %s and %s", ErrSkillReferenceUpgradeInvalid, current.ID, candidate.ID)
		}
	}
	candidateBinding := cloneUpgradeBinding(current)
	candidateBinding.SkillVersion = target.Version
	candidateBinding.SourceIdentity = targetSource
	if err := s.catalog.ValidateBindingCandidate(ctx, candidateBinding); err != nil {
		return nil, fmt.Errorf("%w: target binding contract: %v", ErrSkillReferenceUpgradeInvalid, err)
	}
	previous, err := exactUpgradeDefinition(ctx, s.catalog, current.SkillID, current.SkillVersion, current.SourceIdentity)
	if err != nil || previous == nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: current Skill definition is not installed", ErrSkillReferenceUpgradeInvalid)
	}

	objectives, err := s.store.ListObjectives(ctx, ObjectiveFilter{Scope: req.Scope})
	if err != nil {
		return nil, err
	}
	objectiveImpacts := make([]SkillReferenceObjectiveImpact, 0)
	objectiveIDs := make(map[string]bool)
	referencedActions := make(map[string]bool)
	for _, objective := range objectives {
		references, referenceErr := objectiveSkillReferences(objective, req.DeploymentID, current)
		if referenceErr != nil {
			return nil, fmt.Errorf("%w: objective %s: %v", ErrSkillReferenceUpgradeInvalid, objective.ID, referenceErr)
		}
		if len(references) == 0 {
			continue
		}
		impactReferences := make([]SkillReferenceObjectiveReference, 0, len(references))
		for _, reference := range references {
			if err := s.catalog.ValidateDefinitionInput(ctx, target.ID, target.Version, targetSource, reference.Invocation.Action, reference.Invocation.Inputs); err != nil {
				return nil, fmt.Errorf("%w: objective %s action %s: %v", ErrSkillReferenceUpgradeInvalid, objective.ID, reference.Invocation.Action, err)
			}
			impactReferences = append(impactReferences, SkillReferenceObjectiveReference{
				Kind: reference.Kind, ID: reference.ID, Action: reference.Invocation.Action,
			})
			referencedActions[reference.Invocation.Action] = true
		}
		objectiveImpacts = append(objectiveImpacts, SkillReferenceObjectiveImpact{
			ID: objective.ID, ExpectedRevision: objective.Revision, References: impactReferences,
		})
		objectiveIDs[objective.ID] = true
	}
	initiatives, err := s.store.ListInitiatives(ctx, InitiativeFilter{Scope: req.Scope})
	if err != nil {
		return nil, err
	}
	initiativeImpacts := make([]SkillReferenceInitiativeImpact, 0)
	for _, initiative := range initiatives {
		monitorIDs := make([]string, 0)
		for _, monitor := range initiative.SourceMonitors {
			if !referenceOwnedOrAssigned(initiative.Owner, monitor.AssignedAgentID, req.DeploymentID) ||
				monitor.SkillID != current.SkillID || monitor.SkillVersion != current.SkillVersion {
				continue
			}
			if !objectiveIDs[monitor.ObjectiveID] {
				return nil, fmt.Errorf("%w: Initiative %s monitor %s has no matching scheduled Objective reference", ErrSkillReferenceUpgradeInvalid, initiative.ID, monitor.ID)
			}
			if _, ok := target.Actions[monitor.Action]; !ok {
				return nil, fmt.Errorf("%w: Initiative %s monitor %s action %s is absent from target", ErrSkillReferenceUpgradeInvalid, initiative.ID, monitor.ID, monitor.Action)
			}
			monitorIDs = append(monitorIDs, monitor.ID)
			referencedActions[monitor.Action] = true
		}
		if len(monitorIDs) > 0 {
			sort.Strings(monitorIDs)
			initiativeImpacts = append(initiativeImpacts, SkillReferenceInitiativeImpact{
				ID: initiative.ID, ExpectedRevision: initiative.Revision, MonitorIDs: monitorIDs,
			})
		}
	}
	sort.Slice(objectiveImpacts, func(i, j int) bool { return objectiveImpacts[i].ID < objectiveImpacts[j].ID })
	sort.Slice(initiativeImpacts, func(i, j int) bool { return initiativeImpacts[i].ID < initiativeImpacts[j].ID })

	findings := compareUpgradeContracts(previous, target, current.AllowedActions, referencedActions)
	teamAuthority, err := s.planTeamSkillReferenceAuthority(ctx, req.Scope, req.DeploymentID, current, target)
	if err != nil {
		return nil, err
	}
	plan := &SkillReferenceUpgradePlan{
		APIVersion: SkillReferenceUpgradeAPIVersion, Scope: req.Scope, DeploymentID: req.DeploymentID,
		BindingID: current.ID, ExpectedBindingRevision: current.Revision,
		From:       SkillReferenceIdentity{ID: current.SkillID, Version: current.SkillVersion, SourceIdentity: current.SourceIdentity},
		To:         SkillReferenceIdentity{ID: target.ID, Version: target.Version, SourceIdentity: targetSource},
		Objectives: objectiveImpacts, Initiatives: initiativeImpacts, TeamAuthority: teamAuthority, Findings: findings,
		ApprovalRequired: len(findings) > 0, GeneratedAt: s.now().UTC(),
	}
	plan.Digest, err = skillReferenceUpgradeDigest(plan)
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (s *SkillReferenceUpgradeService) Apply(ctx context.Context, req ApplySkillReferenceUpgradeRequest) (*SkillReferenceUpgradeReceipt, error) {
	if req.Plan == nil || strings.TrimSpace(req.Plan.Digest) == "" {
		return nil, fmt.Errorf("%w: reviewed plan is required", ErrSkillReferenceUpgradeInvalid)
	}
	if strings.TrimSpace(req.Actor.Type) == "" || strings.TrimSpace(req.Actor.ID) == "" || strings.TrimSpace(req.Reason) == "" {
		return nil, fmt.Errorf("%w: actor and reason are required", ErrSkillReferenceUpgradeInvalid)
	}
	current, err := s.Plan(ctx, PlanSkillReferenceUpgradeRequest{
		Scope: req.Plan.Scope, DeploymentID: req.Plan.DeploymentID, BindingID: req.Plan.BindingID,
		ToVersion: req.Plan.To.Version, ToSourceIdentity: req.Plan.To.SourceIdentity,
	})
	if err != nil {
		return nil, err
	}
	if current.Digest != req.Plan.Digest {
		return nil, ErrSkillReferenceUpgradeConflict
	}
	if current.ApprovalRequired {
		if req.Approval == nil || strings.TrimSpace(req.Approval.Principal.Type) == "" ||
			strings.TrimSpace(req.Approval.Principal.ID) == "" || strings.TrimSpace(req.Approval.Reason) == "" {
			return nil, ErrSkillReferenceUpgradeApproval
		}
	}
	now := s.now().UTC()
	scope := skill.ScopeReference{Kind: current.Scope.Kind, ID: current.Scope.ID}
	bindings, err := s.store.ListSkillBindings(ctx, scope, current.DeploymentID)
	if err != nil {
		return nil, err
	}
	var binding *skill.Binding
	for _, candidate := range bindings {
		if candidate != nil && candidate.ID == current.BindingID {
			binding = cloneUpgradeBinding(candidate)
			break
		}
	}
	if binding == nil || binding.Revision != current.ExpectedBindingRevision {
		return nil, ErrSkillReferenceUpgradeConflict
	}
	binding.SkillVersion = current.To.Version
	binding.SourceIdentity = current.To.SourceIdentity
	binding.Revision++
	// Bindings created by an atomic workforce apply predate the management
	// lifecycle until their first explicit mutation. Establish the lifecycle
	// clock at that boundary instead of persisting an updated entry alongside a
	// zero creation timestamp, which would make the binding unreadable.
	if binding.CreatedAt.IsZero() {
		binding.CreatedAt = now
	}
	binding.UpdatedAt = now
	binding.Lifecycle = append(binding.Lifecycle, capability.BindingLifecycleEntry{
		Revision: binding.Revision, Action: capability.BindingLifecycleUpdated,
		Actor: capability.BindingActor{Type: req.Actor.Type, ID: req.Actor.ID}, Reason: req.Reason, At: now,
	})

	objectiveCandidates := make([]SkillReferenceObjectiveMutation, 0, len(current.Objectives))
	activityIDs := make([]string, 0, len(current.Objectives)+len(current.Initiatives))
	for _, impact := range current.Objectives {
		objective, loadErr := s.store.GetObjective(ctx, current.Scope, impact.ID)
		if loadErr != nil || objective == nil || objective.Revision != impact.ExpectedRevision {
			return nil, ErrSkillReferenceUpgradeConflict
		}
		next := cloneObjective(objective)
		if replaceErr := replaceObjectiveSkillReferences(next, current.DeploymentID, current.From, current.To, impact.References); replaceErr != nil {
			return nil, fmt.Errorf("%w: objective %s: %v", ErrSkillReferenceUpgradeConflict, impact.ID, replaceErr)
		}
		next.Revision++
		next.UpdatedAt = now
		event := objectiveActivityEvent(next, req.Actor, ActivityVisibilityScope, "objective.skill_reference_upgraded",
			fmt.Sprintf("Objective Skill reference upgraded from %s to %s", current.From.Version, current.To.Version), now,
			skillReferenceUpgradeActivityPayload(current, req.Reason))
		activityIDs = append(activityIDs, event.ID)
		objectiveCandidates = append(objectiveCandidates, SkillReferenceObjectiveMutation{
			Value: next, ExpectedRevision: impact.ExpectedRevision, Event: event,
		})
	}
	initiativeCandidates := make([]SkillReferenceInitiativeMutation, 0, len(current.Initiatives))
	for _, impact := range current.Initiatives {
		initiative, loadErr := s.store.GetInitiative(ctx, current.Scope, impact.ID)
		if loadErr != nil || initiative == nil || initiative.Revision != impact.ExpectedRevision {
			return nil, ErrSkillReferenceUpgradeConflict
		}
		next := cloneInitiative(initiative)
		selected := make(map[string]bool, len(impact.MonitorIDs))
		for _, id := range impact.MonitorIDs {
			selected[id] = true
		}
		for index := range next.SourceMonitors {
			if selected[next.SourceMonitors[index].ID] {
				next.SourceMonitors[index].SkillVersion = current.To.Version
			}
		}
		next.Revision++
		next.UpdatedAt = now
		event := initiativeEvent(next, "initiative.skill_reference_upgraded", req.Actor, ActivityVisibilityScope,
			fmt.Sprintf("Initiative Skill reference upgraded from %s to %s", current.From.Version, current.To.Version))
		event.Payload = skillReferenceUpgradeActivityPayload(current, req.Reason)
		activityIDs = append(activityIDs, event.ID)
		initiativeCandidates = append(initiativeCandidates, SkillReferenceInitiativeMutation{
			Value: next, ExpectedRevision: impact.ExpectedRevision, Event: event,
		})
	}
	receipt := &SkillReferenceUpgradeReceipt{
		APIVersion: SkillReferenceUpgradeAPIVersion, PlanDigest: current.Digest, Scope: current.Scope,
		DeploymentID: current.DeploymentID, BindingID: current.BindingID, BindingRevision: binding.Revision,
		From: current.From, To: current.To, Objectives: current.Objectives, Initiatives: current.Initiatives,
		Actor: req.Actor, Reason: req.Reason, Approval: req.Approval, ActivityIDs: activityIDs, AppliedAt: now,
	}
	if err := s.store.ApplySkillReferenceUpgrade(ctx, &SkillReferenceUpgradeMutation{
		Plan: current, Binding: binding, Objectives: objectiveCandidates, Initiatives: initiativeCandidates, Receipt: receipt,
	}); err != nil {
		return nil, err
	}
	return receipt, nil
}

type objectiveSkillReference struct {
	Kind       string
	ID         string
	Invocation *ObjectiveCapabilityInvocation
}

func objectiveSkillReferences(objective *Objective, deploymentID string, binding *skill.Binding) ([]objectiveSkillReference, error) {
	if objective == nil || binding == nil {
		return nil, nil
	}
	result := make([]objectiveSkillReference, 0)
	if objective.Cadence != nil && referenceOwnedOrAssigned(objective.Owner, objective.Cadence.AssignedAgentID, deploymentID) &&
		objective.Cadence.RunTemplate != nil && matchesObjectiveSkillReference(objective.Cadence.RunTemplate.Capability, binding.SkillID, binding.SkillVersion) {
		result = append(result, objectiveSkillReference{Kind: "cadence", Invocation: objective.Cadence.RunTemplate.Capability})
	}
	rules, err := DecodeObjectiveEventRules(objective.EventRules)
	if err != nil {
		return nil, err
	}
	if rules == nil {
		return result, nil
	}
	for index := range rules.Rules {
		rule := &rules.Rules[index]
		if referenceOwnedOrAssigned(objective.Owner, rule.AssignedAgentID, deploymentID) && rule.RunTemplate != nil &&
			matchesObjectiveSkillReference(rule.RunTemplate.Capability, binding.SkillID, binding.SkillVersion) {
			result = append(result, objectiveSkillReference{Kind: "event_rule", ID: rule.ID, Invocation: rule.RunTemplate.Capability})
		}
	}
	return result, nil
}

func matchesObjectiveSkillReference(invocation *ObjectiveCapabilityInvocation, skillID, version string) bool {
	return invocation != nil && invocation.SkillID == skillID && invocation.SkillVersion == version
}

func referenceOwnedOrAssigned(owner ObjectiveOwner, assignedID, deploymentID string) bool {
	return owner.ID == deploymentID || assignedID == deploymentID
}

func replaceObjectiveSkillReferences(objective *Objective, deploymentID string, from, to SkillReferenceIdentity, expected []SkillReferenceObjectiveReference) error {
	binding := &skill.Binding{SkillID: from.ID, SkillVersion: from.Version}
	current, err := objectiveSkillReferences(objective, deploymentID, binding)
	if err != nil {
		return err
	}
	projected := make([]SkillReferenceObjectiveReference, 0, len(current))
	for _, reference := range current {
		projected = append(projected, SkillReferenceObjectiveReference{
			Kind: reference.Kind, ID: reference.ID, Action: reference.Invocation.Action,
		})
	}
	if !reflect.DeepEqual(projected, expected) {
		return errors.New("reviewed references changed")
	}
	if objective.Cadence != nil && referenceOwnedOrAssigned(objective.Owner, objective.Cadence.AssignedAgentID, deploymentID) &&
		objective.Cadence.RunTemplate != nil &&
		matchesObjectiveSkillReference(objective.Cadence.RunTemplate.Capability, from.ID, from.Version) {
		objective.Cadence.RunTemplate.Capability.SkillID = to.ID
		objective.Cadence.RunTemplate.Capability.SkillVersion = to.Version
	}
	rules, err := DecodeObjectiveEventRules(objective.EventRules)
	if err != nil || rules == nil {
		return err
	}
	changed := false
	for index := range rules.Rules {
		rule := &rules.Rules[index]
		if referenceOwnedOrAssigned(objective.Owner, rule.AssignedAgentID, deploymentID) && rule.RunTemplate != nil &&
			matchesObjectiveSkillReference(rule.RunTemplate.Capability, from.ID, from.Version) {
			rule.RunTemplate.Capability.SkillID = to.ID
			rule.RunTemplate.Capability.SkillVersion = to.Version
			changed = true
		}
	}
	if changed {
		encoded, encodeErr := json.Marshal(rules)
		if encodeErr != nil {
			return encodeErr
		}
		if err := json.Unmarshal(encoded, &objective.EventRules); err != nil {
			return err
		}
	}
	return nil
}

func exactUpgradeDefinition(ctx context.Context, catalog *skill.Catalog, id, version, sourceIdentity string) (*skill.Definition, error) {
	if strings.TrimSpace(sourceIdentity) == "" {
		return catalog.GetDefinition(ctx, id, version)
	}
	return catalog.GetDefinitionVariant(ctx, id, version, sourceIdentity)
}

func skillReferenceUpgradeActivityPayload(plan *SkillReferenceUpgradePlan, reason string) map[string]interface{} {
	return map[string]interface{}{
		"planDigest":   plan.Digest,
		"bindingId":    plan.BindingID,
		"deploymentId": plan.DeploymentID,
		"from":         map[string]interface{}{"id": plan.From.ID, "version": plan.From.Version, "sourceIdentity": plan.From.SourceIdentity},
		"to":           map[string]interface{}{"id": plan.To.ID, "version": plan.To.Version, "sourceIdentity": plan.To.SourceIdentity},
		"reason":       reason,
	}
}

func (s *SkillReferenceUpgradeService) planTeamSkillReferenceAuthority(
	ctx context.Context,
	scope Scope,
	deploymentID string,
	current *skill.Binding,
	target *skill.Definition,
) (*SkillReferenceTeamAuthorityImpact, error) {
	if s == nil || s.teams == nil {
		return nil, nil
	}
	teamScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	deployment, err := s.teams.GetDeployment(ctx, teamScope, deploymentID)
	if errors.Is(err, kernelteam.ErrDeploymentNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if deployment == nil || deployment.Scope != teamScope || deployment.Status == kernelteam.DeploymentArchived {
		return nil, fmt.Errorf("%w: Team deployment is unavailable for a Skill upgrade", ErrSkillReferenceUpgradeInvalid)
	}
	definition, err := s.teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition == nil {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("%w: active Team definition is unavailable", ErrSkillReferenceUpgradeInvalid)
	}
	fromIdentity := capability.NewSkillIdentity(current.SkillID, current.SkillVersion, current.SourceIdentity)
	toIdentity := capability.NewSkillIdentity(target.ID, target.Version, skill.DefinitionSourceIdentity(target))
	authorizedRoles := make([]string, 0)
	for _, role := range definition.Roles {
		var previous, next *kernelteam.RoleSkillGrant
		for index := range role.SkillGrants {
			grant := &role.SkillGrants[index]
			switch {
			case grant.ExactIdentity().Equal(fromIdentity):
				previous = grant
			case grant.ExactIdentity().Equal(toIdentity):
				next = grant
			}
		}
		if next != nil {
			authorizedRoles = append(authorizedRoles, role.ID)
		}
		if previous == nil {
			continue
		}
		if next == nil {
			return nil, fmt.Errorf("%w: Team role %s must grant target Skill %s@%s before upgrading its binding",
				ErrSkillReferenceUpgradeInvalid, role.ID, target.ID, target.Version)
		}
		if previous.EnablePrompt && !next.EnablePrompt {
			return nil, fmt.Errorf("%w: Team role %s target grant removes prompt authority", ErrSkillReferenceUpgradeInvalid, role.ID)
		}
		for _, actionName := range previous.AllowedActions {
			action, ok := target.Actions[actionName]
			if !ok || !teamSkillGrantAllowsAction(*next, actionName, action.Risk) {
				return nil, fmt.Errorf("%w: Team role %s target grant does not preserve action %s",
					ErrSkillReferenceUpgradeInvalid, role.ID, actionName)
			}
		}
	}
	if len(authorizedRoles) == 0 {
		return nil, fmt.Errorf("%w: active Team definition must grant target Skill %s@%s before upgrading its binding",
			ErrSkillReferenceUpgradeInvalid, target.ID, target.Version)
	}
	sort.Strings(authorizedRoles)
	return &SkillReferenceTeamAuthorityImpact{
		DeploymentID: deployment.ID, ExpectedRevision: deployment.Revision,
		DefinitionID: deployment.DefinitionID, DefinitionVersion: deployment.ActiveVersion,
		AuthorizedRoleIDs: authorizedRoles,
	}, nil
}

func compareUpgradeContracts(previous, target *skill.Definition, bindingActions []string, referenced map[string]bool) []SkillReferenceUpgradeFinding {
	actions := make(map[string]bool, len(bindingActions)+len(referenced))
	for _, action := range bindingActions {
		actions[action] = true
	}
	for action := range referenced {
		actions[action] = true
	}
	names := make([]string, 0, len(actions))
	for action := range actions {
		names = append(names, action)
	}
	sort.Strings(names)
	findings := make([]SkillReferenceUpgradeFinding, 0)
	for _, name := range names {
		before, beforeOK := previous.Actions[name]
		after, afterOK := target.Actions[name]
		if !beforeOK || !afterOK {
			continue
		}
		if upgradeRiskRank(after.Risk) > upgradeRiskRank(before.Risk) {
			findings = append(findings, SkillReferenceUpgradeFinding{
				Code: "risk_increased", Message: fmt.Sprintf("%s risk changes from %s to %s", name, before.Risk, after.Risk),
			})
		}
		if !reflect.DeepEqual(before.InputSchema, after.InputSchema) || !reflect.DeepEqual(before.OutputSchema, after.OutputSchema) ||
			!reflect.DeepEqual(before.Credentials, after.Credentials) || before.SideEffect != after.SideEffect ||
			before.Idempotency != after.Idempotency {
			findings = append(findings, SkillReferenceUpgradeFinding{
				Code: "action_contract_changed", Message: fmt.Sprintf("%s action contract changed", name),
			})
		}
	}
	return findings
}

func upgradeRiskRank(value capability.RiskLevel) int {
	switch value {
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

func skillReferenceUpgradeDigest(plan *SkillReferenceUpgradePlan) (string, error) {
	copy := *plan
	copy.Digest = ""
	copy.GeneratedAt = time.Time{}
	payload, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func cloneUpgradeBinding(value *skill.Binding) *skill.Binding {
	if value == nil {
		return nil
	}
	payload, _ := json.Marshal(value)
	var copy skill.Binding
	_ = json.Unmarshal(payload, &copy)
	return &copy
}
