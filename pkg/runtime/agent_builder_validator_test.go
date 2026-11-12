package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/validation"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type agentBuilderTestHost struct {
	validated []string
	// forbidden lists "user/agentId" pairs AuthorizeAgentChange refuses.
	forbidden  map[string]bool
	authorized []string
}

func (h *agentBuilderTestHost) AuthorizeAgentChange(_ context.Context, _ skill.ScopeReference, actingUserID, deploymentID string) error {
	h.authorized = append(h.authorized, actingUserID+"/"+deploymentID)
	if h.forbidden[actingUserID+"/"+deploymentID] {
		return ErrAgentChangeForbidden
	}
	return nil
}

func (h *agentBuilderTestHost) ValidateAgentModel(_ context.Context, _ skill.ScopeReference, model string) error {
	h.validated = append(h.validated, model)
	if model == "unknown-model" {
		return errors.New("model unknown-model is not available")
	}
	return nil
}

func (h *agentBuilderTestHost) PrepareAgentModel(context.Context, AgentModelRequest) error {
	return nil
}

func (h *agentBuilderTestHost) AfterAgentCreated(context.Context, AgentCreated) {}

type agentBuilderFixture struct {
	scope   Scope
	agents  *kernelagent.Registry
	teams   *kernelteam.Registry
	catalog *skill.Catalog
	spaces  *SpaceService
	salesID string
	// homeID is the default space of the person the fixture's run acts for.
	homeID    string
	run       *AgentRun
	host      *agentBuilderTestHost
	validator *AgentBuilderActionValidator
}

func newAgentBuilderFixture(t *testing.T, withHost bool) *agentBuilderFixture {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "tenant-a"}
	reference := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	agents := kernelagent.NewRegistry()
	addAgent := func(id, name string, amendments workforce.AmendmentPolicy, status kernelagent.RolloutStatus) {
		t.Helper()
		definition, err := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
			ID: id + "-definition", Version: "1.0.0", DisplayName: name, Purpose: name + " work",
			SystemPrompt: "Work as " + name + ".", Personality: "Calm", OperatingPrinciples: []string{"Be brief."},
			Authority:  kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
			Amendments: amendments,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
			ID: id, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
		}, "user", "operator", "fixture"); err != nil {
			t.Fatal(err)
		}
		if status == kernelagent.RolloutRetired {
			deployment, err := agents.GetDeployment(ctx, reference, id)
			if err != nil {
				t.Fatal(err)
			}
			deployment.RolloutStatus = kernelagent.RolloutRetired
			if _, _, err := agents.UpdateDeployment(ctx, deployment, deployment.Revision, "user", "operator", "retire"); err != nil {
				t.Fatal(err)
			}
		}
	}
	addAgent("finn", "Finn", workforce.AmendmentPolicy{}, kernelagent.RolloutActive)
	addAgent("researcher", "Researcher", workforce.AmendmentPolicy{AgentMayPropose: true}, kernelagent.RolloutActive)
	addAgent("platform", "Platform Helper", workforce.AmendmentPolicy{}, kernelagent.RolloutActive)
	addAgent("old-agent", "Old Agent", workforce.AmendmentPolicy{AgentMayPropose: true}, kernelagent.RolloutRetired)

	teams := kernelteam.NewRegistry(agents)
	teamDefinition, err := teams.RegisterDefinition(ctx, &kernelteam.Definition{
		ID: "ops-team-definition", Version: "1", DisplayName: "Ops  Crew", Purpose: "Run operations",
		Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "ops-team", Scope: reference, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Status: kernelteam.DeploymentActive,
		Roster: []kernelteam.RosterAssignment{{ID: "researcher-0", AgentDeploymentID: "researcher"}},
	}, "user", "operator", "fixture"); err != nil {
		t.Fatal(err)
	}

	catalog := skill.NewCatalog()
	for _, definition := range []*skill.Definition{
		AgentManagementSkill(),
		{
			ID: "acme.notes", Version: "1.0.0", Name: "Notes", Description: "Keep notes",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://notes"},
			Actions: map[string]skill.Action{
				"write_note": {
					Name: "write_note", Description: "Write a note", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
					Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
					InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
				},
			},
		},
		{
			ID: "acme.mail", Version: "2.0.0", Name: "Mail", Description: "Send mail",
			Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://mail"},
			Actions: map[string]skill.Action{
				"send": {
					Name: "send", Description: "Send a message", Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal,
					Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
					Credentials: []skill.CredentialRequirement{{Name: "mailbox", Kind: "oauth"}},
					InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
				},
			},
		},
	} {
		if err := catalog.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	for _, binding := range []*skill.Binding{
		{
			ID: "bundled:agents", Revision: 1, Scope: skillScope, DeploymentID: "finn",
			SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
			AllowedActions: []string{AgentActionListAgents, AgentActionCreateAgent, AgentActionUpdateAgent, AgentActionTryAgent},
			MaximumRisk:    skill.RiskLevelWrite,
		},
		{
			ID: "builder:researcher:acme.notes", Revision: 1, Scope: skillScope, DeploymentID: "researcher",
			SkillID: "acme.notes", SkillVersion: "1.0.0", AllowedActions: []string{"write_note"}, MaximumRisk: skill.RiskLevelWrite,
		},
		{
			ID: "bundled:agents", Revision: 1, Scope: skillScope, DeploymentID: "researcher",
			SkillID: AgentManagementSkillID, SkillVersion: AgentManagementSkillVersion,
			AllowedActions: []string{AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
		},
	} {
		if err := catalog.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}

	spaces := NewSpaceService(newTestKernelStore(t))
	salesID, homeID := seedAgentBuilderSpaces(t, spaces, scope)
	fixture := &agentBuilderFixture{
		scope: scope, agents: agents, teams: teams, catalog: catalog, spaces: spaces, salesID: salesID, homeID: homeID,
		run: &AgentRun{
			ID: "builder-run", Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"},
			AssignedAgentID: "finn", ActingUser: agentBuilderApprover.ID, Source: RunSourceChat,
		},
	}
	var host AgentBuilderHost
	if withHost {
		fixture.host = &agentBuilderTestHost{}
		host = fixture.host
	}
	fixture.validator, err = NewAgentBuilderActionValidator(agents, teams, catalog, spaces, nil, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// seedAgentBuilderSpaces creates Sales, owned by an operator, and Home, the
// default space of the person the builder run acts for.
func seedAgentBuilderSpaces(t *testing.T, spaces *SpaceService, scope Scope) (salesID, homeID string) {
	t.Helper()
	ctx := context.Background()
	sales, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Sales", Look: DefaultSpaceLook, OwnerID: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := spaces.CreateSpace(ctx, CreateSpaceRequest{Scope: scope, Name: "Home", Look: DefaultSpaceLook, OwnerID: agentBuilderApprover.ID})
	if err != nil || !home.IsDefault {
		t.Fatalf("home = %+v, err = %v", home, err)
	}
	return sales.ID, home.ID
}

func (f *agentBuilderFixture) bound(t *testing.T, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.catalog.Resolve(context.Background(), skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, "finn",
		AgentManagementSkillID, AgentManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// propose runs the coordinator's order: resolve, schema check, validate.
func (f *agentBuilderFixture) propose(t *testing.T, action string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	ctx := context.Background()
	bound := f.bound(t, action)
	input := ActionProposalValidationInput{Run: f.run, Bound: bound, Arguments: arguments}
	resolved, handled, err := f.validator.ResolveActionProposalArguments(ctx, input)
	if err != nil {
		return nil, err
	}
	if handled {
		input.Arguments = resolved
	}
	if err := f.catalog.ValidateInput(ctx, bound, input.Arguments); err != nil {
		t.Fatalf("%s arguments failed the action schema: %v (%#v)", action, err, input.Arguments)
	}
	return f.validator.ValidateActionProposal(ctx, input)
}

func agentBuilderCreateArguments() map[string]interface{} {
	return map[string]interface{}{
		"displayName":         "  Inbox   Triage ",
		"purpose":             "Sort the support inbox every morning.",
		"systemPrompt":        "Read new support mail and sort it by urgency.",
		"personality":         "Friendly",
		"operatingPrinciples": []interface{}{"Flag anything legal.", " Flag anything legal. "},
		"model":               "gpt-6-luna",
		"skills": []interface{}{map[string]interface{}{
			"skillId": "acme.notes", "skillVersion": "1.0.0", "allowedActions": []interface{}{"write_note"},
			"enablePrompt": false, "maximumRisk": "write",
		}},
		"connections": []interface{}{map[string]interface{}{"skillId": "acme.mail", "skillVersion": "2.0.0"}},
		"rationale":   "The person asked for an inbox helper.",
	}
}

func assertAgentBuilderJSON(t *testing.T, got map[string]interface{}, want string) {
	t.Helper()
	var expected map[string]interface{}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, expected) {
		gotJSON, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("preview mismatch:\n%s", gotJSON)
	}
}

func TestAgentBuilderCreatePreviewGolden(t *testing.T) {
	fixture := newAgentBuilderFixture(t, true)
	arguments := agentBuilderCreateArguments()
	arguments["spaceId"] = fixture.salesID
	preview, err := fixture.propose(t, AgentActionCreateAgent, arguments)
	if err != nil {
		t.Fatal(err)
	}
	assertAgentBuilderJSON(t, preview, `{
		"skillId": "openseal.agents", "skillVersion": "1.6.0", "action": "create_agent",
		"risk": "write", "sideEffect": "write",
		"resourceType": "agent_profile", "operation": "create_agent",
		"agent": {
			"displayName": "Inbox   Triage", "purpose": "Sort the support inbox every morning.",
			"systemPrompt": "Read new support mail and sort it by urgency.", "personality": "Friendly",
			"operatingPrinciples": ["Flag anything legal."]
		},
		"model": {"id": "gpt-6-luna"},
		"space": {"id": "`+fixture.salesID+`", "name": "Sales"},
		"skills": [{"skillId": "acme.notes", "skillVersion": "1.0.0", "sourceIdentity": "", "name": "Notes",
			"allowedActions": ["write_note"], "enablePrompt": false, "maximumRisk": "write"}],
		"authority": {"maximumRisk": "write"},
		"connections": [{"skillId": "acme.mail", "skillVersion": "2.0.0", "name": "Mail"}],
		"rationale": "The person asked for an inbox helper."
	}`)
	if !reflect.DeepEqual(fixture.host.validated, []string{"gpt-6-luna"}) {
		t.Fatalf("host model checks = %#v", fixture.host.validated)
	}
}

func TestAgentBuilderCreateResolvesSpaceToPersonal(t *testing.T) {
	fixture := newAgentBuilderFixture(t, false)
	arguments := agentBuilderCreateArguments()
	delete(arguments, "model")
	preview, err := fixture.propose(t, AgentActionCreateAgent, arguments)
	if err != nil {
		t.Fatal(err)
	}
	space, _ := preview["space"].(map[string]interface{})
	if space["id"] != fixture.homeID || space["name"] != "Home" {
		t.Fatalf("space = %#v", space)
	}
	if model, _ := preview["model"].(map[string]interface{}); model["id"] != "" {
		t.Fatalf("default model = %#v", preview["model"])
	}
}

func TestAgentBuilderModelSchemaHidesKernelResolvedArguments(t *testing.T) {
	fixture := newAgentBuilderFixture(t, false)
	actions, err := fixture.catalog.ListModelActions(context.Background(), skill.ScopeReference{Kind: fixture.scope.Kind, ID: fixture.scope.ID}, "finn")
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
	for _, name := range []string{AgentActionListAgents, AgentActionCreateAgent, AgentActionUpdateAgent, AgentActionTryAgent} {
		if !seen[name] {
			t.Fatalf("model actions %v miss %s", seen, name)
		}
	}
}

func TestAgentBuilderCreateRefusals(t *testing.T) {
	for name, testCase := range map[string]struct {
		withHost bool
		mutate   func(*agentBuilderFixture, map[string]interface{})
		want     string
	}{
		"name used by an Agent, case and spaces differ": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) {
				arguments["displayName"] = " researcher "
			},
			want: `agent name "researcher" is already used by another Agent or Team; choose a different name`,
		},
		"name used by the builder": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) { arguments["displayName"] = "FINN" },
			want:   "already used by another Agent or Team",
		},
		"name used by a Team": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) { arguments["displayName"] = "ops crew" },
			want:   `agent name "ops crew" is already used by another Agent or Team`,
		},
		"sensitive field": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) {
				arguments["systemPrompt"] = "Log in with password: hunter2-correct-horse"
			},
			want: "cannot contain passwords",
		},
		"credentialed grant": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) {
				arguments["skills"] = []interface{}{map[string]interface{}{
					"skillId": "acme.mail", "skillVersion": "2.0.0", "allowedActions": []interface{}{"send"},
					"enablePrompt": false, "maximumRisk": "external",
				}}
			},
			want: "needs a saved mailbox account",
		},
		"management Skill grant": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) {
				arguments["skills"] = []interface{}{map[string]interface{}{
					"skillId": AgentManagementSkillID, "skillVersion": AgentManagementSkillVersion,
					"allowedActions": []interface{}{AgentActionCreateAgent}, "enablePrompt": false, "maximumRisk": "write",
				}}
			},
			want: "built-in management Skill",
		},
		"unknown connection": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) {
				arguments["connections"] = []interface{}{map[string]interface{}{"skillId": "acme.crm", "skillVersion": "1.0.0"}}
			},
			want: "is not registered",
		},
		"unknown space": {
			mutate: func(_ *agentBuilderFixture, arguments map[string]interface{}) { arguments["spaceId"] = "space-missing" },
			want:   `space "space-missing" does not exist`,
		},
		"model without a host": {
			want: "model selection is unavailable here",
		},
		"model the host refuses": {
			withHost: true,
			mutate:   func(_ *agentBuilderFixture, arguments map[string]interface{}) { arguments["model"] = "unknown-model" },
			want:     "not available",
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newAgentBuilderFixture(t, testCase.withHost)
			arguments := agentBuilderCreateArguments()
			if testCase.mutate != nil {
				testCase.mutate(fixture, arguments)
			}
			preview, err := fixture.propose(t, AgentActionCreateAgent, arguments)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("preview=%#v err=%v, want %q", preview, err, testCase.want)
			}
		})
	}
}

func TestAgentBuilderSensitiveFieldIsTyped(t *testing.T) {
	fixture := newAgentBuilderFixture(t, true)
	arguments := agentBuilderCreateArguments()
	arguments["operatingPrinciples"] = []interface{}{"Use api key sk-live-0123456789abcdefghijklmnop"}
	_, err := fixture.propose(t, AgentActionCreateAgent, arguments)
	var sensitive *validation.SensitiveInputError
	if !errors.As(err, &sensitive) && !errors.Is(err, validation.ErrSensitiveInput) {
		t.Fatalf("sensitive principle error = %v", err)
	}
}

func TestAgentBuilderValidatorRefusesForeignBinding(t *testing.T) {
	fixture := newAgentBuilderFixture(t, true)
	run := *fixture.run
	run.AssignedAgentID = "researcher"
	_, err := fixture.validator.ValidateActionProposal(context.Background(), ActionProposalValidationInput{
		Run: &run, Bound: fixture.bound(t, AgentActionCreateAgent), Arguments: agentBuilderCreateArguments(),
	})
	if err == nil || !strings.Contains(err.Error(), "not bound to the current Run's Agent") {
		t.Fatalf("foreign binding error = %v", err)
	}
}

func TestAgentBuilderUpdatePreviewListsOnlyChanges(t *testing.T) {
	fixture := newAgentBuilderFixture(t, true)
	preview, err := fixture.propose(t, AgentActionUpdateAgent, map[string]interface{}{
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
	assertAgentBuilderJSON(t, preview, `{
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
			fixture := newAgentBuilderFixture(t, true)
			preview, err := fixture.propose(t, AgentActionUpdateAgent, testCase.arguments)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("preview=%#v err=%v, want %q", preview, err, testCase.want)
			}
		})
	}
}

func TestAgentBuilderUpdateKeepsOwnNameAndResolvesRevision(t *testing.T) {
	fixture := newAgentBuilderFixture(t, false)
	bound := fixture.bound(t, AgentActionUpdateAgent)
	resolved, handled, err := fixture.validator.ResolveActionProposalArguments(context.Background(), ActionProposalValidationInput{
		Run: fixture.run, Bound: bound, Arguments: map[string]interface{}{"agentId": "researcher", "displayName": "researcher", "rationale": "x"},
	})
	if err != nil || !handled || resolved["expectedDeploymentRevision"] != int64(1) {
		t.Fatalf("resolved=%#v handled=%v err=%v", resolved, handled, err)
	}
	preview, err := fixture.validator.ValidateActionProposal(context.Background(), ActionProposalValidationInput{Run: fixture.run, Bound: bound, Arguments: resolved})
	if err != nil {
		t.Fatal(err)
	}
	changes, _ := preview["changes"].(map[string]interface{})
	if !reflect.DeepEqual(changes, map[string]interface{}{"displayName": "researcher"}) {
		t.Fatalf("own-name recase changes = %#v", changes)
	}
}

func TestAgentBuilderTryAgentValidation(t *testing.T) {
	fixture := newAgentBuilderFixture(t, false)
	base := map[string]interface{}{
		"displayName": "Researcher", "purpose": "Research", "systemPrompt": "Research carefully.", "message": "Hello?",
	}
	// try_agent skips the name check: drafting an Agent named like one that
	// exists is still worth a test message.
	if preview, err := fixture.propose(t, AgentActionTryAgent, base); err != nil || preview != nil {
		t.Fatalf("try_agent preview=%#v err=%v", preview, err)
	}
	withModel := maps.Clone(base)
	withModel["model"] = "gpt-6-luna"
	if _, err := fixture.propose(t, AgentActionTryAgent, withModel); err == nil || err.Error() != "model selection is unavailable here" {
		t.Fatalf("try_agent model without host = %v", err)
	}
	secret := maps.Clone(base)
	secret["message"] = "my password: hunter2-correct-horse"
	if _, err := fixture.propose(t, AgentActionTryAgent, secret); err == nil {
		t.Fatal("try_agent accepted a secret in the message")
	}
}

func TestAgentBuilderActionsExistOnlyInCurrentContract(t *testing.T) {
	definition := AgentManagementSkill()
	if definition.Version != "1.6.0" {
		t.Fatalf("version = %s", definition.Version)
	}
	for _, name := range []string{AgentActionListAgents, AgentActionCreateAgent, AgentActionUpdateAgent, AgentActionTryAgent} {
		action, ok := definition.Actions[name]
		if !ok {
			t.Fatalf("missing %s", name)
		}
		if !isAgentBuilderAction(&skill.BoundAction{Definition: definition, Action: action}) {
			t.Fatalf("%s is not a builder action at %s", name, definition.Version)
		}
		older := AgentManagementSkill()
		older.Version = "1.5.0"
		if isAgentBuilderAction(&skill.BoundAction{Definition: older, Action: action}) {
			t.Fatalf("%s matched the 1.5.0 contract", name)
		}
		if isAgentManagementAction(&skill.BoundAction{Definition: definition, Action: action}) {
			t.Fatalf("%s reached the self-only behavior dispatcher", name)
		}
		wantReview := name == AgentActionCreateAgent || name == AgentActionUpdateAgent
		if action.AlwaysReview() != wantReview {
			t.Fatalf("%s review always = %v", name, action.AlwaysReview())
		}
	}
	if !IsSupportedAgentManagementSkillVersion("1.4.0") {
		t.Fatal("1.4.0 bindings are no longer supported")
	}
}

// update_agent never shows a card for an Agent the Run's person can't change:
// the target's profile stays unread.
func TestAgentBuilderUpdateRefusesAgentThePersonCannotChange(t *testing.T) {
	ctx := context.Background()
	fixture := newAgentBuilderFixture(t, true)
	fixture.run.ActingUser = "person-2"
	fixture.host.forbidden = map[string]bool{"person-2/researcher": true}
	arguments := map[string]interface{}{"agentId": "researcher", "displayName": "Foo", "rationale": "Rename it."}
	preview, err := fixture.propose(t, AgentActionUpdateAgent, arguments)
	if err == nil || preview != nil || !strings.Contains(err.Error(), "can't change it") || strings.Contains(err.Error(), "Researcher work") {
		t.Fatalf("forbidden update = %#v, %v", preview, err)
	}
	arguments["expectedDeploymentRevision"] = 1
	preview, err = fixture.validator.ValidateActionProposal(ctx, ActionProposalValidationInput{
		Run: fixture.run, Bound: fixture.bound(t, AgentActionUpdateAgent), Arguments: arguments,
	})
	if err == nil || preview != nil || !strings.Contains(err.Error(), "can't change it") {
		t.Fatalf("forbidden update with a revision = %#v, %v", preview, err)
	}
	fixture.host.forbidden = nil
	if _, err := fixture.propose(t, AgentActionUpdateAgent, map[string]interface{}{"agentId": "researcher", "displayName": "Foo", "rationale": "Rename it."}); err != nil {
		t.Fatal(err)
	}
	for _, checked := range fixture.host.authorized {
		if checked != "person-2/researcher" {
			t.Fatalf("authorized = %#v", fixture.host.authorized)
		}
	}
}

// A grant's declared ceiling is never trusted: it becomes the riskiest
// allowed action of the exact catalog definition, at least read.
func TestResolveAgentSkillGrantsRiskFromCatalog(t *testing.T) {
	ctx := context.Background()
	catalog := skill.NewCatalog()
	if err := catalog.Register(ctx, &skill.Definition{
		ID: "acme.lookup", Version: "1.0.0", Name: "Lookup", Description: "Look things up",
		Transport: skill.TransportReference{Kind: "kernel", Endpoint: "kernel://lookup"},
		Prompt:    &skill.PromptModule{Instructions: "Look things up."},
		Actions: map[string]skill.Action{
			"find": {
				Name: "find", Description: "Find a thing", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
				InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
			},
			"save": {
				Name: "save", Description: "Save a thing", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite,
				Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
				InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		actions  []string
		prompt   bool
		declared skill.RiskLevel
		want     skill.RiskLevel
	}{
		{"declared destructive on a read action", []string{"find"}, false, skill.RiskLevelDestructive, skill.RiskLevelRead},
		{"declared read on a write action", []string{"find", "save"}, false, skill.RiskLevelRead, skill.RiskLevelWrite},
		{"prompt only", nil, true, skill.RiskLevelProduction, skill.RiskLevelRead},
	} {
		t.Run(test.name, func(t *testing.T) {
			grants, err := resolveAgentSkillGrants(ctx, catalog, []AgentSkillGrant{{
				SkillID: "acme.lookup", SkillVersion: "1.0.0", AllowedActions: test.actions, EnablePrompt: test.prompt, MaximumRisk: test.declared,
			}})
			if err != nil {
				t.Fatal(err)
			}
			if got := grants[0].grant.MaximumRisk; got != test.want {
				t.Fatalf("grant risk = %s, want %s", got, test.want)
			}
			if got := agentGrantsMaximumRisk("", grants); got != test.want {
				t.Fatalf("agent ceiling = %s, want %s", got, test.want)
			}
		})
	}
}
