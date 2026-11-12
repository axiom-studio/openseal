package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"unicode/utf8"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/validation"
)

// errAgentModelUnavailable refuses a model choice on a host without a model
// catalog (the OSS daemon).
var errAgentModelUnavailable = errors.New("model selection is unavailable here")

// AgentBuilderActionValidator checks a builder action (create_agent,
// update_agent, try_agent) before a card exists and returns the secret-free
// preview the review card renders. Every refusal becomes an is_error tool
// result the builder can correct.
type AgentBuilderActionValidator struct {
	agents        *kernelagent.Registry
	teams         *kernelteam.Registry
	catalog       *skill.Catalog
	spaces        *SpaceService
	conversations ConversationStore
	runs          PortfolioStore
	host          AgentBuilderHost
}

// NewAgentBuilderActionValidator needs the Agent and Team registries, the
// Skill catalog and the spaces. conversations and runs place a created Agent
// in the conversation's space; without them it goes to the default space of
// the person the run acts for. host may be nil.
func NewAgentBuilderActionValidator(
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	catalog *skill.Catalog,
	spaces *SpaceService,
	conversations ConversationStore,
	runs PortfolioStore,
	host AgentBuilderHost,
) (*AgentBuilderActionValidator, error) {
	if agents == nil || teams == nil || catalog == nil || spaces == nil {
		return nil, errors.New("agent builder validator requires the Agent and Team registries, the Skill catalog and spaces")
	}
	return &AgentBuilderActionValidator{
		agents: agents, teams: teams, catalog: catalog, spaces: spaces,
		conversations: conversations, runs: runs, host: host,
	}, nil
}

// ResolveActionProposalArguments fills the kernel-resolved arguments: the
// space of a created Agent (the Run's conversation space, else Default) and
// the current revision of an updated Agent.
func (v *AgentBuilderActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isAgentBuilderAction(input.Bound) {
		return nil, false, nil
	}
	switch input.Bound.Action.Name {
	case AgentActionCreateAgent, AgentActionUpdateAgent:
	default:
		return nil, false, nil
	}
	if v == nil || input.Run == nil {
		return nil, true, errors.New("agent builder validator is not configured")
	}
	arguments := maps.Clone(input.Arguments)
	if arguments == nil {
		arguments = map[string]interface{}{}
	}
	switch input.Bound.Action.Name {
	case AgentActionCreateAgent:
		if spaceID, _ := arguments["spaceId"].(string); strings.TrimSpace(spaceID) == "" {
			resolved, err := v.runSpaceID(ctx, input.Run)
			if err != nil {
				return nil, true, err
			}
			arguments["spaceId"] = resolved
		}
	case AgentActionUpdateAgent:
		if _, supplied := arguments["expectedDeploymentRevision"]; !supplied {
			agentID, _ := arguments["agentId"].(string)
			if strings.TrimSpace(agentID) != "" {
				if err := v.authorizeAgentChange(ctx, input.Run, agentID); err != nil {
					return nil, true, err
				}
				deployment, err := v.agents.GetDeployment(ctx, agentBuilderScope(input.Run.Scope), strings.TrimSpace(agentID))
				if err != nil {
					return nil, true, agentBuilderTargetError(agentID, err)
				}
				arguments["expectedDeploymentRevision"] = deployment.Revision
			}
		}
	}
	return arguments, true, nil
}

// ValidateActionProposal returns the §2.2 card preview for create_agent and
// update_agent, and nil for the read actions after checking try_agent.
func (v *AgentBuilderActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isAgentBuilderAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.agents == nil || input.Run == nil || input.Bound.Binding == nil {
		return nil, errors.New("agent builder validator is not configured")
	}
	builderID := strings.TrimSpace(input.Run.AssignedAgentID)
	if builderID == "" || input.Bound.Binding.DeploymentID != builderID {
		return nil, errors.New("agent builder action is not bound to the current Run's Agent deployment")
	}
	scope := input.Run.Scope
	switch input.Bound.Action.Name {
	case AgentActionCreateAgent:
		resolved, err := resolveAgentCreate(ctx, v.agents, v.teams, v.catalog, v.spaces, v.host, scope, input.Arguments)
		if err != nil {
			return nil, err
		}
		return agentCreatePreview(input.Bound, resolved)
	case AgentActionUpdateAgent:
		// The card shows the target's current profile, so the person must
		// be allowed to change it before anything of it is read.
		if agentID, _ := input.Arguments["agentId"].(string); strings.TrimSpace(agentID) != builderID {
			if err := v.authorizeAgentChange(ctx, input.Run, agentID); err != nil {
				return nil, err
			}
		}
		resolved, err := resolveAgentUpdate(ctx, v.agents, v.teams, v.catalog, v.host, scope, builderID, input.Arguments)
		if err != nil {
			return nil, err
		}
		return agentUpdatePreview(input.Bound, resolved)
	case AgentActionTryAgent:
		_, err := resolveAgentTry(ctx, v.host, scope, input.Arguments)
		return nil, err
	}
	return nil, nil
}

// authorizeAgentChange asks the host whether the Run's person may change
// agentID. A refusal reads like a missing Agent, so the card never shows an
// Agent the person can't manage. A nil host (the OSS daemon) has one person.
func (v *AgentBuilderActionValidator) authorizeAgentChange(ctx context.Context, run *AgentRun, agentID string) error {
	agentID = strings.TrimSpace(agentID)
	if v.host == nil || agentID == "" {
		return nil
	}
	err := v.host.AuthorizeAgentChange(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, strings.TrimSpace(run.ActingUser), agentID)
	if errors.Is(err, ErrAgentChangeForbidden) {
		return fmt.Errorf("agent %q does not exist or this person can't change it; use list_agents and choose an Agent they manage", agentID)
	}
	if err != nil {
		return fmt.Errorf("check who may change agent %q: %w", agentID, err)
	}
	return nil
}

// runSpaceID is the space an Agent or Team made by the run goes to when the
// call names none: the space of the run's chat, else the default space of
// the person the run acts for.
func (v *AgentBuilderActionValidator) runSpaceID(ctx context.Context, run *AgentRun) (string, error) {
	if v.runs != nil && v.conversations != nil {
		_, conversation, _, err := conversationWorkOrigin(ctx, v.runs, v.conversations, run)
		if err != nil {
			return "", err
		}
		if conversation != nil && strings.TrimSpace(conversation.SpaceID) != "" {
			return conversation.SpaceID, nil
		}
	}
	if v.spaces == nil {
		return "", errors.New("spaces are not configured")
	}
	space, err := v.spaces.DefaultSpace(ctx, run.Scope, run.ActingUser)
	if err != nil {
		return "", fmt.Errorf("the run has no chat with a space to create this in: %w", err)
	}
	return space.ID, nil
}

// resolvedAgentSkillGrant is a grant checked against its exact definition.
type resolvedAgentSkillGrant struct {
	grant      AgentSkillGrant
	definition *skill.Definition
	// credentials are an account skill's credential references (person
	// refs); nil for a skill that needs no account.
	credentials map[string]skill.CredentialReference
}

type resolvedAgentConnection struct {
	request    AgentConnectionRequest
	definition *skill.Definition
}

type resolvedAgentCreate struct {
	args        agentCreateArguments
	space       *Space
	skills      []resolvedAgentSkillGrant
	connections []resolvedAgentConnection
}

type resolvedAgentRemovedBinding struct {
	binding *skill.Binding
	name    string
}

type resolvedAgentUpdate struct {
	args       agentUpdateArguments
	deployment *kernelagent.AgentDeployment
	definition *kernelagent.AgentDefinition
	// candidate is definition with the profile changes applied; its version
	// is still the base version.
	candidate *kernelagent.AgentDefinition
	// changes lists only the changed profile fields (agentBehaviorChanges).
	changes map[string]interface{}
	// authorityRaised reports that the added grants raise the candidate's
	// Authority.MaximumRisk above the definition's.
	authorityRaised bool
	// model is the requested model when update_agent supplies one.
	model   *string
	skills  []resolvedAgentSkillGrant
	removed []resolvedAgentRemovedBinding
}

func resolveAgentCreate(
	ctx context.Context,
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	catalog *skill.Catalog,
	spaces *SpaceService,
	host AgentBuilderHost,
	scope Scope,
	arguments map[string]interface{},
) (*resolvedAgentCreate, error) {
	var args agentCreateArguments
	if err := decodeAgentBuilderArguments(arguments, &args); err != nil {
		return nil, err
	}
	args.AgentProfileArguments = args.AgentProfileArguments.Normalized()
	args.SpaceID = strings.TrimSpace(args.SpaceID)
	args.Rationale = strings.TrimSpace(args.Rationale)
	if args.Rationale == "" {
		return nil, errors.New("create_agent requires a rationale")
	}
	if err := validateAgentProfileText(args.AgentProfileArguments); err != nil {
		return nil, err
	}
	if err := ensureAgentNameAvailable(ctx, agents, teams, scope, args.DisplayName, ""); err != nil {
		return nil, err
	}
	space, err := resolveAgentBuilderSpace(ctx, spaces, scope, args.SpaceID)
	if err != nil {
		return nil, err
	}
	skills, err := resolveAgentSkillGrants(ctx, catalog, args.Skills)
	if err != nil {
		return nil, err
	}
	connections, err := resolveAgentConnections(ctx, catalog, args.Connections)
	if err != nil {
		return nil, err
	}
	if err := validateAgentBuilderModel(ctx, host, scope, args.Model); err != nil {
		return nil, err
	}
	return &resolvedAgentCreate{args: args, space: space, skills: skills, connections: connections}, nil
}

func resolveAgentUpdate(
	ctx context.Context,
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	catalog *skill.Catalog,
	host AgentBuilderHost,
	scope Scope,
	builderID string,
	arguments map[string]interface{},
) (*resolvedAgentUpdate, error) {
	var args agentUpdateArguments
	if err := decodeAgentBuilderArguments(arguments, &args); err != nil {
		return nil, err
	}
	args.AgentID = strings.TrimSpace(args.AgentID)
	args.Rationale = strings.TrimSpace(args.Rationale)
	if args.AgentID == "" || args.ExpectedDeploymentRevision < 1 || args.Rationale == "" {
		return nil, errors.New("update_agent requires agentId, the current revision and a rationale")
	}
	if args.AgentID == builderID {
		return nil, errors.New("update_agent cannot change the Agent you are; use amend_behavior for your own behavior")
	}
	deployment, err := agents.GetDeployment(ctx, agentBuilderScope(scope), args.AgentID)
	if err != nil {
		return nil, agentBuilderTargetError(args.AgentID, err)
	}
	if deployment.RolloutStatus == kernelagent.RolloutRetired {
		return nil, fmt.Errorf("agent %q is retired and cannot be changed", args.AgentID)
	}
	if deployment.Revision != args.ExpectedDeploymentRevision {
		return nil, kernelagent.ErrRevisionConflict
	}
	definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if agentDefinitionPolicyFixed(definition) {
		return nil, fmt.Errorf("agent %q cannot be changed in chat: %w", args.AgentID, ErrAgentBehaviorFixed)
	}
	candidate := cloneAgentDefinitionForAction(definition)
	applyAgentBehaviorArguments(candidate, agentBehaviorActionArguments{
		DisplayName: args.DisplayName, Purpose: args.Purpose, SystemPrompt: args.SystemPrompt,
		Personality: args.Personality, OperatingPrinciples: args.OperatingPrinciples,
	})
	profile := agentProfileFromDefinition(candidate)
	fields := map[string]bool{}
	if args.Model != nil {
		model := strings.TrimSpace(*args.Model)
		args.Model = &model
		profile.Model = model
		fields["model"] = true
	}
	changes := agentBehaviorChanges(definition, candidate)
	for field := range changes {
		fields[field] = true
	}
	// Only changed fields are checked, so an Agent made under older limits
	// can still be changed in chat.
	if err := validateAgentProfileFields(profile, fields); err != nil {
		return nil, err
	}
	if err := candidate.Validate(); err != nil {
		return nil, err
	}
	if _, renamed := changes["displayName"]; renamed {
		if err := ensureAgentNameAvailable(ctx, agents, teams, scope, candidate.DisplayName, deployment.ID); err != nil {
			return nil, err
		}
	}
	skills, err := resolveAgentSkillGrants(ctx, catalog, args.AddSkills)
	if err != nil {
		return nil, err
	}
	removed, err := resolveAgentRemovedBindings(ctx, catalog, scope, deployment.ID, args.RemoveSkillBindingIDs)
	if err != nil {
		return nil, err
	}
	// The definition ceiling caps the Agent's Team Skills, so it rises to
	// the added grants, as the Agent's create does.
	ceiling := agentGrantsMaximumRisk(definition.Authority.MaximumRisk, skills)
	authorityRaised := ceiling != definition.Authority.MaximumRisk
	candidate.Authority.MaximumRisk = ceiling
	if args.Model != nil {
		if err := validateAgentBuilderModel(ctx, host, scope, *args.Model); err != nil {
			return nil, err
		}
	}
	if len(changes) == 0 && args.Model == nil && len(skills) == 0 && len(removed) == 0 {
		return nil, errors.New("update_agent requires at least one change to the Agent")
	}
	return &resolvedAgentUpdate{
		args: args, deployment: deployment, definition: definition, candidate: candidate,
		changes: changes, authorityRaised: authorityRaised, model: args.Model, skills: skills, removed: removed,
	}, nil
}

func resolveAgentTry(ctx context.Context, host AgentBuilderHost, scope Scope, arguments map[string]interface{}) (agentTryArguments, error) {
	var args agentTryArguments
	if err := decodeAgentBuilderArguments(arguments, &args); err != nil {
		return args, err
	}
	args.AgentProfileArguments = args.AgentProfileArguments.Normalized()
	args.Message = strings.TrimSpace(args.Message)
	if err := validateAgentProfileText(args.AgentProfileArguments); err != nil {
		return args, err
	}
	if args.Message == "" || utf8.RuneCountInString(args.Message) > maxAgentTrialMessageRunes {
		return args, fmt.Errorf("try_agent message must contain 1 to %d characters", maxAgentTrialMessageRunes)
	}
	if err := validation.ValidateNoSensitiveInput(args.Message); err != nil {
		return args, err
	}
	if err := validateAgentBuilderModel(ctx, host, scope, args.Model); err != nil {
		return args, err
	}
	return args, nil
}

// agentGrantsMaximumRisk is the Authority.MaximumRisk an Agent needs for
// grants: the highest of base (read when empty) and each grant's
// maximumRisk. Team Skill actions are capped by it (teamSkillAuthority).
func agentGrantsMaximumRisk(base capability.RiskLevel, grants []resolvedAgentSkillGrant) capability.RiskLevel {
	ceiling := base
	if ceiling == "" {
		ceiling = capability.RiskLevelRead
	}
	for _, resolved := range grants {
		risk := resolved.grant.MaximumRisk
		if risk.Rank() >= 0 && risk.Rank() > ceiling.RankUnknownHighest() {
			ceiling = risk
		}
	}
	return ceiling
}

// agentDefinitionPolicyFixed reports a definition no Agent may amend, such as
// the platform assistant: Agent proposals are off and no field is open.
func agentDefinitionPolicyFixed(definition *kernelagent.AgentDefinition) bool {
	return !definition.Amendments.AgentMayPropose && len(definition.Amendments.AllowedFields) == 0
}

// agentNameTakenError is ErrAgentNameTaken with the wanted name.
type agentNameTakenError struct{ name string }

func (e agentNameTakenError) Error() string {
	return fmt.Sprintf("agent name %q is already used by another Agent or Team; choose a different name", e.name)
}

func (e agentNameTakenError) Is(target error) bool { return target == ErrAgentNameTaken }

// ensureAgentNameAvailable refuses a name any non-retired Agent or Team in the
// scope already uses, including ones the requester cannot read. The error
// says only that the name is taken.
func ensureAgentNameAvailable(
	ctx context.Context,
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	scope Scope,
	name, exceptDeploymentID string,
) error {
	taken, err := takenAgentNames(ctx, agents, teams, scope, exceptDeploymentID)
	if err != nil {
		return err
	}
	if taken[normalizedAgentName(name)] {
		return agentNameTakenError{name: strings.TrimSpace(name)}
	}
	return nil
}

// takenAgentNames is the set of normalized names the scope's non-retired
// Agents (other than exceptDeploymentID) and non-archived Teams use.
func takenAgentNames(
	ctx context.Context,
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	scope Scope,
	exceptDeploymentID string,
) (map[string]bool, error) {
	taken := map[string]bool{}
	reference := agentBuilderScope(scope)
	deployments, err := agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{
		Scope: reference, ExcludeStatuses: []kernelagent.RolloutStatus{kernelagent.RolloutRetired},
	})
	if err != nil {
		return nil, err
	}
	for _, deployment := range deployments {
		if deployment == nil || deployment.ID == exceptDeploymentID {
			continue
		}
		if deployment.DisplayName != "" {
			taken[normalizedAgentName(deployment.DisplayName)] = true
		}
		definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
		if err != nil {
			return nil, err
		}
		if definition != nil {
			taken[normalizedAgentName(definition.DisplayName)] = true
		}
	}
	if teams == nil {
		return taken, nil
	}
	teamDeployments, err := teams.ListDeployments(ctx, reference)
	if err != nil {
		return nil, err
	}
	for _, deployment := range teamDeployments {
		if deployment == nil || deployment.Status == kernelteam.DeploymentArchived {
			continue
		}
		definition, err := teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
		if err != nil {
			return nil, err
		}
		if definition != nil {
			taken[normalizedAgentName(definition.DisplayName)] = true
		}
	}
	return taken, nil
}

func resolveAgentBuilderSpace(ctx context.Context, spaces *SpaceService, scope Scope, spaceID string) (*Space, error) {
	if spaceID == "" {
		return nil, errors.New("create_agent requires a space")
	}
	if spaces == nil {
		return nil, errors.New("spaces are not configured")
	}
	space, err := spaces.GetSpace(ctx, scope, spaceID)
	if errors.Is(err, ErrSpaceNotFound) || err == nil && space == nil {
		return nil, fmt.Errorf("space %q does not exist", spaceID)
	}
	if err != nil {
		return nil, err
	}
	return space, nil
}

// catalogGrantRisk is the binding ceiling allowedActions need: the highest
// Risk among them on the exact catalog definition, at least read. Unknown
// actions are left to validateSkillBindingArguments.
func catalogGrantRisk(definition *skill.Definition, allowedActions []string) skill.RiskLevel {
	ceiling := skill.RiskLevelRead
	if definition == nil {
		return ceiling
	}
	for _, name := range allowedActions {
		action, ok := definition.Actions[strings.TrimSpace(name)]
		if !ok {
			continue
		}
		if rank := action.Risk.Rank(); rank >= 0 && rank > ceiling.RankUnknownHighest() {
			ceiling = action.Risk
		}
	}
	return ceiling
}

func resolveAgentSkillGrants(ctx context.Context, catalog *skill.Catalog, grants []AgentSkillGrant) ([]resolvedAgentSkillGrant, error) {
	if len(grants) > maxAgentBuilderSkillGrants {
		return nil, fmt.Errorf("an agent can be given at most %d Skills at once", maxAgentBuilderSkillGrants)
	}
	result := make([]resolvedAgentSkillGrant, 0, len(grants))
	seen := map[string]bool{}
	for _, grant := range grants {
		grant.SkillID = strings.TrimSpace(grant.SkillID)
		grant.SkillVersion = strings.TrimSpace(grant.SkillVersion)
		grant.SourceIdentity = strings.TrimSpace(grant.SourceIdentity)
		if grant.SkillID == "" || grant.SkillVersion == "" {
			return nil, errors.New("each Skill needs a skillId and skillVersion")
		}
		if seen[grant.SkillID] {
			return nil, fmt.Errorf("Skill %s is listed more than once", grant.SkillID)
		}
		seen[grant.SkillID] = true
		if routineUngrantableSkill(grant.SkillID) {
			return nil, fmt.Errorf("Skill %s is a built-in management Skill the workspace binds itself; leave it out", grant.SkillID)
		}
		if catalog == nil {
			return nil, errors.New("skill catalog is not configured")
		}
		definition, err := exactSkillDefinition(ctx, catalog, grant.SkillID, grant.SkillVersion, grant.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition == nil {
			return nil, fmt.Errorf("Skill %s@%s is not registered; use openseal.skills.discover", grant.SkillID, grant.SkillVersion)
		}
		// The declared ceiling is never trusted: it is the riskiest allowed
		// action of the exact catalog definition.
		grant.MaximumRisk = catalogGrantRisk(definition, grant.AllowedActions)
		if err := validateSkillBindingArguments(definition, skillBindingUpsertArguments{
			SkillID: grant.SkillID, SkillVersion: grant.SkillVersion, SourceIdentity: grant.SourceIdentity,
			AllowedActions: grant.AllowedActions, EnablePrompt: grant.EnablePrompt,
			MaximumRisk: grant.MaximumRisk, Config: grant.Config,
		}, nil); err != nil {
			return nil, fmt.Errorf("Skill %s: %w; list it in connections instead", grant.SkillID, err)
		}
		result = append(result, resolvedAgentSkillGrant{grant: grant, definition: definition})
	}
	return result, nil
}

func resolveAgentConnections(ctx context.Context, catalog *skill.Catalog, requests []AgentConnectionRequest) ([]resolvedAgentConnection, error) {
	if len(requests) > maxAgentBuilderConnections {
		return nil, fmt.Errorf("an agent can list at most %d connections", maxAgentBuilderConnections)
	}
	result := make([]resolvedAgentConnection, 0, len(requests))
	seen := map[string]bool{}
	for _, request := range requests {
		request.SkillID = strings.TrimSpace(request.SkillID)
		request.SkillVersion = strings.TrimSpace(request.SkillVersion)
		request.SourceIdentity = strings.TrimSpace(request.SourceIdentity)
		if request.SkillID == "" || request.SkillVersion == "" {
			return nil, errors.New("each connection needs a skillId and skillVersion")
		}
		key := request.SkillID + "@" + request.SkillVersion + "#" + request.SourceIdentity
		if seen[key] {
			return nil, fmt.Errorf("connection %s is listed more than once", request.SkillID)
		}
		seen[key] = true
		if catalog == nil {
			return nil, errors.New("skill catalog is not configured")
		}
		definition, err := exactSkillDefinition(ctx, catalog, request.SkillID, request.SkillVersion, request.SourceIdentity)
		if err != nil {
			return nil, err
		}
		if definition == nil {
			return nil, fmt.Errorf("Skill %s@%s is not registered; use openseal.skills.discover", request.SkillID, request.SkillVersion)
		}
		result = append(result, resolvedAgentConnection{request: request, definition: definition})
	}
	return result, nil
}

func resolveAgentRemovedBindings(ctx context.Context, catalog *skill.Catalog, scope Scope, deploymentID string, bindingIDs []string) ([]resolvedAgentRemovedBinding, error) {
	result := make([]resolvedAgentRemovedBinding, 0, len(bindingIDs))
	seen := map[string]bool{}
	for _, bindingID := range bindingIDs {
		bindingID = strings.TrimSpace(bindingID)
		if bindingID == "" || seen[bindingID] {
			return nil, errors.New("removeSkillBindingIds must list distinct binding ids")
		}
		seen[bindingID] = true
		if strings.HasPrefix(bindingID, "bundled:") {
			return nil, fmt.Errorf("binding %s is built in and cannot be removed", bindingID)
		}
		if catalog == nil {
			return nil, errors.New("skill catalog is not configured")
		}
		binding, err := catalog.GetBinding(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID, bindingID)
		if errors.Is(err, skill.ErrBindingNotFound) || err == nil && (binding == nil || binding.Disabled) {
			return nil, fmt.Errorf("binding %s is not an enabled Skill of this Agent", bindingID)
		}
		if err != nil {
			return nil, err
		}
		name := binding.SkillID
		if definition, definitionErr := exactSkillDefinition(ctx, catalog, binding.SkillID, binding.SkillVersion, binding.SourceIdentity); definitionErr == nil && definition != nil && definition.Name != "" {
			name = definition.Name
		}
		result = append(result, resolvedAgentRemovedBinding{binding: binding, name: name})
	}
	return result, nil
}

func validateAgentBuilderModel(ctx context.Context, host AgentBuilderHost, scope Scope, model string) error {
	if model == "" {
		return nil
	}
	if host == nil {
		return errAgentModelUnavailable
	}
	return host.ValidateAgentModel(ctx, skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, model)
}

func decodeAgentBuilderArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid agent builder action: %w", err)
	}
	return nil
}

func agentBuilderTargetError(agentID string, err error) error {
	if errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		return fmt.Errorf("agent %q does not exist; use list_agents for the exact agentId", strings.TrimSpace(agentID))
	}
	return err
}

func agentProfileFromDefinition(definition *kernelagent.AgentDefinition) agentProfileArguments {
	return agentProfileArguments{
		DisplayName: definition.DisplayName, Purpose: definition.Purpose, SystemPrompt: definition.SystemPrompt,
		Personality: definition.Personality, OperatingPrinciples: append([]string(nil), definition.OperatingPrinciples...),
	}
}

// agentBuilderPreviewHead copies the generic approvalPreview labels, which the
// validator preview replaces.
func agentBuilderPreviewHead(bound *skill.BoundAction) map[string]interface{} {
	return map[string]interface{}{
		"skillId": bound.Definition.ID, "skillVersion": bound.Definition.Version, "action": bound.Action.Name,
		"risk": bound.Action.Risk, "sideEffect": bound.Action.SideEffect,
		"resourceType": agentProfileResourceType, "operation": bound.Action.Name,
	}
}

func agentProfilePreview(profile agentProfileArguments, deploymentID string) map[string]interface{} {
	principles := profile.OperatingPrinciples
	if principles == nil {
		principles = []string{}
	}
	result := map[string]interface{}{
		"displayName": profile.DisplayName, "purpose": profile.Purpose, "systemPrompt": profile.SystemPrompt,
		"personality": profile.Personality, "operatingPrinciples": principles,
	}
	if deploymentID != "" {
		result["deploymentId"] = deploymentID
	}
	return result
}

func agentSkillGrantsPreview(grants []resolvedAgentSkillGrant) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(grants))
	for _, resolved := range grants {
		actions := resolved.grant.AllowedActions
		if actions == nil {
			actions = []string{}
		}
		name := resolved.definition.Name
		if name == "" {
			name = resolved.grant.SkillID
		}
		result = append(result, map[string]interface{}{
			"skillId": resolved.grant.SkillID, "skillVersion": resolved.grant.SkillVersion,
			"sourceIdentity": resolved.grant.SourceIdentity, "name": name,
			"allowedActions": actions, "enablePrompt": resolved.grant.EnablePrompt,
			"maximumRisk": resolved.grant.MaximumRisk,
		})
	}
	return result
}

func agentCreatePreview(bound *skill.BoundAction, resolved *resolvedAgentCreate) (map[string]interface{}, error) {
	connections := make([]map[string]interface{}, 0, len(resolved.connections))
	for _, connection := range resolved.connections {
		name := connection.definition.Name
		if name == "" {
			name = connection.request.SkillID
		}
		connections = append(connections, map[string]interface{}{
			"skillId": connection.request.SkillID, "skillVersion": connection.request.SkillVersion, "name": name,
		})
	}
	preview := agentBuilderPreviewHead(bound)
	preview["agent"] = agentProfilePreview(resolved.args.AgentProfileArguments, "")
	preview["model"] = map[string]interface{}{"id": resolved.args.Model}
	preview["space"] = map[string]interface{}{"id": resolved.space.ID, "name": resolved.space.Name}
	preview["skills"] = agentSkillGrantsPreview(resolved.skills)
	preview["authority"] = map[string]interface{}{"maximumRisk": agentGrantsMaximumRisk("", resolved.skills)}
	preview["connections"] = connections
	preview["rationale"] = resolved.args.Rationale
	return agentBuilderJSONMap(preview)
}

func agentUpdatePreview(bound *skill.BoundAction, resolved *resolvedAgentUpdate) (map[string]interface{}, error) {
	removed := make([]map[string]interface{}, 0, len(resolved.removed))
	for _, binding := range resolved.removed {
		removed = append(removed, map[string]interface{}{
			"bindingId": binding.binding.ID, "skillId": binding.binding.SkillID, "name": binding.name,
		})
	}
	preview := agentBuilderPreviewHead(bound)
	preview["agent"] = agentProfilePreview(agentProfileFromDefinition(resolved.candidate), resolved.deployment.ID)
	preview["current"] = agentProfilePreview(agentProfileFromDefinition(resolved.definition), resolved.deployment.ID)
	preview["changes"] = resolved.changes
	if resolved.model != nil {
		// The kernel does not know the current model; the host may add it.
		preview["model"] = map[string]interface{}{"id": *resolved.model}
	}
	preview["skills"] = agentSkillGrantsPreview(resolved.skills)
	preview["authority"] = map[string]interface{}{
		"maximumRisk": resolved.candidate.Authority.MaximumRisk, "currentMaximumRisk": resolved.definition.Authority.MaximumRisk,
	}
	preview["removedSkills"] = removed
	preview["rationale"] = resolved.args.Rationale
	return agentBuilderJSONMap(preview)
}

// agentBuilderJSONMap returns value in its JSON form, the form the approval
// checkpoint stores, so the preview reads the same before and after a reload.
func agentBuilderJSONMap(value map[string]interface{}) (map[string]interface{}, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]interface{}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}
