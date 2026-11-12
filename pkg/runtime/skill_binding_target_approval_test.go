package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// modePolicy is the kernel default policy evaluated as if the chat were in
// mode, the way a host that falls through to it decides openseal.* writes.
func modePolicy(mode ConversationApprovalMode) ActionPolicyEvaluator {
	base := NewDefaultActionPolicy()
	return ActionPolicyEvaluatorFunc(func(ctx context.Context, input ActionPolicyInput) (ActionPolicyDecision, error) {
		input.ApprovalMode = mode
		decision, err := base.EvaluateAction(ctx, input)
		if err == nil && decision.Disposition == ActionDispositionRequireApproval {
			decision.ApprovalTimeout = ApprovalTimeoutApprove
			decision.ApprovalDestinations = []ApprovalDestination{{EndpointID: "slack-approvals"}}
		}
		return decision, err
	})
}

// targetedSkillCoordinator is Finn's claimed chat Run in the skills target
// fixture, with researcher and Finn already connected to Reddit, and a
// coordinator deciding with policy.
func targetedSkillCoordinator(t *testing.T, policy ActionPolicyEvaluator) (*skillbindingtest.TargetFixture, *AgentRun, *ActionCoordinator) {
	t.Helper()
	f := skillbindingtest.NewTargetFixture(t)
	for _, deploymentID := range []string{"researcher", "finn"} {
		if _, err := f.Catalog.UpsertBinding(f.Ctx, skill.UpsertBindingRequest{Binding: &skill.Binding{
			ID: "reddit", Scope: f.SkillScope, DeploymentID: deploymentID, SkillID: "reddit.reader", SkillVersion: "1.0.0",
			AllowedActions: []string{}, MaximumRisk: skill.RiskLevelRead,
			Credentials: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "credential://tenant-a/reddit-" + deploymentID}},
		}, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Connect Reddit"}); err != nil {
			t.Fatal(err)
		}
	}
	run := f.ChatRun(t, "finn")
	claimed, err := f.Store.ClaimNextAgentRun(f.Ctx, AgentRunClaim{
		Scope: f.Scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return f, claimed, NewActionCoordinator(f.Store, f.Store, f.Catalog, policy, []ActionProposalValidator{f.Validator})
}

func targetedSkillRequest(f *skillbindingtest.TargetFixture, run *AgentRun, action string, arguments map[string]interface{}, toolCallID string) ProposeActionRequest {
	return loopProposeRequest(ProposeActionRequest{
		Scope: f.Scope, RunID: run.ID, WorkerID: "conversation-worker", DeploymentID: "finn",
		SkillID: SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, Action: action,
		Arguments: arguments, Summary: "Give Researcher the Reddit reader",
	}, toolCallID)
}

func TestTargetedSkillBindingChangeIsReviewedInEveryMode(t *testing.T) {
	disable := map[string]interface{}{"bindingId": "reddit", "expectedRevision": 1, skillbinding.SkillActionTargetAgentArgument: "researcher"}
	for _, mode := range []ConversationApprovalMode{ConversationApprovalAuto, ConversationApprovalSkip, ConversationApprovalManual} {
		for action, arguments := range map[string]map[string]interface{}{
			skillbinding.SkillActionUpsertBinding: skillbindingtest.ReaderUpsertArguments("researcher"), skillbinding.SkillActionDisableBinding: disable,
		} {
			f, run, coordinator := targetedSkillCoordinator(t, modePolicy(mode))
			result, err := coordinator.Propose(f.Ctx, targetedSkillRequest(f, run, action, arguments, action))
			if err != nil {
				t.Fatalf("%s %s: %v", mode, action, err)
			}
			approval := result.Approval
			if approval == nil || result.Call.Status != ActionCallStatusWaitingApproval {
				t.Fatalf("%s %s ran with no card: %#v", mode, action, result)
			}
			if len(approval.EligibleApprovers) != 1 || approval.EligibleApprovers[0] != (ApprovalPrincipal{Type: "user", ID: "7"}) {
				t.Fatalf("%s %s approvers = %#v, want the asking person", mode, action, approval.EligibleApprovers)
			}
			if approval.TimeoutDecision != ApprovalTimeoutExpire || len(approval.Destinations) != 0 {
				t.Fatalf("%s %s card timeout %q destinations %#v", mode, action, approval.TimeoutDecision, approval.Destinations)
			}
			if !isTargetedSkillBindingApproval(approval) || !approvedOnlyByPerson(approval) || isAgentBuilderApproval(approval) {
				t.Fatalf("%s %s card is not a targeted skills card: %#v", mode, action, approval.ProposedAction)
			}
			if binding, _ := f.Catalog.GetBinding(f.Ctx, f.SkillScope, "researcher", "reddit"); binding == nil || binding.Revision != 1 || binding.Disabled {
				t.Fatalf("%s %s changed the target before review: %#v", mode, action, binding)
			}
		}
	}

	// The builder's own Skills keep the chat's approval mode.
	f, run, coordinator := targetedSkillCoordinator(t, modePolicy(ConversationApprovalAuto))
	own, err := coordinator.Propose(f.Ctx, targetedSkillRequest(f, run, skillbinding.SkillActionUpsertBinding, skillbindingtest.ReaderUpsertArguments(""), "own"))
	if err != nil || own.Approval != nil {
		t.Fatalf("untargeted upsert in auto mode = %#v, %v", own, err)
	}
}

func TestTargetedSkillBindingChangeCannotBeAllowedByPolicy(t *testing.T) {
	allow := ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow, Reason: "host allows everything"}, nil
	})
	f, run, coordinator := targetedSkillCoordinator(t, allow)
	_, err := coordinator.Propose(f.Ctx, targetedSkillRequest(f, run, skillbinding.SkillActionUpsertBinding, skillbindingtest.ReaderUpsertArguments("researcher"), "allowed"))
	if err == nil || !strings.Contains(err.Error(), "always requires explicit review") {
		t.Fatalf("an allowed targeted upsert was not refused: %v", err)
	}
	calls, err := f.Store.ListActionCalls(f.Ctx, ActionFilter{Scope: f.Scope, RunID: run.ID})
	if err != nil || len(calls) != 0 {
		t.Fatalf("refused proposal recorded calls %#v, %v", calls, err)
	}
}

// approvalHostCounting counts both host methods.
type approvalHostCounting struct {
	ordinary, builder *int
}

func (h approvalHostCounting) AuthorizeApproval(ctx context.Context, principal ApprovalPrincipal, approval *ApprovalCheckpoint) error {
	*h.ordinary++
	return EligibleApprovalAuthorizer{}.AuthorizeApproval(ctx, principal, approval)
}

func (h approvalHostCounting) AuthorizeAgentBuilderApproval(context.Context, ApprovalPrincipal, *ApprovalCheckpoint) error {
	*h.builder++
	return errors.New("a skills card is not an agent-builder card")
}

func targetedSkillCard(operation string) map[string]interface{} {
	return map[string]interface{}{
		"skillId": SkillManagementSkillID, "action": operation, "resourceType": "skill_binding", "operation": operation,
		"targetAgent": map[string]interface{}{"id": "researcher", "displayName": "Researcher"},
	}
}

func TestTargetedSkillBindingApprovalOnlyThroughHostRoute(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		for _, operation := range []string{skillbinding.SkillActionUpsertBinding, skillbinding.SkillActionDisableBinding} {
			card := targetedSkillCard(operation)
			for resolver, authorizer := range map[string]ApprovalAuthorizer{
				"eligible": EligibleApprovalAuthorizer{}, "allow all": allowAllApprovals, "nil": nil,
			} {
				f, approval := cardApproval(t, store, operation+"-"+resolver, card, ApprovalTimeoutExpire)
				if _, err := f.resolveWith(authorizer, approval, ApprovalDecisionApprove, loopApprover, ""); err == nil {
					t.Fatalf("%s approved a targeted %s card", resolver, operation)
				}
				requireStillPending(t, f, approval)
			}

			f, moded := cardApproval(t, store, operation+"-mode", card, ApprovalTimeoutExpire)
			for _, mode := range []ConversationApprovalMode{ConversationApprovalAuto, ConversationApprovalSkip} {
				if _, err := NewApprovalCoordinator(f.store, f.store, nil).ResolveByApprovalMode(f.ctx, f.scope, moded.ID, moded.Revision, mode, "sweep"); !errors.Is(err, ErrAgentBuilderApprovalForbidden) {
					t.Fatalf("approval mode %s approve of a targeted %s card = %v", mode, operation, err)
				}
			}
			requireStillPending(t, f, moded)

			f, timed := cardApproval(t, store, operation+"-timeout", card, ApprovalTimeoutApprove)
			timer := NewApprovalCoordinator(f.store, f.store, EligibleApprovalAuthorizer{}, models.WithClock(func() time.Time { return timed.ExpiresAt.Add(time.Second) }))
			result, err := timer.ResolveTimeout(f.ctx, f.scope, timed.ID, timed.Revision, timed.ID)
			if err != nil || result.Approval.Status != ApprovalStatusExpired || result.Call.Status != ActionCallStatusDenied {
				t.Fatalf("timeout of a targeted %s card = %#v, %v", operation, result, err)
			}

			// A person through the host route approves; the host checks it
			// with AuthorizeApproval (their right on the target Agent).
			ordinary, builder := 0, 0
			host := approvalHostCounting{ordinary: &ordinary, builder: &builder}
			f, approval := cardApproval(t, store, operation+"-host", card, ApprovalTimeoutExpire, loopApprover, ApprovalPrincipal{Type: "role", ID: "operator"})
			if _, err := f.resolveWith(host, approval, ApprovalDecisionApprove, ApprovalPrincipal{Type: "role", ID: "operator"}, ""); !errors.Is(err, ErrAgentBuilderApprovalForbidden) {
				t.Fatalf("role approve of a targeted %s card = %v", operation, err)
			}
			result, err = f.resolveWith(host, approval, ApprovalDecisionApprove, loopApprover, "")
			if err != nil || result.Approval.Status != ApprovalStatusApproved || result.Call.Status != ActionCallStatusReady {
				t.Fatalf("host approve of a targeted %s card = %#v, %v", operation, result, err)
			}
			if ordinary != 1 || builder != 0 {
				t.Fatalf("host checks: AuthorizeApproval %d, AuthorizeAgentBuilderApproval %d", ordinary, builder)
			}

			// Reject stays allowed everywhere.
			f, rejected := cardApproval(t, store, operation+"-reject", card, ApprovalTimeoutExpire)
			if result, err := f.resolveWith(EligibleApprovalAuthorizer{}, rejected, ApprovalDecisionReject, loopApprover, "not now"); err != nil || result.Approval.Status != ApprovalStatusRejected {
				t.Fatalf("reject of a targeted %s card = %#v, %v", operation, result, err)
			}
		}

		// The chat's own skill change keeps every ordinary path.
		own := map[string]interface{}{"skillId": SkillManagementSkillID, "action": skillbinding.SkillActionUpsertBinding, "resourceType": "skill_binding", "operation": skillbinding.SkillActionUpsertBinding}
		f, approval := cardApproval(t, store, "own-mode", own, ApprovalTimeoutExpire)
		if result, err := NewApprovalCoordinator(f.store, f.store, nil).ResolveByApprovalMode(f.ctx, f.scope, approval.ID, approval.Revision, ConversationApprovalAuto, "sweep"); err != nil || result.Approval.Status != ApprovalStatusApproved {
			t.Fatalf("approval-mode approve of an own skills card = %#v, %v", result, err)
		}
	})
}
