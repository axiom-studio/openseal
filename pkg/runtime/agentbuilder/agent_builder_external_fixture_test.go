// The builder, dispatch and trial fixtures the external agent builder tests
// share. Runtime's in-package tests carry copies of them in
// agent_builder_shared_fixture_test.go.

package agentbuilder_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
	"github.com/google/uuid"
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
		return agentbuilder.ErrAgentChangeForbidden
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

func (h *agentBuilderTestHost) PrepareAgentModel(context.Context, agentbuilder.AgentModelRequest) error {
	return nil
}

func (h *agentBuilderTestHost) AfterAgentCreated(context.Context, agentbuilder.AgentCreated) {}

type agentBuilderFixture struct {
	scope   models.Scope
	agents  *kernelagent.Registry
	teams   *kernelteam.Registry
	catalog *skill.Catalog
	spaces  *portfolio.SpaceService
	salesID string
	// homeID is the default space of the person the fixture's run acts for.
	homeID    string
	run       *models.AgentRun
	host      *agentBuilderTestHost
	validator *agentbuilder.AgentBuilderActionValidator
}

func newAgentBuilderFixture(t *testing.T, withHost bool) *agentBuilderFixture {
	t.Helper()
	ctx := context.Background()
	scope := models.Scope{Kind: "tenant", ID: "tenant-a"}
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
		models.AgentManagementSkill(),
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
			SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
			AllowedActions: []string{models.AgentActionListAgents, models.AgentActionCreateAgent, models.AgentActionUpdateAgent, models.AgentActionTryAgent},
			MaximumRisk:    skill.RiskLevelWrite,
		},
		{
			ID: "builder:researcher:acme.notes", Revision: 1, Scope: skillScope, DeploymentID: "researcher",
			SkillID: "acme.notes", SkillVersion: "1.0.0", AllowedActions: []string{"write_note"}, MaximumRisk: skill.RiskLevelWrite,
		},
		{
			ID: "bundled:agents", Revision: 1, Scope: skillScope, DeploymentID: "researcher",
			SkillID: models.AgentManagementSkillID, SkillVersion: models.AgentManagementSkillVersion,
			AllowedActions: []string{models.AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite,
		},
	} {
		if err := catalog.Bind(ctx, binding); err != nil {
			t.Fatal(err)
		}
	}

	spaces := portfolio.NewSpaceService(conversationtest.NewStore(t))
	salesID, homeID := seedAgentBuilderSpaces(t, spaces, scope)
	fixture := &agentBuilderFixture{
		scope: scope, agents: agents, teams: teams, catalog: catalog, spaces: spaces, salesID: salesID, homeID: homeID,
		run: &models.AgentRun{
			ID: "builder-run", Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"},
			AssignedAgentID: "finn", ActingUser: agentBuilderApprover.ID, Source: models.RunSourceChat,
		},
	}
	var host agentbuilder.AgentBuilderHost
	if withHost {
		fixture.host = &agentBuilderTestHost{}
		host = fixture.host
	}
	fixture.validator, err = agentbuilder.NewAgentBuilderActionValidator(agents, teams, catalog, spaces, nil, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// seedAgentBuilderSpaces creates Sales, owned by an operator, and Home, the
// default space of the person the builder run acts for.
func seedAgentBuilderSpaces(t *testing.T, spaces *portfolio.SpaceService, scope models.Scope) (salesID, homeID string) {
	t.Helper()
	ctx := context.Background()
	sales, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Sales", Look: models.DefaultSpaceLook, OwnerID: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: agentBuilderApprover.ID})
	if err != nil || !home.IsDefault {
		t.Fatalf("home = %+v, err = %v", home, err)
	}
	return sales.ID, home.ID
}

func (f *agentBuilderFixture) bound(t *testing.T, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.catalog.Resolve(context.Background(), skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}, "finn",
		models.AgentManagementSkillID, models.AgentManagementSkillVersion, action)
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
	input := agentrun.ActionProposalValidationInput{Run: f.run, Bound: bound, Arguments: arguments}
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

// agentBuilderDispatchStore serves the approvals the test decides.
type agentBuilderDispatchStore struct {
	*sqlite.Store
	approvals map[string]*models.ApprovalCheckpoint
}

func (s *agentBuilderDispatchStore) GetApproval(ctx context.Context, scope models.Scope, approvalID string) (*models.ApprovalCheckpoint, error) {
	if approval, ok := s.approvals[approvalID]; ok {
		return approval, nil
	}
	return s.Store.GetApproval(ctx, scope, approvalID)
}

// agentBuilderTestProvisioner does what the Engine does for the builder.
type agentBuilderTestProvisioner struct {
	agents        *kernelagent.Registry
	spaces        *portfolio.SpaceService
	conversations *conversation.ConversationService
	created       int
	putErr        error
}

func (p *agentBuilderTestProvisioner) CreateAgentDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	scope := models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}
	if _, err := p.spaces.GetSpace(ctx, scope, spaceID); err != nil {
		return nil, nil, err
	}
	created, activation, err := p.agents.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err != nil {
		return nil, nil, err
	}
	p.created++
	if _, _, err := p.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: models.SpaceMemberAgent, MemberID: created.ID}); err != nil {
		return created, activation, err
	}
	if _, err := p.EnsureAgentControlConversation(ctx, created); err != nil {
		return created, activation, err
	}
	return created, activation, nil
}

func (p *agentBuilderTestProvisioner) EnsureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment) (*models.Conversation, error) {
	conversation, _, err := p.conversations.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deployment.ID},
		Title: "Agent control", Origin: &models.ConversationReference{Kind: models.ConversationReferenceAgentControl, ID: "agent-" + deployment.ID},
		IdempotencyKey: "agent-control:" + deployment.ID,
	})
	return conversation, err
}

func (p *agentBuilderTestProvisioner) CreateTeamDeploymentInSpace(context.Context, string, *kernelteam.Deployment, string, string, string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return nil, nil, errors.New("not used")
}

func (p *agentBuilderTestProvisioner) ListSpaces(ctx context.Context, scope models.Scope) ([]*models.Space, error) {
	return p.spaces.ListSpaces(ctx, scope)
}

func (p *agentBuilderTestProvisioner) ListSpaceMembers(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpaceMember, error) {
	return p.spaces.ListSpaceMembers(ctx, scope, spaceID)
}

func (p *agentBuilderTestProvisioner) PutSpaceMember(ctx context.Context, request portfolio.PutSpaceMemberRequest) (*models.SpaceMember, bool, error) {
	if p.putErr != nil {
		return nil, false, p.putErr
	}
	return p.spaces.PutSpaceMember(ctx, request)
}

// agentBuilderRecordingHost records what the dispatcher asks the host.
type agentBuilderRecordingHost struct {
	agentBuilderTestHost
	prepareErr error
	prepared   []agentbuilder.AgentModelRequest
	created    []agentbuilder.AgentCreated
}

func (h *agentBuilderRecordingHost) PrepareAgentModel(_ context.Context, request agentbuilder.AgentModelRequest) error {
	h.prepared = append(h.prepared, request)
	return h.prepareErr
}

func (h *agentBuilderRecordingHost) AfterAgentCreated(_ context.Context, created agentbuilder.AgentCreated) {
	h.created = append(h.created, created)
}

type agentBuilderDispatchFixture struct {
	*agentBuilderFixture
	store      *agentBuilderDispatchStore
	provision  *agentBuilderTestProvisioner
	recorder   *agentBuilderRecordingHost
	dispatcher *agentbuilder.AgentBuilderActionDispatcher
}

func newAgentBuilderDispatchFixture(t *testing.T) *agentBuilderDispatchFixture {
	t.Helper()
	ctx := context.Background()
	base := newAgentBuilderFixture(t, false)
	store := &agentBuilderDispatchStore{Store: conversationtest.NewStore(t), approvals: map[string]*models.ApprovalCheckpoint{}}
	base.spaces = portfolio.NewSpaceService(store)
	base.salesID, base.homeID = seedAgentBuilderSpaces(t, base.spaces, base.scope)
	recorder := &agentBuilderRecordingHost{}
	var err error
	base.validator, err = agentbuilder.NewAgentBuilderActionValidator(base.agents, base.teams, base.catalog, base.spaces, store, store, recorder)
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: base.scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"},
		AssignedAgentID: "finn", ActingUser: agentBuilderApprover.ID, Goal: "Build an Agent", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	base.run = run
	provision := &agentBuilderTestProvisioner{agents: base.agents, spaces: base.spaces, conversations: conversation.NewConversationService(store)}
	dispatcher, err := newTestAgentBuilderDispatcher(store, base.agents, base.teams, base.catalog, provision, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &agentBuilderDispatchFixture{agentBuilderFixture: base, store: store, provision: provision, recorder: recorder, dispatcher: dispatcher}
}

var agentBuilderApprover = models.ApprovalPrincipal{Type: "user", ID: "person-1"}

// resolve fills the kernel-resolved arguments as the coordinator does at
// proposal time and checks the proposal passes validation.
func (f *agentBuilderDispatchFixture) resolve(t *testing.T, action string, arguments map[string]interface{}) map[string]interface{} {
	t.Helper()
	ctx := context.Background()
	input := agentrun.ActionProposalValidationInput{Run: f.run, Bound: f.bound(t, action), Arguments: arguments}
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
func (f *agentBuilderDispatchFixture) execute(t *testing.T, action, callID string, arguments map[string]interface{}, approver models.ApprovalPrincipal) (map[string]interface{}, error) {
	t.Helper()
	call := &models.ActionCall{
		ID: callID, Scope: f.scope, RunID: f.run.ID, DeploymentID: "finn", SkillID: models.AgentManagementSkillID,
		SkillVersion: models.AgentManagementSkillVersion, Action: action, Arguments: arguments,
	}
	if action != models.AgentActionListAgents {
		call.ApprovalID = "approval-" + callID
		decider := approver
		f.store.approvals[call.ApprovalID] = &models.ApprovalCheckpoint{
			ID: call.ApprovalID, Scope: f.scope, RunID: f.run.ID, ActionCallID: callID,
			Status: models.ApprovalStatusApproved, DecisionBy: &decider,
		}
	}
	return f.dispatcher.DispatchAction(context.Background(), agentrun.ActionDispatchInput{
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
		if member.Type == models.SpaceMemberAgent && member.ID == deploymentID {
			return true
		}
	}
	return false
}

// newTestAgentBuilderDispatcher builds a dispatcher over its own installer.
func newTestAgentBuilderDispatcher(
	store models.KernelStore, agents *kernelagent.Registry, teams *kernelteam.Registry, catalog *skill.Catalog,
	provision agentbuilder.AgentProvisioner, host agentbuilder.AgentBuilderHost, fallback agentrun.ActionDispatcher,
) (*agentbuilder.AgentBuilderActionDispatcher, error) {
	installer, err := agentbuilder.NewAgentInstaller(store, agents, teams, catalog, provision, host)
	if err != nil {
		return nil, err
	}
	return agentbuilder.NewAgentBuilderActionDispatcher(installer, fallback)
}

func agentTrialArguments() map[string]interface{} {
	return map[string]interface{}{
		"displayName":         "Inbox Triage",
		"purpose":             "Sort the support inbox every morning.",
		"systemPrompt":        "Read new support mail and sort it by urgency.",
		"personality":         "Friendly",
		"operatingPrinciples": []interface{}{"Flag anything legal."},
		"model":               "gpt-6-luna",
		"message":             "  What would you do with an angry refund mail?  ",
	}
}

// putAgentTrialRun stores run in whatever state it is in, bypassing
// transition rules: it creates the Run, or saves it as the next revision of
// the stored one and sets run.Revision to that revision.
func putAgentTrialRun(t *testing.T, store *sqlite.Store, run *models.AgentRun) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	current, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current == nil {
		if err := store.CreateAgentRun(ctx, models.CloneAgentRun(run)); err != nil {
			t.Fatalf("create run %s: %v", run.ID, err)
		}
		return run
	}
	run.Revision = current.Revision + 1
	event := &models.ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.updated",
		Summary: "Put agent trial test run", Actor: models.ActivityActor{Type: "test", ID: "agent-trial"},
		Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now().UTC()}
	if _, err := store.UpdateAgentRunWithEvent(ctx, models.CloneAgentRun(run), current.Revision, event, nil); err != nil {
		t.Fatalf("update run %s: %v", run.ID, err)
	}
	return run
}

func mustAgentRun(t *testing.T, store models.PortfolioStore, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

// startAgentTrial dispatches try_agent for the fixture's builder Run.
func startAgentTrial(t *testing.T, f *agentBuilderDispatchFixture, callID string) (*models.ActionCall, *models.AgentRun) {
	t.Helper()
	arguments := f.resolve(t, models.AgentActionTryAgent, agentTrialArguments())
	output, err := f.execute(t, models.AgentActionTryAgent, callID, arguments, agentBuilderApprover)
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := output["trialRunId"].(string)
	if output["resourceType"] != "agent_trial" || output["status"] != "running" || childID == "" {
		t.Fatalf("try_agent output = %#v", output)
	}
	call := &models.ActionCall{
		ID: callID, Scope: f.scope, RunID: f.run.ID, DeploymentID: "finn", SkillID: models.AgentManagementSkillID,
		SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionTryAgent, Status: models.ActionCallStatusSucceeded,
		Origin: models.ActionOriginLoop, Output: output,
	}
	return call, mustAgentRun(t, f.store, f.scope, childID)
}

// agentTrialFinalizerFixture is a builder Run waiting on its trial child.
type agentTrialFinalizerFixture struct {
	store  *sqlite.Store
	scope  models.Scope
	parent *models.AgentRun
	child  *models.AgentRun
	call   *models.ActionCall
}

func newAgentTrialFinalizerFixture(t *testing.T, parentStatus models.AgentRunStatus) *agentTrialFinalizerFixture {
	t.Helper()
	f := newAgentBuilderDispatchFixture(t)
	call, child := startAgentTrial(t, f, "trial-call")
	parent := mustAgentRun(t, f.store, f.scope, f.run.ID)
	parent.Status = parentStatus
	if parentStatus == models.AgentRunStatusWaitingForEvent {
		deadline := agentbuilder.AgentTrialDeadline(child)
		parent.WakeCondition = &models.WakeCondition{Type: agentrun.AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}
	}
	putAgentTrialRun(t, f.store.Store, parent)
	return &agentTrialFinalizerFixture{store: f.store.Store, scope: f.scope, parent: parent, child: child, call: call}
}

func (f *agentTrialFinalizerFixture) endChild(t *testing.T, status models.AgentRunStatus, output map[string]interface{}, message string) *models.AgentRun {
	t.Helper()
	child := mustAgentRun(t, f.store, f.scope, f.child.ID)
	ended := child.CreatedAt.Add(30 * time.Second)
	child.Status, child.Output, child.Error, child.CompletedAt, child.UpdatedAt = status, output, message, &ended, ended
	putAgentTrialRun(t, f.store, child)
	return child
}
