package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *SQLiteStore) ValidateChangeSetReadiness(ctx context.Context, value *authoring.ChangeSet) ([]authoring.ValidationIssue, error) {
	return validateWorkforceChangeSetReadiness(ctx, s, value)
}

func (s *PostgresStore) ValidateChangeSetReadiness(ctx context.Context, value *authoring.ChangeSet) ([]authoring.ValidationIssue, error) {
	return validateWorkforceChangeSetReadiness(ctx, s, value)
}

func validateWorkforceChangeSetReadiness(ctx context.Context, store skill.CatalogStore, value *authoring.ChangeSet) ([]authoring.ValidationIssue, error) {
	if value == nil {
		return nil, fmt.Errorf("ChangeSet is required")
	}
	activation, err := authoring.EffectiveChangeSetActivationIntent(value)
	if err != nil {
		return nil, err
	}
	catalog := skill.NewCatalogWithStore(store)
	issues := make([]authoring.ValidationIssue, 0)
	for _, agentDefinition := range value.Result.Candidate.Agents {
		if agentDefinition == nil {
			continue
		}
		bindings, err := materializeWorkforceSkillBindings(value, agentDefinition, value.Placement.AgentDeploymentIDs[agentDefinition.ID], activation == authoring.WorkforceActivationActive)
		if err != nil {
			issues = append(issues, authoring.ValidationIssue{
				Path:    "agents." + agentDefinition.ID + ".skillRequirements",
				Code:    "skill_binding_materialization_failed",
				Message: err.Error(),
			})
			continue
		}
		for index, binding := range bindings {
			catalogID := agentDefinition.SkillRequirements[index].SkillID
			path := fmt.Sprintf("agents.%s.skillRequirements.%s", agentDefinition.ID, catalogID)
			definition, resolveErr := resolveReadinessSkillDefinition(ctx, catalog, binding)
			if resolveErr != nil && !errors.Is(resolveErr, skill.ErrDefinitionAmbiguous) {
				return nil, resolveErr
			}
			if resolveErr != nil || definition == nil {
				identity := binding.SkillID + "@" + binding.SkillVersion
				if binding.SourceIdentity != "" {
					identity += " from " + binding.SourceIdentity
				}
				message := fmt.Sprintf("The exact immutable Skill %s is not uniquely installed; select an installed source-qualified version before review", identity)
				issues = append(issues, authoring.ValidationIssue{Path: path, Code: "skill_binding_definition_unavailable", Message: message})
				continue
			}
			if binding.SourceIdentity == "" && definition.Source != nil {
				binding.SourceIdentity = strings.TrimSpace(definition.Source.Identity)
			}
			if issue := workforceBindingDefinitionIssue(path, agentDefinition.ID, catalogID, binding, definition); issue != nil {
				issues = append(issues, *issue)
			}
		}
	}
	return issues, nil
}

func resolveReadinessSkillDefinition(ctx context.Context, catalog *skill.Catalog, binding *capability.Binding) (*skill.Definition, error) {
	if strings.TrimSpace(binding.SourceIdentity) != "" {
		return catalog.GetDefinitionVariant(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
	}
	return catalog.GetDefinition(ctx, binding.SkillID, binding.SkillVersion)
}

func workforceBindingDefinitionIssue(path, agentID, catalogID string, binding *capability.Binding, definition *capability.Definition) *authoring.ValidationIssue {
	identity := binding.SkillID + "@" + binding.SkillVersion
	if binding.SourceIdentity != "" {
		identity += " from " + binding.SourceIdentity
	}
	if definition.ID != binding.SkillID || definition.Version != binding.SkillVersion {
		return &authoring.ValidationIssue{Path: path, Code: "skill_binding_definition_mismatch", Message: fmt.Sprintf("Selected Skill %s does not match the installed immutable definition", identity)}
	}
	definitionSource := ""
	if definition.Source != nil {
		definitionSource = strings.TrimSpace(definition.Source.Identity)
	}
	if strings.TrimSpace(binding.SourceIdentity) != definitionSource {
		return &authoring.ValidationIssue{Path: path, Code: "skill_binding_source_mismatch", Message: fmt.Sprintf("Selected Skill %s does not match installed source %s; select the exact source-qualified version", identity, definitionSource)}
	}
	if binding.EnablePrompt && definition.Prompt == nil {
		return &authoring.ValidationIssue{Path: path, Code: "skill_binding_prompt_unavailable", Message: fmt.Sprintf("Skill %s does not provide the prompt requested by Agent %s; disable prompt use or choose a compatible Skill", catalogID, agentID)}
	}
	for _, actionName := range binding.AllowedActions {
		action, ok := definition.Actions[actionName]
		if !ok {
			return &authoring.ValidationIssue{Path: path, Code: "skill_binding_action_unavailable", Message: fmt.Sprintf("Skill %s does not provide action %s in exact version %s; choose a compatible action or Skill", catalogID, actionName, identity)}
		}
		if workforceRiskRank(action.Risk) > workforceRiskRank(binding.MaximumRisk) {
			return &authoring.ValidationIssue{Path: path, Code: "skill_binding_action_risk_exceeded", Message: fmt.Sprintf("Action %s requires %s risk, but Agent %s permits at most %s; choose a compatible read-only action or widen Agent authority through review and approval", actionName, action.Risk, agentID, binding.MaximumRisk)}
		}
		for _, requirement := range action.Credentials {
			reference, exists := binding.Credentials[requirement.Name]
			if requirement.Optional && !exists {
				continue
			}
			if !exists || reference.Kind != requirement.Kind || strings.TrimSpace(reference.ID) == "" {
				return &authoring.ValidationIssue{Path: path, Code: "skill_binding_credential_incompatible", Message: fmt.Sprintf("Action %s requires an authorized %s credential for %s; configure a matching opaque credential binding before review", actionName, requirement.Kind, requirement.Name)}
			}
		}
	}
	return nil
}

var _ authoring.ChangeSetReadinessValidator = (*SQLiteStore)(nil)
var _ authoring.ChangeSetReadinessValidator = (*PostgresStore)(nil)
