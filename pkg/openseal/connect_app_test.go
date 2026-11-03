package openseal

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/mcp"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// waitingConnectAppFixture is a run parked on a Linear connect card. Linear
// is supplied to that run only once connected is set and only for that run.
func waitingConnectAppFixture(t *testing.T) (context.Context, *runtime.MemoryStore, *Engine, *runtime.AgentRun, *SkillSetupRequest, *atomic.Bool) {
	t.Helper()
	ctx := context.Background()
	store := runtime.NewMemoryStore()
	e, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterSkill(ctx, mcp.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "one"}
	run, err := runtime.NewPortfolioService(store).CreateAgentRun(ctx, runtime.CreateAgentRunRequest{Scope: scope, Kind: runtime.RunKindConversation,
		Owner: ObjectiveOwner{Type: "agent", ID: "agent"}, AssignedAgentID: "agent", Goal: "List my Linear issues", Source: runtime.RunSourceChat,
		Context: map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	connected := &atomic.Bool{}
	e.SetHostSkillBindingProvider(func(_ context.Context, skillScope skill.ScopeReference, deploymentID string, actor skill.RunActor) (*skill.HostBindingSet, error) {
		if !connected.Load() || actor.RunID != run.ID {
			return nil, nil
		}
		return &skill.HostBindingSet{Fingerprint: "linear@3", Services: map[string][]string{"mcp:c1": {"linear", "Linear"}}, Bindings: []*skill.Binding{{
			ID: "mcp:c1", Revision: 3, Scope: skillScope, DeploymentID: deploymentID, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion,
			AllowedActions: []string{mcp.Read}, MaximumRisk: skill.RiskLevelRead, Slug: "linear",
			Config: map[string]interface{}{"endpoint": "https://mcp.linear.app/mcp", "transport": "streamable-http", "auth": map[string]interface{}{"kind": "none"}},
			Tools: []skill.BindingTool{{Name: "list_issues", Description: "List issues", Effect: skill.BindingToolEffectRead,
				InputSchema: map[string]interface{}{"type": "object"}, Arguments: map[string]interface{}{"tool": "list_issues", "hash": strings.Repeat("0", 64)}}},
		}}}, nil
	})
	now := time.Now().UTC()
	request := &SkillSetupRequest{ID: "connect-app:call", Scope: scope, DeploymentID: "agent", ConversationID: "chat", TriggerMessageID: "message",
		RunID: run.ID, ActionCallID: "call", Kind: SkillSetupKindConnectApp, SkillID: mcp.SkillID, SkillVersion: mcp.SkillVersion, SkillName: "Linear",
		App: &ConnectAppTarget{CatalogSlug: "linear", Name: "Linear", Official: true, SignIn: AppSignInOAuth}, Reason: "Connect Linear",
		Status: "pending", Revision: 1, CreatedAt: now, UpdatedAt: now}
	if err := store.SaveSkillSetupRequest(ctx, request, 0); err != nil {
		t.Fatal(err)
	}
	run, err = store.ClaimNextAgentRun(ctx, runtime.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run == nil {
		t.Fatal(err)
	}
	waiting, _, err := runtime.NewRunActivityService(store, store).TransitionRun(ctx, scope, run.ID, runtime.RunTransitionRequest{
		ExpectedRevision: run.Revision, LeaseOwner: "worker", Status: runtime.AgentRunStatusWaitingForEvent, EventType: "run.waiting", Summary: "Waiting",
		Actor:         runtime.ActivityActor{Type: "worker", ID: "worker"},
		WakeCondition: &runtime.WakeCondition{Type: ConnectAppWakeType, Reference: request.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return ctx, store, e, waiting, request, connected
}

func TestConnectedAppResolvesTheCardAndResumesTheRunWithItsTools(t *testing.T) {
	ctx, store, e, run, request, connected := waitingConnectAppFixture(t)
	if _, err := e.ResolveConnectAppRequest(ctx, run.Scope, "agent", request.ID, 1, "5", false); !errors.Is(err, ErrConnectAppNotConnected) {
		t.Fatalf("resolved before connecting: %v", err)
	}
	if resolved, err := e.ReconcileConnectAppRequests(ctx, run.Scope); err != nil || len(resolved) != 0 {
		t.Fatalf("reconciled before connecting: %#v %v", resolved, err)
	}
	connected.Store(true)
	if _, err := e.ResolveConnectAppRequest(ctx, Scope{Kind: "tenant", ID: "other"}, "agent", request.ID, 1, "5", false); err == nil {
		t.Fatal("cross-tenant resolution")
	}
	resolved, err := e.ReconcileConnectAppRequests(ctx, run.Scope)
	if err != nil || len(resolved) != 1 || !resolved[0].Continued {
		t.Fatalf("reconciliation: %#v %v", resolved, err)
	}
	settled := resolved[0].Request
	if settled.Status != "resolved" || settled.ResolvedBindingID != "mcp:c1" || settled.ResolvedBindingRevision != 3 || settled.Revision != 2 {
		t.Fatalf("request: %#v", settled)
	}
	resumed, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || resumed.Status != runtime.AgentRunStatusQueued || resumed.WakeCondition != nil || len(resumed.PendingInterventions) != 1 {
		t.Fatalf("run not resumed: %#v %v", resumed, err)
	}
	if instruction := resumed.PendingInterventions[0].Instruction; !strings.Contains(instruction, "mcp.linear.list_issues") {
		t.Fatalf("instruction: %q", instruction)
	}
	// The card's own confirmation after reconciliation is idempotent.
	again, err := e.ResolveSkillSetupRequest(ctx, run.Scope, "agent", request.ID, 1, "", "5", false)
	if err != nil || again.Revision != 2 {
		t.Fatalf("repeat: %#v %v", again, err)
	}
	if latest, _ := store.GetAgentRun(ctx, run.Scope, run.ID); len(latest.PendingInterventions) != 1 {
		t.Fatalf("woken twice: %#v", latest.PendingInterventions)
	}
}

func TestDismissedConnectCardResumesTheRunWithAnExplanation(t *testing.T) {
	ctx, store, e, run, request, _ := waitingConnectAppFixture(t)
	// Not now goes through the ordinary setup request dismissal.
	dismissed, err := e.ResolveSkillSetupRequest(ctx, run.Scope, "agent", request.ID, 1, "", "5", true)
	if err != nil || dismissed.Status != "dismissed" {
		t.Fatalf("dismissal: %#v %v", dismissed, err)
	}
	resumed, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || resumed.Status != runtime.AgentRunStatusQueued || len(resumed.PendingInterventions) != 1 ||
		!strings.Contains(resumed.PendingInterventions[0].Instruction, "chose not to connect Linear") {
		t.Fatalf("run: %#v %v", resumed, err)
	}
}

func TestConnectCardForAnEndedRunAsksTheHostToReply(t *testing.T) {
	ctx, store, e, run, request, connected := waitingConnectAppFixture(t)
	if _, _, err := runtime.NewRunActivityService(store, store).TransitionRun(ctx, run.Scope, run.ID, runtime.RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: runtime.AgentRunStatusCanceled, EventType: "run.canceled", Summary: "Canceled",
		Actor: runtime.ActivityActor{Type: "user", ID: "5"},
	}); err != nil {
		t.Fatal(err)
	}
	connected.Store(true)
	result, err := e.ResolveConnectAppRequest(ctx, run.Scope, "agent", request.ID, 1, "5", false)
	if err != nil || result.Continued || result.Request.Status != "resolved" {
		t.Fatalf("resolution: %#v %v", result, err)
	}
}
