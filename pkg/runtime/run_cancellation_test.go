package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type cancellationStore interface {
	KernelStore
	OutreachStore
	InitiativeStore
	SourceMonitorStore
}

func TestCancelWaitingApprovalClosesActionAndRepairsOutreachProjection(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		store func(*testing.T) cancellationStore
	}{
		{name: "memory", store: func(*testing.T) cancellationStore { return NewMemoryStore() }},
		{name: "sqlite", store: func(t *testing.T) cancellationStore {
			store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "cancel.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			assertCanceledApprovalOutreach(t, fixture.store(t))
		})
	}
}

func assertCanceledApprovalOutreach(t *testing.T, store cancellationStore) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 20, 14, 0, 0, 0, time.UTC)
	catalog, scope := cancellationActionCatalog(t)
	runs := NewRunCommandService(store)
	created, err := runs.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"}, AssignedAgentID: "research-agent",
		Goal: "Send reviewed follow-up", Source: RunSourceRequest,
	})
	if err != nil {
		t.Fatal(err)
	}
	running, _, err := NewRunActivityService(store, store).TransitionRun(ctx, scope, created.Run.ID, RunTransitionRequest{
		ExpectedRevision: created.Run.Revision, Status: AgentRunStatusRunning,
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{
			Disposition: ActionDispositionRequireApproval, Reason: "human review is required", ApprovalTTL: time.Hour,
			EligibleApprovers: []ApprovalPrincipal{{Type: "role", ID: "reviewer"}},
		}, nil
	}))
	coordinator.now = func() time.Time { return now }
	proposal, err := coordinator.Propose(ctx, ProposeActionRequest{
		Scope: scope, RunID: running.ID, DeploymentID: "research-agent", SkillID: "outreach", SkillVersion: "1.0.0", Action: "reply",
		Arguments: map[string]interface{}{"body": "reviewed body"}, IdempotencyKey: "outreach-message-1", Summary: "Send reviewed reply",
	})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval == nil || proposal.Run.Status != AgentRunStatusWaitingForApproval {
		t.Fatalf("proposal = %#v", proposal)
	}

	disclosure := "Disclosure: I am an OpenSeal automated research agent."
	reviewedBody := "Could you share feedback? " + disclosure
	targetURI := "https://forum.example/thread/1"
	thread := &OutreachThread{
		ID: "thread-1", Scope: scope, InitiativeID: "initiative-1", SourceObservationID: "observation-1", MonitorID: "monitor-1",
		StableSourceID: "source-1", TargetURI: targetURI, Owner: proposal.Run.Owner,
		AssignedAgentID: proposal.Run.AssignedAgentID, SourcePolicyRef: "forum-policy", ApprovalPolicyRef: "human-review",
		Identity: OutreachIdentity{ProfileRef: "profile-1", DisplayName: "OpenSeal Research", Affiliation: "OpenSeal", Disclosure: disclosure},
		Status:   OutreachThreadOpen, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Messages: []OutreachMessage{{
			ID: "message-1", Direction: OutreachMessageOutbound, Intent: OutreachIntentRequestFeedback,
			Body: reviewedBody, Status: OutreachMessagePendingApproval,
			Capability: &OutreachCapability{SkillID: "outreach", SkillVersion: "1.0.0", Action: "reply", Arguments: map[string]interface{}{"target": targetURI, "body": reviewedBody}, TargetArgument: "target", BodyArgument: "body"},
			RunID:      proposal.Run.ID, ActionCallID: proposal.Call.ID, ApprovalID: proposal.Approval.ID, CreatedAt: now, UpdatedAt: now,
		}},
	}
	createdEvent := outreachEvent(thread, &thread.Messages[0], "outreach.approval_requested", "Outreach message awaiting approval", ActivityActor{Type: "worker", ID: "test"}, ActivityVisibilityTeam, now)
	if _, err := store.CreateOutreachThreadWithEvent(ctx, thread, createdEvent); err != nil {
		t.Fatal(err)
	}

	result, err := runs.CommandAgentRun(ctx, AgentRunCommandRequest{
		Scope: scope, RunID: proposal.Run.ID, ExpectedRevision: proposal.Run.Revision, Kind: AgentRunCommandCancel,
		Actor: ActivityActor{Type: "user", ID: "operator"}, Summary: "Operator canceled reviewed outreach",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != AgentRunStatusCanceled || result.Event == nil || result.Event.EventType != "run.canceled" {
		t.Fatalf("cancellation = %#v", result)
	}
	call, err := store.GetActionCall(ctx, scope, proposal.Call.ID)
	if err != nil || call.Status != ActionCallStatusCanceled || call.CompletedAt == nil {
		t.Fatalf("call = %#v err=%v", call, err)
	}
	approval, err := store.GetApproval(ctx, scope, proposal.Approval.ID)
	if err != nil || approval.Status != ApprovalStatusCanceled || approval.DecisionBy == nil || approval.DecisionBy.ID != "operator" {
		t.Fatalf("approval = %#v err=%v", approval, err)
	}
	if _, err := NewApprovalCoordinator(store, store, EligibleApprovalAuthorizer{}).Resolve(ctx, ResolveApprovalRequest{
		Scope: scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision, DecisionID: "late-approval", Approve: true,
		Principal: ApprovalPrincipal{Type: "role", ID: "reviewer"},
	}); !errors.Is(err, ErrApprovalResolved) {
		t.Fatalf("late approval error = %v", err)
	}

	outreach := NewOutreachService(store, store, store, store)
	repaired, err := outreach.Get(ctx, scope, thread.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repaired.Messages[0].Status != OutreachMessageCanceled || repaired.Messages[0].Outcome != call.Error || repaired.Revision != 2 {
		t.Fatalf("repaired outreach = %#v", repaired)
	}
	replayed, err := outreach.Get(ctx, scope, thread.ID)
	if err != nil || replayed.Revision != repaired.Revision {
		t.Fatalf("replayed repair = %#v err=%v", replayed, err)
	}
}

func cancellationActionCatalog(t *testing.T) (*skill.Catalog, Scope) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "cancellation"}
	catalog := skill.NewCatalog()
	definition := &skill.Definition{
		ID: "outreach", Version: "1.0.0", Name: "Outreach", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://forum.invalid"},
		Actions: map[string]skill.Action{"reply": {
			Name: "reply", Description: "Post a reviewed reply", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"body": map[string]interface{}{"type": "string"}}, "required": []interface{}{"body"}},
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: skill.IdempotencyRequired,
		}},
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &skill.Binding{
		ID: "outreach-binding", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-agent",
		SkillID: definition.ID, SkillVersion: definition.Version, AllowedActions: []string{"reply"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope
}
