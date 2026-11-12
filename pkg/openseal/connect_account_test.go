package openseal

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type availabilityCheck func(context.Context, CredentialAvailabilityRequest) (CredentialAvailability, error)

func (f availabilityCheck) CheckCredentialAvailability(ctx context.Context, req CredentialAvailabilityRequest) (CredentialAvailability, error) {
	return f(ctx, req)
}

// connectAccountEngine has a per_person Mail binding on Agent "agent"; the
// host reports Ana's ("7") Google sign-in only once connected is set.
func connectAccountEngine(t *testing.T) (context.Context, *sqlite.Store, *Engine, *atomic.Bool, *[]CredentialAvailabilityRequest) {
	t.Helper()
	ctx := context.Background()
	store := kerneltest.NewStore(t)
	e, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	definition := &skill.Definition{
		ID: "mail", Version: "1.0.0", Name: "Mail", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://mail.invalid"},
		Actions: map[string]skill.Action{"send": {
			Name: "send", Description: "Send a message", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelWrite,
			InputSchema: map[string]interface{}{"type": "object"},
			Credentials: []skill.CredentialRequirement{{Name: "google", Kind: "oauth2", OAuth2: &skill.OAuth2Requirement{Provider: "google", Subject: "user", Scopes: []string{"mail.send"}}}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencySupported,
		}},
	}
	if err := e.RegisterSkill(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := e.BindSkill(ctx, &skill.Binding{
		ID: "mail-binding", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent", SkillID: "mail", SkillVersion: "1.0.0",
		AllowedActions: []string{"send"}, MaximumRisk: skill.RiskLevelWrite, Revision: 1,
		Credentials: map[string]skill.CredentialReference{"google": {Kind: "oauth2", ID: skill.PersonCredentialReferenceID}},
	}); err != nil {
		t.Fatal(err)
	}
	connected := &atomic.Bool{}
	checks := &[]CredentialAvailabilityRequest{}
	e.SetCredentialAvailabilityChecker(availabilityCheck(func(_ context.Context, req CredentialAvailabilityRequest) (CredentialAvailability, error) {
		*checks = append(*checks, req)
		if connected.Load() && req.ActingUser == "7" {
			return CredentialAvailability{}, nil
		}
		gaps := map[string]CredentialGap{}
		for name := range req.References {
			gaps[name] = CredentialGapPersonSignIn
		}
		return CredentialAvailability{Gaps: gaps}, nil
	}))
	return ctx, store, e, connected, checks
}

// parkedConnectAccountRun is a chat run acting for actingUser that waits on
// its own connect_account card for requestedFor.
func parkedConnectAccountRun(t *testing.T, ctx context.Context, store *sqlite.Store, call, actingUser, requestedFor, mode string) (*models.AgentRun, *SkillSetupRequest) {
	t.Helper()
	scope := Scope{Kind: "tenant", ID: "one"}
	run, err := portfolio.NewService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation,
		Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, AssignedAgentID: "agent", Goal: "Send mail", Source: models.RunSourceChat, ActingUser: actingUser,
		Context: map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message-" + call}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := &SkillSetupRequest{ID: "connect-account:" + call, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, DeploymentID: "agent", AgentID: "agent",
		ConversationID: "chat", TriggerMessageID: "message-" + call, RunID: run.ID, ActionCallID: call, Kind: SkillSetupKindConnectAccount,
		SkillID: "mail", SkillVersion: "1.0.0", SkillName: "Mail", BindingID: "mail-binding", BindingRevision: 1,
		Slots: []ConnectAccountSlot{{Name: "google", Requirement: skill.CredentialRequirement{Name: "google", Kind: "oauth2",
			OAuth2: &skill.OAuth2Requirement{Provider: "google", Subject: "user", Scopes: []string{"mail.send"}}}}},
		Mode: mode, RequestedFor: requestedFor, Reason: "This Agent uses each person's own Google account.",
		Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveSkillSetupRequest(ctx, request, 0); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v %v", claimed, err)
	}
	waiting, _, err := activity.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, activity.RunTransitionRequest{
		ExpectedRevision: claimed.Revision, LeaseOwner: "worker", Status: models.AgentRunStatusWaitingForEvent, EventType: "run.waiting", Summary: "Waiting",
		Actor:         models.ActivityActor{Type: "worker", ID: "worker"},
		WakeCondition: &models.WakeCondition{Type: ConnectAccountWakeType, Reference: request.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return waiting, request
}

func TestConnectAccountResolveAuthority(t *testing.T) {
	ctx, store, e, connected, _ := connectAccountEngine(t)
	scope, owner := Scope{Kind: "tenant", ID: "one"}, agentOwner("agent")
	_, personal := parkedConnectAccountRun(t, ctx, store, "a", "7", "7", "per_person")
	_, other := parkedConnectAccountRun(t, ctx, store, "b", "8", "8", "per_person")
	_, shared := parkedConnectAccountRun(t, ctx, store, "c", "7", "", "shared")
	// Only Ana resolves her card, even a manager may not.
	if _, err := e.ResolveConnectAccountRequest(ctx, scope, owner, personal.ID, 1, InteractionResolver{UserID: "9", CanManage: true}, false); !errors.Is(err, ErrInteractionNotAuthorized) {
		t.Fatalf("manager resolved a per_person card: %v", err)
	}
	if _, err := e.ResolveConnectAccountRequest(ctx, scope, owner, personal.ID, 1, InteractionResolver{UserID: "9"}, true); !errors.Is(err, ErrInteractionNotAuthorized) {
		t.Fatalf("a stranger dismissed a per_person card: %v", err)
	}
	if _, err := e.ResolveConnectAccountRequest(ctx, scope, owner, personal.ID, 1, InteractionResolver{UserID: "7"}, false); !errors.Is(err, ErrConnectAccountNotSatisfied) {
		t.Fatalf("resolved before connecting: %v", err)
	}
	if _, err := e.ResolveConnectAccountRequest(ctx, scope, owner, personal.ID, 1, InteractionResolver{UserID: "7", System: "system:x"}, false); err == nil {
		t.Fatal("a user and a system resolver at once")
	}
	connected.Store(true)
	resolved, err := e.ResolveConnectAccountRequest(ctx, scope, owner, personal.ID, 1, InteractionResolver{UserID: "7"}, false)
	if err != nil || resolved.Status != "resolved" || resolved.ResolvedBy != "7" || resolved.ResolvedBindingID != "mail-binding" {
		t.Fatalf("resolution: %#v %v", resolved, err)
	}

	// A manager may dismiss a per_person card.
	if dismissed, err := e.ResolveConnectAccountRequest(ctx, scope, owner, other.ID, 1, InteractionResolver{UserID: "9", CanManage: true}, true); err != nil || dismissed.Status != "dismissed" {
		t.Fatalf("manager dismissal: %#v %v", dismissed, err)
	}

	// A shared card is answered by managers only.
	for _, dismiss := range []bool{false, true} {
		if _, err := e.ResolveConnectAccountRequest(ctx, scope, owner, shared.ID, 1, InteractionResolver{UserID: "7"}, dismiss); !errors.Is(err, ErrInteractionNotAuthorized) {
			t.Fatalf("non-manager settled a shared card (dismiss=%v): %v", dismiss, err)
		}
	}
	if settled, err := e.ResolveConnectAccountRequest(ctx, scope, owner, shared.ID, 1, InteractionResolver{UserID: "9", CanManage: true}, false); err != nil || settled.Status != "resolved" {
		t.Fatalf("manager resolution of a shared card: %#v %v", settled, err)
	}
}

func TestConnectAppAndManagerCardAuthority(t *testing.T) {
	ctx, store, e, run, request, connected := waitingConnectAppFixture(t)
	request.Mode, request.RequestedFor = "per_person", "7"
	request.Revision++
	if err := store.SaveSkillSetupRequest(ctx, request, 1); err != nil {
		t.Fatal(err)
	}
	connected.Store(true)
	if _, err := e.ResolveConnectAppRequest(ctx, run.Scope, agentOwner("agent"), request.ID, 2, InteractionResolver{UserID: "9", CanManage: true}, false); !errors.Is(err, ErrInteractionNotAuthorized) {
		t.Fatalf("manager connected someone's per_person app: %v", err)
	}
	if _, err := e.ResolveConnectAppRequest(ctx, run.Scope, agentOwner("agent"), request.ID, 2, InteractionResolver{UserID: "9"}, true); !errors.Is(err, ErrInteractionNotAuthorized) {
		t.Fatalf("stranger dismissed a per_person app card: %v", err)
	}

	credentialCtx, _, credentialEngine, credentialRun, credential := waitingCredentialRequestFixture(t)
	if _, err := credentialEngine.ResolveCredentialRequest(credentialCtx, credentialRun.Scope, agentOwner("agent"), credential.ID, 1, InteractionResolver{UserID: "7"}, false); !errors.Is(err, ErrInteractionNotAuthorized) {
		t.Fatalf("non-manager resolved a credential card: %v", err)
	}
	if _, err := credentialEngine.ResolveCredentialRequest(credentialCtx, credentialRun.Scope, agentOwner("agent"), credential.ID, 1, InteractionResolver{UserID: "7"}, true); !errors.Is(err, ErrInteractionNotAuthorized) {
		t.Fatalf("non-manager dismissed a credential card: %v", err)
	}
}

// Settling one card settles the same request of another run in the chat and
// wakes both runs; a run that acts for someone else is not resumed.
func TestConnectAccountSiblingsSettleAndResumeOnlyTheirPerson(t *testing.T) {
	ctx, store, e, connected, _ := connectAccountEngine(t)
	scope, owner := Scope{Kind: "tenant", ID: "one"}, agentOwner("agent")
	first, firstCard := parkedConnectAccountRun(t, ctx, store, "a", "7", "7", "per_person")
	second, secondCard := parkedConnectAccountRun(t, ctx, store, "b", "7", "7", "per_person")
	stranger, strangerCard := parkedConnectAccountRun(t, ctx, store, "c", "8", "7", "per_person")
	connected.Store(true)
	if _, err := e.ResolveConnectAccountRequest(ctx, scope, owner, firstCard.ID, 1, InteractionResolver{UserID: "7"}, false); err != nil {
		t.Fatal(err)
	}
	for _, run := range []*models.AgentRun{first, second} {
		resumed, err := store.GetAgentRun(ctx, scope, run.ID)
		if err != nil || resumed.Status != models.AgentRunStatusQueued || len(resumed.PendingInterventions) != 1 ||
			!strings.Contains(resumed.PendingInterventions[0].Instruction, "connected Google. Retry the step that needed it.") {
			t.Fatalf("run %s not resumed: %#v %v", run.ID, resumed, err)
		}
	}
	sibling, _ := store.GetSkillSetupRequest(ctx, scope, secondCard.ID)
	if sibling.Status != "resolved" || sibling.ResolvedBy != "7" {
		t.Fatalf("sibling not settled: %#v", sibling)
	}
	// The stranger's run acts for someone else: its card (addressed to Ana) is
	// settled with the others, but that run does not continue as Ana.
	settled, _ := store.GetSkillSetupRequest(ctx, scope, strangerCard.ID)
	if settled.Status != "resolved" {
		t.Fatalf("stranger card = %#v", settled)
	}
	waiting, _ := store.GetAgentRun(ctx, scope, stranger.ID)
	if waiting.Status != models.AgentRunStatusWaitingForEvent {
		t.Fatalf("a run acting for someone else resumed on Ana's card: %s", waiting.Status)
	}
}

func TestReconcileConnectAccountRequestsByRequestedFor(t *testing.T) {
	ctx, store, e, connected, checks := connectAccountEngine(t)
	scope := Scope{Kind: "tenant", ID: "one"}
	run, card := parkedConnectAccountRun(t, ctx, store, "a", "7", "7", "per_person")
	connected.Store(true)
	if resolved, err := e.ReconcileConnectAccountRequests(ctx, scope, ConnectAccountReconcileFilter{RequestedFor: "8"}); err != nil || len(resolved) != 0 {
		t.Fatalf("someone else's sign-in resolved Ana's card: %#v %v", resolved, err)
	}
	resolved, err := e.ReconcileConnectAccountRequests(ctx, scope, ConnectAccountReconcileFilter{RequestedFor: "7"})
	if err != nil || len(resolved) != 1 || resolved[0].ID != card.ID || resolved[0].ResolvedBy != connectAccountReconciliationActor {
		t.Fatalf("reconcile: %#v %v", resolved, err)
	}
	last := (*checks)[len(*checks)-1]
	if last.ActingUser != "7" || last.Umbrella != agentOwner("agent") || last.References["google"].ID != skill.PersonCredentialReferenceID {
		t.Fatalf("evidence check = %#v", last)
	}
	resumed, _ := store.GetAgentRun(ctx, scope, run.ID)
	if resumed.Status != models.AgentRunStatusQueued {
		t.Fatalf("run not resumed: %s", resumed.Status)
	}
}

// A binding change of the umbrella reconciles its cards.
func TestBindingChangeReconcilesConnectAccountCards(t *testing.T) {
	ctx, store, e, connected, _ := connectAccountEngine(t)
	_, card := parkedConnectAccountRun(t, ctx, store, "a", "7", "7", "per_person")
	connected.Store(true)
	e.reconcileConnectAccountRequestsForUmbrella(ctx, skill.ScopeReference{Kind: "tenant", ID: "one"}, agentOwner("agent"))
	settled, _ := store.GetSkillSetupRequest(ctx, card.Scope, card.ID)
	if settled.Status != "resolved" {
		t.Fatalf("card = %#v", settled)
	}
}

func TestSetupCardNeedsManager(t *testing.T) {
	if err := authorizeManagerInteraction(InteractionResolver{UserID: "7"}); !errors.Is(err, ErrInteractionNotAuthorized) {
		t.Fatalf("non-manager: %v", err)
	}
	if authorizeManagerInteraction(InteractionResolver{UserID: "7", CanManage: true}) != nil || authorizeManagerInteraction(InteractionResolver{System: "system:x"}) != nil {
		t.Fatal("manager or system refused")
	}
}
