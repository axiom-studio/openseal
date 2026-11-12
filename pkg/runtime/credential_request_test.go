package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func credentialRequestFixture() *CredentialRequest {
	now := time.Now().UTC()
	return &CredentialRequest{ID: "credential-request:call", Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat",
		TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: CredentialRequestKindWebsiteLogin, Website: "https://www.amazon.in",
		Reason: "Add your amazon.in login", Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
}

func TestCredentialRequestStoresScopeCASAndTerminalState(t *testing.T) {
	ctx := context.Background()
	sqlite, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	for name, store := range map[string]CredentialRequestStore{"sqlite": sqlite} {
		t.Run(name, func(t *testing.T) {
			r := credentialRequestFixture()
			if err := store.SaveCredentialRequest(ctx, r, 0); err != nil {
				t.Fatal(err)
			}
			if other, err := store.GetCredentialRequest(ctx, Scope{Kind: "tenant", ID: "b"}, r.ID); err != nil || other != nil {
				t.Fatalf("cross-tenant read: %#v %v", other, err)
			}
			if other, err := store.ListCredentialRequests(ctx, r.Scope, "foreign-agent", r.ConversationID); err != nil || len(other) != 0 {
				t.Fatalf("cross-agent list: %#v %v", other, err)
			}
			if err := store.SaveCredentialRequest(ctx, r, 0); !errors.Is(err, ErrCredentialRequestConflict) {
				t.Fatalf("duplicate request not rejected: %v", err)
			}
			r.Status, r.ResolvedBy, r.Revision, r.UpdatedAt = CredentialRequestStatusResolved, "user:5", 2, time.Now().UTC()
			if err := store.SaveCredentialRequest(ctx, r, 1); err != nil {
				t.Fatal(err)
			}
			r.Status, r.ResolvedBy, r.Revision = CredentialRequestStatusPending, "", 3
			if err := store.SaveCredentialRequest(ctx, r, 2); !errors.Is(err, ErrCredentialRequestConflict) {
				t.Fatalf("settled request reopened: %v", err)
			}
			listed, err := store.ListCredentialRequests(ctx, r.Scope, r.DeploymentID, r.ConversationID)
			if err != nil || len(listed) != 1 || listed[0].Status != CredentialRequestStatusResolved {
				t.Fatalf("listed: %#v %v", listed, err)
			}
		})
	}
}

func proposeInteraction(t *testing.T, ctx context.Context, store *sqlite.Store, catalog *skill.Catalog, scope Scope, action string, arguments map[string]interface{}) *AgentRun {
	t.Helper()
	f, _ := proposeInteractionCall(t, ctx, store, catalog, scope, action, arguments)
	return f.Run
}

// proposeInteractionCall proposes a SkillManagement interaction action as a
// loop step of a new chat Run leased by "worker" and returns the Run's
// fixture and its ready call.
func proposeInteractionCall(t *testing.T, ctx context.Context, store *sqlite.Store, catalog *skill.Catalog, scope Scope, action string, arguments map[string]interface{}) (*actiontest.LoopProposalFixture, *ActionCall) {
	t.Helper()
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Order a cable on amazon.in", Source: RunSourceChat, Context: map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	f := actiontest.NewLoopProposalFixture(t, store, created, "worker", time.Now().UTC())
	validator, _ := skillbinding.NewSkillBindingActionValidator(catalog, store)
	proposal, err := runtimeaction.NewActionCoordinator(store, store, catalog, runtimeaction.NewDefaultActionPolicy(), []runtimeaction.ActionProposalValidator{validator}).Propose(ctx, f.Request(runtimeaction.ProposeActionRequest{
		DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, Action: action, Arguments: arguments, Summary: "Ask the user"}, "interaction"))
	if err != nil || proposal.Approval != nil || proposal.Call.Status != ActionCallStatusReady {
		t.Fatalf("proposal: %#v %v", proposal, err)
	}
	f.AssertUntouched()
	return f, proposal.Call
}

// assertRunNotParkedByCall checks inline execution of a card call left the
// Run running under its lease: the loop parks it on the card at step end
// (GovernedToolExecutor.PlanPark), never the action worker.
func assertRunNotParkedByCall(t *testing.T, f *actiontest.LoopProposalFixture) *AgentRun {
	t.Helper()
	current := f.Current()
	if current.Status != AgentRunStatusRunning || current.WakeCondition != nil || current.LeaseOwner != f.Worker || current.Revision != f.Run.Revision {
		t.Fatalf("the action worker parked or changed the Run: %#v", current)
	}
	return current
}

func TestRequestCredentialParksRunUntilTheUserSavesIt(t *testing.T) {
	ctx, store, catalog, dispatcher, scope := skillbindingtest.CredentialRequestDispatchFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, agentrun.SkillActionRequestCredential, map[string]interface{}{
		"kind": "website_login", "website": "https://www.amazon.in/", "reason": "Add your amazon.in login so I can finish the order"})
	executed := f.Execute(runtimeaction.NewActionWorker(store, catalog, nil, dispatcher), call.ID)
	if executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["credentialRequest"] == nil {
		t.Fatalf("credential action: %#v", executed.Call)
	}
	requests, err := store.ListCredentialRequests(ctx, scope, "agent", "chat")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests: %#v %v", requests, err)
	}
	request := requests[0]
	if request.Kind != CredentialRequestKindWebsiteLogin || request.Website != "https://www.amazon.in" || request.Status != CredentialRequestStatusPending || request.ActionCallID != executed.Call.ID {
		t.Fatalf("request: %#v", request)
	}
	// The call's output names the card the loop parks the Run on.
	card, _ := executed.Call.Output["credentialRequest"].(map[string]interface{})
	if card["id"] != request.ID {
		t.Fatalf("credential call output = %#v", executed.Call.Output)
	}
	assertRunNotParkedByCall(t, f)
	changes, err := mustConversationChanges(t, ctx, store, scope)
	if err != nil || !changes.CredentialRequestsChanged || len(changes.CredentialRequests) != 1 || changes.CredentialRequests[0].ID != request.ID {
		t.Fatalf("change set: %#v %v", changes, err)
	}
}

func TestFailedInteractionRequestsAreExplainedInsteadOfFailingTheRun(t *testing.T) {
	for name, tc := range map[string]struct {
		action    string
		arguments map[string]interface{}
	}{
		// The production failure: a bundled Skill is not in the installable
		// catalog, so this setup request cannot be created.
		"setup for a bundled Skill":  {agentrun.SkillActionRequestSetup, map[string]interface{}{"kind": "configure", "skillId": "skill-live-browser", "skillVersion": "latest", "reason": "Add your amazon.in login"}},
		"credential with a page URL": {agentrun.SkillActionRequestCredential, map[string]interface{}{"kind": "website_login", "website": "https://www.amazon.in/ap/signin?x=1", "reason": "Add your amazon.in login"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, store, catalog, dispatcher, scope := skillbindingtest.CredentialRequestDispatchFixture(t)
			f, call := proposeInteractionCall(t, ctx, store, catalog, scope, tc.action, tc.arguments)
			executed := f.Execute(runtimeaction.NewActionWorker(store, catalog, nil, dispatcher), call.ID)
			refused, _ := executed.Call.Output["refused"].(map[string]interface{})
			if executed.Call.Status != ActionCallStatusSucceeded || refused == nil || refused["message"] == "" {
				t.Fatalf("refusal not returned to the model: %#v", executed.Call)
			}
			// The refusal is the tool result; the Run keeps going so the model
			// can explain.
			if current := assertRunNotParkedByCall(t, f); len(current.Checkpoint) != 0 {
				t.Fatalf("run must continue so the model can explain: %#v", current)
			}
			if requests, _ := store.ListCredentialRequests(ctx, scope, "agent", "chat"); len(requests) != 0 {
				t.Fatalf("refused request persisted: %#v", requests)
			}
		})
	}
}

func mustConversationChanges(t *testing.T, ctx context.Context, store *sqlite.Store, scope Scope) (*ConversationChangeSet, error) {
	t.Helper()
	now := time.Now().UTC()
	seedTestSpace(t, store, scope)
	if _, _, err := store.CreateConversation(ctx, &Conversation{ID: "chat", Scope: scope, Title: "Amazon order", Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Status: ConversationStatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now}, "chat-key"); err != nil {
		return nil, err
	}
	service, err := NewConversationChangeService(store, store)
	if err != nil {
		return nil, err
	}
	return service.ListChanges(ctx, ConversationChangeRequest{Scope: scope, ConversationID: "chat"})
}

// A member asking in a Team chat saves the request to the Team, stamped with
// the member that asked; the member's own umbrella never sees it.
func TestTeamRunRequestsAreSavedToTheTeam(t *testing.T) {
	ctx, store, catalog, dispatcher, scope := skillbindingtest.CredentialRequestDispatchFixture(t)
	crew := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	deploymentScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	// The store records one definition activation per deployment revision.
	activation := func(deploymentID string, revision int64) workforce.DefinitionActivation {
		return workforce.DefinitionActivation{ID: fmt.Sprintf("%s-%d", deploymentID, revision), Scope: deploymentScope, DeploymentID: deploymentID, DeploymentRevision: revision}
	}
	// A Team run acts under the Team only while its member is an active Agent.
	if err := store.CreateDeployment(ctx, &kernelagent.AgentDeployment{ID: "agent", Scope: deploymentScope,
		RolloutStatus: kernelagent.RolloutActive, Revision: 1}, activation("agent", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTeamDeployment(ctx, &kernelteam.Deployment{ID: crew.ID, Scope: deploymentScope, Status: kernelteam.DeploymentActive,
		Revision: 1, Roster: []kernelteam.RosterAssignment{{ID: "agent", AgentDeploymentID: "agent"}}}, activation(crew.ID, 1)); err != nil {
		t.Fatal(err)
	}
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: crew, AssignedAgentID: "agent",
		Goal: "Order a cable", Source: RunSourceChat, Context: map[string]interface{}{"conversationId": "team-chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	f := actiontest.NewLoopProposalFixture(t, store, created, "worker", time.Now().UTC())
	run := f.Run
	validator, _ := skillbinding.NewSkillBindingActionValidator(catalog, store)
	proposal, err := runtimeaction.NewActionCoordinator(store, store, catalog, runtimeaction.NewDefaultActionPolicy(), []runtimeaction.ActionProposalValidator{validator}).Propose(ctx, f.Request(runtimeaction.ProposeActionRequest{
		DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, Action: agentrun.SkillActionRequestCredential,
		Arguments: map[string]interface{}{"kind": "website_login", "website": "https://www.amazon.in", "reason": "Add the login"}, Summary: "Ask the user"}, "request-credential"))
	if err != nil {
		t.Fatal(err)
	}
	executed, err := runtimeaction.NewActionWorker(store, catalog, nil, dispatcher).ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, f.Worker)
	if err != nil || executed.Call.Status != ActionCallStatusSucceeded || executed.Call.Output["credentialRequest"] == nil {
		t.Fatalf("credential action: %#v %v", executed, err)
	}
	requests, err := store.ListCredentialRequests(ctx, scope, crew.ID, "team-chat")
	if err != nil || len(requests) != 1 || requests[0].Owner != crew || requests[0].DeploymentID != crew.ID || requests[0].AgentID != "agent" || requests[0].RunID != run.ID {
		t.Fatalf("Team requests = %#v, %v", requests, err)
	}
	if own, err := store.ListCredentialRequests(ctx, scope, "agent", "team-chat"); err != nil || len(own) != 0 {
		t.Fatalf("member requests = %#v, %v", own, err)
	}
	parked := assertRunNotParkedByCall(t, f)
	// A member removed from the Team can no longer act under it.
	team, _ := store.GetTeamDeployment(ctx, capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, crew.ID)
	team.Roster, team.Revision = []kernelteam.RosterAssignment{{ID: "other", AgentDeploymentID: "other"}}, 2
	if err := store.UpdateTeamDeployment(ctx, team, 1, activation(crew.ID, 2)); err != nil {
		t.Fatal(err)
	}
	if resumable, err := InteractionRunResumable(ctx, store, parked, crew, "agent"); err != nil || resumable {
		t.Fatalf("removed member resumable = %v, %v", resumable, err)
	}
}
