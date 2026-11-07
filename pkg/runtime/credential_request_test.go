package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestNormalizeCredentialRequestWebsite(t *testing.T) {
	for input, want := range map[string]string{
		"https://www.amazon.in":      "https://www.amazon.in",
		"https://WWW.Amazon.in/":     "https://www.amazon.in",
		"https://shop.example:443":   "https://shop.example",
		"http://localhost:8080":      "http://localhost:8080",
		" https://www.flipkart.com ": "https://www.flipkart.com",
	} {
		if got, err := NormalizeCredentialRequestWebsite(input); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "amazon.in", "ftp://amazon.in", "https://amazon.in/ap/signin", "https://amazon.in?x=1", "https://user:pw@amazon.in"} {
		if _, err := NormalizeCredentialRequestWebsite(input); err == nil {
			t.Errorf("%q accepted", input)
		}
	}
}

func credentialRequestFixture() *CredentialRequest {
	now := time.Now().UTC()
	return &CredentialRequest{ID: "credential-request:call", Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent", ConversationID: "chat",
		TriggerMessageID: "message", RunID: "run", ActionCallID: "call", Kind: CredentialRequestKindWebsiteLogin, Website: "https://www.amazon.in",
		Reason: "Add your amazon.in login", Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
}

func TestCredentialRequestStoresScopeCASAndTerminalState(t *testing.T) {
	ctx := context.Background()
	sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "credentials.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	for name, store := range map[string]CredentialRequestStore{"memory": NewMemoryStore(), "sqlite": sqlite} {
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

func credentialRequestDispatchFixture(t *testing.T) (context.Context, *MemoryStore, *skill.Catalog, *SkillBindingActionDispatcher, Scope) {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "a"}
	catalog := skillActionCatalog(t, ctx, scope, "agent")
	if err := catalog.Bind(ctx, &skill.Binding{ID: "interaction-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent",
		SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, AllowedActions: []string{SkillActionRequestSetup, SkillActionRequestCredential}, MaximumRisk: skill.RiskLevelRead}); err != nil {
		t.Fatal(err)
	}
	// The bundled live browser is never in the installable catalog.
	provider := skill.DiscoveryProviderFunc(func(context.Context, skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{}, nil
	})
	dispatcher, err := NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, catalog, dispatcher, scope
}

func proposeInteraction(t *testing.T, ctx context.Context, store *MemoryStore, catalog *skill.Catalog, scope Scope, action string, arguments map[string]interface{}) *AgentRun {
	t.Helper()
	f, _ := proposeInteractionCall(t, ctx, store, catalog, scope, action, arguments)
	return f.run
}

// proposeInteractionCall proposes a SkillManagement interaction action as a
// loop step of a new chat Run leased by "worker" and returns the Run's
// fixture and its ready call.
func proposeInteractionCall(t *testing.T, ctx context.Context, store *MemoryStore, catalog *skill.Catalog, scope Scope, action string, arguments map[string]interface{}) (*loopProposalFixture, *ActionCall) {
	t.Helper()
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Order a cable on amazon.in", Source: RunSourceChat, Context: map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	f := newLoopProposalFixture(t, store, created, "worker", time.Now().UTC())
	validator, _ := NewSkillBindingActionValidator(catalog, store)
	proposal, err := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator).Propose(ctx, f.request(ProposeActionRequest{
		DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: action, Arguments: arguments, Summary: "Ask the user"}, "interaction"))
	if err != nil || proposal.Approval != nil || proposal.Call.Status != ActionCallStatusReady {
		t.Fatalf("proposal: %#v %v", proposal, err)
	}
	f.assertUntouched()
	return f, proposal.Call
}

// assertRunNotParkedByCall checks inline execution of a card call left the
// Run running under its lease: the loop parks it on the card at step end
// (GovernedToolExecutor.PlanPark), never the action worker.
func assertRunNotParkedByCall(t *testing.T, f *loopProposalFixture) *AgentRun {
	t.Helper()
	current := f.current()
	if current.Status != AgentRunStatusRunning || current.WakeCondition != nil || current.LeaseOwner != f.worker || current.Revision != f.run.Revision {
		t.Fatalf("the action worker parked or changed the Run: %#v", current)
	}
	return current
}

func TestRequestCredentialParksRunUntilTheUserSavesIt(t *testing.T) {
	ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
	f, call := proposeInteractionCall(t, ctx, store, catalog, scope, SkillActionRequestCredential, map[string]interface{}{
		"kind": "website_login", "website": "https://www.amazon.in/", "reason": "Add your amazon.in login so I can finish the order"})
	executed := f.execute(NewActionWorker(store, catalog, nil, dispatcher), call.ID)
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
		"setup for a bundled Skill":  {SkillActionRequestSetup, map[string]interface{}{"kind": "configure", "skillId": "skill-live-browser", "skillVersion": "latest", "reason": "Add your amazon.in login"}},
		"credential with a page URL": {SkillActionRequestCredential, map[string]interface{}{"kind": "website_login", "website": "https://www.amazon.in/ap/signin?x=1", "reason": "Add your amazon.in login"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
			f, call := proposeInteractionCall(t, ctx, store, catalog, scope, tc.action, tc.arguments)
			executed := f.execute(NewActionWorker(store, catalog, nil, dispatcher), call.ID)
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

func TestRepeatedRefusedSetupIsNotRetriedInTheSameRun(t *testing.T) {
	ctx, store, _, dispatcher, scope := credentialRequestDispatchFixture(t)
	run := createClaimedSkillActionRun(t, ctx, store, scope, "agent", "worker")
	run.Context = map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}
	args := map[string]interface{}{"kind": "configure", "skillId": "skill-live-browser", "skillVersion": "latest", "reason": "connect"}
	refused := &ActionCall{ID: "refused", Scope: scope, RunID: run.ID, DeploymentID: "agent", SkillID: SkillManagementSkillID, Action: SkillActionRequestSetup,
		Status: ActionCallStatusSucceeded, Arguments: args, Output: interactionRequestRefusalResult(refuseInteraction("not available"))}
	store.actions[portfolioKey(scope, refused.ID)] = refused
	_, err := dispatcher.requestSkillSetup(ctx, ActionDispatchInput{Call: &ActionCall{ID: "retry"}, Arguments: args}, run, "agent", ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"})
	var refusal *interactionRequestRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("repeated refused setup: %v", err)
	}
}

func mustConversationChanges(t *testing.T, ctx context.Context, store *MemoryStore, scope Scope) (*ConversationChangeSet, error) {
	t.Helper()
	now := time.Now().UTC()
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
	ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
	crew := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	if err := store.CreateTeamDeployment(ctx, &kernelteam.Deployment{ID: crew.ID, Scope: capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}, Status: kernelteam.DeploymentActive,
		Revision: 1, Roster: []kernelteam.RosterAssignment{{ID: "agent", RoleID: "member", AgentDeploymentID: "agent"}}}, workforce.DefinitionActivation{}); err != nil {
		t.Fatal(err)
	}
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: crew, AssignedAgentID: "agent",
		Goal: "Order a cable", Source: RunSourceChat, Context: map[string]interface{}{"conversationId": "team-chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	f := newLoopProposalFixture(t, store, created, "worker", time.Now().UTC())
	run := f.run
	validator, _ := NewSkillBindingActionValidator(catalog, store)
	proposal, err := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator).Propose(ctx, f.request(ProposeActionRequest{
		DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: SkillActionRequestCredential,
		Arguments: map[string]interface{}{"kind": "website_login", "website": "https://www.amazon.in", "reason": "Add the login"}, Summary: "Ask the user"}, "request-credential"))
	if err != nil {
		t.Fatal(err)
	}
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, f.worker)
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
	team.Roster, team.Revision = []kernelteam.RosterAssignment{{ID: "other", RoleID: "member", AgentDeploymentID: "other"}}, 2
	if err := store.UpdateTeamDeployment(ctx, team, 1, workforce.DefinitionActivation{}); err != nil {
		t.Fatal(err)
	}
	if resumable, err := InteractionRunResumable(ctx, store, parked, crew, "agent"); err != nil || resumable {
		t.Fatalf("removed member resumable = %v, %v", resumable, err)
	}
}
