package action_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Agent-builder approval paths (D11, spec-openseal §3.1 "Approval paths").
//
// An approve of an agent_profile card (create_agent, update_agent) or a
// create_team card (and of an openseal.skills binding card aimed at another
// Agent, skill_binding_target_approval_test.go) is accepted only by a coordinator whose authorizer is an
// AgentBuilderApprovalAuthorizer, and only from a "user" principal. Every
// approval resolver in pkg and internal (grep NewApprovalCoordinator and
// ResolveTimeout), with the authorizer it builds:
//
//   - external_conversation_transport_service.go: EligibleApprovalAuthorizer, or an
//     allow-all ApprovalAuthorizerFunc for origin-conversation participants.
//   - callback_approval_consumer.go: EligibleApprovalAuthorizer.
//   - approval_notification_worker.go: EligibleApprovalAuthorizer, ResolveTimeout.
//   - conversation_approval_mode.go: nil, ResolveByApprovalMode.
//   - conversation_run.go (ConversationRunScheduler): nil.
//   - conversation_team_responder.go (team follow-ups): nil.
//   - pkg/openseal Engine: the host's WithApprovalAuthorizer, default
//     EligibleApprovalAuthorizer (cortex passes its Casbin authorizer).
//   - internal/server/action_approvals.go: DesktopApprovalAuthorizer, which
//     implements AgentBuilderApprovalAuthorizer (internal/server
//     TestDesktopApprovalAuthorizerAgentBuilder).
//   - run_commands.go cancel resolves the approval directly (canceled).
//
// Each case below resolves through the authorizer that caller builds.

// builderApprovalHost is a host authorizer that may approve agent-builder
// cards; builderErr, when set, is its refusal.
type builderApprovalHost struct {
	action.EligibleApprovalAuthorizer
	builderErr error
	calls      *int
}

func (h builderApprovalHost) AuthorizeAgentBuilderApproval(_ context.Context, _ models.ApprovalPrincipal, approval *models.ApprovalCheckpoint) error {
	if h.calls != nil {
		*h.calls++
	}
	if approval == nil {
		return errors.New("approval is required")
	}
	return h.builderErr
}

var allowAllApprovals = action.ApprovalAuthorizerFunc(func(context.Context, models.ApprovalPrincipal, *models.ApprovalCheckpoint) error { return nil })

// cardApproval starts its own Run, commits a write loop call waiting on an
// approval whose review preview is proposed, and parks the Run on it.
func cardApproval(t *testing.T, store models.KernelStore, id string, proposed map[string]interface{}, timeout models.ApprovalTimeoutDecision, approvers ...models.ApprovalPrincipal) (*actiontest.LoopStore, *models.ApprovalCheckpoint) {
	t.Helper()
	f := actiontest.NewLoopStore(t, store)
	if len(approvers) == 0 {
		approvers = []models.ApprovalPrincipal{actiontest.LoopApprover}
	}
	call := f.LoopCall(id)
	call.SkillID, call.Action = models.AgentManagementSkillID, models.AgentActionCreateAgent
	call.Risk, call.SideEffect = skill.RiskLevelWrite, skill.SideEffectWrite
	call.Status, call.ApprovalID = models.ActionCallStatusWaitingApproval, "approval-"+id
	call.InvocationDigest = models.ComputeActionInvocationDigest(call)
	call.SemanticDigest = models.ComputeActionSemanticDigest(call)
	approval := &models.ApprovalCheckpoint{
		ID: call.ApprovalID, Scope: f.Scope, RunID: f.Run.ID, ActionCallID: call.ID, Status: models.ApprovalStatusPending,
		Risk: skill.RiskLevelWrite, Summary: "Create the Agent", ProposedAction: proposed,
		EligibleApprovers: approvers, TimeoutDecision: timeout,
		ExpiresAt: f.Now.Add(time.Hour), Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now,
	}
	proposal := f.Proposal(call)
	proposal.Approval = approval
	result, err := f.Store.CreateActionProposal(f.Ctx, proposal)
	if err != nil || !result.Created {
		f.T.Fatalf("propose %s = %#v, %v", id, result, err)
	}
	f.Run = f.CurrentRun()
	f.ParkOn(result.Approval.ID)
	return f, result.Approval
}

func createAgentCard() map[string]interface{} {
	return map[string]interface{}{
		"skillId": models.AgentManagementSkillID, "action": models.AgentActionCreateAgent,
		"resourceType": models.AgentProfileResourceType, "operation": models.AgentActionCreateAgent,
	}
}

func agentBuilderCards() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"create_agent": createAgentCard(),
		"update_agent": {"skillId": models.AgentManagementSkillID, "action": models.AgentActionUpdateAgent, "resourceType": models.AgentProfileResourceType, "operation": models.AgentActionUpdateAgent},
		"create_team":  {"skillId": "openseal.teams", "action": "create_team", "resourceType": "team", "operation": "create_team"},
	}
}

func requireStillPending(t *testing.T, f *actiontest.LoopStore, approval *models.ApprovalCheckpoint) {
	t.Helper()
	current, err := f.Store.GetApproval(f.Ctx, f.Scope, approval.ID)
	if err != nil || current.Status != models.ApprovalStatusPending || current.Revision != approval.Revision {
		t.Fatalf("approval after a refused approve = %#v, %v", current, err)
	}
	if run := f.CurrentRun(); run.Status != models.AgentRunStatusWaitingForApproval {
		t.Fatalf("a refused approve moved the Run: %#v", run)
	}
}

func TestAgentBuilderApprovalPathsRefuseInKernelApprove(t *testing.T) {
	resolvers := map[string]action.ApprovalAuthorizer{
		"external_conversation_transport eligible": action.EligibleApprovalAuthorizer{},
		"external_conversation_transport origin":   allowAllApprovals,
		"callback_approval_consumer":               action.EligibleApprovalAuthorizer{},
		"engine default authorizer":                action.EligibleApprovalAuthorizer{},
	}
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		for cardName, card := range agentBuilderCards() {
			for resolver, authorizer := range resolvers {
				f, approval := cardApproval(t, store, cardName+"-"+resolver, card, models.ApprovalTimeoutExpire)
				if _, err := f.ResolveWith(authorizer, approval, models.ApprovalDecisionApprove, actiontest.LoopApprover, ""); !errors.Is(err, action.ErrAgentBuilderApprovalForbidden) {
					t.Fatalf("%s approve of %s = %v, want ErrAgentBuilderApprovalForbidden", resolver, cardName, err)
				}
				requireStillPending(t, f, approval)
				// Reject stays allowed on the same path.
				result, err := f.ResolveWith(authorizer, approval, models.ApprovalDecisionReject, actiontest.LoopApprover, "not now")
				if err != nil || result.Approval.Status != models.ApprovalStatusRejected || result.Call.Status != models.ActionCallStatusDenied {
					t.Fatalf("%s reject of %s = %#v, %v", resolver, cardName, result, err)
				}
			}
		}
	})
}

func TestAgentBuilderApprovalPathsRefuseApprovalModeAndNilCoordinators(t *testing.T) {
	// conversation_approval_mode.go, conversation_run.go and
	// conversation_team_responder.go build their coordinator with no
	// authorizer: the approval-mode approve is refused, a principal approve is
	// not configured at all.
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f, approval := cardApproval(t, store, "mode", createAgentCard(), models.ApprovalTimeoutExpire)
		coordinator := action.NewApprovalCoordinator(f.Store, f.Store, nil)
		for _, mode := range []models.ConversationApprovalMode{models.ConversationApprovalAuto, models.ConversationApprovalSkip} {
			if _, err := coordinator.ResolveByApprovalMode(f.Ctx, f.Scope, approval.ID, approval.Revision, mode, "sweep"); !errors.Is(err, action.ErrAgentBuilderApprovalForbidden) {
				t.Fatalf("approval mode %s approve = %v", mode, err)
			}
		}
		if _, err := f.ResolveWith(nil, approval, models.ApprovalDecisionApprove, actiontest.LoopApprover, ""); err == nil {
			t.Fatal("a nil-authorizer coordinator approved an agent-builder card")
		}
		requireStillPending(t, f, approval)
		// Supersede-by-message stays allowed.
		result, err := coordinator.ResolveSupersededByMessage(f.Ctx, f.Scope, approval.ID, approval.Revision, "message-1")
		if err != nil || result.Approval.Status != models.ApprovalStatusSuperseded {
			t.Fatalf("supersede = %#v, %v", result, err)
		}
	})
}

func TestAgentBuilderApprovalPathsTimeoutNeverApproves(t *testing.T) {
	// approval_notification_worker.go: ResolveTimeout with
	// EligibleApprovalAuthorizer expires the card even under TimeoutDecision
	// approve.
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f, approval := cardApproval(t, store, "timeout", createAgentCard(), models.ApprovalTimeoutApprove)
		coordinator := action.NewApprovalCoordinator(f.Store, f.Store, action.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return approval.ExpiresAt.Add(time.Second) }))
		result, err := coordinator.ResolveTimeout(f.Ctx, f.Scope, approval.ID, approval.Revision, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != models.ApprovalStatusExpired || result.Call.Status != models.ActionCallStatusDenied || result.Event.EventType != "approval.expired" {
			t.Fatalf("timeout of an agent-builder card = %#v", result)
		}
	})
}

func TestAgentBuilderApprovalPathsChangesRequestedAndCancel(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		f, approval := cardApproval(t, store, "changes", createAgentCard(), models.ApprovalTimeoutExpire)
		result, err := f.ResolveWith(action.EligibleApprovalAuthorizer{}, approval, models.ApprovalDecisionRequestChanges, actiontest.LoopApprover, "Call it Ledger instead.")
		if err != nil || result.Approval.Status != models.ApprovalStatusChangesRequested {
			t.Fatalf("changes requested = %#v, %v", result, err)
		}

		// run_commands.go: canceling the Run cancels the pending card.
		g, pending := cardApproval(t, store, "cancel", createAgentCard(), models.ApprovalTimeoutExpire)
		canceled, err := agentrun.NewRunCommandService(store).CommandAgentRun(g.Ctx, agentrun.AgentRunCommandRequest{
			Scope: g.Scope, RunID: g.Run.ID, ExpectedRevision: g.CurrentRun().Revision, Kind: agentrun.AgentRunCommandCancel,
			Actor: models.ActivityActor{Type: "user", ID: "user"},
		})
		if err != nil || canceled.Run.Status != models.AgentRunStatusCanceled {
			t.Fatalf("cancel = %#v, %v", canceled, err)
		}
		if current, err := store.GetApproval(g.Ctx, g.Scope, pending.ID); err != nil || current.Status != models.ApprovalStatusCanceled {
			t.Fatalf("approval after cancel = %#v, %v", current, err)
		}
	})
}

func TestAgentBuilderApprovalPathsHostAuthorizer(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		role := models.ApprovalPrincipal{Type: "role", ID: "operator"}
		calls := 0
		host := builderApprovalHost{calls: &calls}

		// A role principal is refused even by the host authorizer.
		f, roleCard := cardApproval(t, store, "role", createAgentCard(), models.ApprovalTimeoutExpire, actiontest.LoopApprover, role)
		if _, err := f.ResolveWith(host, roleCard, models.ApprovalDecisionApprove, role, ""); !errors.Is(err, action.ErrAgentBuilderApprovalForbidden) {
			t.Fatalf("role approve = %v", err)
		}
		requireStillPending(t, f, roleCard)

		// The host's refusal stands.
		refused := builderApprovalHost{builderErr: errors.New("not allowed by policy")}
		if _, err := f.ResolveWith(refused, roleCard, models.ApprovalDecisionApprove, actiontest.LoopApprover, ""); err == nil || errors.Is(err, action.ErrAgentBuilderApprovalForbidden) {
			t.Fatalf("host-refused approve = %v", err)
		}
		requireStillPending(t, f, roleCard)

		// A user principal through the host authorizer approves.
		result, err := f.ResolveWith(host, roleCard, models.ApprovalDecisionApprove, actiontest.LoopApprover, "")
		if err != nil || result.Approval.Status != models.ApprovalStatusApproved || result.Call.Status != models.ActionCallStatusReady ||
			result.Approval.DecisionBy == nil || result.Approval.DecisionBy.Type != "user" {
			t.Fatalf("host approve = %#v, %v", result, err)
		}
		if calls != 1 {
			t.Fatalf("AuthorizeAgentBuilderApproval calls = %d, want 1", calls)
		}

		// The ordinary eligibility check still applies.
		f, other := cardApproval(t, store, "ineligible", createAgentCard(), models.ApprovalTimeoutExpire, models.ApprovalPrincipal{Type: "user", ID: "someone-else"})
		if _, err := f.ResolveWith(host, other, models.ApprovalDecisionApprove, actiontest.LoopApprover, ""); err == nil {
			t.Fatal("an ineligible user approved an agent-builder card")
		}
		requireStillPending(t, f, other)
	})
}

func TestAgentBuilderApprovalPathsOtherCardsUnchanged(t *testing.T) {
	actiontest.RunLoopStoreScenario(t, func(t *testing.T, store models.KernelStore) {
		others := map[string]map[string]interface{}{
			"plain":       {"skillId": "github", "action": "push"},
			"team member": {"skillId": "openseal.teams", "action": action.TeamActionUpdateMember, "resourceType": "team", "operation": action.TeamActionUpdateMember},
			"agent other": {"skillId": models.AgentManagementSkillID, "action": "propose_amendment", "resourceType": "agent", "operation": "propose_amendment"},
		}
		for name, card := range others {
			f, approval := cardApproval(t, store, "eligible-"+name, card, models.ApprovalTimeoutExpire)
			result, err := f.ResolveWith(action.EligibleApprovalAuthorizer{}, approval, models.ApprovalDecisionApprove, actiontest.LoopApprover, "")
			if err != nil || result.Approval.Status != models.ApprovalStatusApproved {
				t.Fatalf("%s eligible approve = %#v, %v", name, result, err)
			}
			f, moded := cardApproval(t, store, "mode-"+name, card, models.ApprovalTimeoutExpire)
			coordinator := action.NewApprovalCoordinator(f.Store, f.Store, nil)
			if result, err := coordinator.ResolveByApprovalMode(f.Ctx, f.Scope, moded.ID, moded.Revision, models.ConversationApprovalSkip, "sweep"); err != nil || result.Approval.Status != models.ApprovalStatusApproved {
				t.Fatalf("%s approval-mode approve = %#v, %v", name, result, err)
			}
			f, timed := cardApproval(t, store, "timeout-"+name, card, models.ApprovalTimeoutApprove)
			timer := action.NewApprovalCoordinator(f.Store, f.Store, action.EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return timed.ExpiresAt.Add(time.Second) }))
			if result, err := timer.ResolveTimeout(f.Ctx, f.Scope, timed.ID, timed.Revision, timed.ID); err != nil || result.Approval.Status != models.ApprovalStatusApproved {
				t.Fatalf("%s timeout auto-approve = %#v, %v", name, result, err)
			}
		}
	})
}
