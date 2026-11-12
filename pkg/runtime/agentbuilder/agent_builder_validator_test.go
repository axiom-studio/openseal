package agentbuilder_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder/agentbuildertest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/validation"
)

func TestAgentBuilderCreatePreviewGolden(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, true)
	arguments := agentbuildertest.CreateArguments()
	arguments["spaceId"] = fixture.SalesID
	preview, err := fixture.Propose(t, models.AgentActionCreateAgent, arguments)
	if err != nil {
		t.Fatal(err)
	}
	agentbuildertest.AssertJSON(t, preview, `{
		"skillId": "openseal.agents", "skillVersion": "1.6.0", "action": "create_agent",
		"risk": "write", "sideEffect": "write",
		"resourceType": "agent_profile", "operation": "create_agent",
		"agent": {
			"displayName": "Inbox   Triage", "purpose": "Sort the support inbox every morning.",
			"systemPrompt": "Read new support mail and sort it by urgency.", "personality": "Friendly",
			"operatingPrinciples": ["Flag anything legal."]
		},
		"model": {"id": "gpt-6-luna"},
		"space": {"id": "`+fixture.SalesID+`", "name": "Sales"},
		"skills": [{"skillId": "acme.notes", "skillVersion": "1.0.0", "sourceIdentity": "", "name": "Notes",
			"allowedActions": ["write_note"], "enablePrompt": false, "maximumRisk": "write"}],
		"authority": {"maximumRisk": "write"},
		"connections": [{"skillId": "acme.mail", "skillVersion": "2.0.0", "name": "Mail"}],
		"rationale": "The person asked for an inbox helper."
	}`)
	if !reflect.DeepEqual(fixture.Host.Validated, []string{"gpt-6-luna"}) {
		t.Fatalf("host model checks = %#v", fixture.Host.Validated)
	}
}

func TestAgentBuilderCreateResolvesSpaceToPersonal(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, false)
	arguments := agentbuildertest.CreateArguments()
	delete(arguments, "model")
	preview, err := fixture.Propose(t, models.AgentActionCreateAgent, arguments)
	if err != nil {
		t.Fatal(err)
	}
	space, _ := preview["space"].(map[string]interface{})
	if space["id"] != fixture.HomeID || space["name"] != "Home" {
		t.Fatalf("space = %#v", space)
	}
	if model, _ := preview["model"].(map[string]interface{}); model["id"] != "" {
		t.Fatalf("default model = %#v", preview["model"])
	}
}

func TestAgentBuilderModelSchemaHidesKernelResolvedArguments(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, false)
	actions, err := fixture.Catalog.ListModelActions(context.Background(), skill.ScopeReference{Kind: fixture.Scope.Kind, ID: fixture.Scope.ID}, "finn")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, action := range actions {
		seen[action.Action] = true
		properties, _ := action.InputSchema["properties"].(map[string]interface{})
		for _, hidden := range []string{"spaceId", "expectedDeploymentRevision"} {
			if _, visible := properties[hidden]; visible {
				t.Fatalf("%s leaked %s into the model schema", action.Action, hidden)
			}
		}
	}
	for _, name := range []string{models.AgentActionListAgents, models.AgentActionCreateAgent, models.AgentActionUpdateAgent, models.AgentActionTryAgent} {
		if !seen[name] {
			t.Fatalf("model actions %v miss %s", seen, name)
		}
	}
}

func TestAgentBuilderCreateRefusals(t *testing.T) {
	for name, testCase := range map[string]struct {
		withHost bool
		mutate   func(*agentbuildertest.Fixture, map[string]interface{})
		want     string
	}{
		"name used by an Agent, case and spaces differ": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["displayName"] = " researcher "
			},
			want: `agent name "researcher" is already used by another Agent or Team; choose a different name`,
		},
		"name used by the builder": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) { arguments["displayName"] = "FINN" },
			want:   "already used by another Agent or Team",
		},
		"name used by a Team": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["displayName"] = "ops crew"
			},
			want: `agent name "ops crew" is already used by another Agent or Team`,
		},
		"sensitive field": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["systemPrompt"] = "Log in with password: hunter2-correct-horse"
			},
			want: "cannot contain passwords",
		},
		"credentialed grant": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["skills"] = []interface{}{map[string]interface{}{
					"skillId": "acme.mail", "skillVersion": "2.0.0", "allowedActions": []interface{}{"send"},
					"enablePrompt": false, "maximumRisk": "external",
				}}
			},
			want: "needs a saved mailbox account",
		},
		"management Skill grant": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["skills"] = []interface{}{map[string]interface{}{
					"skillId": models.AgentManagementSkillID, "skillVersion": models.AgentManagementSkillVersion,
					"allowedActions": []interface{}{models.AgentActionCreateAgent}, "enablePrompt": false, "maximumRisk": "write",
				}}
			},
			want: "built-in management Skill",
		},
		"unknown connection": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["connections"] = []interface{}{map[string]interface{}{"skillId": "acme.crm", "skillVersion": "1.0.0"}}
			},
			want: "is not registered",
		},
		"unknown space": {
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["spaceId"] = "space-missing"
			},
			want: `space "space-missing" does not exist`,
		},
		"model without a host": {
			want: "model selection is unavailable here",
		},
		"model the host refuses": {
			withHost: true,
			mutate: func(_ *agentbuildertest.Fixture, arguments map[string]interface{}) {
				arguments["model"] = "unknown-model"
			},
			want: "not available",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := agentbuildertest.NewFixture(t, testCase.withHost)
			arguments := agentbuildertest.CreateArguments()
			if testCase.mutate != nil {
				testCase.mutate(fixture, arguments)
			}
			preview, err := fixture.Propose(t, models.AgentActionCreateAgent, arguments)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("preview=%#v err=%v, want %q", preview, err, testCase.want)
			}
		})
	}
}

func TestAgentBuilderSensitiveFieldIsTyped(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, true)
	arguments := agentbuildertest.CreateArguments()
	arguments["operatingPrinciples"] = []interface{}{"Use api key sk-live-0123456789abcdefghijklmnop"}
	_, err := fixture.Propose(t, models.AgentActionCreateAgent, arguments)
	var sensitive *validation.SensitiveInputError
	if !errors.As(err, &sensitive) && !errors.Is(err, validation.ErrSensitiveInput) {
		t.Fatalf("sensitive principle error = %v", err)
	}
}

func TestAgentBuilderValidatorRefusesForeignBinding(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, true)
	run := *fixture.Run
	run.AssignedAgentID = "researcher"
	_, err := fixture.Validator.ValidateActionProposal(context.Background(), agentrun.ActionProposalValidationInput{
		Run: &run, Bound: fixture.Bound(t, models.AgentActionCreateAgent), Arguments: agentbuildertest.CreateArguments(),
	})
	if err == nil || !strings.Contains(err.Error(), "not bound to the current Run's Agent") {
		t.Fatalf("foreign binding error = %v", err)
	}
}

func TestAgentBuilderUpdatePreviewListsOnlyChanges(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, true)
	preview, err := fixture.Propose(t, models.AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "personality": "Calm", "purpose": "Research competitors weekly.",
		"model": "gpt-6-luna",
		"addSkills": []interface{}{map[string]interface{}{
			"skillId": "acme.notes", "skillVersion": "1.0.0", "allowedActions": []interface{}{"write_note"},
			"enablePrompt": false, "maximumRisk": "write",
		}},
		"removeSkillBindingIds": []interface{}{"builder:researcher:acme.notes"},
		"rationale":             "Sharper focus.",
	})
	if err != nil {
		t.Fatal(err)
	}
	agentbuildertest.AssertJSON(t, preview, `{
		"skillId": "openseal.agents", "skillVersion": "1.6.0", "action": "update_agent",
		"risk": "write", "sideEffect": "write",
		"resourceType": "agent_profile", "operation": "update_agent",
		"agent": {"deploymentId": "researcher", "displayName": "Researcher", "purpose": "Research competitors weekly.",
			"systemPrompt": "Work as Researcher.", "personality": "Calm", "operatingPrinciples": ["Be brief."]},
		"current": {"deploymentId": "researcher", "displayName": "Researcher", "purpose": "Researcher work",
			"systemPrompt": "Work as Researcher.", "personality": "Calm", "operatingPrinciples": ["Be brief."]},
		"changes": {"purpose": "Research competitors weekly."},
		"model": {"id": "gpt-6-luna"},
		"skills": [{"skillId": "acme.notes", "skillVersion": "1.0.0", "sourceIdentity": "", "name": "Notes",
			"allowedActions": ["write_note"], "enablePrompt": false, "maximumRisk": "write"}],
		"authority": {"maximumRisk": "write", "currentMaximumRisk": "read"},
		"removedSkills": [{"bindingId": "builder:researcher:acme.notes", "skillId": "acme.notes", "name": "Notes"}],
		"rationale": "Sharper focus."
	}`)
}

func TestAgentBuilderUpdateRefusals(t *testing.T) {
	for name, testCase := range map[string]struct {
		arguments map[string]interface{}
		want      string
	}{
		"builder itself": {
			arguments: map[string]interface{}{"agentId": "finn", "purpose": "Other", "rationale": "x"},
			want:      "cannot change the Agent you are",
		},
		"fixed policy": {
			arguments: map[string]interface{}{"agentId": "platform", "purpose": "Other", "rationale": "x"},
			want:      "does not allow Agent-proposed amendments",
		},
		"retired": {
			arguments: map[string]interface{}{"agentId": "old-agent", "purpose": "Other", "rationale": "x"},
			want:      "retired",
		},
		"unknown": {
			arguments: map[string]interface{}{"agentId": "nobody", "purpose": "Other", "rationale": "x"},
			want:      "use list_agents",
		},
		"rename to a Team name": {
			arguments: map[string]interface{}{"agentId": "researcher", "displayName": "OPS crew", "rationale": "x"},
			want:      "already used by another Agent or Team",
		},
		"bundled binding removal": {
			arguments: map[string]interface{}{"agentId": "researcher", "removeSkillBindingIds": []interface{}{"bundled:agents"}, "rationale": "x"},
			want:      "built in and cannot be removed",
		},
		"unknown binding removal": {
			arguments: map[string]interface{}{"agentId": "researcher", "removeSkillBindingIds": []interface{}{"builder:researcher:none"}, "rationale": "x"},
			want:      "not an enabled Skill",
		},
		"no actual change": {
			arguments: map[string]interface{}{"agentId": "researcher", "personality": "Calm", "rationale": "x"},
			want:      "at least one change",
		},
		"sensitive change": {
			arguments: map[string]interface{}{"agentId": "researcher", "systemPrompt": "Use password: hunter2-correct-horse", "rationale": "x"},
			want:      "cannot contain passwords",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := agentbuildertest.NewFixture(t, true)
			preview, err := fixture.Propose(t, models.AgentActionUpdateAgent, testCase.arguments)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("preview=%#v err=%v, want %q", preview, err, testCase.want)
			}
		})
	}
}

func TestAgentBuilderUpdateKeepsOwnNameAndResolvesRevision(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, false)
	bound := fixture.Bound(t, models.AgentActionUpdateAgent)
	resolved, handled, err := fixture.Validator.ResolveActionProposalArguments(context.Background(), agentrun.ActionProposalValidationInput{
		Run: fixture.Run, Bound: bound, Arguments: map[string]interface{}{"agentId": "researcher", "displayName": "researcher", "rationale": "x"},
	})
	if err != nil || !handled || resolved["expectedDeploymentRevision"] != int64(1) {
		t.Fatalf("resolved=%#v handled=%v err=%v", resolved, handled, err)
	}
	preview, err := fixture.Validator.ValidateActionProposal(context.Background(), agentrun.ActionProposalValidationInput{Run: fixture.Run, Bound: bound, Arguments: resolved})
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := preview["changes"].(map[string]interface{})
	if !reflect.DeepEqual(changes, map[string]interface{}{"displayName": "researcher"}) {
		t.Fatalf("own-name recase changes = %#v", changes)
	}
}

func TestAgentBuilderTryAgentValidation(t *testing.T) {
	fixture := agentbuildertest.NewFixture(t, false)
	base := map[string]interface{}{
		"displayName": "Researcher", "purpose": "Research", "systemPrompt": "Research carefully.", "message": "Hello?",
	}
	// try_agent skips the name check: drafting an Agent named like one that
	// exists is still worth a test message.
	if preview, err := fixture.Propose(t, models.AgentActionTryAgent, base); err != nil || preview != nil {
		t.Fatalf("try_agent preview=%#v err=%v", preview, err)
	}
	withModel := maps.Clone(base)
	withModel["model"] = "gpt-6-luna"
	if _, err := fixture.Propose(t, models.AgentActionTryAgent, withModel); err == nil || err.Error() != "model selection is unavailable here" {
		t.Fatalf("try_agent model without host = %v", err)
	}
	secret := maps.Clone(base)
	secret["message"] = "my password: hunter2-correct-horse"
	if _, err := fixture.Propose(t, models.AgentActionTryAgent, secret); err == nil {
		t.Fatal("try_agent accepted a secret in the message")
	}
}

// update_agent never shows a card for an Agent the Run's person can't change:
// the target's profile stays unread.
func TestAgentBuilderUpdateRefusesAgentThePersonCannotChange(t *testing.T) {
	ctx := context.Background()
	fixture := agentbuildertest.NewFixture(t, true)
	fixture.Run.ActingUser = "person-2"
	fixture.Host.Forbidden = map[string]bool{"person-2/researcher": true}
	arguments := map[string]interface{}{"agentId": "researcher", "displayName": "Foo", "rationale": "Rename it."}
	preview, err := fixture.Propose(t, models.AgentActionUpdateAgent, arguments)
	if err == nil || preview != nil || !strings.Contains(err.Error(), "can't change it") || strings.Contains(err.Error(), "Researcher work") {
		t.Fatalf("forbidden update = %#v, %v", preview, err)
	}
	arguments["expectedDeploymentRevision"] = 1
	preview, err = fixture.Validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
		Run: fixture.Run, Bound: fixture.Bound(t, models.AgentActionUpdateAgent), Arguments: arguments,
	})
	if err == nil || preview != nil || !strings.Contains(err.Error(), "can't change it") {
		t.Fatalf("forbidden update with a revision = %#v, %v", preview, err)
	}
	fixture.Host.Forbidden = nil
	if _, err := fixture.Propose(t, models.AgentActionUpdateAgent, map[string]interface{}{"agentId": "researcher", "displayName": "Foo", "rationale": "Rename it."}); err != nil {
		t.Fatal(err)
	}
	for _, checked := range fixture.Host.Authorized {
		if checked != "person-2/researcher" {
			t.Fatalf("authorized = %#v", fixture.Host.Authorized)
		}
	}
}
