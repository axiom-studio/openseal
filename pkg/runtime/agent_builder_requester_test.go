package runtime

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder/agentbuildertest"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
)

// TestAgentBuilderCardIsDecidedByTheRequester: an agent or team card names
// the person who asked the builder as its only approver, whatever role the
// host policy named (D9). Without an acting user the policy's approvers stay.
func TestAgentBuilderCardIsDecidedByTheRequester(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		actingUser string
		want       []models.ApprovalPrincipal
	}{
		{"local-operator", []models.ApprovalPrincipal{{Type: "user", ID: "local-operator"}}},
		{"", []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}},
	} {
		fixture := agentbuildertest.NewFixture(t, true)
		store := kerneltest.NewStore(t)
		if _, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: fixture.Scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "finn"},
			AssignedAgentID: "finn", ActingUser: tc.actingUser, Goal: "Build an inbox helper", Source: models.RunSourceChat,
		}); err != nil {
			t.Fatal(err)
		}
		run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
			Scope: fixture.Scope, WorkerID: "conversation-worker", Now: time.Now().UTC(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
		})
		if err != nil || run == nil {
			t.Fatalf("claim = %#v, %v", run, err)
		}
		policy := action.NewDefaultActionPolicy()
		policy.Approvers = []models.ApprovalPrincipal{{Type: "user", ID: "operator"}}
		coordinator := action.NewActionCoordinator(store, store, fixture.Catalog, policy, []action.ActionProposalValidator{fixture.Validator})
		create := agentbuildertest.CreateArguments()
		create["spaceId"] = fixture.SalesID
		result, err := coordinator.Propose(ctx, agentBuilderApprovalRequest(fixture, run, models.AgentActionCreateAgent, create, "requester-"+tc.actingUser))
		if err != nil {
			t.Fatal(err)
		}
		if result.Approval == nil || !reflect.DeepEqual(result.Approval.EligibleApprovers, tc.want) {
			t.Fatalf("acting user %q: approvers = %#v, want %#v", tc.actingUser, result.Approval, tc.want)
		}
	}
}
