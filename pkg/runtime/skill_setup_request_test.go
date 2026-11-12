package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding/skillbindingtest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillSetupStoresScopeCASAndTerminalState(t *testing.T) {
	ctx := context.Background()
	sqlite, err := sqlite.NewSQLiteStore(filepath.Join(t.TempDir(), "setup.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	for name, store := range map[string]models.SkillSetupRequestStore{"sqlite": sqlite} {
		t.Run(name, func(t *testing.T) {
			r := skillbindingtest.SkillSetupRequest()
			if err := store.SaveSkillSetupRequest(ctx, r, 0); err != nil {
				t.Fatal(err)
			}
			if other, err := store.GetSkillSetupRequest(ctx, models.Scope{Kind: "tenant", ID: "b"}, r.ID); err != nil || other != nil {
				t.Fatalf("cross-tenant read: %#v %v", other, err)
			}
			if other, err := store.ListSkillSetupRequests(ctx, r.Scope, "foreign-agent", r.ConversationID); err != nil || len(other) != 0 {
				t.Fatalf("cross-agent list: %#v %v", other, err)
			}
			if other, err := store.ListSkillSetupRequests(ctx, r.Scope, r.DeploymentID, "foreign-chat"); err != nil || len(other) != 0 {
				t.Fatalf("cross-conversation list: %#v %v", other, err)
			}
			if err := store.SaveSkillSetupRequest(ctx, r, 0); !errors.Is(err, models.ErrSkillSetupConflict) {
				t.Fatalf("duplicate request not rejected: %v", err)
			}
			r.Status = "resolved"
			r.Revision = 2
			r.ResolvedBindingID = "reddit"
			r.ResolvedBindingRevision = 1
			r.ResolvedBy = "user"
			r.UpdatedAt = time.Now().UTC()
			if err := store.SaveSkillSetupRequest(ctx, r, 1); err != nil {
				t.Fatal(err)
			}
			loaded, err := store.GetSkillSetupRequest(ctx, r.Scope, r.ID)
			if err != nil || loaded.Status != "resolved" {
				t.Fatalf("saved status: %#v %v", loaded, err)
			}
			r.Status = "pending"
			r.Revision = 3
			r.ResolvedBindingID = ""
			r.ResolvedBindingRevision = 0
			r.ResolvedBy = ""
			if err := store.SaveSkillSetupRequest(ctx, r, 2); !errors.Is(err, models.ErrSkillSetupConflict) {
				t.Fatalf("terminal request reopened: %v", err)
			}
		})
	}
}
func TestSkillSetupSurvivesSQLiteReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "setup.db")
	store, err := sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	r := skillbindingtest.SkillSetupRequest()
	if err := store.SaveSkillSetupRequest(context.Background(), r, 0); err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = sqlite.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	requests, err := store.ListSkillSetupRequests(context.Background(), r.Scope, r.DeploymentID, r.ConversationID)
	if err != nil || len(requests) != 1 || requests[0].ID != r.ID {
		t.Fatalf("reopened requests: %#v %v", requests, err)
	}
}

func TestSkillSetupActionRunsThroughCanonicalProposalAndWorker(t *testing.T) {
	ctx := context.Background()
	store := newTestKernelStore(t)
	scope := models.Scope{Kind: "tenant", ID: "a"}
	catalog := skillbindingtest.SkillActionCatalog(t, ctx, scope, "agent")
	if err := catalog.Bind(ctx, &skill.Binding{ID: "setup-actions", Revision: 1, Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "agent", SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, AllowedActions: []string{agentrun.SkillActionRequestSetup, skillbinding.SkillActionListSetupRequests}, MaximumRisk: skill.RiskLevelRead}); err != nil {
		t.Fatal(err)
	}
	created, err := portfolio.NewPortfolioService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Connect Reddit", Source: models.RunSourceChat, Context: map[string]interface{}{"conversationId": "chat", "triggerMessageId": "message"}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{Scope: scope, WorkerID: "worker", Now: time.Now().UTC(), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run.ID != created.ID {
		t.Fatal(err)
	}
	validator, _ := skillbinding.NewSkillBindingActionValidator(catalog, store)
	coordinator := action.NewActionCoordinator(store, store, catalog, action.NewDefaultActionPolicy(), []action.ActionProposalValidator{validator})
	step, err := activity.NewAgentTurnService(store, store).BeginTurn(ctx, activity.BeginAgentTurnRequest{Scope: scope, RunID: run.ID, SessionID: run.SessionID, Producer: models.StepProducerModel, WorkerID: "worker", LeaseDuration: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := coordinator.Propose(ctx, action.ProposeActionRequest{Scope: scope, RunID: run.ID, TurnID: step.ID, WorkerID: "worker", DeploymentID: "agent", SkillID: models.SkillManagementSkillID, SkillVersion: skillbinding.SkillManagementSkillVersion, Action: agentrun.SkillActionRequestSetup, Arguments: map[string]interface{}{"kind": "configure", "skillId": "reddit.reader", "skillVersion": "1.0.0", "reason": "Read the community"}, Summary: "Ask for Reddit setup",
		Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop, IdempotencyKey: "loop:" + step.ID + ":call-setup", CausationID: step.ID})
	if err != nil {
		t.Fatal(err)
	}
	if proposal.Approval != nil || proposal.Call.Status != models.ActionCallStatusReady {
		t.Fatalf("request improperly grants or requires execution approval: %#v", proposal)
	}
	provider := skill.DiscoveryProviderFunc(func(context.Context, skill.DiscoveryRequest) (*skill.DiscoveryPage, error) {
		return &skill.DiscoveryPage{Items: []skill.DiscoveryCandidate{{ID: "reddit.reader", Version: "1.0.0", Name: "Reddit", Readiness: skill.DiscoveryReadinessBindable}}}, nil
	})
	dispatcher, _ := skillbinding.NewSkillBindingActionDispatcher(store, catalog, nil, provider)
	executed, err := action.NewActionWorker(store, catalog, nil, dispatcher).ExecuteOwnLoopCall(ctx, scope, proposal.Call.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if executed.Call.Status != models.ActionCallStatusSucceeded || executed.Call.Output["setupRequest"] == nil {
		t.Fatalf("setup action: %#v", executed.Call)
	}
	records, err := store.ListSkillSetupRequests(ctx, scope, "agent", "chat")
	if err != nil || len(records) != 1 || records[0].ActionCallID != proposal.Call.ID {
		t.Fatalf("durable request: %#v %v", records, err)
	}
}
