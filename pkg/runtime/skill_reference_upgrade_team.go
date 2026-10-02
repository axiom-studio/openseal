package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// This transformation only changes an existing exact identity. It never adds
// a role grant, merges action sets, changes a publisher, or restores revocation.
func deriveTeamSkillUpgradeDefinition(previous *kernelteam.Definition, from, to capability.SkillIdentity, now time.Time) (*kernelteam.Definition, []string, error) {
	if previous == nil || !from.Valid() || !to.Valid() || from.ID != to.ID {
		return nil, nil, ErrSkillReferenceUpgradeInvalid
	}
	candidate := cloneUpgradeTeamDefinition(previous)
	roles := make([]string, 0)
	changed := false
	for roleIndex := range candidate.Roles {
		role := &candidate.Roles[roleIndex]
		oldIndex, targetIndex := -1, -1
		for index, grant := range role.SkillGrants {
			if grant.ExactIdentity().Equal(from) {
				oldIndex = index
			}
			if grant.ExactIdentity().Equal(to) {
				targetIndex = index
			}
		}
		if oldIndex < 0 {
			if targetIndex >= 0 {
				roles = append(roles, role.ID)
			}
			continue
		}
		if from.SourceIdentity != to.SourceIdentity {
			return nil, nil, fmt.Errorf("%w: Team role %s cannot inherit a different Skill publisher", ErrSkillReferenceUpgradeInvalid, role.ID)
		}
		replacement := role.SkillGrants[oldIndex]
		replacement.SkillVersion = to.Version
		if replacement.RuntimeIdentity != nil || to.SourceIdentity != "" {
			identity := to
			replacement.RuntimeIdentity = &identity
		}
		if targetIndex >= 0 && targetIndex != oldIndex {
			if !equalTeamSkillGrantAuthority(replacement, role.SkillGrants[targetIndex]) {
				return nil, nil, fmt.Errorf("%w: Team role %s has conflicting target Skill authority", ErrSkillReferenceUpgradeInvalid, role.ID)
			}
			role.SkillGrants = append(role.SkillGrants[:oldIndex], role.SkillGrants[oldIndex+1:]...)
		} else {
			role.SkillGrants[oldIndex] = replacement
		}
		roles = append(roles, role.ID)
		changed = true
	}
	if len(roles) == 0 {
		return nil, nil, fmt.Errorf("%w: active Team definition has no exact Skill authority to upgrade", ErrSkillReferenceUpgradeInvalid)
	}
	sort.Strings(roles)
	if !changed {
		return candidate, roles, nil
	}
	// Version identity depends on all preserved behavior and authority, not on
	// deployment IDs, actor timestamps, or how often a controller retried.
	candidate.Version = "skill-upgrade"
	candidate, err := kernelteam.PrepareDefinition(candidate, time.Time{})
	if err != nil {
		return nil, nil, err
	}
	candidate.Version, candidate.Digest, candidate.CreatedAt = "", "", time.Time{}
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return nil, nil, err
	}
	digest := sha256.Sum256(encoded)
	candidate.Version = "skill-upgrade-" + hex.EncodeToString(digest[:])
	candidate, err = kernelteam.PrepareDefinition(candidate, now)
	return candidate, roles, err
}

func equalTeamSkillGrantAuthority(left, right kernelteam.RoleSkillGrant) bool {
	if !left.ExactIdentity().Equal(right.ExactIdentity()) || left.SkillID != right.SkillID || left.SkillVersion != right.SkillVersion ||
		left.CatalogID != right.CatalogID || left.EnablePrompt != right.EnablePrompt || left.MaximumRisk != right.MaximumRisk ||
		len(left.AllowedActions) != len(right.AllowedActions) {
		return false
	}
	actions := make(map[string]bool, len(left.AllowedActions))
	for _, action := range left.AllowedActions {
		actions[action] = true
	}
	for _, action := range right.AllowedActions {
		if !actions[action] {
			return false
		}
	}
	return true
}

func skillUpgradeTeamDefinitionEqual(left, right *kernelteam.Definition) bool {
	if left == nil || right == nil {
		return left == right
	}
	before, after := cloneUpgradeTeamDefinition(left), cloneUpgradeTeamDefinition(right)
	before.CreatedAt, after.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(before, after)
}

func validateCurrentSkillUpgradeTeamAuthority(deployment *kernelteam.Deployment, definition *kernelteam.Definition, mutation *SkillReferenceTeamAuthorityMutation) error {
	if mutation == nil || deployment == nil || definition == nil || !reflect.DeepEqual(deployment, mutation.PreviousDeployment) ||
		!skillUpgradeTeamDefinitionEqual(definition, mutation.PreviousDefinition) || deployment.Status == kernelteam.DeploymentArchived || deployment.Activation != nil {
		return ErrSkillReferenceUpgradeConflict
	}
	return nil
}

func validateSkillReferenceTeamAuthorityMutation(application *SkillReferenceUpgradeMutation) error {
	impact, mutation := application.Plan.TeamAuthority, application.TeamAuthority
	if impact == nil {
		if mutation != nil {
			return ErrSkillReferenceUpgradeInvalid
		}
		return nil
	}
	if mutation == nil || mutation.PreviousDeployment == nil || mutation.PreviousDefinition == nil {
		return ErrSkillReferenceUpgradeInvalid
	}
	previous, definition := mutation.PreviousDeployment, mutation.PreviousDefinition
	if previous.Scope != (capability.ScopeReference{Kind: application.Plan.Scope.Kind, ID: application.Plan.Scope.ID}) ||
		previous.ID != application.Plan.DeploymentID || previous.ID != impact.DeploymentID || previous.Revision != impact.ExpectedRevision ||
		previous.DefinitionID != impact.DefinitionID || previous.ActiveVersion != impact.DefinitionVersion ||
		definition.ID != impact.DefinitionID || definition.Version != impact.DefinitionVersion || definition.Digest != impact.DefinitionDigest ||
		previous.Activation != nil || previous.Status == kernelteam.DeploymentArchived {
		return ErrSkillReferenceUpgradeInvalid
	}
	if impact.TargetDefinitionVersion == impact.DefinitionVersion {
		if impact.TargetDefinitionDigest != impact.DefinitionDigest || mutation.Definition != nil || mutation.Deployment != nil || mutation.Activation != nil {
			return ErrSkillReferenceUpgradeInvalid
		}
		return nil
	}
	derived, roles, err := deriveTeamSkillUpgradeDefinition(definition,
		capability.NewSkillIdentity(application.Plan.From.ID, application.Plan.From.Version, application.Plan.From.SourceIdentity),
		capability.NewSkillIdentity(application.Plan.To.ID, application.Plan.To.Version, application.Plan.To.SourceIdentity), application.Receipt.AppliedAt)
	if err != nil || !skillUpgradeTeamDefinitionEqual(derived, mutation.Definition) || !reflect.DeepEqual(roles, impact.AuthorizedRoleIDs) ||
		derived.Version != impact.TargetDefinitionVersion || derived.Digest != impact.TargetDefinitionDigest || mutation.Deployment == nil || mutation.Activation == nil {
		return ErrSkillReferenceUpgradeInvalid
	}
	deployment := cloneUpgradeTeamDeployment(previous)
	deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt = derived.Version, previous.Revision+1, application.Receipt.AppliedAt
	if !reflect.DeepEqual(deployment, mutation.Deployment) {
		return ErrSkillReferenceUpgradeInvalid
	}
	if err := derived.Validate(); err != nil {
		return fmt.Errorf("%w: derived Team definition: %v", ErrSkillReferenceUpgradeInvalid, err)
	}
	if err := deployment.Validate(derived); err != nil {
		return fmt.Errorf("%w: derived Team deployment: %v", ErrSkillReferenceUpgradeInvalid, err)
	}
	activation := mutation.Activation
	if activation.ID != "skill-upgrade-"+application.Plan.Digest || activation.Scope != previous.Scope || activation.DeploymentID != previous.ID ||
		activation.DefinitionID != previous.DefinitionID || activation.FromVersion != previous.ActiveVersion || activation.ToVersion != derived.Version ||
		activation.DeploymentRevision != deployment.Revision || activation.ActorType != application.Receipt.Actor.Type ||
		activation.ActorID != application.Receipt.Actor.ID || activation.Reason != application.Receipt.Reason || activation.CreatedAt != application.Receipt.AppliedAt {
		return ErrSkillReferenceUpgradeInvalid
	}
	return nil
}

func (s *SkillReferenceUpgradeService) materializeTeamSkillReferenceAuthority(ctx context.Context, plan *SkillReferenceUpgradePlan, actor ActivityActor, reason string, now time.Time) (*SkillReferenceTeamAuthorityMutation, error) {
	if plan.TeamAuthority == nil {
		return nil, nil
	}
	impact := plan.TeamAuthority
	previous, err := s.teams.GetDeployment(ctx, capability.ScopeReference{Kind: plan.Scope.Kind, ID: plan.Scope.ID}, impact.DeploymentID)
	if err != nil || previous == nil || previous.Revision != impact.ExpectedRevision || previous.DefinitionID != impact.DefinitionID || previous.ActiveVersion != impact.DefinitionVersion {
		return nil, ErrSkillReferenceUpgradeConflict
	}
	definition, err := s.teams.GetDefinition(ctx, impact.DefinitionID, impact.DefinitionVersion)
	if err != nil || definition == nil || definition.Digest != impact.DefinitionDigest {
		return nil, ErrSkillReferenceUpgradeConflict
	}
	mutation := &SkillReferenceTeamAuthorityMutation{PreviousDeployment: cloneUpgradeTeamDeployment(previous), PreviousDefinition: cloneUpgradeTeamDefinition(definition)}
	if impact.TargetDefinitionVersion == impact.DefinitionVersion {
		return mutation, nil
	}
	derived, _, err := deriveTeamSkillUpgradeDefinition(definition,
		capability.NewSkillIdentity(plan.From.ID, plan.From.Version, plan.From.SourceIdentity),
		capability.NewSkillIdentity(plan.To.ID, plan.To.Version, plan.To.SourceIdentity), now)
	if err != nil || derived == nil || derived.Version != impact.TargetDefinitionVersion || derived.Digest != impact.TargetDefinitionDigest {
		return nil, ErrSkillReferenceUpgradeConflict
	}
	deployment := cloneUpgradeTeamDeployment(previous)
	deployment.ActiveVersion, deployment.Revision, deployment.UpdatedAt = derived.Version, previous.Revision+1, now
	mutation.Definition, mutation.Deployment = derived, deployment
	mutation.Activation = &workforce.DefinitionActivation{
		ID: "skill-upgrade-" + plan.Digest, Scope: previous.Scope, DeploymentID: previous.ID, DefinitionID: previous.DefinitionID,
		FromVersion: previous.ActiveVersion, ToVersion: derived.Version, DeploymentRevision: deployment.Revision,
		ActorType: actor.Type, ActorID: actor.ID, Reason: reason, CreatedAt: now,
	}
	return mutation, nil
}

func cloneUpgradeTeamDeployment(value *kernelteam.Deployment) *kernelteam.Deployment {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result kernelteam.Deployment
	_ = json.Unmarshal(encoded, &result)
	return &result
}

func cloneUpgradeTeamDefinition(value *kernelteam.Definition) *kernelteam.Definition {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result kernelteam.Definition
	_ = json.Unmarshal(encoded, &result)
	return &result
}
