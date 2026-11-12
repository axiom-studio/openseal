package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestAgentBuilderCardIsDecidedByTheRequester: an agent or team card names
// the person who asked the builder as its only approver, whatever role the
// host policy named (D9). Without an acting user the policy's approvers stay.
func TestAgentBuilderCardIsDecidedByTheRequester(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		actingUser string
		want       []ApprovalPrincipal
	}{
		{"local-operator", []ApprovalPrincipal{{Type: "user", ID: "local-operator"}}},
		{"", []ApprovalPrincipal{{Type: "user", ID: "operator"}}},
	} {
		fixture := newAgentBuilderFixture(t, true)
		store := newTestKernelStore(t)
		if _, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: fixture.scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "finn"},
			AssignedAgentID: "finn", ActingUser: tc.actingUser, Goal: "Build an inbox helper", Source: RunSourceChat,
		}); err != nil {
			t.Fatal(err)
		}
		run, err := store.ClaimNextAgentRun(ctx, AgentRunClaim{
			Scope: fixture.scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
		})
		if err != nil || run == nil {
			t.Fatalf("claim = %#v, %v", run, err)
		}
		policy := NewDefaultActionPolicy()
		policy.Approvers = []ApprovalPrincipal{{Type: "user", ID: "operator"}}
		coordinator := NewActionCoordinator(store, store, fixture.catalog, policy, []ActionProposalValidator{fixture.validator})
		create := agentBuilderCreateArguments()
		create["spaceId"] = fixture.salesID
		result, err := coordinator.Propose(ctx, agentBuilderApprovalRequest(fixture, run, AgentActionCreateAgent, create, "requester-"+tc.actingUser))
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval == nil || !reflect.DeepEqual(result.Approval.EligibleApprovers, tc.want) {
			t.Fatalf("acting user %q: approvers = %#v, want %#v", tc.actingUser, result.Approval, tc.want)
		}
	}
}
