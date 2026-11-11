package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/validation"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	teamResourceType          = "team"
	teamDefinitionVersion     = "1"
	teamCreateReason          = "Created in chat after review"
	maxTeamDisplayNameRunes   = 120
	maxTeamPurposeRunes       = 2000
	minTeamCreateMembers      = 2
	maxTeamCreateMembers      = 12
	maxTeamMemberIDCharacters = 128
)

func teamCreateTeamAction() skill.Action {
	return skill.Action{
		Name:        TeamActionCreateTeam,
		Description: "Create a Team from two or more existing Agents, after the person reviews it. Use openseal.agents.list_agents for the exact Agent ids; you cannot be a member. Choose a short, distinctive name that no current Agent or Team uses.",
		Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
		Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"displayName": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxTeamDisplayNameRunes},
				"purpose":     map[string]interface{}{"type": "string", "maxLength": maxTeamPurposeRunes},
				"members": map[string]interface{}{
					"type": "array", "minItems": minTeamCreateMembers, "maxItems": maxTeamCreateMembers, "uniqueItems": true,
					"items": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maxTeamMemberIDCharacters},
				},
				"spaceId":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 128, skill.SchemaExtensionKernelResolved: true},
				"rationale": map[string]interface{}{"type": "string", "minLength": 1},
			},
			"required": []interface{}{"displayName", "members", "rationale"},
		},
		OutputSchema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"resourceType": map[string]interface{}{"type": "string", "const": teamResourceType},
				"operation":    map[string]interface{}{"type": "string", "const": TeamActionCreateTeam},
				"replayed":     map[string]interface{}{"type": "boolean"},
				"deploymentId": map[string]interface{}{"type": "string"},
				"displayName":  map[string]interface{}{"type": "string"},
				"definition":   map[string]interface{}{"type": "object"},
				"deployment":   map[string]interface{}{"type": "object"},
				"space":        map[string]interface{}{"type": "object"},
			},
			"required": []interface{}{"resourceType", "operation", "replayed", "definition", "deployment", "space"},
		},
	}
}

type teamCreateArguments struct {
	DisplayName string   `json:"displayName"`
	Purpose     string   `json:"purpose,omitempty"`
	Members     []string `json:"members"`
	SpaceID     string   `json:"spaceId,omitempty"`
	Rationale   string   `json:"rationale"`
}

// isTeamCreateAction reports openseal.teams create_team. Only the current
// version has it.
func isTeamCreateAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil && bound.Definition.ID == TeamManagementSkillID &&
		bound.Definition.Version == TeamManagementSkillVersion && bound.Action.Name == TeamActionCreateTeam
}

// TeamCreateActionValidator checks create_team before a card exists and
// returns the secret-free preview the review card renders.
type TeamCreateActionValidator struct {
	builder *AgentBuilderActionValidator
}

// NewTeamCreateActionValidator needs the Agent and Team registries and the
// spaces. conversations and runs place the Team in the conversation's space;
// without them it goes to Default.
func NewTeamCreateActionValidator(
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	spaces *SpaceService,
	conversations ConversationStore,
	runs PortfolioStore,
) (*TeamCreateActionValidator, error) {
	if agents == nil || teams == nil || spaces == nil {
		return nil, errors.New("team create validator requires the Agent and Team registries and spaces")
	}
	return &TeamCreateActionValidator{builder: &AgentBuilderActionValidator{
		agents: agents, teams: teams, spaces: spaces, conversations: conversations, runs: runs,
	}}, nil
}

// ResolveActionProposalArguments fills spaceId with the Run's conversation
// space, else Default.
func (v *TeamCreateActionValidator) ResolveActionProposalArguments(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, bool, error) {
	if !isTeamCreateAction(input.Bound) {
		return nil, false, nil
	}
	if v == nil || v.builder == nil || input.Run == nil {
		return nil, true, errors.New("team create validator is not configured")
	}
	arguments := cloneMap(input.Arguments)
	if arguments == nil {
		arguments = map[string]interface{}{}
	}
	if spaceID, _ := arguments["spaceId"].(string); strings.TrimSpace(spaceID) == "" {
		resolved, err := v.builder.runSpaceID(ctx, input.Run)
		if err != nil {
			return nil, true, err
		}
		arguments["spaceId"] = resolved
	}
	return arguments, true, nil
}

// ValidateActionProposal returns the create_team card preview.
func (v *TeamCreateActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isTeamCreateAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.builder == nil || input.Run == nil || input.Bound.Binding == nil {
		return nil, errors.New("team create validator is not configured")
	}
	builderID := strings.TrimSpace(input.Run.AssignedAgentID)
	if builderID == "" || input.Bound.Binding.DeploymentID != builderID {
		return nil, errors.New("create_team is not bound to the current Run's Agent deployment")
	}
	resolved, err := resolveTeamCreate(ctx, v.builder.agents, v.builder.teams, input.Run.Scope, builderID, input.Arguments)
	if err != nil {
		return nil, err
	}
	space, err := resolveTeamSpace(ctx, v.builder.spaces, input.Run.Scope, resolved.args.SpaceID)
	if err != nil {
		return nil, err
	}
	if err := v.builder.spaces.CheckSpaceAgents(ctx, input.Run.Scope, space.ID, resolved.args.Members); err != nil {
		return nil, err
	}
	members := make([]map[string]interface{}, 0, len(resolved.members))
	for _, member := range resolved.members {
		members = append(members, map[string]interface{}{
			"agentId": member.deployment.ID, "displayName": member.name, "purpose": member.definition.Purpose,
		})
	}
	preview := agentBuilderPreviewHead(input.Bound)
	preview["resourceType"] = teamResourceType
	preview["team"] = map[string]interface{}{"displayName": resolved.args.DisplayName, "purpose": resolved.args.Purpose}
	preview["members"] = members
	preview["space"] = map[string]interface{}{"id": space.ID, "name": space.Name}
	preview["approvals"] = map[string]interface{}{"maximumRisk": teamMembersMaximumRisk(resolved.members)}
	preview["rationale"] = resolved.args.Rationale
	return agentBuilderJSONMap(preview)
}

type resolvedTeamMember struct {
	deployment *kernelagent.AgentDeployment
	definition *kernelagent.AgentDefinition
	name       string
}

type resolvedTeamCreate struct {
	args    teamCreateArguments
	members []resolvedTeamMember
}

// resolveTeamCreate decodes and checks create_team: the name is free across
// Agents and Teams, and every member is a distinct, current Agent other than
// the builder.
func resolveTeamCreate(
	ctx context.Context,
	agents *kernelagent.Registry,
	teams *kernelteam.Registry,
	scope Scope,
	builderID string,
	arguments map[string]interface{},
) (*resolvedTeamCreate, error) {
	args, err := decodeTeamCreateArguments(arguments)
	if err != nil {
		return nil, err
	}
	if err := validateTeamCreateText(args); err != nil {
		return nil, err
	}
	if len(args.Members) < minTeamCreateMembers || len(args.Members) > maxTeamCreateMembers {
		return nil, fmt.Errorf("a Team needs %d to %d member Agents", minTeamCreateMembers, maxTeamCreateMembers)
	}
	if err := ensureAgentNameAvailable(ctx, agents, teams, scope, args.DisplayName, ""); err != nil {
		return nil, err
	}
	members := make([]resolvedTeamMember, 0, len(args.Members))
	seen := map[string]bool{}
	for _, memberID := range args.Members {
		if memberID == "" {
			return nil, errors.New("each Team member needs an Agent id")
		}
		if memberID == builderID {
			return nil, errors.New("you cannot be a member of a Team you create; choose other Agents")
		}
		if seen[memberID] {
			return nil, fmt.Errorf("agent %q is listed more than once", memberID)
		}
		seen[memberID] = true
		deployment, err := agents.GetDeployment(ctx, agentBuilderScope(scope), memberID)
		if err != nil {
			return nil, agentBuilderTargetError(memberID, err)
		}
		if deployment.RolloutStatus == kernelagent.RolloutRetired {
			return nil, fmt.Errorf("agent %q is retired and cannot join a Team", memberID)
		}
		definition, err := agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
		if err != nil {
			return nil, err
		}
		name := strings.TrimSpace(agentDeploymentDisplayName(deployment, definition))
		if name == "" {
			name = deployment.ID
		}
		members = append(members, resolvedTeamMember{deployment: deployment, definition: definition, name: name})
	}
	return &resolvedTeamCreate{args: args, members: members}, nil
}

func decodeTeamCreateArguments(arguments map[string]interface{}) (teamCreateArguments, error) {
	var args teamCreateArguments
	if err := decodeAgentBuilderArguments(arguments, &args); err != nil {
		return args, err
	}
	args.DisplayName = strings.TrimSpace(args.DisplayName)
	args.Purpose = strings.TrimSpace(args.Purpose)
	args.SpaceID = strings.TrimSpace(args.SpaceID)
	args.Rationale = strings.TrimSpace(args.Rationale)
	for index := range args.Members {
		args.Members[index] = strings.TrimSpace(args.Members[index])
	}
	return args, nil
}

func validateTeamCreateText(args teamCreateArguments) error {
	if args.DisplayName == "" || utf8.RuneCountInString(args.DisplayName) > maxTeamDisplayNameRunes ||
		strings.IndexFunc(args.DisplayName, unicode.IsControl) >= 0 {
		return fmt.Errorf("team name must contain 1 to %d characters without control characters", maxTeamDisplayNameRunes)
	}
	if utf8.RuneCountInString(args.Purpose) > maxTeamPurposeRunes {
		return fmt.Errorf("team purpose must be at most %d characters", maxTeamPurposeRunes)
	}
	if args.Rationale == "" {
		return errors.New("create_team requires a rationale")
	}
	for _, value := range []string{args.DisplayName, args.Purpose} {
		if err := validation.ValidateNoSensitiveInput(value); err != nil {
			return err
		}
	}
	return nil
}

func resolveTeamSpace(ctx context.Context, spaces *SpaceService, scope Scope, spaceID string) (*Space, error) {
	if spaceID == "" {
		return nil, errors.New("create_team requires a space")
	}
	return resolveAgentBuilderSpace(ctx, spaces, scope, spaceID)
}

// teamBuilderDefinitionID is "team-<slug>-<8 hex>", derived from the call so a
// replay registers nothing new.
func teamBuilderDefinitionID(scope Scope, displayName, callID string) string {
	digest := sha256.Sum256([]byte(scope.Kind + ":" + scope.ID + ":" + callID))
	return "team-" + kernelteam.RoleSlug(displayName) + "-" + hex.EncodeToString(digest[:])[:8]
}

// teamBuilderDefinition builds the Team the way HelloSeal's team composer
// does: one active role per member, required to run that member's
// definition.
func teamBuilderDefinition(id string, args teamCreateArguments, members []resolvedTeamMember, callID, approver string) *kernelteam.Definition {
	purpose := args.Purpose
	if purpose == "" {
		purpose = args.DisplayName + " team"
	}
	roles := make([]kernelteam.RoleSlot, 0, len(members))
	for index, member := range members {
		rolePurpose := strings.TrimSpace(member.definition.Purpose)
		if rolePurpose == "" {
			rolePurpose = member.name + " team member"
		}
		roles = append(roles, kernelteam.RoleSlot{
			ID: fmt.Sprintf("%s-%d", kernelteam.RoleSlug(member.name), index+1), DisplayName: member.name, Purpose: rolePurpose,
			MinimumMembers: 1, MaximumMembers: 1, RequiredDefinitionIDs: []string{member.definition.ID},
			ChannelParticipation: kernelteam.RoleChannelActive,
		})
	}
	return &kernelteam.Definition{
		ID: id, Version: teamDefinitionVersion, DisplayName: args.DisplayName, Purpose: purpose, Roles: roles,
		Approvals:  kernelteam.ApprovalPolicy{MaximumRisk: teamMembersMaximumRisk(members)},
		Provenance: workforce.DefinitionProvenance{Source: agentBuilderProvenanceSource, Reference: callID, CreatedBy: approver},
	}
}

// teamMembersMaximumRisk is the Team's approval ceiling: the highest member
// Agent ceiling, at least write. Each member's own ceiling still caps what it
// may do with the Team's Skills (teamSkillAuthority).
func teamMembersMaximumRisk(members []resolvedTeamMember) capability.RiskLevel {
	ceiling := capability.RiskLevelWrite
	for _, member := range members {
		risk := member.definition.Authority.MaximumRisk
		if teamSkillRiskRank(risk) < 1<<30 && teamSkillRiskRank(risk) > teamSkillRiskRank(ceiling) {
			ceiling = risk
		}
	}
	return ceiling
}

// createTeam runs an approved create_team. Ids come from the call, so a
// retry or a crash-resumed execution converges on the same Team.
func (d *AgentBuilderActionDispatcher) createTeam(ctx context.Context, input ActionDispatchInput, run *AgentRun) (map[string]interface{}, error) {
	call := input.Call
	approval, err := approvedAgentBuilderCheckpoint(ctx, d.store, call)
	if err != nil {
		return nil, err
	}
	scope := call.Scope
	reference := agentBuilderScope(scope)
	approver := approval.DecisionBy.ID
	args, err := decodeTeamCreateArguments(input.Arguments)
	if err != nil {
		return nil, err
	}
	teamID := teamBuilderDefinitionID(scope, args.DisplayName, call.ID)
	deployment, err := d.teams.GetDeployment(ctx, reference, teamID)
	if err != nil && !errors.Is(err, kernelteam.ErrDeploymentNotFound) {
		return nil, err
	}
	replayed := deployment != nil
	space, err := d.teamSpace(ctx, scope, args.SpaceID)
	if err != nil {
		return nil, err
	}
	var definition *kernelteam.Definition
	var activation *workforce.DefinitionActivation
	if replayed {
		if deployment.DefinitionID != teamID {
			return nil, fmt.Errorf("team deployment %s already exists for another definition", teamID)
		}
		if definition, err = d.teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion); err != nil {
			return nil, err
		}
	} else {
		// The name may have been taken, or a member retired, while the card
		// waited.
		resolved, err := resolveTeamCreate(ctx, d.agents, d.teams, scope, strings.TrimSpace(run.AssignedAgentID), input.Arguments)
		if err != nil {
			return nil, fmt.Errorf("%w. Propose create_team again", err)
		}
		if definition, err = d.registerTeamBuilderDefinition(ctx, teamBuilderDefinition(teamID, resolved.args, resolved.members, call.ID, approver)); err != nil {
			return nil, err
		}
		if len(definition.Roles) != len(args.Members) {
			return nil, fmt.Errorf("team definition %s does not match the reviewed members", teamID)
		}
		roster := make([]kernelteam.RosterAssignment, 0, len(definition.Roles))
		for index, role := range definition.Roles {
			roster = append(roster, kernelteam.RosterAssignment{
				ID: role.ID, RoleID: role.ID, AgentDeploymentID: args.Members[index], DisplayName: role.DisplayName,
			})
		}
		deployment, activation, err = d.provision.CreateTeamDeploymentInSpace(ctx, space.ID, &kernelteam.Deployment{
			ID: teamID, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			Roster: roster, Status: kernelteam.DeploymentActive, Revision: 1,
		}, "user", approver, teamCreateReason)
		if err != nil {
			return nil, err
		}
	}
	if err := d.placeTeamInSpace(ctx, scope, space, teamID); err != nil {
		return nil, err
	}
	result := map[string]interface{}{
		"resourceType": teamResourceType, "operation": TeamActionCreateTeam,
		"deploymentId": deployment.ID, "displayName": definition.DisplayName, "replayed": replayed,
		"definition": definition, "deployment": deployment,
		"space":   map[string]interface{}{"id": space.ID, "name": space.Name},
		"message": fmt.Sprintf("Created the Team %s. Offer 'Chat with %s'.", definition.DisplayName, definition.DisplayName),
	}
	if activation != nil {
		result["activation"] = activation
	}
	return agentBuilderJSONMap(result)
}

// registerTeamBuilderDefinition registers definition, or returns the one an
// earlier attempt of the same call registered.
func (d *AgentBuilderActionDispatcher) registerTeamBuilderDefinition(ctx context.Context, definition *kernelteam.Definition) (*kernelteam.Definition, error) {
	sameCall := func(existing *kernelteam.Definition) bool {
		return existing != nil && existing.Provenance.Source == agentBuilderProvenanceSource &&
			existing.Provenance.Reference == definition.Provenance.Reference
	}
	existing, err := d.teams.GetDefinition(ctx, definition.ID, definition.Version)
	if err == nil && existing != nil {
		if !sameCall(existing) {
			return nil, fmt.Errorf("team definition %s already exists for another change", definition.ID)
		}
		return existing, nil
	}
	if err != nil && !errors.Is(err, kernelteam.ErrDefinitionNotFound) {
		return nil, err
	}
	registered, err := d.teams.RegisterDefinition(ctx, definition)
	if err != nil {
		// A concurrent attempt of the same call may have registered it.
		if existing, getErr := d.teams.GetDefinition(ctx, definition.ID, definition.Version); getErr == nil && sameCall(existing) {
			return existing, nil
		}
		return nil, err
	}
	return registered, nil
}

// teamSpace resolves the reviewed space at execution. A space deleted while
// the card waited fails the call.
func (d *AgentBuilderActionDispatcher) teamSpace(ctx context.Context, scope Scope, spaceID string) (*Space, error) {
	if spaceID == "" {
		return nil, errors.New("create_team requires a space")
	}
	if spaceID == DefaultSpaceID(scope) {
		return &Space{ID: spaceID, Scope: scope, Name: DefaultSpaceName, IsDefault: true}, nil
	}
	spaces, err := d.provision.ListSpaces(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, space := range spaces {
		if space != nil && space.ID == spaceID {
			return space, nil
		}
	}
	return nil, fmt.Errorf("space %q no longer exists; propose create_team again", spaceID)
}

// placeTeamInSpace keeps a replayed Team in the reviewed space only: the
// create places it there, or in Default when it could not reach the space.
func (d *AgentBuilderActionDispatcher) placeTeamInSpace(ctx context.Context, scope Scope, space *Space, teamID string) error {
	if _, _, err := d.provision.PutSpaceMember(ctx, PutSpaceMemberRequest{
		Scope: scope, SpaceID: space.ID, Type: SpaceMemberTeam, MemberID: teamID,
	}); err != nil {
		return err
	}
	defaultSpace := DefaultSpaceID(scope)
	if space.ID == defaultSpace {
		return nil
	}
	if err := d.provision.DeleteSpaceMember(ctx, DeleteSpaceMemberRequest{Scope: scope, SpaceID: defaultSpace, Type: SpaceMemberTeam, MemberID: teamID}); err != nil && !errors.Is(err, ErrSpaceNotFound) {
		return err
	}
	return nil
}
