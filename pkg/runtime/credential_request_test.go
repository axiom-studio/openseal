package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
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
	return &CredentialRequest{ID: "credential-request:call", Scope: Scope{Kind: "tenant", ID: "a"}, DeploymentID: "agent", ConversationID: "chat",
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
	created, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Order a cable on amazon.in", Source: RunSourceChat, Context: map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run.ID != created.ID {
		t.Fatal(err)
	}
	validator, _ := NewSkillBindingActionValidator(catalog)
	proposal, err := NewActionCoordinator(store, store, catalog, NewDefaultActionPolicy(), validator).Propose(ctx, ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "worker",
		DeploymentID: "agent", SkillID: SkillManagementSkillID, SkillVersion: SkillManagementSkillVersion, Action: action, Arguments: arguments, Summary: "Ask the user"})
	if err != nil || proposal.Approval != nil || proposal.Call.Status != ActionCallStatusReady {
		t.Fatalf("proposal: %#v %v", proposal, err)
	}
	return run
}

func TestRequestCredentialParksRunUntilTheUserSavesIt(t *testing.T) {
	ctx, store, catalog, dispatcher, scope := credentialRequestDispatchFixture(t)
	run := proposeInteraction(t, ctx, store, catalog, scope, SkillActionRequestCredential, map[string]interface{}{
		"kind": "website_login", "website": "https://www.amazon.in/", "reason": "Add your amazon.in login so I can finish the order"})
	executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
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
	parked, err := store.GetAgentRun(ctx, scope, run.ID)
	if err != nil || !IsExactCredentialRequestWait(parked, request.ID) {
		t.Fatalf("run not parked on the request: %#v %v", parked, err)
	}
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
			run := proposeInteraction(t, ctx, store, catalog, scope, tc.action, tc.arguments)
			executed, err := NewActionWorker(store, catalog, nil, dispatcher).RunOnce(ctx, scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			refused, _ := executed.Call.Output["refused"].(map[string]interface{})
			if executed.Call.Status != ActionCallStatusSucceeded || refused == nil || refused["message"] == "" {
				t.Fatalf("refusal not returned to the model: %#v", executed.Call)
			}
			current, err := store.GetAgentRun(ctx, scope, run.ID)
			if err != nil || current.Status != AgentRunStatusQueued || requiresFinalFailureExplanation(current.Checkpoint) {
				t.Fatalf("run must continue so the model can explain: %#v %v", current, err)
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
	_, err := dispatcher.requestSkillSetup(ctx, ActionDispatchInput{Call: &ActionCall{ID: "retry"}, Arguments: args}, run, "agent")
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
