package agentbuilder_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder/agentbuildertest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func agentBuilderDispatchCreateArguments(spaceID string) map[string]interface{} {
	arguments := agentbuildertest.CreateArguments()
	arguments["spaceId"] = spaceID
	return arguments
}

func TestAgentBuilderCreateAgentExecutes(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	output, err := f.Execute(t, models.AgentActionCreateAgent, "call-create", arguments, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	definitionID := agentbuilder.AgentBuilderResourceID("agent-definition", f.Scope, "call-create")
	deploymentID := agentbuilder.AgentBuilderResourceID("agent-deployment", f.Scope, "call-create")
	if len(definitionID) != 21 || len(deploymentID) != 21 || definitionID == deploymentID {
		t.Fatalf("ids = %q, %q", definitionID, deploymentID)
	}
	if again := agentbuilder.AgentBuilderResourceID("agent-deployment", f.Scope, "call-create"); again != deploymentID {
		t.Fatalf("deployment id is not deterministic: %q != %q", again, deploymentID)
	}
	if output["resourceType"] != models.AgentProfileResourceType || output["operation"] != models.AgentActionCreateAgent ||
		output["deploymentId"] != deploymentID || output["displayName"] != "Inbox   Triage" || output["replayed"] != false {
		t.Fatalf("output head = %#v", output)
	}
	definition, err := f.Agents.GetDefinition(ctx, definitionID, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if definition.Purpose != "Sort the support inbox every morning." || definition.Personality != "Friendly" ||
		len(definition.OperatingPrinciples) != 1 || definition.Authority.MaximumRisk != capability.RiskLevelWrite ||
		definition.Authority.MaxConcurrentRuns != 1 || !definition.Amendments.AgentMayPropose ||
		definition.Provenance.Source != "agent-builder" || definition.Provenance.Reference != "call-create" ||
		definition.Provenance.CreatedBy != agentbuildertest.Approver.ID || definition.AuthoringKey != "" {
		t.Fatalf("definition = %#v", definition)
	}
	deployment, err := f.Agents.GetDeployment(ctx, f.Reference(), deploymentID)
	if err != nil || deployment.DefinitionID != definitionID || deployment.RolloutStatus != kernelagent.RolloutActive ||
		deployment.ActiveVersion != "1.0.0" || deployment.Capacity.MaxConcurrentRuns != 1 {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	binding, err := f.Catalog.GetBinding(ctx, skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, deploymentID, "builder:"+deploymentID+":acme.notes")
	if err != nil || binding == nil || binding.Disabled || binding.MaximumRisk != skill.RiskLevelWrite ||
		len(binding.Lifecycle) != 1 || binding.Lifecycle[0].Actor.Type != "user" || binding.Lifecycle[0].Actor.ID != agentbuildertest.Approver.ID {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	if !f.SpaceMember(t, f.SalesID, deploymentID) || f.SpaceMember(t, f.HomeID, deploymentID) {
		t.Fatal("the created Agent is not only in the reviewed space")
	}
	control, err := f.Provision.EnsureAgentControlConversation(ctx, deployment)
	if err != nil {
		t.Fatal(err)
	}
	chat, _ := output["chat"].(map[string]interface{})
	if chat["conversationId"] != control.ID {
		t.Fatalf("chat = %#v, want %s", chat, control.ID)
	}
	space, _ := output["space"].(map[string]interface{})
	if space["id"] != f.SalesID || space["name"] != "Sales" {
		t.Fatalf("space = %#v", space)
	}
	if bindings, _ := output["bindings"].([]interface{}); len(bindings) != 1 {
		t.Fatalf("bindings = %#v", output["bindings"])
	}
	if connections, _ := output["connections"].([]interface{}); len(connections) != 1 {
		t.Fatalf("connections = %#v", output["connections"])
	}
	if message, _ := output["message"].(string); !strings.Contains(message, "targetAgentId="+deploymentID) {
		t.Fatalf("message = %q", message)
	}
	if len(f.Recorder.Prepared) != 1 || f.Recorder.Prepared[0] != (agentbuilder.AgentModelRequest{
		Scope: skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, DeploymentID: deploymentID, Model: "gpt-6-luna",
		OwnerUserID: agentbuildertest.Approver.ID, ActionCallID: "call-create",
	}) {
		t.Fatalf("prepared = %#v", f.Recorder.Prepared)
	}
	if len(f.Recorder.Created) != 1 || f.Recorder.Created[0] != (agentbuilder.AgentCreated{
		Scope: f.Scope, DeploymentID: deploymentID, DisplayName: "Inbox   Triage", Purpose: "Sort the support inbox every morning.",
		ActionCallID: "call-create", RunID: f.Run.ID, BillingUserID: agentbuildertest.Approver.ID,
	}) {
		t.Fatalf("created = %#v", f.Recorder.Created)
	}

	// The same call again (a crash-resumed execution) converges.
	replay, err := f.Execute(t, models.AgentActionCreateAgent, "call-create", arguments, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	if replay["replayed"] != true || replay["deploymentId"] != deploymentID {
		t.Fatalf("replay = %#v", replay)
	}
	if f.Provision.Created != 1 {
		t.Fatalf("deployments created = %d", f.Provision.Created)
	}
	activations, err := f.Agents.ListActivations(ctx, f.Reference(), deploymentID)
	if err != nil || len(activations) != 1 {
		t.Fatalf("activations = %#v, %v", activations, err)
	}
	again, err := f.Catalog.GetBinding(ctx, skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, deploymentID, binding.ID)
	if err != nil || again.Revision != binding.Revision {
		t.Fatalf("replayed binding = %#v, %v", again, err)
	}
	versions, err := f.Agents.ListDefinitionVersions(ctx, definitionID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("definition versions = %#v, %v", versions, err)
	}
}

func TestAgentBuilderCreateAgentInDefaultSpace(t *testing.T) {
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := agentBuilderDispatchCreateArguments(f.HomeID)
	delete(arguments, "skills")
	delete(arguments, "connections")
	delete(arguments, "model")
	output, err := f.Execute(t, models.AgentActionCreateAgent, "call-personal", f.Resolve(t, models.AgentActionCreateAgent, arguments), agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	deploymentID, _ := output["deploymentId"].(string)
	if !f.SpaceMember(t, f.HomeID, deploymentID) || f.SpaceMember(t, f.SalesID, deploymentID) {
		t.Fatal("the created Agent is not only in the person's default space")
	}
	if message, _ := output["message"].(string); strings.Contains(message, "request_connection") {
		t.Fatalf("message = %q", message)
	}
	if len(f.Recorder.Prepared) != 1 || f.Recorder.Prepared[0].Model != "" {
		t.Fatalf("prepared = %#v", f.Recorder.Prepared)
	}
}

func TestAgentBuilderCreateAgentRefusesNonUserApprover(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	for _, approver := range []models.ApprovalPrincipal{{Type: "role", ID: "operator"}, {Type: "agent", ID: "finn"}} {
		if _, err := f.Execute(t, models.AgentActionCreateAgent, "call-"+approver.Type, arguments, approver); err == nil ||
			!strings.Contains(err.Error(), "only by a person") {
			t.Fatalf("%s approver error = %v", approver.Type, err)
		}
		deploymentID := agentbuilder.AgentBuilderResourceID("agent-deployment", f.Scope, "call-"+approver.Type)
		if _, err := f.Agents.GetDeployment(ctx, f.Reference(), deploymentID); !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
			t.Fatalf("deployment after refused approver: %v", err)
		}
	}
	if len(f.Recorder.Created) != 0 {
		t.Fatalf("created = %#v", f.Recorder.Created)
	}
}

func TestAgentBuilderCreateAgentFailsWhenNameTakenWhileWaiting(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	definition, err := f.Agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "rival-definition", Version: "1.0.0", DisplayName: "inbox triage", Purpose: "Rival", SystemPrompt: "Rival.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "rival", Scope: f.Reference(), DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Execute(t, models.AgentActionCreateAgent, "call-taken", arguments, agentbuildertest.Approver); err == nil ||
		!strings.Contains(err.Error(), "already used") || !strings.Contains(err.Error(), "another name") {
		t.Fatalf("taken name error = %v", err)
	}
	if f.Provision.Created != 0 || len(f.Recorder.Prepared) != 0 {
		t.Fatalf("created %d, prepared %#v", f.Provision.Created, f.Recorder.Prepared)
	}
}

func TestAgentBuilderCreateAgentRetriesAfterModelFailure(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	f.Recorder.PrepareErr = errors.New("gateway unavailable")
	if _, err := f.Execute(t, models.AgentActionCreateAgent, "call-retry", arguments, agentbuildertest.Approver); err == nil ||
		!strings.Contains(err.Error(), "gateway unavailable") {
		t.Fatalf("first attempt error = %v", err)
	}
	if len(f.Recorder.Created) != 0 {
		t.Fatalf("AfterAgentCreated ran before the model was prepared: %#v", f.Recorder.Created)
	}
	// A failed model leaves no Agent holding the reviewed name.
	failedID := agentbuilder.AgentBuilderResourceID("agent-deployment", f.Scope, "call-retry")
	if _, err := f.Agents.GetDeployment(ctx, f.Reference(), failedID); f.Provision.Created != 0 || !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		t.Fatalf("deployment after a failed model: created %d, %v", f.Provision.Created, err)
	}
	f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	f.Recorder.PrepareErr = nil
	output, err := f.Execute(t, models.AgentActionCreateAgent, "call-retry", arguments, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	if f.Provision.Created != 1 || len(f.Recorder.Prepared) != 2 || len(f.Recorder.Created) != 1 {
		t.Fatalf("created %d, prepared %d, after %d", f.Provision.Created, len(f.Recorder.Prepared), len(f.Recorder.Created))
	}
	deploymentID, _ := output["deploymentId"].(string)
	deployments, err := f.Agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: f.Reference()})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, deployment := range deployments {
		if deployment.ID == deploymentID {
			found++
		}
	}
	if found != 1 || !f.SpaceMember(t, f.SalesID, deploymentID) {
		t.Fatalf("deployment %s found %d times", deploymentID, found)
	}
}

func TestAgentBuilderUpdateAgentProfileActivatesCandidate(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "displayName": "Scout", "systemPrompt": "Research competitors every week.",
		"rationale": "The person wants a weekly scout.",
	})
	output, err := f.Execute(t, models.AgentActionUpdateAgent, "call-update", arguments, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	want := agentbuilder.AgentBehaviorActionCandidateVersion("1.0.0", "call-update")
	if !strings.Contains(want, ".action.") {
		t.Fatalf("candidate version = %q", want)
	}
	deployment, err := f.Agents.GetDeployment(ctx, f.Reference(), "researcher")
	if err != nil || deployment.ActiveVersion != want {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	definition, err := f.Agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.DisplayName != "Scout" || definition.SystemPrompt != "Research competitors every week." || definition.Purpose != "Researcher work" {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	if output["resourceType"] != models.AgentProfileResourceType || output["operation"] != models.AgentActionUpdateAgent ||
		output["deploymentId"] != "researcher" || output["displayName"] != "Scout" || output["replayed"] != false ||
		output["amendment"] == nil || output["activation"] == nil {
		t.Fatalf("output = %#v", output)
	}
	amendments, err := f.Agents.ListAmendments(ctx, f.Reference(), "researcher")
	if err != nil || len(amendments) != 1 || amendments[0].Status != kernelagent.AmendmentActivated ||
		amendments[0].ProposerType != "agent" || amendments[0].ProposerID != "finn" {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	activations, err := f.Agents.ListActivations(ctx, f.Reference(), "researcher")
	if err != nil || activations[len(activations)-1].ActorType != "user" || activations[len(activations)-1].ActorID != agentbuildertest.Approver.ID {
		t.Fatalf("activations = %#v, %v", activations, err)
	}
	if len(f.Recorder.Prepared) != 0 {
		t.Fatalf("a profile change prepared a model: %#v", f.Recorder.Prepared)
	}
	if len(f.Recorder.Created) != 1 || !f.Recorder.Created[0].Updated || f.Recorder.Created[0].DisplayName != "Scout" {
		t.Fatalf("created = %#v", f.Recorder.Created)
	}

	replay, err := f.Execute(t, models.AgentActionUpdateAgent, "call-update", arguments, agentbuildertest.Approver)
	if err != nil || replay["replayed"] != true || replay["displayName"] != "Scout" || replay["amendment"] == nil {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	amendments, err = f.Agents.ListAmendments(ctx, f.Reference(), "researcher")
	if err != nil || len(amendments) != 1 {
		t.Fatalf("amendments after replay = %#v, %v", amendments, err)
	}
}

func TestAgentBuilderUpdateAgentModelOnly(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "model": "gpt-6-luna", "rationale": "Faster model.",
	})
	output, err := f.Execute(t, models.AgentActionUpdateAgent, "call-model", arguments, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	if output["amendment"] != nil || output["activation"] != nil || output["replayed"] != false {
		t.Fatalf("output = %#v", output)
	}
	amendments, err := f.Agents.ListAmendments(ctx, f.Reference(), "researcher")
	if err != nil || len(amendments) != 0 {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	if len(f.Recorder.Prepared) != 1 || f.Recorder.Prepared[0] != (agentbuilder.AgentModelRequest{
		Scope: skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, DeploymentID: "researcher", Model: "gpt-6-luna",
		OwnerUserID: agentbuildertest.Approver.ID, ActionCallID: "call-model", Updated: true,
	}) {
		t.Fatalf("prepared = %#v", f.Recorder.Prepared)
	}
}

func TestAgentBuilderUpdateAgentSkills(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	skillScope := skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}
	bindingID := "builder:researcher:acme.notes"
	remove := f.Resolve(t, models.AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "removeSkillBindingIds": []interface{}{bindingID}, "rationale": "Notes are not needed.",
	})
	output, err := f.Execute(t, models.AgentActionUpdateAgent, "call-remove", remove, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := f.Catalog.GetBinding(ctx, skillScope, "researcher", bindingID)
	if err != nil || binding == nil || !binding.Disabled {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	if removed, _ := output["removedBindings"].([]interface{}); len(removed) != 1 {
		t.Fatalf("removedBindings = %#v", output["removedBindings"])
	}
	replay, err := f.Execute(t, models.AgentActionUpdateAgent, "call-remove", remove, agentbuildertest.Approver)
	if err != nil || replay["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if again, _ := f.Catalog.GetBinding(ctx, skillScope, "researcher", bindingID); again.Revision != binding.Revision {
		t.Fatalf("replay changed the binding: %#v", again)
	}

	add := f.Resolve(t, models.AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "rationale": "Notes again.",
		"addSkills": []interface{}{map[string]interface{}{
			"skillId": "acme.notes", "skillVersion": "1.0.0", "allowedActions": []interface{}{"write_note"},
			"enablePrompt": false, "maximumRisk": "write",
		}},
	})
	output, err = f.Execute(t, models.AgentActionUpdateAgent, "call-add", add, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := f.Catalog.GetBinding(ctx, skillScope, "researcher", bindingID)
	if err != nil || enabled.Disabled || enabled.Revision != binding.Revision+1 {
		t.Fatalf("re-added binding = %#v, %v", enabled, err)
	}
	if bindings, _ := output["bindings"].([]interface{}); len(bindings) != 1 {
		t.Fatalf("bindings = %#v", output["bindings"])
	}
	if _, err := f.Execute(t, models.AgentActionUpdateAgent, "call-add", add, agentbuildertest.Approver); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.Catalog.GetBinding(ctx, skillScope, "researcher", bindingID); again.Revision != enabled.Revision {
		t.Fatalf("replay changed the binding: %#v", again)
	}
}

func TestAgentBuilderUpdateAgentRefusals(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	revision := func(id string) int64 {
		deployment, err := f.Agents.GetDeployment(ctx, f.Reference(), id)
		if err != nil {
			t.Fatal(err)
		}
		return deployment.Revision
	}
	for name, test := range map[string]struct {
		arguments map[string]interface{}
		want      string
	}{
		"builder": {map[string]interface{}{"agentId": "finn", "expectedDeploymentRevision": revision("finn"), "purpose": "Other", "rationale": "x"}, "another Agent"},
		"fixed":   {map[string]interface{}{"agentId": "platform", "expectedDeploymentRevision": revision("platform"), "purpose": "Other", "rationale": "x"}, skillbinding.ErrAgentBehaviorFixed.Error()},
		"retired": {map[string]interface{}{"agentId": "old-agent", "expectedDeploymentRevision": revision("old-agent"), "purpose": "Other", "rationale": "x"}, "retired"},
		"bundled": {map[string]interface{}{"agentId": "researcher", "expectedDeploymentRevision": revision("researcher"), "removeSkillBindingIds": []interface{}{"bundled:agents"}, "rationale": "x"}, "built in"},
		"stale":   {map[string]interface{}{"agentId": "researcher", "expectedDeploymentRevision": revision("researcher") + 5, "purpose": "Other", "rationale": "x"}, "changed while the card waited"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.Execute(t, models.AgentActionUpdateAgent, "call-"+name, test.arguments, agentbuildertest.Approver); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if binding, err := f.Catalog.GetBinding(ctx, skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, "researcher", "bundled:agents"); err != nil || binding.Disabled {
		t.Fatalf("bundled binding = %#v, %v", binding, err)
	}
	if _, err := f.Execute(t, models.AgentActionUpdateAgent, "call-role", map[string]interface{}{
		"agentId": "researcher", "expectedDeploymentRevision": revision("researcher"), "purpose": "Other", "rationale": "x",
	}, models.ApprovalPrincipal{Type: "role", ID: "operator"}); err == nil || !strings.Contains(err.Error(), "only by a person") {
		t.Fatalf("role approver error = %v", err)
	}
}

func TestAgentBuilderListAgents(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	if _, err := f.Spaces.EnsureMemberHasSpace(ctx, f.Scope, agentbuildertest.Approver.ID, models.SpaceMemberAgent, "researcher"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.Spaces.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: f.Scope, SpaceID: f.SalesID, Type: models.SpaceMemberAgent, MemberID: "researcher"}); err != nil {
		t.Fatal(err)
	}
	output, err := f.Execute(t, models.AgentActionListAgents, "call-list", map[string]interface{}{}, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	if output["resourceType"] != "agent_list" {
		t.Fatalf("output = %#v", output)
	}
	listed := map[string]map[string]interface{}{}
	for _, entry := range output["agents"].([]interface{}) {
		agent := entry.(map[string]interface{})
		listed[agent["id"].(string)] = agent
	}
	if len(listed) != 3 || listed["old-agent"] != nil {
		t.Fatalf("agents = %#v", listed)
	}
	if listed["finn"]["manageable"] != false || listed["platform"]["manageable"] != false || listed["researcher"]["manageable"] != true {
		t.Fatalf("manageable = %#v", listed)
	}
	if listed["researcher"]["displayName"] != "Researcher" || listed["researcher"]["purpose"] != "Researcher work" ||
		listed["researcher"]["rolloutStatus"] != "active" || len(listed["researcher"]["spaceIds"].([]interface{})) != 2 {
		t.Fatalf("researcher = %#v", listed["researcher"])
	}
	filtered, err := f.Execute(t, models.AgentActionListAgents, "call-query", map[string]interface{}{"query": "PLATFORM"}, agentbuildertest.Approver)
	if err != nil || len(filtered["agents"].([]interface{})) != 1 {
		t.Fatalf("filtered = %#v, %v", filtered, err)
	}
}

func TestAgentBuilderDispatcherPassesOtherActionsOn(t *testing.T) {
	f := agentbuildertest.NewDispatchFixture(t)
	fallback := action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	dispatcher, err := agentbuildertest.NewDispatcher(f.Store, f.Agents, f.Teams, f.Catalog, f.Provision, nil, fallback)
	if err != nil {
		t.Fatal(err)
	}
	other := *f.Bound(t, models.AgentActionTryAgent)
	other.Action.Name = models.AgentActionAmendBehavior
	result, err := dispatcher.DispatchAction(context.Background(), agentrun.ActionDispatchInput{Bound: &other})
	if err != nil || result["fallback"] != true {
		t.Fatalf("amend_behavior = %#v, %v", result, err)
	}
	result, err = dispatcher.DispatchAction(context.Background(), agentrun.ActionDispatchInput{})
	if err != nil || result["fallback"] != true {
		t.Fatalf("other action = %#v, %v", result, err)
	}
}

// A builder Run parked on a pending card holds no lease, so it does not count
// against the Agent's concurrency: a new chat Run is still admitted
// (spec-openseal risk 10).
func TestAgentBuilderParkedRunDoesNotBlockNewChatRuns(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f := actiontest.NewLoopStore(t, store)
		_, approval := f.ProposeLoopApproval("create-agent", actiontest.LoopApprovalOptions{AlwaysReview: true})
		f.ParkOn(approval.ID)
		if f.Run.Status != models.AgentRunStatusWaitingForApproval {
			t.Fatalf("parked Run = %s", f.Run.Status)
		}
		portfolio := portfolio.NewService(store, models.WithClock(func() time.Time { return f.Now }))
		next, err := portfolio.CreateAgentRun(f.Ctx, models.CreateAgentRunRequest{
			Scope: f.Scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"},
			AssignedAgentID: "agent", Goal: "Build another Agent", Source: models.RunSourceChat,
		})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{
			Scope: f.Scope, WorkerID: "chat-worker", Now: f.Now.Add(time.Second), LeaseDuration: time.Minute,
			AgingInterval: time.Minute, AssignedAgentID: "agent", MaxActiveForAgent: 1,
		})
		if err != nil || claimed == nil || claimed.ID != next.ID {
			t.Fatalf("claim with a parked builder Run = %#v, %v", claimed, err)
		}
	})
}

// A create_agent that fails after its deployment committed retires it, so the
// half-built Agent doesn't keep the reviewed name.
func TestAgentBuilderCreateAgentRetiresAgentOnLateFailure(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	f.Provision.PutErr = errors.New("space store unavailable")
	if _, err := f.Execute(t, models.AgentActionCreateAgent, "call-late", arguments, agentbuildertest.Approver); err == nil ||
		!strings.Contains(err.Error(), "space store unavailable") || !strings.Contains(err.Error(), "propose create_agent again") {
		t.Fatalf("late failure error = %v", err)
	}
	deploymentID := agentbuilder.AgentBuilderResourceID("agent-deployment", f.Scope, "call-late")
	deployment, err := f.Agents.GetDeployment(ctx, f.Reference(), deploymentID)
	if err != nil || f.Provision.Created != 1 || deployment.RolloutStatus != kernelagent.RolloutRetired {
		t.Fatalf("deployment after a late failure = %#v, %v", deployment, err)
	}
	if len(f.Recorder.Created) != 0 {
		t.Fatalf("AfterAgentCreated ran for a failed create: %#v", f.Recorder.Created)
	}
	f.Provision.PutErr = nil
	// The name is free again, and the failed call does not revive its Agent.
	f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	if _, err := f.Execute(t, models.AgentActionCreateAgent, "call-late", arguments, agentbuildertest.Approver); err == nil ||
		!strings.Contains(err.Error(), "propose create_agent again") {
		t.Fatalf("replay of the failed call = %v", err)
	}
	output, err := f.Execute(t, models.AgentActionCreateAgent, "call-again", arguments, agentbuildertest.Approver)
	if err != nil || output["displayName"] != "Inbox   Triage" {
		t.Fatalf("second create = %#v, %v", output, err)
	}
}

// The created definition's ceiling is the reviewed grants' highest risk, so a
// Team made of the Agent keeps its write actions.
func TestAgentBuilderCreatedAgentKeepsGrantRiskInTeams(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.SalesID))
	output, err := f.Execute(t, models.AgentActionCreateAgent, "call-team-member", arguments, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := f.Agents.GetDeployment(ctx, f.Reference(), output["deploymentId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	definition, err := f.Agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.Authority.MaximumRisk != capability.RiskLevelWrite {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	team := agentbuilder.TeamBuilderDefinition("team-x", "Crew", []*kernelagent.AgentDefinition{definition}, "call-team", agentbuildertest.Approver.ID)
	// The Team Skill authority of the member in a Team deployment of that
	// definition with no restrictions of its own.
	validator, err := skillbinding.NewTeamSkillActionValidator(builderTeamAuthorityCatalog{
		deployment: &kernelteam.Deployment{
			ID: "team-x", Scope: f.Reference(), DefinitionID: team.ID, ActiveVersion: team.Version, Status: kernelteam.DeploymentActive,
			Roster: []kernelteam.RosterAssignment{{ID: "member", AgentDeploymentID: deployment.ID}},
		},
		definition: team, agent: deployment, agentDefinition: definition,
	})
	if err != nil {
		t.Fatal(err)
	}
	memberRun := &models.AgentRun{Scope: f.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-x"}, AssignedAgentID: deployment.ID}
	teamBinding := &skill.Binding{Scope: f.Reference(), DeploymentID: "team-x", SkillID: "acme.notes", SkillVersion: "1.0.0"}
	allowsRisk := func(risk capability.RiskLevel) bool {
		_, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{
			Run: memberRun, Bound: &skill.BoundAction{Binding: teamBinding, Action: capability.Action{Name: "write_note", Risk: risk}},
		})
		return err == nil
	}
	if !allowsRisk(capability.RiskLevelWrite) || allowsRisk(capability.RiskLevelExternal) {
		t.Fatalf("Team ceiling for the member: write %v, external %v", allowsRisk(capability.RiskLevelWrite), allowsRisk(capability.RiskLevelExternal))
	}
	external := agentbuilder.CloneAgentDefinitionForAction(definition)
	external.Authority.MaximumRisk = capability.RiskLevelExternal
	if got := agentbuilder.TeamMembersMaximumRisk([]*kernelagent.AgentDefinition{external}); got != capability.RiskLevelExternal {
		t.Fatalf("Team ceiling = %s, want the members' external", got)
	}
}

// update_agent raises the ceiling to an added grant, activated by the person
// who approved the card (an Agent amendment can't widen authority).
func TestAgentBuilderUpdateAgentRaisesAuthorityForAddedSkills(t *testing.T) {
	ctx := context.Background()
	f := agentbuildertest.NewDispatchFixture(t)
	arguments := f.Resolve(t, models.AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "purpose": "Research and take notes.", "rationale": "Notes need writing.",
		"addSkills": []interface{}{map[string]interface{}{
			"skillId": "acme.notes", "skillVersion": "1.0.0", "allowedActions": []interface{}{"write_note"},
			"enablePrompt": false, "maximumRisk": "write",
		}},
	})
	output, err := f.Execute(t, models.AgentActionUpdateAgent, "call-raise", arguments, agentbuildertest.Approver)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := f.Agents.GetDeployment(ctx, f.Reference(), "researcher")
	if err != nil || deployment.ActiveVersion != agentbuilder.AgentBehaviorActionCandidateVersion("1.0.0", "call-raise") {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	definition, err := f.Agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.Authority.MaximumRisk != capability.RiskLevelWrite || definition.Purpose != "Research and take notes." {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	activations, err := f.Agents.ListActivations(ctx, f.Reference(), "researcher")
	if err != nil || len(activations) != 2 || activations[1].ActorType != "user" || activations[1].ActorID != agentbuildertest.Approver.ID {
		t.Fatalf("activations = %#v, %v", activations, err)
	}
	if output["activation"] == nil || output["replayed"] != false {
		t.Fatalf("output = %#v", output)
	}
	replay, err := f.Execute(t, models.AgentActionUpdateAgent, "call-raise", arguments, agentbuildertest.Approver)
	if err != nil || replay["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if again, _ := f.Agents.ListActivations(ctx, f.Reference(), "researcher"); len(again) != 2 {
		t.Fatalf("replay activated again: %#v", again)
	}
}

// builderTeamAuthorityCatalog serves one Team and member Agent to the Team
// Skill authority.
type builderTeamAuthorityCatalog struct {
	deployment      *kernelteam.Deployment
	definition      *kernelteam.Definition
	agent           *kernelagent.AgentDeployment
	agentDefinition *kernelagent.AgentDefinition
}

func (c builderTeamAuthorityCatalog) GetAgentDeployment(context.Context, skill.ScopeReference, string) (*kernelagent.AgentDeployment, error) {
	return c.agent, nil
}

func (c builderTeamAuthorityCatalog) GetAgentDefinition(context.Context, string, string) (*kernelagent.AgentDefinition, error) {
	return c.agentDefinition, nil
}

func (c builderTeamAuthorityCatalog) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return c.deployment, nil
}

func (c builderTeamAuthorityCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return c.definition, nil
}
