package openseal

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
	"github.com/axiom-studio/openseal/pkg/runtime/routine"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The builder actions changed three bundled Skills. A changed definition
// under an old id@version is refused by hosts, so each one carries a new
// version, and the versions bound before the upgrade keep working for the
// actions they had until the host reconcile moves the bindings.
func TestBundledSkillVersionsForAgentBuilder(t *testing.T) {
	for _, tc := range []struct {
		definition *skill.Definition
		alias      string
		version    string
		actions    []string
	}{
		{models.AgentManagementSkill(), AgentManagementSkillVersion, "1.7.0",
			[]string{AgentActionAmendBehavior, AgentActionListChannels, AgentActionConfigureChannel,
				models.AgentActionListAgents, models.AgentActionCreateAgent, models.AgentActionUpdateAgent, models.AgentActionTryAgent}},
		{collaboration.TeamManagementSkill(), TeamManagementSkillVersion, "2.3.0",
			[]string{TeamActionUpdateMember, TeamActionUpdateProfile, TeamActionCreateTeam}},
		{skillbinding.SkillManagementSkill(), SkillManagementSkillVersion, "1.7.0",
			[]string{SkillActionDiscover, SkillActionUpsertBinding, SkillActionDisableBinding, SkillActionRequestSetup, SkillActionRequestConnection}},
	} {
		if tc.definition.Version != tc.version || tc.alias != tc.version {
			t.Fatalf("%s version = %s (alias %s), want %s", tc.definition.ID, tc.definition.Version, tc.alias, tc.version)
		}
		for _, name := range tc.actions {
			if _, ok := tc.definition.Actions[name]; !ok {
				t.Fatalf("%s@%s has no %s action", tc.definition.ID, tc.version, name)
			}
		}
	}

	skills := skillbinding.SkillManagementSkill()
	for _, name := range []string{SkillActionDiscover, SkillActionUpsertBinding, SkillActionDisableBinding, SkillActionRequestSetup, SkillActionRequestConnection} {
		properties, _ := skills.Actions[name].InputSchema["properties"].(map[string]interface{})
		if _, ok := properties[skillbinding.SkillActionTargetAgentArgument]; !ok {
			t.Fatalf("%s@%s %s has no %s", skills.ID, skills.Version, name, skillbinding.SkillActionTargetAgentArgument)
		}
	}

	for _, version := range []string{"1.4.0", "1.5.0", "1.6.0", "1.7.0"} {
		if !IsSupportedAgentManagementSkillVersion(version) {
			t.Fatalf("openseal.agents %s must run", version)
		}
	}
	for _, version := range []string{"2.0.0", "2.1.0", "2.2.0", "2.3.0"} {
		if !collaboration.IsSupportedTeamManagementSkillVersion(version) {
			t.Fatalf("openseal.teams %s must run", version)
		}
	}
	if IsSupportedAgentManagementSkillVersion("1.8.0") || collaboration.IsSupportedTeamManagementSkillVersion("2.4.0") {
		t.Fatal("an unreleased version must not be supported")
	}
}

// openseal.agents 1.7.0 dropped the top-level anyOf from update_agent and
// amend_behavior. A published definition never changes under its version:
// any edit needs a new version, or hosts refuse to register it.
func TestBundledAgentManagementSkill170IsFrozen(t *testing.T) {
	definition := models.AgentManagementSkill()
	if definition.Version != "1.7.0" {
		t.Fatalf("version = %s", definition.Version)
	}
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	const published170SHA256 = "da6cefcce644a4c04662472697e5ee3bb0003ff4f7d55710343a8d7f63371533"
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != published170SHA256 {
		t.Fatalf("published 1.7.0 definition changed: got %s", got)
	}
	properties, _ := definition.Actions[AgentActionConfigureChannel].InputSchema["properties"].(map[string]interface{})
	if _, ok := properties["trigger"]; ok {
		t.Fatal("configure_channel still takes a trigger")
	}
}

// Anthropic refuses a tool whose input schema has oneOf, anyOf or allOf at
// the top level, so one such action fails every Claude step that loads it.
// Rules like "at least one field" belong in the description and the handler.
func TestBundledSkillInputSchemasHaveNoTopLevelCombinator(t *testing.T) {
	for _, definition := range []*skill.Definition{
		models.AgentManagementSkill(), collaboration.TeamManagementSkill(), skillbinding.SkillManagementSkill(),
		routine.RunbookManagementSkill(), agentrun.RunManagementSkill(), action.ObjectiveManagementSkill(),
	} {
		for name, bound := range definition.Actions {
			for _, keyword := range []string{"oneOf", "anyOf", "allOf"} {
				if _, ok := bound.InputSchema[keyword]; ok {
					t.Errorf("%s@%s %s input schema has a top-level %s", definition.ID, definition.Version, name, keyword)
				}
			}
		}
	}
}
