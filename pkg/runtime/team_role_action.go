package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const (
	TeamManagementSkillID      = "openseal.teams"
	TeamManagementSkillVersion = "1.0.0"
	TeamActionUpdateRole       = "update_role"
	TeamManagementEndpoint     = "kernel://teams"
)

// TeamManagementSkill exposes bounded Team behavior changes to the current
// Team conversation. Deployment identity and candidate definitions are never
// model inputs: the kernel derives both from the durable Run and active Team.
func TeamManagementSkill() *skill.Definition {
	return &skill.Definition{
		ID: TeamManagementSkillID, Version: TeamManagementSkillVersion,
		Name: "Teams", Description: "Propose governed changes to the current Team's role behavior.",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: TeamManagementEndpoint},
		Actions: map[string]skill.Action{
			TeamActionUpdateRole: {
				Name:        TeamActionUpdateRole,
				Description: "Propose changing one role's channel participation or purpose. The kernel creates and activates a new immutable Team definition after required approval.",
				Risk:        skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2},
				InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"roleId":                     map[string]interface{}{"type": "string", "minLength": 1},
						"expectedDeploymentRevision": map[string]interface{}{"type": "integer", "minimum": 1},
						"channelParticipation": map[string]interface{}{"type": "string", "enum": []interface{}{
							string(kernelteam.RoleChannelActive), string(kernelteam.RoleChannelObserveOnly), string(kernelteam.RoleChannelDisabled),
						}},
						"purpose": map[string]interface{}{"type": "string", "minLength": 1},
					},
					"required": []interface{}{"roleId", "expectedDeploymentRevision"},
				},
				OutputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"resourceType": map[string]interface{}{"type": "string", "const": "team"},
						"operation":    map[string]interface{}{"type": "string", "const": TeamActionUpdateRole},
						"replayed":     map[string]interface{}{"type": "boolean"},
						"amendment":    map[string]interface{}{"type": "object"},
						"deployment":   map[string]interface{}{"type": "object"},
						"activation":   map[string]interface{}{"type": "object"},
					},
					"required": []interface{}{"resourceType", "operation", "replayed", "deployment"},
				},
			},
		},
	}
}

type teamRoleActionArguments struct {
	RoleID                     string                               `json:"roleId"`
	ExpectedDeploymentRevision int64                                `json:"expectedDeploymentRevision"`
	ChannelParticipation       *kernelteam.RoleChannelParticipation `json:"channelParticipation,omitempty"`
	Purpose                    *string                              `json:"purpose,omitempty"`
}

// TeamRoleActionValidator resolves the active Team server-side and emits an
// exact, secret-free approval diff. It rejects immutable, stale, evaluated, or
// out-of-policy changes before an ActionCall can be persisted.
type TeamRoleActionValidator struct{ teams *kernelteam.Registry }

func NewTeamRoleActionValidator(teams *kernelteam.Registry) (*TeamRoleActionValidator, error) {
	if teams == nil {
		return nil, errors.New("team registry is required")
	}
	return &TeamRoleActionValidator{teams: teams}, nil
}

func (v *TeamRoleActionValidator) ValidateActionProposal(ctx context.Context, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if !isTeamRoleAction(input.Bound) {
		return nil, nil
	}
	if v == nil || v.teams == nil || input.Run == nil {
		return nil, errors.New("team role action validator is not configured")
	}
	if input.Run.Owner.Type != OwnerTypeTeam {
		return nil, errors.New("team role actions require a Team-owned conversation Run")
	}
	args, deployment, definition, role, err := resolveTeamRoleAction(ctx, v.teams, input.Run, input.Arguments)
	if err != nil {
		return nil, err
	}
	changes := teamRoleChanges(role, args)
	if len(changes) == 0 {
		return nil, errors.New("team role update requires at least one changed field")
	}
	return map[string]interface{}{
		"resourceType": "team", "operation": TeamActionUpdateRole,
		"deploymentId": deployment.ID, "definitionId": definition.ID,
		"baseVersion": definition.Version, "expectedDeploymentRevision": deployment.Revision,
		"roleId": role.ID, "roleName": role.DisplayName,
		"current": map[string]interface{}{"purpose": role.Purpose, "channelParticipation": role.ChannelParticipation},
		"changes": changes,
	}, nil
}

// TeamRoleActionDispatcher materializes the approved diff through the existing
// immutable Team amendment lifecycle. The generic Action approval principal is
// reused only when it is also eligible under the Team definition policy.
type TeamRoleActionDispatcher struct {
	store    KernelStore
	teams    *kernelteam.Registry
	fallback ActionDispatcher
}

func NewTeamRoleActionDispatcher(store KernelStore, teams *kernelteam.Registry, fallback ActionDispatcher) (*TeamRoleActionDispatcher, error) {
	if store == nil || teams == nil {
		return nil, errors.New("kernel store and team registry are required")
	}
	return &TeamRoleActionDispatcher{store: store, teams: teams, fallback: fallback}, nil
}

func (d *TeamRoleActionDispatcher) DispatchAction(ctx context.Context, input ActionDispatchInput) (map[string]interface{}, error) {
	if !isTeamRoleAction(input.Bound) {
		if d == nil || d.fallback == nil {
			return nil, errors.New("action dispatcher does not support this action")
		}
		return d.fallback.DispatchAction(ctx, input)
	}
	if d == nil || d.store == nil || d.teams == nil || input.Call == nil {
		return nil, errors.New("team role action dispatcher is not configured")
	}
	run, err := d.store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil || run == nil {
		if err == nil {
			err = ErrRunNotFound
		}
		return nil, err
	}
	args, deployment, definition, _, err := resolveTeamRoleAction(ctx, d.teams, run, input.Arguments)
	if err != nil {
		// A retry after atomic activation observes the next deployment revision.
		if errors.Is(err, ErrRevisionConflict) {
			return d.replayedResult(ctx, input.Call, run, args)
		}
		return nil, err
	}
	candidateVersion := teamActionCandidateVersion(definition.Version, input.Call.ID)
	if deployment.ActiveVersion == candidateVersion {
		return teamRoleActionResult(nil, deployment, nil, true), nil
	}

	amendment, err := findTeamActionAmendment(ctx, d.teams, input.Call.Scope, deployment.ID, candidateVersion)
	if err != nil {
		return nil, err
	}
	if amendment == nil {
		candidate := cloneTeamDefinitionForAction(definition)
		candidate.Version = candidateVersion
		applyTeamRoleArguments(&candidate.Roles, args)
		amendment, err = d.teams.ProposeAmendment(ctx, kernelteam.ProposeAmendmentRequest{
			Scope: capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, DeploymentID: deployment.ID,
			Candidate: candidate, ProposerType: "agent", ProposerID: teamActionActorID(run),
			Rationale: "Team role behavior changed through conversation", EvidenceRefs: append([]string(nil), input.Call.EvidenceRefs...),
		})
		if err != nil {
			return nil, err
		}
	}
	if amendment.Status == kernelteam.AmendmentEvaluating {
		return nil, errors.New("team role action requires definition evaluations that cannot be skipped")
	}
	actorType, actorID := "agent", teamActionActorID(run)
	if amendment.Status == kernelteam.AmendmentAwaitingApproval {
		approval, approvalErr := approvedActionCheckpoint(ctx, d.store, input.Call)
		if approvalErr != nil {
			return nil, approvalErr
		}
		actorType, actorID = approval.DecisionBy.Type, approval.DecisionBy.ID
		amendment, err = d.teams.ResolveAmendment(ctx, kernelteam.ResolveAmendmentRequest{
			Scope: capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, AmendmentID: amendment.ID,
			ExpectedRevision: amendment.Revision, Approved: true, ActorType: actorType, ActorID: actorID,
			Reason: approval.DecisionReason,
		})
		if err != nil {
			return nil, err
		}
	}
	if amendment.Status != kernelteam.AmendmentReady && amendment.Status != kernelteam.AmendmentApproved {
		return nil, fmt.Errorf("team amendment is not ready after governed action approval: %s", amendment.Status)
	}
	amendment, deployment, activation, err := d.teams.ActivateAmendment(
		ctx, capability.ScopeReference{Kind: input.Call.Scope.Kind, ID: input.Call.Scope.ID}, amendment.ID, amendment.Revision,
		actorType, actorID, "Approved Team role change from conversation",
	)
	if err != nil {
		return nil, err
	}
	return teamRoleActionResult(amendment, deployment, activation, false), nil
}

func resolveTeamRoleAction(ctx context.Context, teams *kernelteam.Registry, run *AgentRun, arguments map[string]interface{}) (teamRoleActionArguments, *kernelteam.Deployment, *kernelteam.Definition, kernelteam.RoleSlot, error) {
	var args teamRoleActionArguments
	if err := decodeTeamRoleArguments(arguments, &args); err != nil {
		return args, nil, nil, kernelteam.RoleSlot{}, err
	}
	if strings.TrimSpace(args.RoleID) == "" || args.ExpectedDeploymentRevision < 1 || args.ChannelParticipation == nil && args.Purpose == nil {
		return args, nil, nil, kernelteam.RoleSlot{}, errors.New("roleId, expectedDeploymentRevision, and at least one role change are required")
	}
	scope := capability.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	deployment, err := teams.GetDeployment(ctx, scope, run.Owner.ID)
	if err != nil {
		return args, nil, nil, kernelteam.RoleSlot{}, err
	}
	if deployment.Revision != args.ExpectedDeploymentRevision {
		return args, nil, nil, kernelteam.RoleSlot{}, ErrRevisionConflict
	}
	definition, err := teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return args, nil, nil, kernelteam.RoleSlot{}, err
	}
	if !definition.Amendments.AgentMayPropose || !containsString(definition.Amendments.AllowedFields, "roles") {
		return args, nil, nil, kernelteam.RoleSlot{}, errors.New("team definition policy does not allow Agent-proposed role amendments")
	}
	if len(definition.Evaluations) > 0 {
		return args, nil, nil, kernelteam.RoleSlot{}, errors.New("team role action requires definition evaluations and cannot be proposed as an immediate conversation action")
	}
	for _, role := range definition.Roles {
		if role.ID == args.RoleID {
			candidate := role
			if args.ChannelParticipation != nil {
				candidate.ChannelParticipation = *args.ChannelParticipation
			}
			if args.Purpose != nil {
				candidate.Purpose = strings.TrimSpace(*args.Purpose)
			}
			probe := cloneTeamDefinitionForAction(definition)
			for index := range probe.Roles {
				if probe.Roles[index].ID == role.ID {
					probe.Roles[index] = candidate
				}
			}
			if err := probe.Validate(); err != nil {
				return args, nil, nil, kernelteam.RoleSlot{}, err
			}
			return args, deployment, definition, role, nil
		}
	}
	return args, nil, nil, kernelteam.RoleSlot{}, errors.New("team role action target does not exist in the active definition")
}

func decodeTeamRoleArguments(arguments map[string]interface{}, target interface{}) error {
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid team role action: %w", err)
	}
	return nil
}

func teamRoleChanges(role kernelteam.RoleSlot, args teamRoleActionArguments) map[string]interface{} {
	changes := make(map[string]interface{})
	if args.ChannelParticipation != nil && role.ChannelParticipation != *args.ChannelParticipation {
		changes["channelParticipation"] = *args.ChannelParticipation
	}
	if args.Purpose != nil && role.Purpose != strings.TrimSpace(*args.Purpose) {
		changes["purpose"] = strings.TrimSpace(*args.Purpose)
	}
	return changes
}

func applyTeamRoleArguments(roles *[]kernelteam.RoleSlot, args teamRoleActionArguments) {
	for index := range *roles {
		if (*roles)[index].ID != args.RoleID {
			continue
		}
		if args.ChannelParticipation != nil {
			(*roles)[index].ChannelParticipation = *args.ChannelParticipation
		}
		if args.Purpose != nil {
			(*roles)[index].Purpose = strings.TrimSpace(*args.Purpose)
		}
	}
}

func cloneTeamDefinitionForAction(value *kernelteam.Definition) *kernelteam.Definition {
	copyValue := *value
	copyValue.Roles = append([]kernelteam.RoleSlot(nil), value.Roles...)
	copyValue.OperatingPrinciples = append([]string(nil), value.OperatingPrinciples...)
	copyValue.Approvals.ApproverRoleIDs = append([]string(nil), value.Approvals.ApproverRoleIDs...)
	copyValue.Approvals.ApproverPrincipals = append([]string(nil), value.Approvals.ApproverPrincipals...)
	copyValue.Amendments.AllowedFields = append([]string(nil), value.Amendments.AllowedFields...)
	copyValue.Amendments.ApproverPrincipals = append([]string(nil), value.Amendments.ApproverPrincipals...)
	copyValue.Digest = ""
	copyValue.CreatedAt = copyValue.CreatedAt.UTC()
	return &copyValue
}

func teamActionCandidateVersion(baseVersion, actionID string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(actionID)))
	suffix := ".action." + hex.EncodeToString(digest[:6])
	baseVersion = strings.TrimSpace(baseVersion)
	if len(baseVersion)+len(suffix) > 128 {
		baseVersion = baseVersion[:128-len(suffix)]
	}
	return baseVersion + suffix
}

func findTeamActionAmendment(ctx context.Context, teams *kernelteam.Registry, scope Scope, deploymentID, version string) (*kernelteam.DefinitionAmendment, error) {
	amendments, err := teams.ListAmendments(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, deploymentID)
	if err != nil {
		return nil, err
	}
	for _, amendment := range amendments {
		if amendment.Candidate.Version == version {
			return amendment, nil
		}
	}
	return nil, nil
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

func (d *TeamRoleActionDispatcher) replayedResult(ctx context.Context, call *ActionCall, run *AgentRun, args teamRoleActionArguments) (map[string]interface{}, error) {
	deployment, err := d.teams.GetDeployment(ctx, capability.ScopeReference{Kind: call.Scope.Kind, ID: call.Scope.ID}, run.Owner.ID)
	if err != nil {
		return nil, err
	}
	definition, err := d.teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(call.ID)))
	if !strings.HasSuffix(deployment.ActiveVersion, ".action."+hex.EncodeToString(digest[:6])) {
		return nil, ErrRevisionConflict
	}
	for _, role := range definition.Roles {
		if role.ID == args.RoleID && len(teamRoleChanges(role, args)) == 0 {
			return teamRoleActionResult(nil, deployment, nil, true), nil
		}
	}
	return nil, ErrRevisionConflict
}

func teamRoleActionResult(amendment *kernelteam.DefinitionAmendment, deployment *kernelteam.Deployment, activation *workforce.DefinitionActivation, replayed bool) map[string]interface{} {
	result := map[string]interface{}{
		"resourceType": "team", "operation": TeamActionUpdateRole, "replayed": replayed, "deployment": deployment,
	}
	if amendment != nil {
		result["amendment"] = amendment
	}
	if activation != nil {
		result["activation"] = activation
	}
	return result
}

func teamActionActorID(run *AgentRun) string {
	if strings.TrimSpace(run.AssignedAgentID) != "" {
		return run.AssignedAgentID
	}
	return run.Owner.ID
}

func isTeamRoleAction(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil && bound.Definition.ID == TeamManagementSkillID &&
		bound.Definition.Version == TeamManagementSkillVersion && bound.Action.Name == TeamActionUpdateRole
}
