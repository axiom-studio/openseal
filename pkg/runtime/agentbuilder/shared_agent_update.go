package agentbuilder

import (
	"context"
	"errors"
	"maps"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// sharedAgentUpdatePlan is a resolved update of one copy.
type sharedAgentUpdatePlan struct {
	preview    *SharedAgentUpdatePreview
	deployment *kernelagent.AgentDeployment
	definition *kernelagent.AgentDefinition
	candidate  *kernelagent.AgentDefinition
	grants     []resolvedAgentSkillGrant
	account    map[string]map[string]skill.CredentialRequirement
	removals   []string
}

// PreviewSharedAgentUpdate lists what moving the copy from From to To
// changes: each differing profile field (the instructions with their full
// text), skills added, removed or changed, the fields the adopter edited
// that To replaces, new account slots, skills not available here and the
// copy's ceiling before and after, recomputed over the resulting skills.
func (i *AgentInstaller) PreviewSharedAgentUpdate(ctx context.Context, request SharedAgentUpdateRequest) (*SharedAgentUpdatePreview, error) {
	plan, err := i.planSharedAgentUpdate(ctx, request)
	if err != nil {
		return nil, err
	}
	return plan.preview, nil
}

// ApplySharedAgentUpdate writes To's value into every field where From and
// To differ, approved by the acting person's Update: a new definition version
// (provenance share@version, ceiling recomputed) activated by that person,
// added and changed skills granted, removed ones disabled. A replay with the
// same IdempotencyKey converges on the same version.
func (i *AgentInstaller) ApplySharedAgentUpdate(ctx context.Context, request SharedAgentUpdateRequest) (*SharedAgentUpdated, error) {
	request.ActingUserID = strings.TrimSpace(request.ActingUserID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	if request.ActingUserID == "" {
		return nil, errors.New("a shared agent update runs only after a person approves it")
	}
	if request.IdempotencyKey == "" {
		return nil, errors.New("a shared agent update needs an idempotency key")
	}
	if strings.TrimSpace(request.Source.ShareID) == "" || request.Source.Version < 1 {
		return nil, errors.New("a shared agent update needs its share and version")
	}
	scope := request.Scope
	reference := models.ScopeReference(scope)
	actor := skill.BindingActor{Type: "user", ID: request.ActingUserID}
	callRef := sharedAgentUpdatePrefix + request.IdempotencyKey
	current, err := i.agents.GetDeployment(ctx, reference, strings.TrimSpace(request.DeploymentID))
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(current.ActiveVersion, agentBehaviorActionCandidateVersion("", callRef)) {
		return i.replaySharedAgentUpdate(ctx, request, current, actor)
	}
	plan, err := i.planSharedAgentUpdate(ctx, request)
	if err != nil {
		return nil, err
	}
	if request.ExpectedDeploymentRevision < 1 || plan.deployment.Revision != request.ExpectedDeploymentRevision {
		return nil, kernelagent.ErrRevisionConflict
	}
	if len(plan.preview.Unavailable) > 0 {
		first := plan.preview.Unavailable[0]
		return nil, SharedAgentSkillUnavailableError{SkillID: first.SkillID, Name: first.Name}
	}
	if request.ExpectedMaximumRisk != plan.preview.Risk.To {
		return nil, ErrSharedAgentReviewChanged
	}
	candidate := plan.candidate
	candidate.Version = agentBehaviorActionCandidateVersion(plan.definition.Version, callRef)
	candidate.Provenance = workforce.DefinitionProvenance{
		Source: SharedAgentProvenanceSource, Reference: sharedAgentReference(request.Source), CreatedBy: request.ActingUserID,
	}
	deployment, activation, err := i.activateApprovedAgentVersion(ctx, reference, plan.deployment, candidate, request.ActingUserID)
	if err != nil {
		return nil, err
	}
	return i.finishSharedAgentUpdate(ctx, scope, deployment, activation, plan.grants, plan.account, plan.removals, actor, request, callRef)
}

// replaySharedAgentUpdate finishes an update whose version is already
// active: the bindings are granted and disabled again (both idempotent).
func (i *AgentInstaller) replaySharedAgentUpdate(ctx context.Context, request SharedAgentUpdateRequest, deployment *kernelagent.AgentDeployment, actor skill.BindingActor) (*SharedAgentUpdated, error) {
	reference := models.ScopeReference(request.Scope)
	activation, err := i.agentActivationTo(ctx, reference, deployment.ID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	diff, err := i.resolveSharedAgentSkillDiff(ctx, request.Scope, deployment.ID, request.From, request.To)
	if err != nil {
		return nil, err
	}
	if diff.unavailable != nil {
		return nil, *diff.unavailable
	}
	return i.finishSharedAgentUpdate(ctx, request.Scope, deployment, activation, diff.grants, diff.account, diff.removals, actor, request, sharedAgentUpdatePrefix+request.IdempotencyKey)
}

func (i *AgentInstaller) finishSharedAgentUpdate(
	ctx context.Context,
	scope models.Scope,
	deployment *kernelagent.AgentDeployment,
	activation *kernelagent.DefinitionActivation,
	grants []resolvedAgentSkillGrant,
	account map[string]map[string]skill.CredentialRequirement,
	removals []string,
	actor skill.BindingActor,
	request SharedAgentUpdateRequest,
	callRef string,
) (*SharedAgentUpdated, error) {
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	// A changed account skill keeps the adopter's own links: only slots the
	// copy has no reference for get the person ref.
	for index, grant := range grants {
		requirements, ok := account[grant.grant.SkillID]
		if !ok {
			continue
		}
		current, err := i.catalog.GetBinding(ctx, skillScope, deployment.ID, agentBuilderBindingID(deployment.ID, grant.grant.SkillID))
		if err != nil && !errors.Is(err, skill.ErrBindingNotFound) {
			return nil, err
		}
		if current == nil || current.Disabled {
			continue
		}
		credentials := maps.Clone(grant.credentials)
		for slot, requirement := range requirements {
			if existing, ok := current.Credentials[slot]; ok && existing.Kind == requirement.Kind && strings.TrimSpace(existing.ID) != "" {
				credentials[slot] = existing
			}
		}
		grants[index].credentials = credentials
	}
	for _, grant := range grants {
		if _, err := i.grantAgentSkill(ctx, scope, deployment.ID, grant.grant, grant.credentials, actor, sharedAgentUpdateReason); err != nil {
			return nil, err
		}
	}
	for _, bindingID := range removals {
		if err := i.disableAgentSkill(ctx, skillScope, deployment.ID, bindingID, actor); err != nil {
			return nil, err
		}
	}
	bindings, unconnected, err := i.sharedAgentBindings(ctx, scope, deployment.ID, grants, account)
	if err != nil {
		return nil, err
	}
	definition, err := i.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if i.host != nil {
		i.host.AfterAgentCreated(ctx, AgentCreated{
			Scope: scope, DeploymentID: deployment.ID, DisplayName: agentDeploymentDisplayName(deployment, definition),
			Purpose: definition.Purpose, ActionCallID: callRef, BillingUserID: request.ActingUserID,
			InstallKey: request.IdempotencyKey, Updated: true,
		})
	}
	return &SharedAgentUpdated{
		DefinitionVersion: deployment.ActiveVersion, Activation: activation, Bindings: bindings, Unconnected: unconnected,
	}, nil
}

func (i *AgentInstaller) planSharedAgentUpdate(ctx context.Context, request SharedAgentUpdateRequest) (*sharedAgentUpdatePlan, error) {
	scope := request.Scope
	reference := models.ScopeReference(scope)
	deployment, err := i.agents.GetDeployment(ctx, reference, strings.TrimSpace(request.DeploymentID))
	if err != nil {
		return nil, err
	}
	if deployment.RolloutStatus == kernelagent.RolloutRetired {
		return nil, kernelagent.ErrDeploymentNotFound
	}
	definition, err := i.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if definition.Provenance.Source != SharedAgentProvenanceSource {
		return nil, ErrSharedAgentNotCopy
	}
	from, to := sharedAgentProfileArguments(request.From.Profile), sharedAgentProfileArguments(request.To.Profile)
	if err := models.ValidateAgentProfileText(to); err != nil {
		return nil, err
	}
	preview := &SharedAgentUpdatePreview{
		Changes: []SharedAgentChange{}, ReplacesYourEdit: []string{}, NewSlots: []SharedAgentPlannedSlot{},
		Unavailable: []SharedAgentPlannedSkill{}, DeploymentRevision: deployment.Revision,
	}
	candidate := cloneAgentDefinitionForAction(definition)
	current := agentProfileFromDefinition(definition)
	currentName := agentDeploymentDisplayName(deployment, definition)
	type profileField struct {
		field, label  string
		before, after string
		changed       bool
		edited        bool
		apply         func()
		withText      bool
	}
	fields := []profileField{
		{
			field: SharedAgentFieldName, label: "Name",
			changed: from.DisplayName != to.DisplayName, edited: !sharedAgentNameMatches(currentName, from.DisplayName),
		},
		{
			field: SharedAgentFieldPurpose, label: "Purpose",
			changed: from.Purpose != to.Purpose, edited: current.Purpose != from.Purpose,
			apply: func() { candidate.Purpose = to.Purpose },
		},
		{
			field: SharedAgentFieldInstructions, label: "Instructions", before: from.SystemPrompt, after: to.SystemPrompt,
			changed: from.SystemPrompt != to.SystemPrompt, edited: current.SystemPrompt != from.SystemPrompt,
			apply: func() { candidate.SystemPrompt = to.SystemPrompt }, withText: true,
		},
		{
			field: SharedAgentFieldPersonality, label: "Personality",
			changed: from.Personality != to.Personality, edited: current.Personality != from.Personality,
			apply: func() { candidate.Personality = to.Personality },
		},
		{
			field: SharedAgentFieldPrinciples, label: "Principles",
			changed: !equalStrings(from.OperatingPrinciples, to.OperatingPrinciples),
			edited:  !equalStrings(current.OperatingPrinciples, from.OperatingPrinciples),
			apply:   func() { candidate.OperatingPrinciples = append([]string(nil), to.OperatingPrinciples...) },
		},
	}
	for _, field := range fields {
		if !field.changed {
			continue
		}
		change := SharedAgentChange{Field: field.field, Kind: SharedAgentChangeProfile, Label: field.label}
		if field.withText {
			change.Before, change.After = field.before, field.after
		}
		preview.Changes = append(preview.Changes, change)
		if field.edited {
			preview.ReplacesYourEdit = append(preview.ReplacesYourEdit, field.field)
		}
		if field.apply != nil {
			field.apply()
		}
	}
	if from.DisplayName != to.DisplayName {
		name, err := i.sharedAgentName(ctx, scope, to.DisplayName, "", deployment.ID)
		if err != nil {
			return nil, err
		}
		candidate.DisplayName = name
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}

	diff, err := i.resolveSharedAgentSkillDiff(ctx, scope, deployment.ID, request.From, request.To)
	if err != nil {
		return nil, err
	}
	preview.Changes = append(preview.Changes, diff.changes...)
	if diff.replacesYourEdit {
		preview.ReplacesYourEdit = append(preview.ReplacesYourEdit, SharedAgentFieldSkills)
	}
	preview.NewSlots = append(preview.NewSlots, diff.newSlots...)
	preview.Unavailable = append(preview.Unavailable, diff.unavailableSkills...)
	preview.Risk = SharedAgentRiskChange{From: definition.Authority.MaximumRisk, To: diff.ceiling}
	if preview.Risk.From == "" {
		preview.Risk.From = capability.RiskLevelRead
	}
	for index := range preview.Changes {
		change := &preview.Changes[index]
		if change.Kind == SharedAgentChangeSkillAdded && diff.risks[change.Target].RankUnknownHighest() > preview.Risk.From.RankUnknownHighest() {
			change.RiskUp = true
		}
	}
	candidate.Authority.MaximumRisk = diff.ceiling
	return &sharedAgentUpdatePlan{
		preview: preview, deployment: deployment, definition: definition, candidate: candidate,
		grants: diff.grants, account: diff.account, removals: diff.removals,
	}, nil
}

// sharedAgentSkillDiff is the skill side of an update.
type sharedAgentSkillDiff struct {
	changes           []SharedAgentChange
	newSlots          []SharedAgentPlannedSlot
	unavailableSkills []SharedAgentPlannedSkill
	unavailable       *SharedAgentSkillUnavailableError
	grants            []resolvedAgentSkillGrant
	account           map[string]map[string]skill.CredentialRequirement
	removals          []string
	// risks are the catalog risks of added skills by change Target.
	risks   map[string]capability.RiskLevel
	ceiling capability.RiskLevel
	// replacesYourEdit is set when a skill the update grants is one the
	// adopter turned off or changed on the copy.
	replacesYourEdit bool
}

// resolveSharedAgentSkillDiff compares From's and To's skills by id: added
// and changed skills are granted with To's value, removed ones disabled. The
// ceiling is recomputed over the copy's resulting enabled non-bundled
// bindings, each at its catalog risk. A granted skill whose copy binding the
// adopter turned off or changed away from From replaces their edit.
func (i *AgentInstaller) resolveSharedAgentSkillDiff(ctx context.Context, scope models.Scope, deploymentID string, from, to SharedAgentSpec) (*sharedAgentSkillDiff, error) {
	diff := &sharedAgentSkillDiff{
		changes: []SharedAgentChange{}, newSlots: []SharedAgentPlannedSlot{}, unavailableSkills: []SharedAgentPlannedSkill{},
		account: map[string]map[string]skill.CredentialRequirement{}, risks: map[string]capability.RiskLevel{},
	}
	before := sharedAgentSpecSkills(from)
	toSkills, err := i.resolveSharedSkills(ctx, to.Grants, to.AccountSkills)
	if err != nil {
		return nil, err
	}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	bindings, err := i.catalog.ListBindings(ctx, skillScope, deploymentID)
	if err != nil {
		return nil, err
	}
	// The resulting binding set, by binding id, at catalog risk.
	result := map[string]capability.RiskLevel{}
	byID := map[string]*skill.Binding{}
	for _, binding := range bindings {
		if binding != nil {
			byID[binding.ID] = binding
		}
		if binding == nil || binding.Disabled || strings.HasPrefix(binding.ID, "bundled:") {
			continue
		}
		risk := binding.MaximumRisk
		definition, err := sharedSkillDefinition(ctx, i.catalog, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition != nil {
			risk = catalogGrantRisk(definition, binding.AllowedActions)
		}
		result[binding.ID] = risk
	}
	inTo := map[string]bool{}
	for _, shared := range toSkills {
		id := shared.spec.SkillID
		inTo[id] = true
		previous, existed := before[id]
		if existed && sameSharedAgentSkill(previous.skill, shared.spec) && previous.account == shared.account {
			continue
		}
		bindingID := agentBuilderBindingID(deploymentID, id)
		kind, label := SharedAgentChangeSkillAdded, "Adds "+shared.name
		if existed {
			kind, label = SharedAgentChangeSkillChanged, "Changes "+shared.name
			current := byID[bindingID]
			switch {
			case current == nil || current.Disabled:
				label = "Turns " + shared.name + " back on with changes"
				diff.replacesYourEdit = true
			case !sharedAgentBindingMatches(current, previous.skill):
				diff.replacesYourEdit = true
			}
		} else if current := byID[bindingID]; current != nil && !current.Disabled {
			// The adopter gave the copy this skill themselves.
			diff.replacesYourEdit = true
		}
		change := SharedAgentChange{Field: SharedAgentFieldSkills, Kind: kind, Label: label, Target: id}
		if !shared.available {
			diff.changes = append(diff.changes, change)
			planned := SharedAgentPlannedSkill{SkillID: id, Name: shared.name, Reason: SharedAgentSkillUnavailable}
			diff.unavailableSkills = append(diff.unavailableSkills, planned)
			if diff.unavailable == nil {
				diff.unavailable = &SharedAgentSkillUnavailableError{SkillID: id, Name: shared.name}
			}
			continue
		}
		risk := shared.grant.grant.MaximumRisk
		if existed {
			if old, ok := result[bindingID]; ok && risk.RankUnknownHighest() > old.RankUnknownHighest() {
				change.RiskUp = true
			}
		} else {
			diff.risks[id] = risk
		}
		diff.changes = append(diff.changes, change)
		result[bindingID] = risk
		diff.grants = append(diff.grants, shared.grant)
		if shared.account {
			diff.account[id] = shared.requirements
			for _, slot := range sortedKeys(shared.requirements) {
				if existed && previous.account && sharedAgentSkillHasSlot(previous.skill, slot) {
					continue
				}
				diff.newSlots = append(diff.newSlots, sharedAgentPlannedSlot(shared, slot, bindingID))
			}
		}
	}
	for _, id := range sortedKeys(before) {
		if inTo[id] {
			continue
		}
		previous := before[id]
		name := previous.skill.Name
		if name == "" {
			name = id
		}
		bindingID := agentBuilderBindingID(deploymentID, id)
		diff.changes = append(diff.changes, SharedAgentChange{Field: SharedAgentFieldSkills, Kind: SharedAgentChangeSkillRemoved, Label: "Removes " + name, Target: id})
		diff.removals = append(diff.removals, bindingID)
		delete(result, bindingID)
	}
	diff.ceiling = capability.RiskLevelRead
	for _, risk := range result {
		if rank := risk.Rank(); rank >= 0 && rank > diff.ceiling.RankUnknownHighest() {
			diff.ceiling = risk
		}
	}
	return diff, nil
}

type sharedAgentSpecSkill struct {
	skill   SharedAgentSkill
	account bool
}

func sharedAgentSpecSkills(spec SharedAgentSpec) map[string]sharedAgentSpecSkill {
	result := map[string]sharedAgentSpecSkill{}
	for _, value := range spec.Grants {
		result[strings.TrimSpace(value.SkillID)] = sharedAgentSpecSkill{skill: value}
	}
	for _, value := range spec.AccountSkills {
		result[strings.TrimSpace(value.SkillID)] = sharedAgentSpecSkill{skill: value, account: true}
	}
	return result
}

func sameSharedAgentSkill(left, right SharedAgentSkill) bool {
	if strings.TrimSpace(left.SkillVersion) != strings.TrimSpace(right.SkillVersion) ||
		strings.TrimSpace(left.SourceIdentity) != strings.TrimSpace(right.SourceIdentity) || left.EnablePrompt != right.EnablePrompt ||
		!equalStrings(sortedStrings(left.AllowedActions), sortedStrings(right.AllowedActions)) || len(left.Slots) != len(right.Slots) {
		return false
	}
	for _, slot := range left.Slots {
		if !sharedAgentSkillHasSlot(right, slot.Slot) {
			return false
		}
	}
	return true
}

// sharedAgentBindingMatches reports whether the copy's binding still holds
// value's version, actions and prompt, i.e. the adopter did not change it.
func sharedAgentBindingMatches(binding *skill.Binding, value SharedAgentSkill) bool {
	return strings.TrimSpace(binding.SkillVersion) == strings.TrimSpace(value.SkillVersion) &&
		strings.TrimSpace(binding.SourceIdentity) == strings.TrimSpace(value.SourceIdentity) &&
		binding.EnablePrompt == value.EnablePrompt &&
		equalStrings(sortedStrings(models.TrimmedUniqueStrings(binding.AllowedActions)), sortedStrings(models.TrimmedUniqueStrings(value.AllowedActions)))
}

func sharedAgentSkillHasSlot(value SharedAgentSkill, slot string) bool {
	for _, candidate := range value.Slots {
		if candidate.Slot == slot {
			return true
		}
	}
	return false
}
