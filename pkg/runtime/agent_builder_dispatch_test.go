package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

// agentBuilderDispatchStore serves the approvals the test decides.
type agentBuilderDispatchStore struct {
	*sqlite.Store
	approvals map[string]*ApprovalCheckpoint
}

func (s *agentBuilderDispatchStore) GetApproval(ctx context.Context, scope Scope, approvalID string) (*ApprovalCheckpoint, error) {
	if approval, ok := s.approvals[approvalID]; ok {
		return approval, nil
	}
	return s.Store.GetApproval(ctx, scope, approvalID)
}

// agentBuilderTestProvisioner does what the Engine does for the builder.
type agentBuilderTestProvisioner struct {
	agents        *kernelagent.Registry
	spaces        *SpaceService
	conversations *ConversationService
	created       int
	putErr        error
}

func (p *agentBuilderTestProvisioner) CreateAgentDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	scope := Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}
	if _, err := p.spaces.GetSpace(ctx, scope, spaceID); err != nil {
		return nil, nil, err
	}
	created, activation, err := p.agents.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err != nil {
		return nil, nil, err
	}
	p.created++
	if _, _, err := p.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: SpaceMemberAgent, MemberID: created.ID}); err != nil {
		return created, activation, err
	}
	if _, err := p.EnsureAgentControlConversation(ctx, created); err != nil {
		return created, activation, err
	}
	return created, activation, nil
}

func (p *agentBuilderTestProvisioner) EnsureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment) (*Conversation, error) {
	conversation, _, err := p.conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment.ID},
		Title: "Agent control", Origin: &ConversationReference{Kind: ConversationReferenceAgentControl, ID: "agent-" + deployment.ID},
		IdempotencyKey: "agent-control:" + deployment.ID,
	})
	return conversation, err
}

func (p *agentBuilderTestProvisioner) CreateTeamDeploymentInSpace(context.Context, string, *kernelteam.Deployment, string, string, string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return nil, nil, errors.New("not used")
}

func (p *agentBuilderTestProvisioner) ListSpaces(ctx context.Context, scope Scope) ([]*Space, error) {
	return p.spaces.ListSpaces(ctx, scope)
}

func (p *agentBuilderTestProvisioner) ListSpaceMembers(ctx context.Context, scope Scope, spaceID string) ([]*SpaceMember, error) {
	return p.spaces.ListSpaceMembers(ctx, scope, spaceID)
}

func (p *agentBuilderTestProvisioner) PutSpaceMember(ctx context.Context, request PutSpaceMemberRequest) (*SpaceMember, bool, error) {
	if p.putErr != nil {
		return nil, false, p.putErr
	}
	return p.spaces.PutSpaceMember(ctx, request)
}

// agentBuilderRecordingHost records what the dispatcher asks the host.
type agentBuilderRecordingHost struct {
	agentBuilderTestHost
	prepareErr error
	prepared   []AgentModelRequest
	created    []AgentCreated
}

func (h *agentBuilderRecordingHost) PrepareAgentModel(_ context.Context, request AgentModelRequest) error {
	h.prepared = append(h.prepared, request)
	return h.prepareErr
}

func (h *agentBuilderRecordingHost) AfterAgentCreated(_ context.Context, created AgentCreated) {
	h.created = append(h.created, created)
}

type agentBuilderDispatchFixture struct {
	*agentBuilderFixture
	store      *agentBuilderDispatchStore
	provision  *agentBuilderTestProvisioner
	recorder   *agentBuilderRecordingHost
	dispatcher *AgentBuilderActionDispatcher
}

func newAgentBuilderDispatchFixture(t *testing.T) *agentBuilderDispatchFixture {
	t.Helper()
	ctx := context.Background()
	base := newAgentBuilderFixture(t, false)
	store := &agentBuilderDispatchStore{Store: newTestKernelStore(t), approvals: map[string]*ApprovalCheckpoint{}}
	base.spaces = NewSpaceService(store)
	base.salesID, base.homeID = seedAgentBuilderSpaces(t, base.spaces, base.scope)
	recorder := &agentBuilderRecordingHost{}
	var err error
	base.validator, err = NewAgentBuilderActionValidator(base.agents, base.teams, base.catalog, base.spaces, store, store, recorder)
	if err != nil {
		t.Fatal(err)
	}
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: base.scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"},
		AssignedAgentID: "finn", ActingUser: agentBuilderApprover.ID, Goal: "Build an Agent", Source: RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	base.run = run
	provision := &agentBuilderTestProvisioner{agents: base.agents, spaces: base.spaces, conversations: NewConversationService(store)}
	dispatcher, err := newTestAgentBuilderDispatcher(store, base.agents, base.teams, base.catalog, provision, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &agentBuilderDispatchFixture{agentBuilderFixture: base, store: store, provision: provision, recorder: recorder, dispatcher: dispatcher}
}

var agentBuilderApprover = ApprovalPrincipal{Type: "user", ID: "person-1"}

// resolve fills the kernel-resolved arguments as the coordinator does at
// proposal time and checks the proposal passes validation.
func (f *agentBuilderDispatchFixture) resolve(t *testing.T, action string, arguments map[string]interface{}) map[string]interface{} {
	t.Helper()
	ctx := context.Background()
	input := ActionProposalValidationInput{Run: f.run, Bound: f.bound(t, action), Arguments: arguments}
	resolved, handled, err := f.validator.ResolveActionProposalArguments(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		input.Arguments = resolved
	}
	if _, err := f.validator.ValidateActionProposal(ctx, input); err != nil {
		t.Fatalf("%s proposal refused: %v", action, err)
	}
	return input.Arguments
}

// execute dispatches callID as approved by approver.
func (f *agentBuilderDispatchFixture) execute(t *testing.T, action, callID string, arguments map[string]interface{}, approver ApprovalPrincipal) (map[string]interface{}, error) {
	t.Helper()
	call := &ActionCall{
		ID: callID, Scope: f.scope, RunID: f.run.ID, DeploymentID: "finn", SkillID: AgentManagementSkillID,
		SkillVersion: AgentManagementSkillVersion, Action: action, Arguments: arguments,
	}
	if action != AgentActionListAgents {
		call.ApprovalID = "approval-" + callID
		decider := approver
		f.store.approvals[call.ApprovalID] = &ApprovalCheckpoint{
			ID: call.ApprovalID, Scope: f.scope, RunID: f.run.ID, ActionCallID: callID,
			Status: ApprovalStatusApproved, DecisionBy: &decider,
		}
	}
	return f.dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Call: call, Run: f.run, Bound: f.bound(t, action), Arguments: arguments,
	})
}

func (f *agentBuilderDispatchFixture) reference() capability.ScopeReference {
	return capability.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
}

func (f *agentBuilderDispatchFixture) spaceMember(t *testing.T, spaceID, deploymentID string) bool {
	t.Helper()
	members, err := f.spaces.ListSpaceMembers(context.Background(), f.scope, spaceID)
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		if member.Type == SpaceMemberAgent && member.ID == deploymentID {
			return true
		}
	}
	return false
}

func agentBuilderDispatchCreateArguments(spaceID string) map[string]interface{} {
	arguments := agentBuilderCreateArguments()
	arguments["spaceId"] = spaceID
	return arguments
}

func TestAgentBuilderCreateAgentExecutes(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	output, err := f.execute(t, AgentActionCreateAgent, "call-create", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	definitionID := agentBuilderResourceID("agent-definition", f.scope, "call-create")
	deploymentID := agentBuilderResourceID("agent-deployment", f.scope, "call-create")
	if len(definitionID) != 21 || len(deploymentID) != 21 || definitionID == deploymentID {
		t.Fatalf("ids = %q, %q", definitionID, deploymentID)
	}
	if again := agentBuilderResourceID("agent-deployment", f.scope, "call-create"); again != deploymentID {
		t.Fatalf("deployment id is not deterministic: %q != %q", again, deploymentID)
	}
	if output["resourceType"] != agentProfileResourceType || output["operation"] != AgentActionCreateAgent ||
		output["deploymentId"] != deploymentID || output["displayName"] != "Inbox   Triage" || output["replayed"] != false {
		t.Fatalf("output head = %#v", output)
	}
	definition, err := f.agents.GetDefinition(ctx, definitionID, "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if definition.Purpose != "Sort the support inbox every morning." || definition.Personality != "Friendly" ||
		len(definition.OperatingPrinciples) != 1 || definition.Authority.MaximumRisk != capability.RiskLevelWrite ||
		definition.Authority.MaxConcurrentRuns != 1 || !definition.Amendments.AgentMayPropose ||
		definition.Provenance.Source != "agent-builder" || definition.Provenance.Reference != "call-create" ||
		definition.Provenance.CreatedBy != agentBuilderApprover.ID || definition.AuthoringKey != "" {
		t.Fatalf("definition = %#v", definition)
	}
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), deploymentID)
	if err != nil || deployment.DefinitionID != definitionID || deployment.RolloutStatus != kernelagent.RolloutActive ||
		deployment.ActiveVersion != "1.0.0" || deployment.Capacity.MaxConcurrentRuns != 1 {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	binding, err := f.catalog.GetBinding(ctx, skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, deploymentID, "builder:"+deploymentID+":acme.notes")
	if err != nil || binding == nil || binding.Disabled || binding.MaximumRisk != skill.RiskLevelWrite ||
		len(binding.Lifecycle) != 1 || binding.Lifecycle[0].Actor.Type != "user" || binding.Lifecycle[0].Actor.ID != agentBuilderApprover.ID {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	if !f.spaceMember(t, f.salesID, deploymentID) || f.spaceMember(t, f.homeID, deploymentID) {
		t.Fatal("the created Agent is not only in the reviewed space")
	}
	control, err := f.provision.EnsureAgentControlConversation(ctx, deployment)
	if err != nil {
		t.Fatal(err)
	}
	chat, _ := output["chat"].(map[string]interface{})
	if chat["conversationId"] != control.ID {
		t.Fatalf("chat = %#v, want %s", chat, control.ID)
	}
	space, _ := output["space"].(map[string]interface{})
	if space["id"] != f.salesID || space["name"] != "Sales" {
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
	if len(f.recorder.prepared) != 1 || f.recorder.prepared[0] != (AgentModelRequest{
		Scope: skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, DeploymentID: deploymentID, Model: "gpt-6-luna",
		OwnerUserID: agentBuilderApprover.ID, ActionCallID: "call-create",
	}) {
		t.Fatalf("prepared = %#v", f.recorder.prepared)
	}
	if len(f.recorder.created) != 1 || f.recorder.created[0] != (AgentCreated{
		Scope: f.scope, DeploymentID: deploymentID, DisplayName: "Inbox   Triage", Purpose: "Sort the support inbox every morning.",
		ActionCallID: "call-create", RunID: f.run.ID, BillingUserID: agentBuilderApprover.ID,
	}) {
		t.Fatalf("created = %#v", f.recorder.created)
	}

	// The same call again (a crash-resumed execution) converges.
	replay, err := f.execute(t, AgentActionCreateAgent, "call-create", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	if replay["replayed"] != true || replay["deploymentId"] != deploymentID {
		t.Fatalf("replay = %#v", replay)
	}
	if f.provision.created != 1 {
		t.Fatalf("deployments created = %d", f.provision.created)
	}
	activations, err := f.agents.ListActivations(ctx, f.reference(), deploymentID)
	if err != nil || len(activations) != 1 {
		t.Fatalf("activations = %#v, %v", activations, err)
	}
	again, err := f.catalog.GetBinding(ctx, skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, deploymentID, binding.ID)
	if err != nil || again.Revision != binding.Revision {
		t.Fatalf("replayed binding = %#v, %v", again, err)
	}
	versions, err := f.agents.ListDefinitionVersions(ctx, definitionID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("definition versions = %#v, %v", versions, err)
	}
}

func TestAgentBuilderCreateAgentInDefaultSpace(t *testing.T) {
	f := newAgentBuilderDispatchFixture(t)
	arguments := agentBuilderDispatchCreateArguments(f.homeID)
	delete(arguments, "skills")
	delete(arguments, "connections")
	delete(arguments, "model")
	output, err := f.execute(t, AgentActionCreateAgent, "call-personal", f.resolve(t, AgentActionCreateAgent, arguments), agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	deploymentID, _ := output["deploymentId"].(string)
	if !f.spaceMember(t, f.homeID, deploymentID) || f.spaceMember(t, f.salesID, deploymentID) {
		t.Fatal("the created Agent is not only in the person's default space")
	}
	if message, _ := output["message"].(string); strings.Contains(message, "request_connection") {
		t.Fatalf("message = %q", message)
	}
	if len(f.recorder.prepared) != 1 || f.recorder.prepared[0].Model != "" {
		t.Fatalf("prepared = %#v", f.recorder.prepared)
	}
}

func TestAgentBuilderCreateAgentRefusesNonUserApprover(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	for _, approver := range []ApprovalPrincipal{{Type: "role", ID: "operator"}, {Type: "agent", ID: "finn"}} {
		if _, err := f.execute(t, AgentActionCreateAgent, "call-"+approver.Type, arguments, approver); err == nil ||
			!strings.Contains(err.Error(), "only by a person") {
			t.Fatalf("%s approver error = %v", approver.Type, err)
		}
		deploymentID := agentBuilderResourceID("agent-deployment", f.scope, "call-"+approver.Type)
		if _, err := f.agents.GetDeployment(ctx, f.reference(), deploymentID); !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
			t.Fatalf("deployment after refused approver: %v", err)
		}
	}
	if len(f.recorder.created) != 0 {
		t.Fatalf("created = %#v", f.recorder.created)
	}
}

func TestAgentBuilderCreateAgentFailsWhenNameTakenWhileWaiting(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	definition, err := f.agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "rival-definition", Version: "1.0.0", DisplayName: "inbox triage", Purpose: "Rival", SystemPrompt: "Rival.",
		Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
		ID: "rival", Scope: f.reference(), DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: kernelagent.RolloutActive, Environment: "default", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "operator", "fixture"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.execute(t, AgentActionCreateAgent, "call-taken", arguments, agentBuilderApprover); err == nil ||
		!strings.Contains(err.Error(), "already used") || !strings.Contains(err.Error(), "another name") {
		t.Fatalf("taken name error = %v", err)
	}
	if f.provision.created != 0 || len(f.recorder.prepared) != 0 {
		t.Fatalf("created %d, prepared %#v", f.provision.created, f.recorder.prepared)
	}
}

func TestAgentBuilderCreateAgentRetriesAfterModelFailure(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	f.recorder.prepareErr = errors.New("gateway unavailable")
	if _, err := f.execute(t, AgentActionCreateAgent, "call-retry", arguments, agentBuilderApprover); err == nil ||
		!strings.Contains(err.Error(), "gateway unavailable") {
		t.Fatalf("first attempt error = %v", err)
	}
	if len(f.recorder.created) != 0 {
		t.Fatalf("AfterAgentCreated ran before the model was prepared: %#v", f.recorder.created)
	}
	// A failed model leaves no Agent holding the reviewed name.
	failedID := agentBuilderResourceID("agent-deployment", f.scope, "call-retry")
	if _, err := f.agents.GetDeployment(ctx, f.reference(), failedID); f.provision.created != 0 || !errors.Is(err, kernelagent.ErrDeploymentNotFound) {
		t.Fatalf("deployment after a failed model: created %d, %v", f.provision.created, err)
	}
	f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	f.recorder.prepareErr = nil
	output, err := f.execute(t, AgentActionCreateAgent, "call-retry", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	if f.provision.created != 1 || len(f.recorder.prepared) != 2 || len(f.recorder.created) != 1 {
		t.Fatalf("created %d, prepared %d, after %d", f.provision.created, len(f.recorder.prepared), len(f.recorder.created))
	}
	deploymentID, _ := output["deploymentId"].(string)
	deployments, err := f.agents.ListDeployments(ctx, kernelagent.AgentDeploymentFilter{Scope: f.reference()})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, deployment := range deployments {
		if deployment.ID == deploymentID {
			found++
		}
	}
	if found != 1 || !f.spaceMember(t, f.salesID, deploymentID) {
		t.Fatalf("deployment %s found %d times", deploymentID, found)
	}
}

func TestAgentBuilderUpdateAgentProfileActivatesCandidate(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "displayName": "Scout", "systemPrompt": "Research competitors every week.",
		"rationale": "The person wants a weekly scout.",
	})
	output, err := f.execute(t, AgentActionUpdateAgent, "call-update", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	want := agentBehaviorActionCandidateVersion("1.0.0", "call-update")
	if !strings.Contains(want, ".action.") {
		t.Fatalf("candidate version = %q", want)
	}
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), "researcher")
	if err != nil || deployment.ActiveVersion != want {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	definition, err := f.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.DisplayName != "Scout" || definition.SystemPrompt != "Research competitors every week." || definition.Purpose != "Researcher work" {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	if output["resourceType"] != agentProfileResourceType || output["operation"] != AgentActionUpdateAgent ||
		output["deploymentId"] != "researcher" || output["displayName"] != "Scout" || output["replayed"] != false ||
		output["amendment"] == nil || output["activation"] == nil {
		t.Fatalf("output = %#v", output)
	}
	amendments, err := f.agents.ListAmendments(ctx, f.reference(), "researcher")
	if err != nil || len(amendments) != 1 || amendments[0].Status != kernelagent.AmendmentActivated ||
		amendments[0].ProposerType != "agent" || amendments[0].ProposerID != "finn" {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	activations, err := f.agents.ListActivations(ctx, f.reference(), "researcher")
	if err != nil || activations[len(activations)-1].ActorType != "user" || activations[len(activations)-1].ActorID != agentBuilderApprover.ID {
		t.Fatalf("activations = %#v, %v", activations, err)
	}
	if len(f.recorder.prepared) != 0 {
		t.Fatalf("a profile change prepared a model: %#v", f.recorder.prepared)
	}
	if len(f.recorder.created) != 1 || !f.recorder.created[0].Updated || f.recorder.created[0].DisplayName != "Scout" {
		t.Fatalf("created = %#v", f.recorder.created)
	}

	replay, err := f.execute(t, AgentActionUpdateAgent, "call-update", arguments, agentBuilderApprover)
	if err != nil || replay["replayed"] != true || replay["displayName"] != "Scout" || replay["amendment"] == nil {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	amendments, err = f.agents.ListAmendments(ctx, f.reference(), "researcher")
	if err != nil || len(amendments) != 1 {
		t.Fatalf("amendments after replay = %#v, %v", amendments, err)
	}
}

func TestAgentBuilderUpdateAgentModelOnly(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "model": "gpt-6-luna", "rationale": "Faster model.",
	})
	output, err := f.execute(t, AgentActionUpdateAgent, "call-model", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	if output["amendment"] != nil || output["activation"] != nil || output["replayed"] != false {
		t.Fatalf("output = %#v", output)
	}
	amendments, err := f.agents.ListAmendments(ctx, f.reference(), "researcher")
	if err != nil || len(amendments) != 0 {
		t.Fatalf("amendments = %#v, %v", amendments, err)
	}
	if len(f.recorder.prepared) != 1 || f.recorder.prepared[0] != (AgentModelRequest{
		Scope: skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, DeploymentID: "researcher", Model: "gpt-6-luna",
		OwnerUserID: agentBuilderApprover.ID, ActionCallID: "call-model", Updated: true,
	}) {
		t.Fatalf("prepared = %#v", f.recorder.prepared)
	}
}

func TestAgentBuilderUpdateAgentSkills(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	skillScope := skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
	bindingID := "builder:researcher:acme.notes"
	remove := f.resolve(t, AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "removeSkillBindingIds": []interface{}{bindingID}, "rationale": "Notes are not needed.",
	})
	output, err := f.execute(t, AgentActionUpdateAgent, "call-remove", remove, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := f.catalog.GetBinding(ctx, skillScope, "researcher", bindingID)
	if err != nil || binding == nil || !binding.Disabled {
		t.Fatalf("binding = %#v, %v", binding, err)
	}
	if removed, _ := output["removedBindings"].([]interface{}); len(removed) != 1 {
		t.Fatalf("removedBindings = %#v", output["removedBindings"])
	}
	replay, err := f.execute(t, AgentActionUpdateAgent, "call-remove", remove, agentBuilderApprover)
	if err != nil || replay["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if again, _ := f.catalog.GetBinding(ctx, skillScope, "researcher", bindingID); again.Revision != binding.Revision {
		t.Fatalf("replay changed the binding: %#v", again)
	}

	add := f.resolve(t, AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "rationale": "Notes again.",
		"addSkills": []interface{}{map[string]interface{}{
			"skillId": "acme.notes", "skillVersion": "1.0.0", "allowedActions": []interface{}{"write_note"},
			"enablePrompt": false, "maximumRisk": "write",
		}},
	})
	output, err = f.execute(t, AgentActionUpdateAgent, "call-add", add, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := f.catalog.GetBinding(ctx, skillScope, "researcher", bindingID)
	if err != nil || enabled.Disabled || enabled.Revision != binding.Revision+1 {
		t.Fatalf("re-added binding = %#v, %v", enabled, err)
	}
	if bindings, _ := output["bindings"].([]interface{}); len(bindings) != 1 {
		t.Fatalf("bindings = %#v", output["bindings"])
	}
	if _, err := f.execute(t, AgentActionUpdateAgent, "call-add", add, agentBuilderApprover); err != nil {
		t.Fatal(err)
	}
	if again, _ := f.catalog.GetBinding(ctx, skillScope, "researcher", bindingID); again.Revision != enabled.Revision {
		t.Fatalf("replay changed the binding: %#v", again)
	}
}

func TestAgentBuilderUpdateAgentRefusals(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	revision := func(id string) int64 {
		deployment, err := f.agents.GetDeployment(ctx, f.reference(), id)
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
		"fixed":   {map[string]interface{}{"agentId": "platform", "expectedDeploymentRevision": revision("platform"), "purpose": "Other", "rationale": "x"}, ErrAgentBehaviorFixed.Error()},
		"retired": {map[string]interface{}{"agentId": "old-agent", "expectedDeploymentRevision": revision("old-agent"), "purpose": "Other", "rationale": "x"}, "retired"},
		"bundled": {map[string]interface{}{"agentId": "researcher", "expectedDeploymentRevision": revision("researcher"), "removeSkillBindingIds": []interface{}{"bundled:agents"}, "rationale": "x"}, "built in"},
		"stale":   {map[string]interface{}{"agentId": "researcher", "expectedDeploymentRevision": revision("researcher") + 5, "purpose": "Other", "rationale": "x"}, "changed while the card waited"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.execute(t, AgentActionUpdateAgent, "call-"+name, test.arguments, agentBuilderApprover); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
	if binding, err := f.catalog.GetBinding(ctx, skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, "researcher", "bundled:agents"); err != nil || binding.Disabled {
		t.Fatalf("bundled binding = %#v, %v", binding, err)
	}
	if _, err := f.execute(t, AgentActionUpdateAgent, "call-role", map[string]interface{}{
		"agentId": "researcher", "expectedDeploymentRevision": revision("researcher"), "purpose": "Other", "rationale": "x",
	}, ApprovalPrincipal{Type: "role", ID: "operator"}); err == nil || !strings.Contains(err.Error(), "only by a person") {
		t.Fatalf("role approver error = %v", err)
	}
}

func TestAgentBuilderListAgents(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	if _, err := f.spaces.EnsureMemberHasSpace(ctx, f.scope, agentBuilderApprover.ID, SpaceMemberAgent, "researcher"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.spaces.PutSpaceMember(ctx, PutSpaceMemberRequest{Scope: f.scope, SpaceID: f.salesID, Type: SpaceMemberAgent, MemberID: "researcher"}); err != nil {
		t.Fatal(err)
	}
	output, err := f.execute(t, AgentActionListAgents, "call-list", map[string]interface{}{}, agentBuilderApprover)
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
	filtered, err := f.execute(t, AgentActionListAgents, "call-query", map[string]interface{}{"query": "PLATFORM"}, agentBuilderApprover)
	if err != nil || len(filtered["agents"].([]interface{})) != 1 {
		t.Fatalf("filtered = %#v, %v", filtered, err)
	}
}

func TestAgentBuilderDispatcherPassesOtherActionsOn(t *testing.T) {
	f := newAgentBuilderDispatchFixture(t)
	fallback := ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
		return map[string]interface{}{"fallback": true}, nil
	})
	dispatcher, err := newTestAgentBuilderDispatcher(f.store, f.agents, f.teams, f.catalog, f.provision, nil, fallback)
	if err != nil {
		t.Fatal(err)
	}
	other := *f.bound(t, AgentActionTryAgent)
	other.Action.Name = AgentActionAmendBehavior
	result, err := dispatcher.DispatchAction(context.Background(), ActionDispatchInput{Bound: &other})
	if err != nil || result["fallback"] != true {
		t.Fatalf("amend_behavior = %#v, %v", result, err)
	}
	result, err = dispatcher.DispatchAction(context.Background(), ActionDispatchInput{})
	if err != nil || result["fallback"] != true {
		t.Fatalf("other action = %#v, %v", result, err)
	}
}

// A builder Run parked on a pending card holds no lease, so it does not count
// against the Agent's concurrency: a new chat Run is still admitted
// (spec-openseal risk 10).
func TestAgentBuilderParkedRunDoesNotBlockNewChatRuns(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		_, approval := f.proposeLoopApproval("create-agent", loopApprovalOptions{alwaysReview: true})
		f.parkOn(approval.ID)
		if f.run.Status != AgentRunStatusWaitingForApproval {
			t.Fatalf("parked Run = %s", f.run.Status)
		}
		portfolio := NewPortfolioService(store, models.WithClock(func() time.Time { return f.now }))
		next, err := portfolio.CreateAgentRun(f.ctx, CreateAgentRunRequest{
			Scope: f.scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
			AssignedAgentID: "agent", Goal: "Build another Agent", Source: RunSourceChat,
		})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := store.ClaimNextAgentRun(f.ctx, AgentRunClaim{
			Scope: f.scope, WorkerID: "chat-worker", Now: f.now.Add(time.Second), LeaseDuration: time.Minute,
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
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	f.provision.putErr = errors.New("space store unavailable")
	if _, err := f.execute(t, AgentActionCreateAgent, "call-late", arguments, agentBuilderApprover); err == nil ||
		!strings.Contains(err.Error(), "space store unavailable") || !strings.Contains(err.Error(), "propose create_agent again") {
		t.Fatalf("late failure error = %v", err)
	}
	deploymentID := agentBuilderResourceID("agent-deployment", f.scope, "call-late")
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), deploymentID)
	if err != nil || f.provision.created != 1 || deployment.RolloutStatus != kernelagent.RolloutRetired {
		t.Fatalf("deployment after a late failure = %#v, %v", deployment, err)
	}
	if len(f.recorder.created) != 0 {
		t.Fatalf("AfterAgentCreated ran for a failed create: %#v", f.recorder.created)
	}
	f.provision.putErr = nil
	// The name is free again, and the failed call does not revive its Agent.
	f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	if _, err := f.execute(t, AgentActionCreateAgent, "call-late", arguments, agentBuilderApprover); err == nil ||
		!strings.Contains(err.Error(), "propose create_agent again") {
		t.Fatalf("replay of the failed call = %v", err)
	}
	output, err := f.execute(t, AgentActionCreateAgent, "call-again", arguments, agentBuilderApprover)
	if err != nil || output["displayName"] != "Inbox   Triage" {
		t.Fatalf("second create = %#v, %v", output, err)
	}
}

// The created definition's ceiling is the reviewed grants' highest risk, so a
// Team made of the Agent keeps its write actions.
func TestAgentBuilderCreatedAgentKeepsGrantRiskInTeams(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionCreateAgent, agentBuilderDispatchCreateArguments(f.salesID))
	output, err := f.execute(t, AgentActionCreateAgent, "call-team-member", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), output["deploymentId"].(string))
	if err != nil {
		t.Fatal(err)
	}
	definition, err := f.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.Authority.MaximumRisk != capability.RiskLevelWrite {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	members := []resolvedTeamMember{{deployment: deployment, definition: definition, name: definition.DisplayName}}
	team := teamBuilderDefinition("team-x", teamCreateArguments{DisplayName: "Crew"}, members, "call-team", agentBuilderApprover.ID)
	authority := &teamSkillAuthority{deployment: &kernelteam.Deployment{}, definition: team, agent: deployment, agentDefinition: definition}
	if !authority.allowsRisk(capability.RiskLevelWrite) || authority.allowsRisk(capability.RiskLevelExternal) {
		t.Fatalf("Team ceiling for the member: write %v, external %v", authority.allowsRisk(capability.RiskLevelWrite), authority.allowsRisk(capability.RiskLevelExternal))
	}
	external := cloneAgentDefinitionForAction(definition)
	external.Authority.MaximumRisk = capability.RiskLevelExternal
	members[0].definition = external
	if got := teamMembersMaximumRisk(members); got != capability.RiskLevelExternal {
		t.Fatalf("Team ceiling = %s, want the members' external", got)
	}
}

// update_agent raises the ceiling to an added grant, activated by the person
// who approved the card (an Agent amendment can't widen authority).
func TestAgentBuilderUpdateAgentRaisesAuthorityForAddedSkills(t *testing.T) {
	ctx := context.Background()
	f := newAgentBuilderDispatchFixture(t)
	arguments := f.resolve(t, AgentActionUpdateAgent, map[string]interface{}{
		"agentId": "researcher", "purpose": "Research and take notes.", "rationale": "Notes need writing.",
		"addSkills": []interface{}{map[string]interface{}{
			"skillId": "acme.notes", "skillVersion": "1.0.0", "allowedActions": []interface{}{"write_note"},
			"enablePrompt": false, "maximumRisk": "write",
		}},
	})
	output, err := f.execute(t, AgentActionUpdateAgent, "call-raise", arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	deployment, err := f.agents.GetDeployment(ctx, f.reference(), "researcher")
	if err != nil || deployment.ActiveVersion != agentBehaviorActionCandidateVersion("1.0.0", "call-raise") {
		t.Fatalf("deployment = %#v, %v", deployment, err)
	}
	definition, err := f.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil || definition.Authority.MaximumRisk != capability.RiskLevelWrite || definition.Purpose != "Research and take notes." {
		t.Fatalf("definition = %#v, %v", definition, err)
	}
	activations, err := f.agents.ListActivations(ctx, f.reference(), "researcher")
	if err != nil || len(activations) != 2 || activations[1].ActorType != "user" || activations[1].ActorID != agentBuilderApprover.ID {
		t.Fatalf("activations = %#v, %v", activations, err)
	}
	if output["activation"] == nil || output["replayed"] != false {
		t.Fatalf("output = %#v", output)
	}
	replay, err := f.execute(t, AgentActionUpdateAgent, "call-raise", arguments, agentBuilderApprover)
	if err != nil || replay["replayed"] != true {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	if again, _ := f.agents.ListActivations(ctx, f.reference(), "researcher"); len(again) != 2 {
		t.Fatalf("replay activated again: %#v", again)
	}
}

// newTestAgentBuilderDispatcher builds a dispatcher over its own installer.
func newTestAgentBuilderDispatcher(
	store KernelStore, agents *kernelagent.Registry, teams *kernelteam.Registry, catalog *skill.Catalog,
	provision AgentProvisioner, host AgentBuilderHost, fallback ActionDispatcher,
) (*AgentBuilderActionDispatcher, error) {
	installer, err := NewAgentInstaller(store, agents, teams, catalog, provision, host)
	if err != nil {
		return nil, err
	}
	return NewAgentBuilderActionDispatcher(installer, fallback)
}
