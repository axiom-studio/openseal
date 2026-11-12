package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	TeamManagementSkillVersion = "2.2.0"
	TeamActionUpdateMember     = "update_member"
	TeamActionUpdateProfile    = "update_profile"
	TeamActionCreateTeam       = "create_team"
	TeamManagementEndpoint     = "kernel://teams"
)

// IsSupportedTeamManagementSkillVersion reports a version of openseal.teams
// whose update_profile action the kernel still runs. Bindings of the versions
// before update_member (2.1.0) and before create_team (2.0.0) keep matching
// it until the host reconcile upgrades them.
func IsSupportedTeamManagementSkillVersion(version string) bool {
	return version == TeamManagementSkillVersion || version == "2.1.0" || version == "2.0.0"
}

// TeamManagementSkill exposes bounded Team behavior changes to the current
// Team conversation, and Team creation to the Agent that builds Agents.
// Deployment identity and candidate definitions are never model inputs: the
// kernel derives both from the durable Run and active Team.
func TeamManagementSkill() *skill.Definition {
	return &skill.Definition{
		ID: TeamManagementSkillID, Version: TeamManagementSkillVersion,
		Name: "Teams", Description: "Create Teams from existing Agents, and propose governed changes to the current Team's profile and its members' behavior.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: TeamManagementEndpoint},
		Actions: map[string]skill.Action{
			TeamActionCreateTeam: teamCreateTeamAction(),
			TeamActionUpdateProfile: {
				Name:        TeamActionUpdateProfile,
				Description: "Change the current Team's name or description when the user asks the Team to rename or describe itself. Supply only the changed fields; each replaces the current value in full. The user approves the change before it applies.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Review: skill.ActionReviewAlways,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"displayName": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 120},
						"purpose":     map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 2000},
						"rationale":   map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"rationale"},
					"anyOf": []interface{}{
						map[string]interface{}{"required": []interface{}{"displayName"}},
						map[string]interface{}{"required": []interface{}{"purpose"}},
					},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": "team"},
						"operation":    map[string]interface{}{"type": "string", "const": TeamActionUpdateProfile},
						"deployment":   map[string]interface{}{"type": "object"},
						"definition":   map[string]interface{}{"type": "object"},
					},
					"required": []interface{}{"resourceType", "operation", "deployment", "definition"},
				},
			},
			TeamActionUpdateMember: {
				Name:        TeamActionUpdateMember,
				Description: "Propose changing one Team member's channel participation or purpose. memberId is the member's roster id. The kernel applies the change to the Team roster after required approval.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"memberId":                   map[string]interface{}{"type": "string", "minLength": 1},
						"expectedDeploymentRevision": map[string]interface{}{"type": "integer", "minimum": 1},
						"channelParticipation": map[string]interface{}{"type": "string", "enum": []interface{}{
							string(kernelteam.ChannelActive), string(kernelteam.ChannelObserveOnly), string(kernelteam.ChannelDisabled),
						}},
						"purpose": map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"memberId", "expectedDeploymentRevision"},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": "team"},
						"operation":    map[string]interface{}{"type": "string", "const": TeamActionUpdateMember},
						"replayed":     map[string]interface{}{"type": "boolean"},
						"deployment":   map[string]interface{}{"type": "object"},
						"activation":   map[string]interface{}{"type": "object"},
					},
					"required": []interface{}{"resourceType", "operation", "replayed", "deployment"},
				},
			},
		},
	}
}

type teamMemberActionArguments struct {
	MemberID                   string                           `json:"memberId"`
	ExpectedDeploymentRevision int64                            `json:"expectedDeploymentRevision"`
	ChannelParticipation       *kernelteam.ChannelParticipation `json:"channelParticipation,omitempty"`
	Purpose                    *string                          `json:"purpose,omitempty"`
}

// TeamMemberActionValidator resolves the active Team server-side and emits an
// exact, secret-free approval diff. It rejects stale, evaluated, or
// out-of-policy changes before an ActionCall can be persisted.
type TeamMemberActionValidator struct{ teams *kernelteam.Registry }

func NewTeamMemberActionValidator(teams *kernelteam.Registry) (*TeamMemberActionValidator, error) {
	if teams == nil {
		return nil, errors.New("team registry is required")
	}
	return &TeamMemberActionValidator{teams: teams}, nil
}

func (v *TeamMemberActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isTeamMemberAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.teams == nil || input.Run == nil {
		return nil, errors.New("team member action validator is not configured")
	}
	if input.Run.Owner.Type != OwnerTypeTeam {
		return nil, errors.New("team member actions require a Team-owned conversation Run")
	}
	args, deployment, _, member, err := resolveTeamMemberAction(ctx, v.teams, input.Run, input.Arguments)
	if err != nil {
		return nil, err
	}
	changes := teamMemberChanges(member, args)
	if len(changes) == 0 {
		return nil, errors.New("team member update requires at least one changed field")
	}
	return map[string]interface{}{
		"resourceType": "team", "operation": TeamActionUpdateMember,
		"deploymentId": deployment.ID, "expectedDeploymentRevision": deployment.Revision,
		"memberId": member.ID, "memberName": member.DisplayName,
		"current": map[string]interface{}{"purpose": member.Purpose, "channelParticipation": member.ChannelParticipation},
		"changes": changes,
	}, nil
}

// TeamMemberActionDispatcher applies the approved diff to the Team roster.
// The generic Action approval principal is reused only when it is also
// eligible under the Team definition's amendment policy.
type TeamMemberActionDispatcher struct {
	store    KernelStore
	teams    *kernelteam.Registry
	fallback ActionDispatcher
}

func NewTeamMemberActionDispatcher(store KernelStore, teams *kernelteam.Registry, fallback ActionDispatcher) (*TeamMemberActionDispatcher, error) {
	if store == nil || teams == nil {
		return nil, errors.New("kernel store and team registry are required")
	}
	return &TeamMemberActionDispatcher{store: store, teams: teams, fallback: fallback}, nil
}

func (d *TeamMemberActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isTeamMemberAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || d.teams == nil || input.Call == nil {
		return nil, errors.New("team member action dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	args, deployment, definition, _, err := resolveTeamMemberAction(ctx, d.teams, run, input.Arguments)
	if err != nil {
		// A retry after the roster update observes the next deployment revision.
		if errors.Is(err, ErrRevisionConflict) {
			return d.replayedResult(ctx, input.Call, run, args)
		}
		return nil, err
	}
	actorType, actorID := "agent", strings.TrimSpace(run.AssignedAgentID)
	if definition.Amendments.RequiresApproval {
		approval, err := approvedActionCheckpoint(ctx, d.store, input.Call)
		if err != nil {
			return nil, err
		}
		actorType, actorID = approval.DecisionBy.Type, approval.DecisionBy.ID
		if !kernelteam.AmendmentApproverEligible(definition, actorType, actorID) {
			return nil, errors.New("principal is not eligible to approve Team member changes")
		}
	}
	proposed := *deployment
	proposed.Roster = slices.Clone(deployment.Roster)
	for index := range proposed.Roster {
		if proposed.Roster[index].ID == args.MemberID {
			applyTeamMemberArguments(&proposed.Roster[index], args)
		}
	}
	updated, activation, err := d.teams.UpdateDeployment(ctx, &proposed, deployment.Revision, actorType, actorID, teamMemberActionReason(input.Call.ID))
	if err != nil {
		if errors.Is(err, kernelteam.ErrRevisionConflict) {
			return d.replayedResult(ctx, input.Call, run, args)
		}
		return nil, err
	}
	return teamMemberActionResult(updated, activation, false), nil
}

func resolveTeamMemberAction(ctx context.Context, teams *kernelteam.Registry, run *AgentRun, arguments map[string]interface{}) (teamMemberActionArguments, *kernelteam.Deployment, *kernelteam.Definition, kernelteam.RosterAssignment, error) {
	var args teamMemberActionArguments
	none := kernelteam.RosterAssignment{}
	if err := decodeTeamMemberArguments(arguments, &args); err != nil {
		return args, nil, nil, none, err
	}
	if strings.TrimSpace(args.MemberID) == "" || args.ExpectedDeploymentRevision < 1 || args.ChannelParticipation == nil && args.Purpose == nil {
		return args, nil, nil, none, errors.New("memberId, expectedDeploymentRevision, and at least one member change are required")
	}
	scope := capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	deployment, err := teams.GetDeployment(ctx, scope, run.Owner.ID)
	if err != nil {
		return args, nil, nil, none, err
	}
	onRoster, err := AgentOnChatRoster(ctx, newRoutineTeamRoster(teams), run.Scope, run.Owner, strings.TrimSpace(run.AssignedAgentID))
	if err != nil {
		return args, nil, nil, none, err
	}
	if !onRoster {
		return args, nil, nil, none, errors.New("team member action Agent is not on the active Team roster")
	}
	if deployment.Revision != args.ExpectedDeploymentRevision {
		return args, nil, nil, none, ErrRevisionConflict
	}
	definition, err := teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return args, nil, nil, none, err
	}
	if !definition.Amendments.AgentMayPropose || !containsString(definition.Amendments.AllowedFields, TeamMemberAmendmentField) {
		return args, nil, nil, none, errors.New("team definition policy does not allow Agent-proposed member changes")
	}
	if len(definition.Evaluations) > 0 {
		return args, nil, nil, none, errors.New("team member action requires definition evaluations and cannot be proposed as an immediate conversation action")
	}
	index := slices.IndexFunc(deployment.Roster, func(member kernelteam.RosterAssignment) bool { return member.ID == args.MemberID })
	if index < 0 {
		return args, nil, nil, none, errors.New("team member action target is not on the Team roster")
	}
	probe := *deployment
	probe.Roster = slices.Clone(deployment.Roster)
	applyTeamMemberArguments(&probe.Roster[index], args)
	if err := probe.Validate(definition); err != nil {
		return args, nil, nil, none, err
	}
	return args, deployment, definition, deployment.Roster[index], nil
}

func decodeTeamMemberArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid team member action: %w", err)
	}
	return nil
}

func teamMemberChanges(member kernelteam.RosterAssignment, args teamMemberActionArguments) map[string]interface{} {
	changes := make(map[string]interface{})
	if args.ChannelParticipation != nil && member.ChannelParticipation != *args.ChannelParticipation {
		changes["channelParticipation"] = *args.ChannelParticipation
	}
	if args.Purpose != nil && member.Purpose != strings.TrimSpace(*args.Purpose) {
		changes["purpose"] = strings.TrimSpace(*args.Purpose)
	}
	return changes
}

func applyTeamMemberArguments(member *kernelteam.RosterAssignment, args teamMemberActionArguments) {
	if args.ChannelParticipation != nil {
		member.ChannelParticipation = *args.ChannelParticipation
	}
	if args.Purpose != nil {
		member.Purpose = strings.TrimSpace(*args.Purpose)
	}
}

// teamMemberActionReason is the activation reason of the call's roster
// update; a retry finds its own update by it.
func teamMemberActionReason(callID string) string {
	return "Approved Team member change from conversation (action " + strings.TrimSpace(callID) + ")"
}

func approvedActionCheckpoint(ctx context.Context, store ActionStore, call *ActionCall) (*ApprovalCheckpoint, error) {
	if strings.TrimSpace(call.ApprovalID) == "" {
		return nil, errors.New("Team definition policy requires an approved action checkpoint")
	}
	approval, err := store.GetApproval(ctx, call.Scope, call.ApprovalID)
	if err != nil {
		return nil, err
	}
	if approval.Status != ApprovalStatusApproved || approval.DecisionBy == nil {
		return nil, errors.New("Team definition policy requires an approved action checkpoint with a decision principal")
	}
	return approval, nil
}

// replayedResult answers a retry whose roster update already committed: the
// next revision's activation carries this call's reason.
func (d *TeamMemberActionDispatcher) replayedResult(ctx context.Context, call *ActionCall, run *AgentRun, args teamMemberActionArguments) (map[string]interface{}, error) {
	scope := capability.ScopeReference{Kind: call.Scope.Kind, ID: call.Scope.ID}
	deployment, err := d.teams.GetDeployment(ctx, scope, run.Owner.ID)
	if err != nil {
		return nil, err
	}
	activations, err := d.teams.ListActivations(ctx, scope, deployment.ID)
	if err != nil {
		return nil, err
	}
	reason := teamMemberActionReason(call.ID)
	if deployment.Revision != args.ExpectedDeploymentRevision+1 || !slices.ContainsFunc(activations, func(activation workforce.DefinitionActivation) bool {
		return activation.DeploymentRevision == deployment.Revision && activation.Reason == reason
	}) {
		return nil, ErrRevisionConflict
	}
	return teamMemberActionResult(deployment, nil, true), nil
}

func teamMemberActionResult(deployment *kernelteam.Deployment, activation *workforce.DefinitionActivation, replayed bool) map[string]interface{} {
	result := map[string]interface{}{
		"resourceType": "team", "operation": TeamActionUpdateMember, "replayed": replayed, "deployment": deployment,
	}
	if activation != nil {
		result["activation"] = activation
	}
	return result
}

func isTeamMemberAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil && bound.Definition.ID == TeamManagementSkillID &&
		bound.Definition.Version == TeamManagementSkillVersion && bound.Action.Name == TeamActionUpdateMember
}
