package agentbuildertest

import (
	"context"
	"errors"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
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
	Prepared   []agentbuilder.AgentModelRequest
	Created    []agentbuilder.AgentCreated
}

func (h *RecordingHost) PrepareAgentModel(_ context.Context, request agentbuilder.AgentModelRequest) error {
	h.Prepared = append(h.Prepared, request)
	return h.PrepareErr
}

func (h *RecordingHost) AfterAgentCreated(_ context.Context, created agentbuilder.AgentCreated) {
	h.Created = append(h.Created, created)
}

// DispatchFixture is a Fixture on a stored builder Run, with a dispatcher
// that installs Agents through Provisioner.
type DispatchFixture struct {
	*Fixture
	Store      *DispatchStore
	Provision  *Provisioner
	Recorder   *RecordingHost
	Dispatcher *agentbuilder.AgentBuilderActionDispatcher
}

// NewDispatchFixture builds the DispatchFixture on a new in-memory store.
func NewDispatchFixture(t *testing.T) *DispatchFixture {
	t.Helper()
	ctx := context.Background()
	base := NewFixture(t, false)
	store := &DispatchStore{Store: kerneltest.NewStore(t), Approvals: map[string]*models.ApprovalCheckpoint{}}
	base.Spaces = portfolio.NewSpaceService(store)
	base.SalesID, base.HomeID = SeedSpaces(t, base.Spaces, base.Scope)
	recorder := &RecordingHost{}
	var err error
	base.Validator, err = agentbuilder.NewAgentBuilderActionValidator(base.Agents, base.Teams, base.Catalog, base.Spaces, store, store, recorder)
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

// Approver is the person the builder Run acts for, who approves its cards.
var Approver = models.ApprovalPrincipal{Type: "user", ID: "person-1"}

// Resolve fills the kernel-resolved arguments as the coordinator does at
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

// Execute dispatches callID as approved by approver.
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
	provision agentbuilder.AgentProvisioner, host agentbuilder.AgentBuilderHost, fallback agentrun.ActionDispatcher,
) (*agentbuilder.AgentBuilderActionDispatcher, error) {
	installer, err := agentbuilder.NewAgentInstaller(store, agents, teams, catalog, provision, host)
	if err != nil {
		return nil, err
	}
	return agentbuilder.NewAgentBuilderActionDispatcher(installer, fallback)
}
