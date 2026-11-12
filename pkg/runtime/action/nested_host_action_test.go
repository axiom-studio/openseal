package action_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// newNestedHostFixture opens an in-flight step offering a credentialed
// repository read and branch write, as a git host borrows them.
func newNestedHostFixture(t *testing.T, store models.KernelStore) *nestedActionFixture {
	t.Helper()
	repository := map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"owner", "repository"},
		"properties": map[string]interface{}{"owner": map[string]interface{}{"type": "string"}, "repository": map[string]interface{}{"type": "string"}}}
	branch := map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"owner", "repository", "branch", "sha"},
		"properties": map[string]interface{}{"owner": map[string]interface{}{"type": "string"}, "repository": map[string]interface{}{"type": "string"},
			"branch": map[string]interface{}{"type": "string"}, "sha": map[string]interface{}{"type": "string"}}}
	token := []skill.CredentialRequirement{{Name: "token", Kind: "github_token"}}
	definition := &skill.Definition{
		ID: "skill-github", Version: "1.0.0", Name: "GitHub", Transport: skill.TransportReference{Kind: "tool", Endpoint: "skill-github"},
		Actions: map[string]skill.Action{
			"repository-get": {Name: "repository-get", Description: "Read a repository", SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead, InputSchema: repository, Idempotency: skill.IdempotencySupported, Credentials: token},
			"branch-create":  {Name: "branch-create", Description: "Create a branch", SideEffect: skill.SideEffectExternal, Risk: skill.RiskLevelExternal, InputSchema: branch, Idempotency: skill.IdempotencyRequired, Credentials: token, Retry: skill.ActionRetryPolicy{MaxAttempts: 1}},
			"issue-delete":   {Name: "issue-delete", Description: "Delete an issue", SideEffect: skill.SideEffectDestructive, Risk: skill.RiskLevelDestructive, InputSchema: repository, Idempotency: skill.IdempotencySupported, Credentials: token},
		},
	}
	binding := &skill.Binding{
		ID: "github-binding", DeploymentID: "agent", SkillID: "skill-github", SkillVersion: "1.0.0", AllowedActions: []string{"repository-get", "branch-create", "issue-delete"},
		MaximumRisk: skill.RiskLevelDestructive, Revision: 1, Credentials: map[string]skill.CredentialReference{"token": {Kind: "github_token", ID: "credential://github"}},
	}
	return newNestedActionFixtureWith(t, store, definition, binding, func(action capability.ModelAction) bool { return action.Action != "issue-delete" })
}

func (f *nestedActionFixture) hostRead(key string) action.NestedHostActionRequest {
	return action.NestedHostActionRequest{
		Scope: f.scope, RunID: f.run.ID, TurnID: f.turn.ID, SkillID: "skill-github", Action: "repository-get",
		Arguments: map[string]interface{}{"owner": "acme", "repository": "widgets"}, OriginRef: "command-1", IdempotencyKey: key, Summary: "git fetch acme/widgets",
	}
}

func (f *nestedActionFixture) hostPush(key string) action.NestedHostActionRequest {
	return action.NestedHostActionRequest{
		Scope: f.scope, RunID: f.run.ID, TurnID: f.turn.ID, SkillID: "skill-github", Action: "branch-create",
		Arguments: map[string]interface{}{"owner": "acme", "repository": "widgets", "branch": "fix/x", "sha": strings.Repeat("a", 40)},
		OriginRef: "command-1", IdempotencyKey: key, Summary: "git push acme/widgets fix/x",
	}
}

// hostIssuer signs a credential lease for each acquisition, as a host's
// issuer does, and counts them.
func (f *nestedActionFixture) hostIssuer(t *testing.T, issued *atomic.Int64) action.CredentialLeaseIssuer {
	return action.CredentialLeaseIssuerFunc(func(ctx context.Context, request action.CredentialLeaseIssueRequest) (*models.SignedActionCredentialLease, error) {
		if request.Transport != "skill-github" || request.References["token"].ID != "credential://github" || request.Call.Execution != models.ActionExecutionHost {
			t.Fatalf("host lease request = %#v", request)
		}
		count := issued.Add(1)
		issuedAt := f.now.Add(time.Second)
		lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
			TenantID: request.Call.Scope.ID, Call: request.Call, Run: request.Run, CredentialFields: map[string][]string{"token": {"access_token"}}, Transport: request.Transport,
			Issuer: "host", Audience: "host", IssuedAt: issuedAt, ExpiresAt: issuedAt.Add(time.Minute), Nonce: fmt.Sprintf("host-lease-nonce-%04d", count),
		})
		if err != nil {
			return nil, err
		}
		return action.SignActionCredentialLease(ctx, *lease, actiontest.CredentialLeaseSigner{Key: []byte("host-lease-key")})
	})
}

// refusingWorker fails the test if it dispatches anything.
func (f *nestedActionFixture) refusingWorker(t *testing.T, at time.Time) *action.Worker {
	worker := action.NewWorker(f.store, f.catalog, nil, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		t.Fatal("an action worker dispatched a host call")
		return nil, nil
	}), models.WithClock(func() time.Time { return at }))
	return worker
}

func TestNestedHostReadIsClaimedLeasedAndFinishedByItsHostOnly(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedHostFixture(t, testCase.Open(t))
			proposed, err := f.coordinator.ProposeNestedHost(ctx, f.hostRead("git-1"))
			if err != nil {
				t.Fatal(err)
			}
			call := proposed.Call
			if call.Status != models.ActionCallStatusReady || call.Execution != models.ActionExecutionHost || call.Origin != models.ActionOriginVM || call.Action != "repository-get" ||
				call.BindingID != "github-binding" || call.CredentialRefs["token"].ID != "credential://github" || !proposed.Settled() {
				t.Fatalf("host proposal = %#v", proposed)
			}
			f.assertRunUnchanged(t)

			// No action worker takes a ready host call from its host.
			if executed, err := f.refusingWorker(t, f.now.Add(2*time.Second)).RunOnce(ctx, f.scope, "action-worker", time.Minute); err != nil || executed != nil {
				t.Fatalf("worker claimed a host call: %#v, %v", executed, err)
			}

			var issued atomic.Int64
			grant, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, call.ID, f.hostIssuer(t, &issued))
			if err != nil {
				t.Fatal(err)
			}
			if grant.Call.Status != models.ActionCallStatusRunning || grant.Call.LeaseOwner != "agent-worker" || grant.Lease == nil ||
				grant.Lease.Lease.ActionCallID != call.ID || grant.Lease.Lease.Credentials[0].Reference.ID != "credential://github" {
				t.Fatalf("host grant = %#v", grant)
			}
			again, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, call.ID, f.hostIssuer(t, &issued))
			if err != nil {
				t.Fatal(err)
			}
			if again.Lease.Lease.Nonce == grant.Lease.Lease.Nonce || again.Call.Revision <= grant.Call.Revision || issued.Load() != 2 {
				t.Fatalf("renewed grant = %#v (first %#v)", again, grant)
			}

			finished, err := f.coordinator.FinishNestedHostAction(ctx, action.NestedHostActionOutcome{
				Scope: f.scope, RunID: f.run.ID, TurnID: f.turn.ID, ActionCallID: call.ID, Performed: true,
				Output: map[string]interface{}{"repository": "acme/widgets", "bytes": 1234}, Summary: "git clone acme/widgets",
			})
			if err != nil {
				t.Fatal(err)
			}
			if finished.Status != models.ActionCallStatusSucceeded || finished.Output["repository"] != "acme/widgets" || finished.LeaseOwner != "" {
				t.Fatalf("finished host call = %#v", finished)
			}
			f.assertRunUnchanged(t)
			if _, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, call.ID, f.hostIssuer(t, &issued)); !errors.Is(err, action.ErrNestedHostActionClosed) {
				t.Fatalf("acquire after finish err = %v", err)
			}
			events, err := f.store.ListActivity(ctx, models.ActivityFilter{Scope: f.scope, RunID: f.run.ID})
			if err != nil {
				t.Fatal(err)
			}
			summaries := map[string]string{}
			for _, event := range events {
				if event.Payload["actionCallId"] == call.ID {
					if event.Payload["origin"] != "vm" || event.Payload["originRef"] != "command-1" || event.Payload["execution"] != "host" {
						t.Fatalf("host activity %s is not groupable: %#v", event.EventType, event.Payload)
					}
					summaries[event.EventType] = event.Summary
				}
			}
			if summaries["action.proposed"] != "git fetch acme/widgets" || summaries["action.succeeded"] != "git clone acme/widgets" {
				t.Fatalf("host activity = %#v", summaries)
			}
		})
	}
}

func TestNestedHostCallsActOnlyAsOfferedActionsOfTheTurn(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedHostFixture(t, testCase.Open(t))
			for _, refused := range []action.NestedHostActionRequest{
				{SkillID: "skill-github", Action: "issue-delete"},
				{SkillID: "skill-gitlab", Action: "repository-get"},
			} {
				request := f.hostRead("refused-" + refused.SkillID + refused.Action)
				request.SkillID, request.Action = refused.SkillID, refused.Action
				if _, err := f.coordinator.ProposeNestedHost(ctx, request); !errors.Is(err, action.ErrNestedActionNotAuthorized) {
					t.Fatalf("%s %s err = %v, want not authorized", refused.SkillID, refused.Action, err)
				}
			}
			invalid := f.hostRead("invalid")
			invalid.Arguments = map[string]interface{}{"owner": "acme"}
			if _, err := f.coordinator.ProposeNestedHost(ctx, invalid); err == nil {
				t.Fatal("host call arguments are validated as the action's input")
			}
			otherTurn := f.hostRead("other-turn")
			otherTurn.TurnID = "turn-unknown"
			if _, err := f.coordinator.ProposeNestedHost(ctx, otherTurn); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("unknown turn err = %v", err)
			}

			proposed, err := f.coordinator.ProposeNestedHost(ctx, f.hostRead("git-1"))
			if err != nil {
				t.Fatal(err)
			}
			var issued atomic.Int64
			if _, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, "turn-unknown", proposed.Call.ID, f.hostIssuer(t, &issued)); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("acquire from another turn err = %v", err)
			}
			name := ""
			for _, action := range f.binding.ModelActions {
				if action.Action == "repository-get" {
					name = action.Name
				}
			}
			modelRequest := f.request(name, "model-like")
			modelRequest.Arguments = map[string]interface{}{"owner": "acme", "repository": "widgets"}
			model, err := f.coordinator.ProposeNested(ctx, modelRequest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, model.Call.ID, f.hostIssuer(t, &issued)); !errors.Is(err, models.ErrActionNotFound) {
				t.Fatalf("acquiring a worker-dispatched call err = %v", err)
			}
			if issued.Load() != 0 {
				t.Fatalf("leases issued for refused acquisitions: %d", issued.Load())
			}
		})
	}
}

func TestNestedHostPushWaitsForApprovalAndHonorsTheDecision(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedHostFixture(t, testCase.Open(t))
			var issued atomic.Int64
			pushed, err := f.coordinator.ProposeNestedHost(ctx, f.hostPush("push-1"))
			if err != nil {
				t.Fatal(err)
			}
			if pushed.Call.Status != models.ActionCallStatusWaitingApproval || pushed.Approval == nil || pushed.Approval.Summary != "git push acme/widgets fix/x" {
				t.Fatalf("host push = %#v", pushed)
			}
			if _, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, pushed.Call.ID, f.hostIssuer(t, &issued)); !errors.Is(err, action.ErrNestedHostActionNotReady) {
				t.Fatalf("acquire before approval err = %v", err)
			}
			approvals := action.NewApprovalCoordinator(f.store, f.store, action.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return f.now.Add(2 * time.Second) }))
			if _, err := approvals.Resolve(ctx, action.ResolveApprovalRequest{
				Scope: f.scope, ApprovalID: pushed.Approval.ID, ExpectedRevision: pushed.Approval.Revision, DecisionID: "decision-1",
				Decision: models.ApprovalDecisionApprove, Principal: models.ApprovalPrincipal{Type: "user", ID: "user"},
			}); err != nil {
				t.Fatal(err)
			}
			if executed, err := f.refusingWorker(t, f.now.Add(3*time.Second)).RunOnce(ctx, f.scope, "action-worker", time.Minute); err != nil || executed != nil {
				t.Fatalf("worker claimed an approved host call: %#v, %v", executed, err)
			}
			f.clock = func() time.Time { return f.now.Add(3 * time.Second) }
			grant, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, pushed.Call.ID, f.hostIssuer(t, &issued))
			if err != nil {
				t.Fatal(err)
			}
			if grant.Call.Arguments["branch"] != "fix/x" || grant.Call.Arguments["sha"] != strings.Repeat("a", 40) {
				t.Fatalf("approved push arguments = %#v", grant.Call.Arguments)
			}
			failed, err := f.coordinator.FinishNestedHostAction(ctx, action.NestedHostActionOutcome{
				Scope: f.scope, RunID: f.run.ID, TurnID: f.turn.ID, ActionCallID: pushed.Call.ID, Performed: true, Error: "GitHub rejected the push",
			})
			if err != nil || failed.Status != models.ActionCallStatusFailed || failed.FailurePhase != models.ActionFailureAfterDispatch || failed.Error != "GitHub rejected the push" {
				t.Fatalf("failed push = %#v, %v", failed, err)
			}

			rejected, err := f.coordinator.ProposeNestedHost(ctx, f.hostPush("push-2"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := approvals.Resolve(ctx, action.ResolveApprovalRequest{
				Scope: f.scope, ApprovalID: rejected.Approval.ID, ExpectedRevision: rejected.Approval.Revision, DecisionID: "decision-2",
				Decision: models.ApprovalDecisionReject, Principal: models.ApprovalPrincipal{Type: "user", ID: "user"}, Reason: "not now",
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, rejected.Call.ID, f.hostIssuer(t, &issued)); !errors.Is(err, action.ErrNestedHostActionClosed) {
				t.Fatalf("acquire after rejection err = %v", err)
			}
			if issued.Load() != 1 {
				t.Fatalf("leases issued = %d, want only the approved push's", issued.Load())
			}
			f.assertRunUnchanged(t)
		})
	}
}

func TestNestedHostCallsTheirHostAbandonedAreClosedWithoutDispatch(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedHostFixture(t, testCase.Open(t))
			var issued atomic.Int64

			// A ready call its host never claimed waits out the grace, then
			// a worker closes it.
			ready, err := f.coordinator.ProposeNestedHost(ctx, f.hostRead("never-claimed"))
			if err != nil {
				t.Fatal(err)
			}
			recovered, err := f.refusingWorker(t, f.now.Add(models.NestedHostActionReadyGrace+2*time.Second)).RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if recovered == nil || recovered.Call.ID != ready.Call.ID || recovered.Call.Status != models.ActionCallStatusFailed || recovered.Call.FailurePhase != models.ActionFailureBeforeDispatch {
				t.Fatalf("recovered ready host call = %#v", recovered)
			}

			// A running call whose host stopped renewing it may have reached
			// GitHub: it fails after dispatch and is never sent again.
			running, err := f.coordinator.ProposeNestedHost(ctx, f.hostRead("host-stopped"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, running.Call.ID, f.hostIssuer(t, &issued)); err != nil {
				t.Fatal(err)
			}
			stopped, err := f.refusingWorker(t, f.now.Add(action.NestedHostActionLease+2*time.Second)).RunOnce(ctx, f.scope, "action-worker", time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if stopped == nil || stopped.Call.ID != running.Call.ID || stopped.Call.Status != models.ActionCallStatusFailed || stopped.Call.FailurePhase != models.ActionFailureAfterDispatch {
				t.Fatalf("recovered running host call = %#v", stopped)
			}
			f.assertRunUnchanged(t)
		})
	}
}

func TestNestedHostCallEndsWithItsTurn(t *testing.T) {
	for _, testCase := range actiontest.Stores(t) {
		t.Run(testCase.Name, func(t *testing.T) {
			ctx := context.Background()
			f := newNestedHostFixture(t, testCase.Open(t))
			var issued atomic.Int64
			proposed, err := f.coordinator.ProposeNestedHost(ctx, f.hostRead("git-1"))
			if err != nil {
				t.Fatal(err)
			}
			finished := models.CloneAgentTurn(f.turn)
			finished.Status = models.AgentTurnStatusCompleted
			completedAt := f.now.Add(time.Second)
			finished.CompletedAt = &completedAt
			finished.Revision++
			if err := f.store.UpdateAgentTurn(ctx, finished, f.turn.Revision, "agent-worker"); err != nil {
				t.Fatal(err)
			}
			if _, err := f.coordinator.AcquireNestedHostAction(ctx, f.scope, f.run.ID, f.turn.ID, proposed.Call.ID, f.hostIssuer(t, &issued)); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("acquire after the turn err = %v", err)
			}
			call, err := f.store.GetActionCall(ctx, f.scope, proposed.Call.ID)
			if err != nil || call.Status != models.ActionCallStatusCanceled || issued.Load() != 0 {
				t.Fatalf("host call of an ended turn = %#v, %v (leases %d)", call, err, issued.Load())
			}
			f.closeTurn()
			if _, err := f.coordinator.FinishNestedHostAction(ctx, action.NestedHostActionOutcome{Scope: f.scope, RunID: f.run.ID, TurnID: f.turn.ID, ActionCallID: proposed.Call.ID}); !errors.Is(err, models.ErrNestedTurnNotActive) {
				t.Fatalf("finish after the turn closed err = %v", err)
			}
		})
	}
}
