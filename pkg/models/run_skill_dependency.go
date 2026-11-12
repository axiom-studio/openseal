package models

import (
	"errors"
	"strings"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

var ErrRunSkillDependencyUnavailable = errors.New("selected Skill executable version is unavailable")

func RunSkillDependencyDeployment(run *AgentRun) string {
	// A Team-owned method may use either member or Team grants. Its immutable
	// method does not identify which live account supplies each future action.
	if run.Owner.Type == OwnerTypeTeam {
		return ""
	}
	return run.AssignedAgentID
}

func LegacyAcceptedRunExecution(run *AgentRun, deployment *kernelagent.AgentDeployment, definition *kernelagent.AgentDefinition) *AcceptedRunExecution {
	if run == nil || IsTerminalAgentRunStatus(run.Status) || run.Entrypoint == "" || run.Context[AcceptedRunExecutionContextKey] != nil || deployment == nil || definition == nil || definition.Runbook == nil || deployment.ID != run.AssignedAgentID {
		return nil
	}
	method := definition.Runbook
	proved := false
	contextID, _ := run.Context["runbookDefinitionId"].(string)
	contextVersion, _ := run.Context["runbookDefinitionVersion"].(string)
	if contextID != "" || contextVersion != "" {
		if contextID != method.ID || contextVersion != method.Version {
			return nil
		}
		proved = true
	}
	if raw, exists := run.Plan["runbook"]; exists {
		plan, ok := raw.(map[string]interface{})
		if !ok || plan["id"] != method.ID || plan["version"] != method.Version {
			return nil
		}
		proved = true
	}
	if !proved || strings.TrimSpace(run.Entrypoint) != run.Entrypoint {
		return nil
	}
	pin, err := NewAcceptedRunExecution(run.Scope, deployment, definition, run.Entrypoint)
	if err != nil {
		return nil
	}
	return pin
}

// RunSkillActor is the actor a run activates and resolves Skills as: its
// umbrella is always run.Owner, and it carries the person the run acts for.
// An Agent-owned run whose assigned Agent is another Agent (a delegated
// request, a completion review) gets an invalid actor, so it has no host
// bindings and no lease: a delegated Agent never lends its own connections.
// It does not check the roster: callers that use connections also call
// RunUmbrella or the Team authority checks.
func RunSkillActor(run *AgentRun) skill.RunActor {
	if run == nil {
		return skill.RunActor{}
	}
	// RunActor.Valid refuses an Agent umbrella that is not the acting Agent.
	return skill.RunActor{RunID: run.ID, AgentID: run.AssignedAgentID, Umbrella: SkillUmbrella(run.Owner), ActingUser: run.ActingUser}
}

// SkillUmbrella converts an owner to the skill package's umbrella.
func SkillUmbrella(owner ObjectiveOwner) skill.Umbrella {
	return skill.Umbrella{Kind: skill.UmbrellaKind(owner.Type), ID: owner.ID}
}
