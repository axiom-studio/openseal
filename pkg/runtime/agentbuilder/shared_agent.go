package agentbuilder

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
	"github.com/axiom-studio/openseal/pkg/models"
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
	scope        models.Scope
	actor        string
	key          string
	definitionID string
	deploymentID string
	spaceID      string
	profile      models.AgentProfileArguments
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
func (i *AgentInstaller) ExportSharedAgent(ctx context.Context, scope models.Scope, deploymentID string) (*SharedAgentExport, error) {
	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return nil, errors.New("an agent id is required")
	}
	reference := models.ScopeReference(scope)
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
	routines, err := i.store.ListObjectives(ctx, models.ObjectiveFilter{Scope: scope, Owner: &models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deployment.ID}})
	if err != nil {
		return nil, err
	}
	export.Excluded.Routines = len(routines)
	bindings, err := i.catalog.ListBindings(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deployment.ID)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if binding == nil || binding.Disabled || strings.HasPrefix(binding.ID, "bundled:") || models.RoutineUngrantableSkill(binding.SkillID) {
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
	if err := models.ValidateAgentProfileText(profile); err != nil {
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
		candidate := skillbinding.Candidate(reference, plan.DeploymentID, sharedSkillBindingArguments(bindingID, shared.grant.grant), nil)
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
func (i *AgentInstaller) RetireSharedAgentInstall(ctx context.Context, scope models.Scope, deploymentID, actor string) error {
	deploymentID, actor = strings.TrimSpace(deploymentID), strings.TrimSpace(actor)
	if deploymentID == "" || actor == "" {
		return errors.New("retiring a shared agent install needs the agent and the acting person")
	}
	reference := models.ScopeReference(scope)
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

// resolveSharedSkills resolves every grant and account skill against the
// catalog. A skill that does not resolve exactly is unavailable, never
// dropped. Only catalog failures are errors.
func (i *AgentInstaller) resolveSharedSkills(ctx context.Context, grants, accountSkills []SharedAgentSkill) ([]sharedSkill, error) {
	if len(grants)+len(accountSkills) > models.MaxAgentBuilderSkillGrants {
		return nil, fmt.Errorf("an agent can be given at most %d Skills at once", models.MaxAgentBuilderSkillGrants)
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
	value.AllowedActions = models.TrimmedUniqueStrings(value.AllowedActions)
	resolved := sharedSkill{spec: value, account: account, name: strings.TrimSpace(value.Name)}
	if resolved.name == "" {
		resolved.name = value.SkillID
	}
	if models.RoutineUngrantableSkill(value.SkillID) || value.SkillID == sharedAgentMCPSkillID || value.SkillID == sharedAgentExternalConversationSkillID {
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
	grant := models.AgentSkillGrant{
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
func (i *AgentInstaller) sharedAgentBindings(ctx context.Context, scope models.Scope, deploymentID string, grants []resolvedAgentSkillGrant, account map[string]map[string]skill.CredentialRequirement) ([]*skill.Binding, []SharedAgentPlannedSlot, error) {
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
func (i *AgentInstaller) sharedAgentName(ctx context.Context, scope models.Scope, shared, requested, exceptDeploymentID string) (string, error) {
	if requested != "" {
		if err := models.ValidateAgentDisplayName(requested); err != nil {
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
	if !taken[models.NormalizedAgentName(shared)] {
		return shared, nil
	}
	for suffix := 2; suffix <= maxSharedAgentNameSuffix; suffix++ {
		tail := " " + strconv.Itoa(suffix)
		base := shared
		if overflow := utf8.RuneCountInString(base) + len(tail) - models.MaxAgentDisplayNameRunes; overflow > 0 {
			runes := []rune(base)
			base = strings.TrimSpace(string(runes[:len(runes)-overflow]))
		}
		candidate := base + tail
		if !taken[models.NormalizedAgentName(candidate)] {
			return candidate, nil
		}
	}
	return "", agentNameTakenError{name: shared}
}

// sharedAgentNameMatches reports whether current is the shared name or a
// collision variant of it ("X 2"), which is not an edit.
func sharedAgentNameMatches(current, shared string) bool {
	if models.NormalizedAgentName(current) == models.NormalizedAgentName(shared) {
		return true
	}
	prefix := models.NormalizedAgentName(shared) + " "
	rest, ok := strings.CutPrefix(models.NormalizedAgentName(current), prefix)
	if !ok {
		return false
	}
	number, err := strconv.Atoi(rest)
	return err == nil && number >= 2 && number <= maxSharedAgentNameSuffix
}

func sharedAgentProfileArguments(profile SharedAgentProfile) models.AgentProfileArguments {
	return models.AgentProfileArguments{
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

func sharedSkillBindingArguments(bindingID string, grant models.AgentSkillGrant) skillbinding.UpsertArguments {
	return skillbinding.UpsertArguments{
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
