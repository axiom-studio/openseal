package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// The shared-agent engine. Export reads a whitelist snapshot of an Agent in
// the creator's scope; plan, install and update work in the adopter's scope
// from a snapshot the host holds. Every install and update goes through the
// one creation pipeline (AgentInstaller.build) and recomputes each skill's
// risk from the adopter's catalog: nothing in a snapshot is trusted.

const (
	sharedAgentInstallReason = "Added from a shared agent"
	sharedAgentUpdateReason  = "Updated from a shared agent"
	sharedAgentRetireReason  = "Adding it from a shared agent failed"
	sharedAgentCallPrefix    = "shared:"
	sharedAgentUpdatePrefix  = "shared-update:"
	// maxSharedAgentNameSuffix bounds the "X 2" collision search.
	maxSharedAgentNameSuffix = 99
)

// Connection skills: the host supplies MCP apps itself, and an external
// conversation is a channel.
const (
	sharedAgentMCPSkillID                  = "openseal.mcp"
	sharedAgentExternalConversationSkillID = "openseal.external-conversation"
)

// Fields of a SharedAgentChange of kind profile.
const (
	SharedAgentFieldName         = "name"
	SharedAgentFieldPurpose      = "purpose"
	SharedAgentFieldInstructions = "instructions"
	SharedAgentFieldPersonality  = "personality"
	SharedAgentFieldPrinciples   = "principles"
	SharedAgentFieldSkills       = "skills"
)

// sharedAgentResolvedInstall is a planned install as the kernel resolved it.
type sharedAgentResolvedInstall struct {
	scope        Scope
	actor        string
	key          string
	definitionID string
	deploymentID string
	spaceID      string
	profile      agentProfileArguments
	grants       []resolvedAgentSkillGrant
	// account marks the grants (by skill id) that are account skills, with
	// their credential requirements by slot.
	account     map[string]map[string]skill.CredentialRequirement
	maximumRisk capability.RiskLevel
	provenance  workforce.DefinitionProvenance
}

// sharedSkill is one shared skill resolved against the adopter's catalog.
type sharedSkill struct {
	spec       SharedAgentSkill
	account    bool
	definition *skill.Definition
	name       string
	available  bool
	grant      resolvedAgentSkillGrant
	// requirements are an account skill's credential slots.
	requirements map[string]skill.CredentialRequirement
}

// ExportSharedAgent reads the whitelist snapshot of deploymentID: the active
// profile, enabled non-bundled skills by exact identity (never Config or
// credentials) and per-slot provider names. Skills that keep the Agent from
// being shared are listed as refusals. The platform assistant and copies of
// shared agents can't be exported.
func (i *AgentInstaller) ExportSharedAgent(ctx context.Context, scope Scope, deploymentID string) (*SharedAgentExport, error) {
	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return nil, errors.New("an agent id is required")
	}
	reference := agentBuilderScope(scope)
	deployment, err := i.agents.GetDeployment(ctx, reference, deploymentID)
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
	if deployment.ID == conversationrun.AgentBuilderDeploymentID || skillbinding.AgentDefinitionPolicyFixed(definition) {
		return nil, ErrSharedAgentNotShareable
	}
	if definition.Provenance.Source == SharedAgentProvenanceSource {
		return nil, fmt.Errorf("%w: %w", ErrSharedAgentNotShareable, ErrSharedAgentIsCopy)
	}
	export := &SharedAgentExport{
		Profile: SharedAgentProfile{
			DisplayName: agentDeploymentDisplayName(deployment, definition), Purpose: definition.Purpose,
			SystemPrompt: definition.SystemPrompt, Personality: definition.Personality,
			OperatingPrinciples: append([]string{}, definition.OperatingPrinciples...),
		},
		Grants: []SharedAgentExportSkill{}, AccountSkills: []SharedAgentExportSkill{}, Refusals: []SharedAgentRefusal{},
		Excluded: SharedAgentExcluded{
			Channels:  len(definition.Channels),
			Approvals: len(definition.Authority.StandingGrants) + len(definition.Authority.ApprovalDestinations),
		},
		DefinitionVersion: deployment.ActiveVersion,
	}
	routines, err := i.store.ListObjectives(ctx, ObjectiveFilter{Scope: scope, Owner: &ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID}})
	if err != nil {
		return nil, err
	}
	export.Excluded.Routines = len(routines)
	bindings, err := i.catalog.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if binding == nil || binding.Disabled || strings.HasPrefix(binding.ID, "bundled:") || routineUngrantableSkill(binding.SkillID) {
			continue
		}
		shared, refusal, err := i.exportSharedSkill(ctx, binding)
		if err != nil {
			return nil, err
		}
		switch {
		case refusal != nil:
			export.Refusals = append(export.Refusals, *refusal)
		case len(shared.Slots) > 0:
			export.AccountSkills = append(export.AccountSkills, shared)
		default:
			export.Grants = append(export.Grants, shared)
		}
	}
	sortSharedAgentExportSkills(export.Grants)
	sortSharedAgentExportSkills(export.AccountSkills)
	sortSharedAgentRefusals(export.Refusals)
	return export, nil
}

// exportSharedSkill projects one enabled binding, or names why it blocks the
// share. Its slots are exactly the ones resolveSharedSkill derives from the
// allowed actions, the prompt and the filled optional slots, so a stale
// credential on a slot nothing allowed uses is not exported.
func (i *AgentInstaller) exportSharedSkill(ctx context.Context, binding *skill.Binding) (SharedAgentExportSkill, *SharedAgentRefusal, error) {
	refuse := func(name, reason string) (SharedAgentExportSkill, *SharedAgentRefusal, error) {
		if name == "" {
			name = binding.SkillID
		}
		return SharedAgentExportSkill{}, &SharedAgentRefusal{SkillID: binding.SkillID, Name: name, Reason: reason}, nil
	}
	definition, err := sharedSkillDefinition(ctx, i.catalog, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
	if err != nil {
		return SharedAgentExportSkill{}, nil, err
	}
	name := ""
	if definition != nil {
		name = definition.Name
	}
	if binding.SkillID == sharedAgentExternalConversationSkillID ||
		len(binding.EnabledConversationAdapters) > 0 || len(binding.EnabledCallbackAdapters) > 0 {
		return refuse(name, SharedAgentRefusalChannelAdapter)
	}
	if definition == nil || binding.SkillID == sharedAgentMCPSkillID {
		return refuse(name, SharedAgentRefusalNotInCatalog)
	}
	requirements := sharedSkillRequirements(definition, binding.AllowedActions, binding.EnablePrompt, func(slot string) bool {
		_, ok := binding.Credentials[slot]
		return ok
	})
	shared := SharedAgentExportSkill{
		SharedAgentSkill: SharedAgentSkill{
			SkillID: binding.SkillID, SkillVersion: binding.SkillVersion, SourceIdentity: binding.SourceIdentity,
			Name: definition.Name, AllowedActions: sortedStrings(binding.AllowedActions), EnablePrompt: binding.EnablePrompt,
		},
		Risk: catalogGrantRisk(definition, binding.AllowedActions),
	}
	for _, slot := range sortedKeys(requirements) {
		if !skill.PersonCredentialAllowed(definition, binding, slot) {
			return refuse(name, SharedAgentRefusalVaultCredential)
		}
		provider := ""
		if requirement := requirements[slot]; requirement.OAuth2 != nil {
			provider = requirement.OAuth2.Provider
		}
		shared.Slots = append(shared.Slots, SharedAgentSlot{Slot: slot, Provider: provider})
	}
	return shared, nil, nil
}

// PlanSharedAgentInstall resolves a shared Agent against the adopter's
// catalog and writes nothing. Every skill's risk is recomputed from the
// catalog and the ceiling covers grants and account skills.
//
// Without an IdempotencyKey it is a preview: an empty DisplayName resolves a
// collision to "X 2", and unavailable skills are only marked. With one it
// plans an install: it fails with ErrSharedAgentSkillUnavailable,
// ErrAgentNameTaken, or ErrSharedAgentReviewChanged when the ceiling differs
// from ExpectedMaximumRisk, and the plan derives the copy's ids from the key.
func (i *AgentInstaller) PlanSharedAgentInstall(ctx context.Context, request SharedAgentInstallRequest) (*SharedAgentInstallPlan, error) {
	request.ActingUserID = strings.TrimSpace(request.ActingUserID)
	request.IdempotencyKey = strings.TrimSpace(request.IdempotencyKey)
	request.SpaceID = strings.TrimSpace(request.SpaceID)
	request.DisplayName = strings.TrimSpace(request.DisplayName)
	install := request.IdempotencyKey != ""
	if strings.TrimSpace(request.Scope.Kind) == "" || strings.TrimSpace(request.Scope.ID) == "" {
		return nil, errors.New("a shared agent install needs a scope")
	}
	if install {
		if request.ActingUserID == "" {
			return nil, errors.New("an Agent is created only after a person approves it")
		}
		if request.SpaceID == "" {
			return nil, errors.New("an Agent needs a space")
		}
		if strings.TrimSpace(request.Source.ShareID) == "" || request.Source.Version < 1 {
			return nil, errors.New("a shared agent install needs its share and version")
		}
	}
	profile := sharedAgentProfileArguments(request.Profile)
	if err := validateAgentProfileText(profile); err != nil {
		return nil, err
	}
	plan := &SharedAgentInstallPlan{Request: request, Skills: []SharedAgentPlannedSkill{}, Slots: []SharedAgentPlannedSlot{}, Bindings: []*skill.Binding{}}
	if install {
		key := sharedAgentCallPrefix + request.IdempotencyKey
		plan.DefinitionID = agentBuilderResourceID("agent-definition", request.Scope, key)
		plan.DeploymentID = agentBuilderResourceID("agent-deployment", request.Scope, key)
	}
	name, err := i.sharedAgentName(ctx, request.Scope, profile.DisplayName, request.DisplayName, plan.DeploymentID)
	if err != nil {
		return nil, err
	}
	profile.DisplayName = name
	plan.DisplayName = name

	skills, err := i.resolveSharedSkills(ctx, request.Grants, request.AccountSkills)
	if err != nil {
		return nil, err
	}
	resolved := &sharedAgentResolvedInstall{
		scope: request.Scope, actor: request.ActingUserID, key: request.IdempotencyKey,
		definitionID: plan.DefinitionID, deploymentID: plan.DeploymentID, spaceID: request.SpaceID, profile: profile,
		account: map[string]map[string]skill.CredentialRequirement{},
		provenance: workforce.DefinitionProvenance{
			Source: SharedAgentProvenanceSource, Reference: sharedAgentReference(request.Source), CreatedBy: request.ActingUserID,
		},
	}
	var unavailable *SharedAgentSkillUnavailableError
	reference := skill.ScopeReference{Kind: request.Scope.Kind, ID: request.Scope.ID}
	for _, shared := range skills {
		planned := SharedAgentPlannedSkill{SkillID: shared.spec.SkillID, Name: shared.name, Available: shared.available}
		if !shared.available {
			planned.Reason = SharedAgentSkillUnavailable
			plan.Skills = append(plan.Skills, planned)
			if unavailable == nil {
				unavailable = &SharedAgentSkillUnavailableError{SkillID: shared.spec.SkillID, Name: shared.name}
			}
			continue
		}
		planned.Risk = shared.grant.grant.MaximumRisk
		plan.Skills = append(plan.Skills, planned)
		resolved.grants = append(resolved.grants, shared.grant)
		bindingID := ""
		if plan.DeploymentID != "" {
			bindingID = agentBuilderBindingID(plan.DeploymentID, shared.spec.SkillID)
		}
		if shared.account {
			resolved.account[shared.spec.SkillID] = shared.requirements
			for _, slot := range sortedKeys(shared.requirements) {
				plan.Slots = append(plan.Slots, sharedAgentPlannedSlot(shared, slot, bindingID))
			}
		}
		candidate := skillbinding.SkillBindingCandidate(reference, plan.DeploymentID, sharedSkillBindingArguments(bindingID, shared.grant.grant), nil)
		candidate.Credentials = maps.Clone(shared.grant.credentials)
		plan.Bindings = append(plan.Bindings, candidate)
	}
	resolved.maximumRisk = agentGrantsMaximumRisk("", resolved.grants)
	plan.MaximumRisk = resolved.maximumRisk
	if install {
		if unavailable != nil {
			return nil, *unavailable
		}
		if request.ExpectedMaximumRisk != plan.MaximumRisk {
			return nil, ErrSharedAgentReviewChanged
		}
		plan.resolved = resolved
	}
	return plan, nil
}

// InstallSharedAgent creates the planned copy through the one creation
// pipeline, approved by the acting person's Add. The host may have replaced
// an account slot's person reference in plan.Bindings with the adopter's own
// reference of the same kind; nothing else of the exported plan is read
// back. A failure after the deployment exists retires it.
func (i *AgentInstaller) InstallSharedAgent(ctx context.Context, plan *SharedAgentInstallPlan) (*SharedAgentInstalled, error) {
	if plan == nil || plan.resolved == nil {
		return nil, errors.New("install a shared agent from the plan PlanSharedAgentInstall returned for it")
	}
	resolved := plan.resolved
	grants, err := sharedAgentPlanCredentials(resolved, plan.Bindings)
	if err != nil {
		return nil, err
	}
	built, err := i.build(ctx, agentBuildInput{
		Scope: resolved.scope, DefinitionID: resolved.definitionID, DeploymentID: resolved.deploymentID,
		Profile: resolved.profile, SpaceID: resolved.spaceID, Grants: grants, MaximumRisk: resolved.maximumRisk,
		Provenance: resolved.provenance, Approver: resolved.actor, CallRef: sharedAgentCallPrefix + resolved.key,
		InstallKey: resolved.key, Reason: sharedAgentInstallReason,
	})
	if err != nil {
		return nil, err
	}
	bindings, unconnected, err := i.sharedAgentBindings(ctx, resolved.scope, built.deployment.ID, grants, resolved.account)
	if err != nil {
		return nil, err
	}
	return &SharedAgentInstalled{
		AgentID: built.deployment.ID, DisplayName: built.definition.DisplayName, Space: built.space,
		ConversationID: built.conversation.ID, Activation: built.activation, Bindings: bindings,
		Unconnected: unconnected, Replayed: built.replayed,
	}, nil
}

// RetireSharedAgentInstall retires a copy whose install the host could not
// finish (links, apps), so its name is free again. Only a copy can be
// retired this way; a retired copy is done.
func (i *AgentInstaller) RetireSharedAgentInstall(ctx context.Context, scope Scope, deploymentID, actor string) error {
	deploymentID, actor = strings.TrimSpace(deploymentID), strings.TrimSpace(actor)
	if deploymentID == "" || actor == "" {
		return errors.New("retiring a shared agent install needs the agent and the acting person")
	}
	reference := agentBuilderScope(scope)
	current, err := i.agents.GetDeployment(ctx, reference, deploymentID)
	if err != nil {
		return err
	}
	if current.RolloutStatus == kernelagent.RolloutRetired {
		return nil
	}
	definition, err := i.agents.GetDefinition(ctx, current.DefinitionID, current.ActiveVersion)
	if err != nil {
		return err
	}
	if definition.Provenance.Source != SharedAgentProvenanceSource {
		return ErrSharedAgentNotCopy
	}
	retired := *current
	retired.RolloutStatus = kernelagent.RolloutRetired
	_, _, err = i.agents.UpdateDeployment(ctx, &retired, current.Revision, "user", actor, sharedAgentRetireReason)
	return err
}

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
	reference := agentBuilderScope(scope)
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
	reference := agentBuilderScope(request.Scope)
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
	scope Scope,
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
	reference := agentBuilderScope(scope)
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
	if err := validateAgentProfileText(to); err != nil {
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
func (i *AgentInstaller) resolveSharedAgentSkillDiff(ctx context.Context, scope Scope, deploymentID string, from, to SharedAgentSpec) (*sharedAgentSkillDiff, error) {
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
		equalStrings(sortedStrings(normalizedActionStrings(binding.AllowedActions)), sortedStrings(normalizedActionStrings(value.AllowedActions)))
}

func sharedAgentSkillHasSlot(value SharedAgentSkill, slot string) bool {
	for _, candidate := range value.Slots {
		if candidate.Slot == slot {
			return true
		}
	}
	return false
}

// resolveSharedSkills resolves every grant and account skill against the
// catalog. A skill that does not resolve exactly is unavailable, never
// dropped. Only catalog failures are errors.
func (i *AgentInstaller) resolveSharedSkills(ctx context.Context, grants, accountSkills []SharedAgentSkill) ([]sharedSkill, error) {
	if len(grants)+len(accountSkills) > maxAgentBuilderSkillGrants {
		return nil, fmt.Errorf("an agent can be given at most %d Skills at once", maxAgentBuilderSkillGrants)
	}
	result := make([]sharedSkill, 0, len(grants)+len(accountSkills))
	seen := map[string]bool{}
	add := func(value SharedAgentSkill, account bool) error {
		id := strings.TrimSpace(value.SkillID)
		if id == "" || strings.TrimSpace(value.SkillVersion) == "" {
			return errors.New("each Skill needs a skillId and skillVersion")
		}
		if seen[id] {
			return fmt.Errorf("Skill %s is listed more than once", id)
		}
		seen[id] = true
		resolved, err := i.resolveSharedSkill(ctx, value, account)
		if err != nil {
			return err
		}
		result = append(result, resolved)
		return nil
	}
	for _, value := range grants {
		if err := add(value, false); err != nil {
			return nil, err
		}
	}
	for _, value := range accountSkills {
		if err := add(value, true); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// resolveSharedSkill resolves one shared skill. Its risk is the catalog's.
// An account skill must have only OAuth2 slots the person's own sign-in may
// fill; each is bound to the person ref.
func (i *AgentInstaller) resolveSharedSkill(ctx context.Context, value SharedAgentSkill, account bool) (sharedSkill, error) {
	value.SkillID = strings.TrimSpace(value.SkillID)
	value.SkillVersion = strings.TrimSpace(value.SkillVersion)
	value.SourceIdentity = strings.TrimSpace(value.SourceIdentity)
	value.AllowedActions = normalizedActionStrings(value.AllowedActions)
	resolved := sharedSkill{spec: value, account: account, name: strings.TrimSpace(value.Name)}
	if resolved.name == "" {
		resolved.name = value.SkillID
	}
	if routineUngrantableSkill(value.SkillID) || value.SkillID == sharedAgentMCPSkillID || value.SkillID == sharedAgentExternalConversationSkillID {
		return resolved, nil
	}
	definition, err := sharedSkillDefinition(ctx, i.catalog, value.SkillID, value.SkillVersion, value.SourceIdentity)
	if err != nil || definition == nil {
		return resolved, err
	}
	resolved.definition = definition
	if definition.Name != "" {
		resolved.name = definition.Name
	}
	grant := AgentSkillGrant{
		SkillID: value.SkillID, SkillVersion: value.SkillVersion, SourceIdentity: value.SourceIdentity,
		AllowedActions: value.AllowedActions, EnablePrompt: value.EnablePrompt,
		MaximumRisk: catalogGrantRisk(definition, value.AllowedActions),
	}
	var credentials map[string]skill.CredentialReference
	requirements := sharedSkillRequirements(definition, value.AllowedActions, value.EnablePrompt, func(slot string) bool {
		return sharedAgentSkillHasSlot(value, slot)
	})
	if account {
		if len(requirements) == 0 {
			return resolved, nil
		}
		probe := &skill.Binding{SkillID: value.SkillID, SkillVersion: value.SkillVersion, AllowedActions: value.AllowedActions, EnablePrompt: value.EnablePrompt}
		credentials = map[string]skill.CredentialReference{}
		for slot, requirement := range requirements {
			if !skill.PersonCredentialAllowed(definition, probe, slot) {
				return resolved, nil
			}
			credentials[slot] = skill.CredentialReference{Kind: requirement.Kind, ID: skill.PersonCredentialReferenceID}
		}
		resolved.requirements = requirements
	} else if len(requirements) > 0 {
		// A skill that needs an account here can't be a plain grant.
		return resolved, nil
	}
	if err := skillbinding.ValidateSkillBindingArguments(definition, sharedSkillBindingArguments("", grant), credentials); err != nil {
		return resolved, nil
	}
	resolved.available = true
	resolved.grant = resolvedAgentSkillGrant{grant: grant, definition: definition, credentials: credentials}
	return resolved, nil
}

// sharedAgentPlanCredentials returns the planned grants with each account
// slot's reference taken from the host's plan bindings when it has the
// slot's kind, else the person ref. Nothing else is read back.
func sharedAgentPlanCredentials(resolved *sharedAgentResolvedInstall, bindings []*skill.Binding) ([]resolvedAgentSkillGrant, error) {
	bySkill := map[string]*skill.Binding{}
	for _, binding := range bindings {
		if binding != nil {
			bySkill[binding.SkillID] = binding
		}
	}
	grants := make([]resolvedAgentSkillGrant, 0, len(resolved.grants))
	for _, grant := range resolved.grants {
		requirements, account := resolved.account[grant.grant.SkillID]
		if !account {
			grants = append(grants, grant)
			continue
		}
		credentials := maps.Clone(grant.credentials)
		if binding := bySkill[grant.grant.SkillID]; binding != nil {
			for slot, reference := range binding.Credentials {
				requirement, ok := requirements[slot]
				if !ok {
					return nil, fmt.Errorf("skill %s has no account slot %q", grant.grant.SkillID, slot)
				}
				if strings.TrimSpace(reference.Kind) != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
					return nil, fmt.Errorf("skill %s slot %s needs a %s account", grant.grant.SkillID, slot, requirement.Kind)
				}
				credentials[slot] = skill.CredentialReference{Kind: requirement.Kind, ID: strings.TrimSpace(reference.ID)}
			}
		}
		if err := skillbinding.ValidateSkillBindingArguments(grant.definition, sharedSkillBindingArguments("", grant.grant), credentials); err != nil {
			return nil, fmt.Errorf("Skill %s: %w", grant.grant.SkillID, err)
		}
		grant.credentials = credentials
		grants = append(grants, grant)
	}
	return grants, nil
}

// sharedAgentBindings reads back the copy's bindings for grants and lists
// the account slots still on the person ref: the host links each to the
// adopter's own sign-in or shows a connect card for it.
func (i *AgentInstaller) sharedAgentBindings(ctx context.Context, scope Scope, deploymentID string, grants []resolvedAgentSkillGrant, account map[string]map[string]skill.CredentialRequirement) ([]*skill.Binding, []SharedAgentPlannedSlot, error) {
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	bindings := make([]*skill.Binding, 0, len(grants))
	unconnected := []SharedAgentPlannedSlot{}
	for _, grant := range grants {
		bindingID := agentBuilderBindingID(deploymentID, grant.grant.SkillID)
		binding, err := i.catalog.GetBinding(ctx, skillScope, deploymentID, bindingID)
		if err != nil {
			return nil, nil, err
		}
		bindings = append(bindings, binding)
		requirements := account[grant.grant.SkillID]
		for _, slot := range sortedKeys(requirements) {
			if reference, ok := binding.Credentials[slot]; ok && !skill.IsPersonCredentialReference(reference) {
				continue
			}
			planned := SharedAgentPlannedSlot{SkillID: grant.grant.SkillID, Slot: slot, PersonAllowed: true, BindingID: bindingID}
			if requirement := requirements[slot]; requirement.OAuth2 != nil {
				planned.Provider = requirement.OAuth2.Provider
			}
			unconnected = append(unconnected, planned)
		}
	}
	return bindings, unconnected, nil
}

// sharedAgentName is the copy's name. A requested name must be free; without
// one the shared name is used, or "X 2", "X 3"… when it is taken.
func (i *AgentInstaller) sharedAgentName(ctx context.Context, scope Scope, shared, requested, exceptDeploymentID string) (string, error) {
	if requested != "" {
		if err := validateAgentDisplayName(requested); err != nil {
			return "", err
		}
		if err := ensureAgentNameAvailable(ctx, i.agents, i.teams, scope, requested, exceptDeploymentID); err != nil {
			return "", err
		}
		return requested, nil
	}
	taken, err := takenAgentNames(ctx, i.agents, i.teams, scope, exceptDeploymentID)
	if err != nil {
		return "", err
	}
	if !taken[normalizedAgentName(shared)] {
		return shared, nil
	}
	for suffix := 2; suffix <= maxSharedAgentNameSuffix; suffix++ {
		tail := " " + strconv.Itoa(suffix)
		base := shared
		if overflow := utf8.RuneCountInString(base) + len(tail) - maxAgentDisplayNameRunes; overflow > 0 {
			runes := []rune(base)
			base = strings.TrimSpace(string(runes[:len(runes)-overflow]))
		}
		candidate := base + tail
		if !taken[normalizedAgentName(candidate)] {
			return candidate, nil
		}
	}
	return "", agentNameTakenError{name: shared}
}

// sharedAgentNameMatches reports whether current is the shared name or a
// collision variant of it ("X 2"), which is not an edit.
func sharedAgentNameMatches(current, shared string) bool {
	if normalizedAgentName(current) == normalizedAgentName(shared) {
		return true
	}
	prefix := normalizedAgentName(shared) + " "
	rest, ok := strings.CutPrefix(normalizedAgentName(current), prefix)
	if !ok {
		return false
	}
	number, err := strconv.Atoi(rest)
	return err == nil && number >= 2 && number <= maxSharedAgentNameSuffix
}

func sharedAgentProfileArguments(profile SharedAgentProfile) agentProfileArguments {
	return agentProfileArguments{
		DisplayName: profile.DisplayName, Purpose: profile.Purpose, SystemPrompt: profile.SystemPrompt,
		Personality: profile.Personality, OperatingPrinciples: profile.OperatingPrinciples,
	}.Normalized()
}

func sharedAgentReference(source SharedAgentSource) string {
	return strings.TrimSpace(source.ShareID) + "@" + strconv.Itoa(source.Version)
}

func sharedAgentPlannedSlot(shared sharedSkill, slot, bindingID string) SharedAgentPlannedSlot {
	planned := SharedAgentPlannedSlot{SkillID: shared.spec.SkillID, Slot: slot, PersonAllowed: true, BindingID: bindingID}
	if requirement := shared.requirements[slot]; requirement.OAuth2 != nil {
		planned.Provider = requirement.OAuth2.Provider
	}
	return planned
}

func sharedSkillBindingArguments(bindingID string, grant AgentSkillGrant) skillbinding.SkillBindingUpsertArguments {
	return skillbinding.SkillBindingUpsertArguments{
		BindingID: bindingID, SkillID: grant.SkillID, SkillVersion: grant.SkillVersion, SourceIdentity: grant.SourceIdentity,
		AllowedActions: grant.AllowedActions, EnablePrompt: grant.EnablePrompt, MaximumRisk: grant.MaximumRisk,
	}
}

// sharedSkillDefinition is the exact catalog definition, or nil when this
// catalog does not register it by that identity.
func sharedSkillDefinition(ctx context.Context, catalog *skill.Catalog, id, version, sourceIdentity string) (*skill.Definition, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(version) == "" {
		return nil, nil
	}
	if sourceIdentity != "" {
		return catalog.GetDefinitionVariant(ctx, id, version, sourceIdentity)
	}
	definition, err := catalog.GetDefinition(ctx, id, version)
	if err != nil || definition == nil {
		return definition, err
	}
	if skill.DefinitionSourceIdentity(definition) != "" {
		return nil, nil
	}
	return definition, nil
}

// sharedSkillRequirements are the credential slots the allowed actions (and
// the prompt, when enabled) use, by slot name. An optional slot counts only
// when filled reports it: on export, the creator's binding holds a credential
// for it; on install, the snapshot lists it.
func sharedSkillRequirements(definition *skill.Definition, allowedActions []string, enablePrompt bool, filled func(slot string) bool) map[string]skill.CredentialRequirement {
	requirements := map[string]skill.CredentialRequirement{}
	add := func(values []skill.CredentialRequirement) {
		for _, requirement := range values {
			if requirement.Optional && !filled(requirement.Name) {
				continue
			}
			if existing, ok := requirements[requirement.Name]; !ok || existing.Optional && !requirement.Optional {
				requirements[requirement.Name] = requirement
			}
		}
	}
	for _, name := range allowedActions {
		add(definition.Actions[strings.TrimSpace(name)].Credentials)
	}
	if enablePrompt && definition.Prompt != nil {
		add(definition.Prompt.Credentials)
	}
	return requirements
}

func sortSharedAgentExportSkills(values []SharedAgentExportSkill) {
	sort.Slice(values, func(a, b int) bool { return values[a].SkillID < values[b].SkillID })
}

func sortSharedAgentRefusals(values []SharedAgentRefusal) {
	sort.Slice(values, func(a, b int) bool { return values[a].SkillID < values[b].SkillID })
}

func sortedStrings(values []string) []string {
	result := append([]string{}, values...)
	sort.Strings(result)
	return result
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
