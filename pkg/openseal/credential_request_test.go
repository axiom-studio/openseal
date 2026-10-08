package openseal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func waitingCredentialRequestFixture(t *testing.T) (context.Context, *runtime.MemoryStore, *Engine, *runtime.AgentRun, *CredentialRequest) {
	t.Helper()
	ctx := context.Background()
	store := runtime.NewMemoryStore()
	e, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(ctx, runtime.CreateAgentRunRequest{Scope: scope, Kind: runtime.RunKindConversation,
		Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, AssignedAgentID: "agent", Goal: "Order a cable", Source: runtime.RunSourceChat,
		Context: map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := &CredentialRequest{ID: "credential-request:call", Scope: scope, DeploymentID: "agent", ConversationID: "chat", TriggerMessageID: "message",
		RunID: run.ID, ActionCallID: "call", Kind: CredentialRequestKindWebsiteLogin, Website: "https://www.amazon.in", Reason: "Add your amazon.in login",
		Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveCredentialRequest(ctx, request, 0); err != nil {
		t.Fatal(err)
	}
	run, err = store.ClaimNextAgentRun(ctx, runtime.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run == nil {
		t.Fatal(err)
	}
	waiting, _, err := runtime.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, runtime.RunTransitionRequest{
		ExpectedRevision: run.Revision, LeaseOwner: "worker", Status: runtime.AgentRunStatusWaitingForEvent, EventType: "run.waiting", Summary: "Waiting",
		Actor:         runtime.ActivityActor{Type: "worker", ID: "worker"},
		WakeCondition: &runtime.WakeCondition{Type: CredentialRequestWakeType, Reference: request.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, e, waiting, request
}

func TestSavedCredentialResumesTheWaitingRunWithGuidance(t *testing.T) {
	ctx, store, e, run, request := waitingCredentialRequestFixture(t)
	if _, err := e.ResolveCredentialRequest(ctx, Scope{Kind: "tenant", ID: "other"}, "agent", request.ID, 1, "user:5", false); err == nil {
		t.Fatal("cross-tenant resolution")
	}
	if _, err := e.ResolveCredentialRequest(ctx, run.Scope, "other-agent", request.ID, 1, "user:5", false); err == nil {
		t.Fatal("cross-agent resolution")
	}
	if _, err := e.ResolveCredentialRequest(ctx, run.Scope, "agent", request.ID, 7, "user:5", false); !errors.Is(err, ErrCredentialRequestConflict) {
		t.Fatalf("stale revision: %v", err)
	}
	result, err := e.ResolveCredentialRequest(ctx, run.Scope, "agent", request.ID, 1, "user:5", false)
	if err != nil || !result.Continued || result.Request.Status != CredentialRequestStatusResolved || result.Request.Revision != 2 {
		t.Fatalf("resolution: %#v %v", result, err)
	}
	resumed, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || resumed.Status != runtime.AgentRunStatusQueued || resumed.WakeCondition != nil || len(resumed.PendingInterventions) != 1 {
		t.Fatalf("run not resumed: %#v %v", resumed, err)
	}
	if instruction := resumed.PendingInterventions[0].Instruction; instruction == "" || !contains(instruction, "live-browser-sign-in") {
		t.Fatalf("instruction: %q", instruction)
	}
	// A repeated confirmation from the card is idempotent and does not wake twice.
	again, err := e.ResolveCredentialRequest(ctx, run.Scope, "agent", request.ID, 1, "user:5", false)
	if err != nil || again.Request.Revision != 2 {
		t.Fatalf("repeat: %#v %v", again, err)
	}
	if latest, _ := store.GetAgentRun(ctx, run.Scope, run.ID); len(latest.PendingInterventions) != 1 {
		t.Fatalf("woken twice: %#v", latest.PendingInterventions)
	}
	if _, err := e.ResolveCredentialRequest(ctx, run.Scope, "agent", request.ID, 2, "user:5", true); !errors.Is(err, ErrCredentialRequestConflict) {
		t.Fatalf("resolved request dismissed: %v", err)
	}
}

func TestCredentialRequestForAnEndedRunAsksTheHostToReply(t *testing.T) {
	ctx, store, e, run, request := waitingCredentialRequestFixture(t)
	if _, _, err := runtime.NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, runtime.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: runtime.AgentRunStatusCanceled, EventType: "run.canceled", Summary: "Canceled",
		Actor: runtime.ActivityActor{Type: "user", ID: "5"},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := e.ResolveCredentialRequest(ctx, run.Scope, "agent", request.ID, 1, "user:5", true)
	if err != nil || result.Continued || result.Request.Status != CredentialRequestStatusDismissed {
		t.Fatalf("dismissal: %#v %v", result, err)
	}
}

func contains(value, part string) bool {
	for i := 0; i+len(part) <= len(value); i++ {
		if value[i:i+len(part)] == part {
			return true
		}
	}
	return false
}
