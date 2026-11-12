// Copies of the agent builder test fixtures, under the names they get in
// agentbuildertest, for runtime's tests once the agent builder tests move
// with their code.

package runtime

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

// DispatchStore serves the approvals the test decides.
type DispatchStore struct {
	*sqlite.Store
	Approvals map[string]*models.ApprovalCheckpoint
}

func (s *DispatchStore) GetApproval(ctx context.Context, scope models.Scope, approvalID string) (*models.ApprovalCheckpoint, error) {
	if approval, ok := s.Approvals[approvalID]; ok {
		return approval, nil
	}
	return s.Store.GetApproval(ctx, scope, approvalID)
}

// Provisioner does what the Engine does for the builder.
type Provisioner struct {
	Agents        *kernelagent.Registry
	Spaces        *portfolio.SpaceService
	Conversations *conversation.ConversationService
	Created       int
	PutErr        error
}

func (p *Provisioner) CreateAgentDeploymentInSpace(ctx context.Context, spaceID string, deployment *kernelagent.AgentDeployment, actorType, actorID, reason string) (*kernelagent.AgentDeployment, *kernelagent.DefinitionActivation, error) {
	scope := models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}
	if _, err := p.Spaces.GetSpace(ctx, scope, spaceID); err != nil {
		return nil, nil, err
	}
	created, activation, err := p.Agents.CreateDeployment(ctx, deployment, actorType, actorID, reason)
	if err != nil {
		return nil, nil, err
	}
	p.Created++
	if _, _, err := p.PutSpaceMember(ctx, portfolio.PutSpaceMemberRequest{Scope: scope, SpaceID: spaceID, Type: models.SpaceMemberAgent, MemberID: created.ID}); err != nil {
		return created, activation, err
	}
	if _, err := p.EnsureAgentControlConversation(ctx, created); err != nil {
		return created, activation, err
	}
	return created, activation, nil
}

func (p *Provisioner) EnsureAgentControlConversation(ctx context.Context, deployment *kernelagent.AgentDeployment) (*models.Conversation, error) {
	conversation, _, err := p.Conversations.CreateConversation(ctx, conversation.CreateConversationRequest{
		Scope: models.Scope{Kind: deployment.Scope.Kind, ID: deployment.Scope.ID}, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: deployment.ID},
		Title: "Agent control", Origin: &models.ConversationReference{Kind: models.ConversationReferenceAgentControl, ID: "agent-" + deployment.ID},
		IdempotencyKey: "agent-control:" + deployment.ID,
	})
	return conversation, err
}

func (p *Provisioner) CreateTeamDeploymentInSpace(context.Context, string, *kernelteam.Deployment, string, string, string) (*kernelteam.Deployment, *workforce.DefinitionActivation, error) {
	return nil, nil, errors.New("not used")
}

func (p *Provisioner) ListSpaces(ctx context.Context, scope models.Scope) ([]*models.Space, error) {
	return p.Spaces.ListSpaces(ctx, scope)
}

func (p *Provisioner) ListSpaceMembers(ctx context.Context, scope models.Scope, spaceID string) ([]*models.SpaceMember, error) {
	return p.Spaces.ListSpaceMembers(ctx, scope, spaceID)
}

func (p *Provisioner) PutSpaceMember(ctx context.Context, request portfolio.PutSpaceMemberRequest) (*models.SpaceMember, bool, error) {
	if p.PutErr != nil {
		return nil, false, p.PutErr
	}
	return p.Spaces.PutSpaceMember(ctx, request)
}

// RecordingHost records what the dispatcher asks the host.
type RecordingHost struct {
	Host
	PrepareErr error
	Prepared   []AgentModelRequest
	Created    []AgentCreated
}

func (h *RecordingHost) PrepareAgentModel(_ context.Context, request AgentModelRequest) error {
	h.Prepared = append(h.Prepared, request)
	return h.PrepareErr
}

func (h *RecordingHost) AfterAgentCreated(_ context.Context, created AgentCreated) {
	h.Created = append(h.Created, created)
}

type DispatchFixture struct {
	*Fixture
	Store      *DispatchStore
	Provision  *Provisioner
	Recorder   *RecordingHost
	Dispatcher *AgentBuilderActionDispatcher
}

func NewDispatchFixture(t *testing.T) *DispatchFixture {
	t.Helper()
	ctx := context.Background()
	base := NewFixture(t, false)
	store := &DispatchStore{Store: conversationtest.NewStore(t), Approvals: map[string]*models.ApprovalCheckpoint{}}
	base.Spaces = portfolio.NewSpaceService(store)
	base.SalesID, base.HomeID = SeedSpaces(t, base.Spaces, base.Scope)
	recorder := &RecordingHost{}
	var err error
	base.Validator, err = NewAgentBuilderActionValidator(base.Agents, base.Teams, base.Catalog, base.Spaces, store, store, recorder)
	if err != nil {
		t.Fatal(err)
	}
	run, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: base.Scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"},
		AssignedAgentID: "finn", ActingUser: Approver.ID, Goal: "Build an Agent", Source: models.RunSourceChat,
	})
	if err != nil {
		t.Fatal(err)
	}
	base.Run = run
	provision := &Provisioner{Agents: base.Agents, Spaces: base.Spaces, Conversations: conversation.NewConversationService(store)}
	dispatcher, err := NewDispatcher(store, base.Agents, base.Teams, base.Catalog, provision, recorder, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &DispatchFixture{Fixture: base, Store: store, Provision: provision, Recorder: recorder, Dispatcher: dispatcher}
}

var Approver = models.ApprovalPrincipal{Type: "user", ID: "person-1"}

// resolve fills the kernel-resolved arguments as the coordinator does at
// proposal time and checks the proposal passes validation.
func (f *DispatchFixture) Resolve(t *testing.T, action string, arguments map[string]interface{}) map[string]interface{} {
	t.Helper()
	ctx := context.Background()
	input := agentrun.ActionProposalValidationInput{Run: f.Run, Bound: f.Bound(t, action), Arguments: arguments}
	resolved, handled, err := f.Validator.ResolveActionProposalArguments(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if handled {
		input.Arguments = resolved
	}
	if _, err := f.Validator.ValidateActionProposal(ctx, input); err != nil {
		t.Fatalf("%s proposal refused: %v", action, err)
	}
	return input.Arguments
}

// execute dispatches callID as approved by approver.
func (f *DispatchFixture) Execute(t *testing.T, action, callID string, arguments map[string]interface{}, approver models.ApprovalPrincipal) (map[string]interface{}, error) {
	t.Helper()
	call := &models.ActionCall{
		ID: callID, Scope: f.Scope, RunID: f.Run.ID, DeploymentID: "finn", SkillID: models.AgentManagementSkillID,
		SkillVersion: models.AgentManagementSkillVersion, Action: action, Arguments: arguments,
	}
	if action != models.AgentActionListAgents {
		call.ApprovalID = "approval-" + callID
		decider := approver
		f.Store.Approvals[call.ApprovalID] = &models.ApprovalCheckpoint{
			ID: call.ApprovalID, Scope: f.Scope, RunID: f.Run.ID, ActionCallID: callID,
			Status: models.ApprovalStatusApproved, DecisionBy: &decider,
		}
	}
	return f.Dispatcher.DispatchAction(context.Background(), agentrun.ActionDispatchInput{
		Call: call, Run: f.Run, Bound: f.Bound(t, action), Arguments: arguments,
	})
}

func (f *DispatchFixture) Reference() capability.ScopeReference {
	return capability.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}
}

func (f *DispatchFixture) SpaceMember(t *testing.T, spaceID, deploymentID string) bool {
	t.Helper()
	members, err := f.Spaces.ListSpaceMembers(context.Background(), f.Scope, spaceID)
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

// NewDispatcher builds a dispatcher over its own installer.
func NewDispatcher(
	store models.KernelStore, agents *kernelagent.Registry, teams *kernelteam.Registry, catalog *skill.Catalog,
	provision AgentProvisioner, host AgentBuilderHost, fallback agentrun.ActionDispatcher,
) (*AgentBuilderActionDispatcher, error) {
	installer, err := NewAgentInstaller(store, agents, teams, catalog, provision, host)
	if err != nil {
		return nil, err
	}
	return NewAgentBuilderActionDispatcher(installer, fallback)
}

type Host struct {
	Validated []string
	// forbidden lists "user/agentId" pairs AuthorizeAgentChange refuses.
	Forbidden  map[string]bool
	Authorized []string
}

func (h *Host) AuthorizeAgentChange(_ context.Context, _ skill.ScopeReference, actingUserID, deploymentID string) error {
	h.Authorized = append(h.Authorized, actingUserID+"/"+deploymentID)
	if h.Forbidden[actingUserID+"/"+deploymentID] {
		return ErrAgentChangeForbidden
	}
	return nil
}

func (h *Host) ValidateAgentModel(_ context.Context, _ skill.ScopeReference, model string) error {
	h.Validated = append(h.Validated, model)
	if model == "unknown-model" {
		return errors.New("model unknown-model is not available")
	}
	return nil
}

func (h *Host) PrepareAgentModel(context.Context, AgentModelRequest) error {
	return nil
}

func (h *Host) AfterAgentCreated(context.Context, AgentCreated) {}

type Fixture struct {
	Scope   models.Scope
	Agents  *kernelagent.Registry
	Teams   *kernelteam.Registry
	Catalog *skill.Catalog
	Spaces  *portfolio.SpaceService
	SalesID string
	// homeID is the default space of the person the fixture's run acts for.
	HomeID    string
	Run       *models.AgentRun
	Host      *Host
	Validator *AgentBuilderActionValidator
}

func NewFixture(t *testing.T, withHost bool) *Fixture {
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
	salesID, homeID := SeedSpaces(t, spaces, scope)
	fixture := &Fixture{
		Scope: scope, Agents: agents, Teams: teams, Catalog: catalog, Spaces: spaces, SalesID: salesID, HomeID: homeID,
		Run: &models.AgentRun{
			ID: "builder-run", Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"},
			AssignedAgentID: "finn", ActingUser: Approver.ID, Source: models.RunSourceChat,
		},
	}
	var host AgentBuilderHost
	if withHost {
		fixture.Host = &Host{}
		host = fixture.Host
	}
	fixture.Validator, err = NewAgentBuilderActionValidator(agents, teams, catalog, spaces, nil, nil, host)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

// SeedSpaces creates Sales, owned by an operator, and Home, the
// default space of the person the builder run acts for.
func SeedSpaces(t *testing.T, spaces *portfolio.SpaceService, scope models.Scope) (salesID, homeID string) {
	t.Helper()
	ctx := context.Background()
	sales, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Sales", Look: models.DefaultSpaceLook, OwnerID: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	home, err := spaces.CreateSpace(ctx, portfolio.CreateSpaceRequest{Scope: scope, Name: "Home", Look: models.DefaultSpaceLook, OwnerID: Approver.ID})
	if err != nil || !home.IsDefault {
		t.Fatalf("home = %+v, err = %v", home, err)
	}
	return sales.ID, home.ID
}

func (f *Fixture) Bound(t *testing.T, action string) *skill.BoundAction {
	t.Helper()
	bound, err := f.Catalog.Resolve(context.Background(), skill.ScopeReference{Kind: f.Scope.Kind, ID: f.Scope.ID}, "finn",
		models.AgentManagementSkillID, models.AgentManagementSkillVersion, action)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

// propose runs the coordinator's order: resolve, schema check, validate.
func (f *Fixture) Propose(t *testing.T, action string, arguments map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	ctx := context.Background()
	bound := f.Bound(t, action)
	input := agentrun.ActionProposalValidationInput{Run: f.Run, Bound: bound, Arguments: arguments}
	resolved, handled, err := f.Validator.ResolveActionProposalArguments(ctx, input)
	if err != nil {
		return nil, err
	}
	if handled {
		input.Arguments = resolved
	}
	if err := f.Catalog.ValidateInput(ctx, bound, input.Arguments); err != nil {
		t.Fatalf("%s arguments failed the action schema: %v (%#v)", action, err, input.Arguments)
	}
	return f.Validator.ValidateActionProposal(ctx, input)
}

func CreateArguments() map[string]interface{} {
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

func AssertJSON(t *testing.T, got map[string]interface{}, want string) {
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

func TrialArguments() map[string]interface{} {
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

// PutTrialRun stores run in whatever state it is in, bypassing
// transition rules: it creates the Run, or saves it as the next revision of
// the stored one and sets run.Revision to that revision.
func PutTrialRun(t *testing.T, store *sqlite.Store, run *models.AgentRun) *models.AgentRun {
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

func MustRun(t *testing.T, store models.PortfolioStore, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil || run == nil {
		t.Fatalf("run %s = %#v, %v", id, run, err)
	}
	return run
}

// StartTrial dispatches try_agent for the fixture's builder Run.
func StartTrial(t *testing.T, f *DispatchFixture, callID string) (*models.ActionCall, *models.AgentRun) {
	t.Helper()
	arguments := f.Resolve(t, models.AgentActionTryAgent, TrialArguments())
	output, err := f.Execute(t, models.AgentActionTryAgent, callID, arguments, Approver)
	if err != nil {
		t.Fatal(err)
	}
	childID, _ := output["trialRunId"].(string)
	if output["resourceType"] != "agent_trial" || output["status"] != "running" || childID == "" {
		t.Fatalf("try_agent output = %#v", output)
	}
	call := &models.ActionCall{
		ID: callID, Scope: f.Scope, RunID: f.Run.ID, DeploymentID: "finn", SkillID: models.AgentManagementSkillID,
		SkillVersion: models.AgentManagementSkillVersion, Action: models.AgentActionTryAgent, Status: models.ActionCallStatusSucceeded,
		Origin: models.ActionOriginLoop, Output: output,
	}
	return call, MustRun(t, f.Store, f.Scope, childID)
}

// TrialFinalizerFixture is a builder Run waiting on its trial child.
type TrialFinalizerFixture struct {
	Store  *sqlite.Store
	Scope  models.Scope
	Parent *models.AgentRun
	Child  *models.AgentRun
	Call   *models.ActionCall
}

func NewTrialFinalizerFixture(t *testing.T, parentStatus models.AgentRunStatus) *TrialFinalizerFixture {
	t.Helper()
	f := NewDispatchFixture(t)
	call, child := StartTrial(t, f, "trial-call")
	parent := MustRun(t, f.Store, f.Scope, f.Run.ID)
	parent.Status = parentStatus
	if parentStatus == models.AgentRunStatusWaitingForEvent {
		deadline := AgentTrialDeadline(child)
		parent.WakeCondition = &models.WakeCondition{Type: agentrun.AgentTrialWakeType, Reference: child.ID, WakeAt: &deadline}
	}
	PutTrialRun(t, f.Store.Store, parent)
	return &TrialFinalizerFixture{Store: f.Store.Store, Scope: f.Scope, Parent: parent, Child: child, Call: call}
}

func (f *TrialFinalizerFixture) EndChild(t *testing.T, status models.AgentRunStatus, output map[string]interface{}, message string) *models.AgentRun {
	t.Helper()
	child := MustRun(t, f.Store, f.Scope, f.Child.ID)
	ended := child.CreatedAt.Add(30 * time.Second)
	child.Status, child.Output, child.Error, child.CompletedAt, child.UpdatedAt = status, output, message, &ended, ended
	PutTrialRun(t, f.Store, child)
	return child
}
