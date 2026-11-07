package runtime

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

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// builderApprovalHost is a host authorizer that may approve agent-builder
// cards; builderErr, when set, is its refusal.
type builderApprovalHost struct {
	EligibleApprovalAuthorizer
	builderErr error
	calls      *int
}

func (h builderApprovalHost) AuthorizeAgentBuilderApproval(_ context.Context, _ ApprovalPrincipal, approval *ApprovalCheckpoint) error {
	if h.calls != nil {
		*h.calls++
	}
	if approval == nil {
		return errors.New("approval is required")
	}
	return h.builderErr
}

var allowAllApprovals = ApprovalAuthorizerFunc(func(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error { return nil })

// cardApproval starts its own Run, commits a write loop call waiting on an
// approval whose review preview is proposed, and parks the Run on it.
func cardApproval(t *testing.T, store KernelStore, id string, proposed map[string]interface{}, timeout ApprovalTimeoutDecision, approvers ...ApprovalPrincipal) (*loopStoreFixture, *ApprovalCheckpoint) {
	t.Helper()
	f := newLoopStoreFixture(t, store)
	if len(approvers) == 0 {
		approvers = []ApprovalPrincipal{loopApprover}
	}
	call := f.loopCall(id)
	call.SkillID, call.Action = AgentManagementSkillID, AgentActionCreateAgent
	call.Risk, call.SideEffect = skill.RiskLevelWrite, skill.SideEffectWrite
	call.Status, call.ApprovalID = ActionCallStatusWaitingApproval, "approval-"+id
	call.InvocationDigest = ComputeActionInvocationDigest(call)
	call.SemanticDigest = ComputeActionSemanticDigest(call)
	approval := &ApprovalCheckpoint{
		ID: call.ApprovalID, Scope: f.scope, RunID: f.run.ID, ActionCallID: call.ID, Status: ApprovalStatusPending,
		Risk: skill.RiskLevelWrite, Summary: "Create the Agent", ProposedAction: proposed,
		EligibleApprovers: approvers, TimeoutDecision: timeout,
		ExpiresAt: f.now.Add(time.Hour), Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	proposal := f.proposal(call)
	proposal.Approval = approval
	result, err := f.store.CreateActionProposal(f.ctx, proposal)
	if err != nil || !result.Created {
		f.t.Fatalf("propose %s = %#v, %v", id, result, err)
	}
	f.run = f.currentRun()
	f.parkOn(result.Approval.ID)
	return f, result.Approval
}

func createAgentCard() map[string]interface{} {
	return map[string]interface{}{
		"skillId": AgentManagementSkillID, "action": AgentActionCreateAgent,
		"resourceType": agentProfileResourceType, "operation": AgentActionCreateAgent,
	}
}

func agentBuilderCards() map[string]map[string]interface{} {
	return map[string]map[string]interface{}{
		"create_agent": createAgentCard(),
		"update_agent": {"skillId": AgentManagementSkillID, "action": AgentActionUpdateAgent, "resourceType": agentProfileResourceType, "operation": AgentActionUpdateAgent},
		"create_team":  {"skillId": "openseal.teams", "action": "create_team", "resourceType": "team", "operation": "create_team"},
	}
}

func (f *loopStoreFixture) resolveWith(authorizer ApprovalAuthorizer, approval *ApprovalCheckpoint, decision ApprovalDecision, principal ApprovalPrincipal, reason string) (*ApprovalResolutionResult, error) {
	coordinator := NewApprovalCoordinator(f.store, f.store, authorizer)
	coordinator.now = func() time.Time { return f.now.Add(time.Second) }
	return coordinator.Resolve(f.ctx, ResolveApprovalRequest{
		Scope: f.scope, ApprovalID: approval.ID, ExpectedRevision: approval.Revision,
		DecisionID: "decision-" + approval.ID, Decision: decision, Principal: principal, Reason: reason,
	})
}

func requireStillPending(t *testing.T, f *loopStoreFixture, approval *ApprovalCheckpoint) {
	t.Helper()
	current, err := f.store.GetApproval(f.ctx, f.scope, approval.ID)
	if err != nil || current.Status != ApprovalStatusPending || current.Revision != approval.Revision {
		t.Fatalf("approval after a refused approve = %#v, %v", current, err)
	}
	if run := f.currentRun(); run.Status != AgentRunStatusWaitingForApproval {
		t.Fatalf("a refused approve moved the Run: %#v", run)
	}
}

func TestAgentBuilderApprovalPathsRefuseInKernelApprove(t *testing.T) {
	resolvers := map[string]ApprovalAuthorizer{
		"external_conversation_transport eligible": EligibleApprovalAuthorizer{},
		"external_conversation_transport origin":   allowAllApprovals,
		"callback_approval_consumer":               EligibleApprovalAuthorizer{},
		"engine default authorizer":                EligibleApprovalAuthorizer{},
	}
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		for cardName, card := range agentBuilderCards() {
			for resolver, authorizer := range resolvers {
				f, approval := cardApproval(t, store, cardName+"-"+resolver, card, ApprovalTimeoutExpire)
				if _, err := f.resolveWith(authorizer, approval, ApprovalDecisionApprove, loopApprover, ""); !errors.Is(err, ErrAgentBuilderApprovalForbidden) {
					t.Fatalf("%s approve of %s = %v, want ErrAgentBuilderApprovalForbidden", resolver, cardName, err)
				}
				requireStillPending(t, f, approval)
				// Reject stays allowed on the same path.
				result, err := f.resolveWith(authorizer, approval, ApprovalDecisionReject, loopApprover, "not now")
				if err != nil || result.Approval.Status != ApprovalStatusRejected || result.Call.Status != ActionCallStatusDenied {
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
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f, approval := cardApproval(t, store, "mode", createAgentCard(), ApprovalTimeoutExpire)
		coordinator := NewApprovalCoordinator(f.store, f.store, nil)
		for _, mode := range []ConversationApprovalMode{ConversationApprovalAuto, ConversationApprovalSkip} {
			if _, err := coordinator.ResolveByApprovalMode(f.ctx, f.scope, approval.ID, approval.Revision, mode, "sweep"); !errors.Is(err, ErrAgentBuilderApprovalForbidden) {
				t.Fatalf("approval mode %s approve = %v", mode, err)
			}
		}
		if _, err := f.resolveWith(nil, approval, ApprovalDecisionApprove, loopApprover, ""); err == nil {
			t.Fatal("a nil-authorizer coordinator approved an agent-builder card")
		}
		requireStillPending(t, f, approval)
		// Supersede-by-message stays allowed.
		result, err := coordinator.ResolveSupersededByMessage(f.ctx, f.scope, approval.ID, approval.Revision, "message-1")
		if err != nil || result.Approval.Status != ApprovalStatusSuperseded {
			t.Fatalf("supersede = %#v, %v", result, err)
		}
	})
}

func TestAgentBuilderApprovalPathsTimeoutNeverApproves(t *testing.T) {
	// approval_notification_worker.go: ResolveTimeout with
	// EligibleApprovalAuthorizer expires the card even under TimeoutDecision
	// approve.
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f, approval := cardApproval(t, store, "timeout", createAgentCard(), ApprovalTimeoutApprove)
		coordinator := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{})
		coordinator.now = func() time.Time { return approval.ExpiresAt.Add(time.Second) }
		result, err := coordinator.ResolveTimeout(f.ctx, f.scope, approval.ID, approval.Revision, approval.ID)
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied || result.Event.EventType != "approval.expired" {
			t.Fatalf("timeout of an agent-builder card = %#v", result)
		}
	})
}

func TestAgentBuilderApprovalPathsChangesRequestedAndCancel(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f, approval := cardApproval(t, store, "changes", createAgentCard(), ApprovalTimeoutExpire)
		result, err := f.resolveWith(EligibleApprovalAuthorizer{}, approval, ApprovalDecisionRequestChanges, loopApprover, "Call it Ledger instead.")
		if err != nil || result.Approval.Status != ApprovalStatusChangesRequested {
			t.Fatalf("changes requested = %#v, %v", result, err)
		}

		// run_commands.go: canceling the Run cancels the pending card.
		g, pending := cardApproval(t, store, "cancel", createAgentCard(), ApprovalTimeoutExpire)
		canceled, err := NewRunCommandService(store).CommandAgentRun(g.ctx, AgentRunCommandRequest{
			Scope: g.scope, RunID: g.run.ID, ExpectedRevision: g.currentRun().Revision, Kind: AgentRunCommandCancel,
			Actor: ActivityActor{Type: "user", ID: "user"},
		})
		if err != nil || canceled.Run.Status != AgentRunStatusCanceled {
			t.Fatalf("cancel = %#v, %v", canceled, err)
		}
		if current, err := store.GetApproval(g.ctx, g.scope, pending.ID); err != nil || current.Status != ApprovalStatusCanceled {
			t.Fatalf("approval after cancel = %#v, %v", current, err)
		}
	})
}

func TestAgentBuilderApprovalPathsHostAuthorizer(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		role := ApprovalPrincipal{Type: "role", ID: "operator"}
		calls := 0
		host := builderApprovalHost{calls: &calls}

		// A role principal is refused even by the host authorizer.
		f, roleCard := cardApproval(t, store, "role", createAgentCard(), ApprovalTimeoutExpire, loopApprover, role)
		if _, err := f.resolveWith(host, roleCard, ApprovalDecisionApprove, role, ""); !errors.Is(err, ErrAgentBuilderApprovalForbidden) {
			t.Fatalf("role approve = %v", err)
		}
		requireStillPending(t, f, roleCard)

		// The host's refusal stands.
		refused := builderApprovalHost{builderErr: errors.New("not allowed by policy")}
		if _, err := f.resolveWith(refused, roleCard, ApprovalDecisionApprove, loopApprover, ""); err == nil || errors.Is(err, ErrAgentBuilderApprovalForbidden) {
			t.Fatalf("host-refused approve = %v", err)
		}
		requireStillPending(t, f, roleCard)

		// A user principal through the host authorizer approves.
		result, err := f.resolveWith(host, roleCard, ApprovalDecisionApprove, loopApprover, "")
		if err != nil || result.Approval.Status != ApprovalStatusApproved || result.Call.Status != ActionCallStatusReady ||
			result.Approval.DecisionBy == nil || result.Approval.DecisionBy.Type != "user" {
			t.Fatalf("host approve = %#v, %v", result, err)
		}
		if calls != 1 {
			t.Fatalf("AuthorizeAgentBuilderApproval calls = %d, want 1", calls)
		}

		// The ordinary eligibility check still applies.
		f, other := cardApproval(t, store, "ineligible", createAgentCard(), ApprovalTimeoutExpire, ApprovalPrincipal{Type: "user", ID: "someone-else"})
		if _, err := f.resolveWith(host, other, ApprovalDecisionApprove, loopApprover, ""); err == nil {
			t.Fatal("an ineligible user approved an agent-builder card")
		}
		requireStillPending(t, f, other)
	})
}

func TestAgentBuilderApprovalPathsOtherCardsUnchanged(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		others := map[string]map[string]interface{}{
			"plain":       {"skillId": "github", "action": "push"},
			"team role":   {"skillId": "openseal.teams", "action": TeamActionUpdateRole, "resourceType": "team", "operation": TeamActionUpdateRole},
			"agent other": {"skillId": AgentManagementSkillID, "action": "propose_amendment", "resourceType": "agent", "operation": "propose_amendment"},
		}
		for name, card := range others {
			f, approval := cardApproval(t, store, "eligible-"+name, card, ApprovalTimeoutExpire)
			result, err := f.resolveWith(EligibleApprovalAuthorizer{}, approval, ApprovalDecisionApprove, loopApprover, "")
			if err != nil || result.Approval.Status != ApprovalStatusApproved {
				t.Fatalf("%s eligible approve = %#v, %v", name, result, err)
			}
			f, moded := cardApproval(t, store, "mode-"+name, card, ApprovalTimeoutExpire)
			coordinator := NewApprovalCoordinator(f.store, f.store, nil)
			if result, err := coordinator.ResolveByApprovalMode(f.ctx, f.scope, moded.ID, moded.Revision, ConversationApprovalSkip, "sweep"); err != nil || result.Approval.Status != ApprovalStatusApproved {
				t.Fatalf("%s approval-mode approve = %#v, %v", name, result, err)
			}
			f, timed := cardApproval(t, store, "timeout-"+name, card, ApprovalTimeoutApprove)
			timer := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{})
			timer.now = func() time.Time { return timed.ExpiresAt.Add(time.Second) }
			if result, err := timer.ResolveTimeout(f.ctx, f.scope, timed.ID, timed.Revision, timed.ID); err != nil || result.Approval.Status != ApprovalStatusApproved {
				t.Fatalf("%s timeout auto-approve = %#v, %v", name, result, err)
			}
		}
	})
}
