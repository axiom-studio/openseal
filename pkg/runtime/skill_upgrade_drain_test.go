package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	runtimeactivity "github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeDrainsQueuedCallsAndLiveReceiptsAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		catalog, proposal, plan := skillUpgradeDrainFixture(t, store)
		service := skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog)
		apply := func() error {
			_, err := service.Apply(t.Context(), skillbinding.ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: models.ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Adopt compatible Skill version"})
			return err
		}
		if err := apply(); !errors.Is(err, models.ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("queued call was not drained: %v", err)
		}
		assertSkillUpgradePin(t, catalog, proposal.Call, "1.0.0", 1)
		worker := action.NewWorker(store, catalog, nil, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"channel": "provider-conversation"}, nil
		}))
		result, err := worker.ExecuteOwnLoopCall(t.Context(), proposal.Call.Scope, proposal.Call.ID, "chat-worker")
		if err != nil || result == nil || result.Call.Status != models.ActionCallStatusSucceeded {
			t.Fatalf("execute original version: %#v %v", result, err)
		}
		if err := apply(); !errors.Is(err, models.ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("live receipt was invalidated: %v", err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(models.KernelStore)
			catalog = skill.NewCatalogWithStore(store.(skill.CatalogStore))
			service = skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog)
			if err := apply(); !errors.Is(err, models.ErrSkillReferenceUpgradeBusy) {
				t.Fatalf("restart lost live receipt fence: %v", err)
			}
		}
		run, err := store.GetAgentRun(t.Context(), proposal.Call.Scope, proposal.Call.RunID)
		if err != nil {
			t.Fatal(err)
		}
		activity := runtimeactivity.NewRunActivityService(store, store)
		run, _, err = activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusRunning, Summary: "Finish task"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = activity.TransitionRun(t.Context(), run.Scope, run.ID, runtimeactivity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusCompleted, Summary: "Task finished"}); err != nil {
			t.Fatal(err)
		}
		if err := apply(); err != nil {
			t.Fatalf("drained upgrade: %v", err)
		}
		assertSkillUpgradePin(t, catalog, proposal.Call, "1.1.0", 2)
		call, err := store.GetActionCall(t.Context(), proposal.Call.Scope, proposal.Call.ID)
		if err != nil || call.SkillVersion != "1.0.0" || call.BindingRevision != 1 || call.Status != models.ActionCallStatusSucceeded {
			t.Fatalf("historical receipt mutated: %#v %v", call, err)
		}
	})
}

func skillUpgradeDrainFixture(t *testing.T, store models.KernelStore) (*skill.Catalog, *models.ActionProposalResult, *models.SkillReferenceUpgradePlan) {
	t.Helper()
	catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if err := catalog.Register(t.Context(), &skill.Definition{ID: "follow-up", Version: version, Name: "Follow up", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://skill.invalid"}, Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read messages", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	scope := models.Scope{Kind: "tenant", ID: "drain"}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "follow-up", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	portfolio := portfolio.NewService(store)
	run, err := portfolio.CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Read and follow up", Source: models.RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.ClaimNextAgentRun(t.Context(), models.AgentRunClaim{Scope: scope, WorkerID: "chat-worker", Now: time.Now().UTC().Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run == nil {
		t.Fatalf("claim run: %#v %v", run, err)
	}
	coordinator := action.NewCoordinator(store, store, catalog, action.PolicyEvaluatorFunc(func(context.Context, action.PolicyInput) (action.PolicyDecision, error) {
		return action.PolicyDecision{Disposition: action.DispositionAllow}, nil
	}), nil)
	proposal, err := coordinator.Propose(t.Context(), actiontest.LoopProposeRequest(action.ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "chat-worker", DeploymentID: "agent", BindingID: "account", BindingRevision: 1, SkillID: "follow-up", SkillVersion: "1.0.0", Action: "read", Arguments: map[string]interface{}{}, Summary: "Read messages"}, "read"))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog).Plan(t.Context(), skillbinding.PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0"})
	if err != nil || plan.ApprovalRequired {
		t.Fatalf("compatible plan: %#v %v", plan, err)
	}
	return catalog, proposal, plan
}

func assertSkillUpgradePin(t *testing.T, catalog *skill.Catalog, call *models.ActionCall, version string, revision int64) {
	t.Helper()
	binding, err := catalog.GetBinding(t.Context(), skill.ScopeReference(call.Scope), call.DeploymentID, call.BindingID)
	if err != nil || binding == nil || binding.SkillVersion != version || binding.Revision != revision {
		t.Fatalf("binding pin: %#v %v", binding, err)
	}
}

func TestSkillRuntimeUsageScopesAndRetentionAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		catalog, proposal, _ := skillUpgradeDrainFixture(t, store)
		usage := store.(models.SkillRuntimeUsageStore)
		filter := models.SkillRuntimeUsageFilter{Scope: proposal.Call.Scope, SkillID: proposal.Call.SkillID, SkillVersion: proposal.Call.SkillVersion}
		busy, err := usage.HasSkillRuntimeUsage(t.Context(), filter)
		if err != nil || !busy {
			t.Fatalf("runtime usage: %v %v", busy, err)
		}
		for _, other := range []models.SkillRuntimeUsageFilter{
			{Scope: models.Scope{Kind: "tenant", ID: "other"}, SkillID: filter.SkillID, SkillVersion: filter.SkillVersion},
			{Scope: filter.Scope, SkillID: filter.SkillID, SkillVersion: "1.1.0"},
			{Scope: filter.Scope, SkillID: filter.SkillID, SkillVersion: filter.SkillVersion, DeploymentID: "agent", BindingID: "other-account"},
		} {
			if busy, err := usage.HasSkillRuntimeUsage(t.Context(), other); err != nil || busy {
				t.Fatalf("unrelated usage matched: %#v %v %v", other, busy, err)
			}
		}
		binding, err := catalog.GetBinding(t.Context(), skill.ScopeReference(filter.Scope), "agent", "account")
		if err != nil {
			t.Fatal(err)
		}
		_, err = catalog.DisableBinding(t.Context(), skill.DisableBindingRequest{Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Disconnect account"})
		if err != nil {
			t.Fatal(err)
		}
		refs, err := usage.ListReferencedSkillRuntimeVersions(t.Context(), filter.Scope)
		if err != nil || len(refs) != 1 || refs[0].SkillVersion != "1.0.0" {
			t.Fatalf("unfinished call not retained: %#v %v", refs, err)
		}
		// The loop's inline execution retires a stale/revoked call instead of
		// issuing side effects.
		worker := action.NewWorker(store, catalog, nil, action.DispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
			t.Fatal("revoked call dispatched")
			return nil, nil
		}))
		if result, err := worker.ExecuteOwnLoopCall(t.Context(), filter.Scope, proposal.Call.ID, "chat-worker"); err != nil || result == nil || result.Call.Status != models.ActionCallStatusFailed {
			t.Fatalf("revoked loop call: %#v %v", result, err)
		}
		refs, err = usage.ListReferencedSkillRuntimeVersions(t.Context(), filter.Scope)
		if err != nil || len(refs) != 0 {
			t.Fatalf("disabled drained binding retained runtime: %#v %v", refs, err)
		}
	})
}

func TestSkillUpgradeDrainProjectsRollingVersionWritesAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		db, actions := fixture.db, fixture.table("action_calls")
		_, postgres := store.(*postgres.Store)
		scope := models.Scope{Kind: "tenant", ID: "rolling"}
		run, err := portfolio.NewService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "Rolling-version write", Source: models.RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		call := models.ActionCall{ID: "old-writer", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "account", BindingRevision: 7, SkillID: "skill", SkillVersion: "1", Status: models.ActionCallStatusReady}
		payload, _ := json.Marshal(call)
		placeholder := func(n int) string {
			if postgres {
				return fmt.Sprintf("$%d", n)
			}
			return "?"
		}
		params := make([]string, 9)
		for i := range params {
			params[i] = placeholder(i + 1)
		}
		// Earlier binaries omit every newly projected metadata column.
		_, err = db.Exec("INSERT INTO "+actions+" (id,scope_kind,scope_id,run_id,status,available_at,revision,created_at,payload) VALUES("+strings.Join(params, ",")+")", call.ID, scope.Kind, scope.ID, run.ID, call.Status, time.Now(), 1, time.Now(), string(payload))
		if err != nil {
			t.Fatal(err)
		}
		filter := models.SkillRuntimeUsageFilter{Scope: scope, SkillID: call.SkillID, SkillVersion: call.SkillVersion, DeploymentID: call.DeploymentID, BindingID: call.BindingID}
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("rolling writer escaped drain: %v %v", busy, err)
		}
		// The conditional update projection covers restored legacy rows as well.
		_, err = db.Exec("UPDATE "+actions+" SET deployment_id='',skill_id='',skill_version='',payload="+placeholder(1)+" WHERE id="+placeholder(2), string(payload), call.ID)
		if err != nil {
			t.Fatal(err)
		}
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("rolling update escaped drain: %v %v", busy, err)
		}
	})
}

func TestSkillUpgradeDrainFencesLegacyWritersAgainstExistingAuthority(t *testing.T) {
	for _, invalid := range []string{"stale_revision", "stale_version", "disabled", "different_skill"} {
		t.Run(invalid, func(t *testing.T) {
			runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
				store := fixture.store.(models.KernelStore)
				db, actions := fixture.db, fixture.table("action_calls")
				_, postgres := store.(*postgres.Store)
				scope := models.Scope{Kind: "tenant", ID: "legacy-fence"}
				run, err := portfolio.NewService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "Fence old writer", Source: models.RunSourceManual})
				if err != nil {
					t.Fatal(err)
				}
				binding := &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "skill", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}
				if err = store.(skill.CatalogStore).SaveSkillBinding(t.Context(), binding, 0); err != nil {
					t.Fatal(err)
				}
				call := models.ActionCall{ID: "old-writer", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "account", BindingRevision: 1, SkillID: "skill", SkillVersion: "1", Status: models.ActionCallStatusReady}
				switch invalid {
				case "stale_revision":
					call.BindingRevision++
				case "stale_version":
					call.SkillVersion = "old"
				case "disabled":
					binding.Disabled = true
					binding.Revision++
					call.BindingRevision = binding.Revision
					if err = store.(skill.CatalogStore).SaveSkillBinding(t.Context(), binding, 1); err != nil {
						t.Fatal(err)
					}
				case "different_skill":
					call.SkillID = "other"
				}
				payload, _ := json.Marshal(call)
				placeholder := func(n int) string {
					if postgres {
						return fmt.Sprintf("$%d", n)
					}
					return "?"
				}
				params := make([]string, 9)
				for i := range params {
					params[i] = placeholder(i + 1)
				}
				_, err = db.Exec("INSERT INTO "+actions+" (id,scope_kind,scope_id,run_id,status,available_at,revision,created_at,payload) VALUES("+strings.Join(params, ",")+")", call.ID, scope.Kind, scope.ID, run.ID, call.Status, time.Now(), 1, time.Now(), string(payload))
				if err == nil || !strings.Contains(err.Error(), "binding is unavailable or stale") {
					t.Fatalf("legacy writer accepted %s authority: %v", invalid, err)
				}
				var count int
				if err = db.QueryRow("SELECT COUNT(*) FROM "+actions+" WHERE scope_kind="+placeholder(1)+" AND scope_id="+placeholder(2), scope.Kind, scope.ID).Scan(&count); err != nil || count != 0 {
					t.Fatalf("rejected writer left a call: %d %v", count, err)
				}
			})
		})
	}
}

func cloneUpgradeTestDefinition(t *testing.T, value *skill.Definition) *skill.Definition {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result skill.Definition
	if err = json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	return &result
}

func TestSkillUpgradePostgresUsageSQLIsMetadataOnlyAndScoped(t *testing.T) {
	filter := models.SkillRuntimeUsageFilter{Scope: models.Scope{Kind: "tenant", ID: "one"}, SkillID: "skill", SkillVersion: "1", DeploymentID: "agent", BindingID: "account"}
	query, args := sqlstore.SkillRuntimeUsageQuery(filter, `"kernel"."action_calls"`, `"kernel"."agent_runs"`, true)
	if len(args) != 6 || strings.Contains(query, "payload") || strings.Contains(query, "?") {
		t.Fatalf("invalid PostgreSQL scoped probe: %s %#v", query, args)
	}
	for _, needle := range []string{"a.scope_kind=$1", "a.scope_id=$2", "a.skill_id=$3", "a.skill_version=$4", "a.deployment_id=$5", "a.binding_id IN ($6,'')", "a.run_id=r.id"} {
		if !strings.Contains(query, needle) {
			t.Fatalf("missing %s: %s", needle, query)
		}
	}
	if got := fmt.Sprint(args); !strings.Contains(got, "one skill 1 agent account") {
		t.Fatal(got)
	}
}

func TestSkillRuntimeUsageStatusesAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		scope := models.Scope{Kind: "tenant", ID: "statuses"}
		run, err := portfolio.NewService(store).CreateAgentRun(t.Context(), models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "Check status metadata", Source: models.RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		call := &models.ActionCall{ID: "call", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "account", BindingRevision: 1, SkillID: "skill", SkillVersion: "1"}
		filter := models.SkillRuntimeUsageFilter{Scope: scope, SkillID: call.SkillID, SkillVersion: call.SkillVersion, DeploymentID: call.DeploymentID, BindingID: call.BindingID}
		statuses := []models.ActionCallStatus{models.ActionCallStatusReady, models.ActionCallStatusWaitingApproval, models.ActionCallStatusRunning, models.ActionCallStatusCompensating, models.ActionCallStatusSucceeded, models.ActionCallStatusDenied, models.ActionCallStatusFailed, models.ActionCallStatusCanceled, models.ActionCallStatusCompensated}
		for _, runStatus := range []models.AgentRunStatus{models.AgentRunStatusRunning, models.AgentRunStatusPaused, models.AgentRunStatusWaitingForEvent, models.AgentRunStatusCompleted, models.AgentRunStatusFailed, models.AgentRunStatusCanceled} {
			run.Status = runStatus
			for _, status := range statuses {
				call.Status = status
				seedSkillUsageMetadata(t, fixture, run, call)
				unfinished := status == models.ActionCallStatusReady || status == models.ActionCallStatusWaitingApproval || status == models.ActionCallStatusRunning || status == models.ActionCallStatusCompensating
				want := unfinished || (status == models.ActionCallStatusSucceeded && !models.IsTerminalAgentRunStatus(runStatus))
				busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter)
				if err != nil || busy != want {
					t.Fatalf("%s action / %s run: busy=%v want=%v error=%v", status, runStatus, busy, want, err)
				}
			}
		}
		call.BindingID, call.BindingRevision = "", 0
		call.Status, run.Status = models.ActionCallStatusReady, models.AgentRunStatusRunning
		seedSkillUsageMetadata(t, fixture, run, call)
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("unbound legacy call escaped exact binding drain: %v %v", busy, err)
		}
		call.Status = models.ActionCallStatusSucceeded
		seedSkillUsageMetadata(t, fixture, run, call)
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("live unbound legacy receipt escaped binding drain: %v %v", busy, err)
		}
		run.Status = models.AgentRunStatusCompleted
		seedSkillUsageMetadata(t, fixture, run, call)
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || busy {
			t.Fatalf("completed unbound legacy receipt retained binding drain: %v %v", busy, err)
		}
	})
}

// Status-table coverage uses the store's metadata commit boundaries without
// decoding deliberately absent action outputs. Lifecycle behavior itself is
// exercised through Coordinator/Worker in the drain regression above.
func seedSkillUsageMetadata(t *testing.T, fixture runStoreContractFixture, run *models.AgentRun, call *models.ActionCall) {
	t.Helper()
	db := fixture.db
	_, postgres := fixture.store.(*postgres.Store)
	actions, runs := fixture.table("action_calls"), fixture.table("agent_runs")
	payload, _ := json.Marshal(run)
	placeholder := func(n int) string {
		if postgres {
			return fmt.Sprintf("$%d", n)
		}
		return "?"
	}
	_, err := db.Exec("UPDATE "+runs+" SET status="+placeholder(1)+",payload="+placeholder(2)+" WHERE scope_kind="+placeholder(3)+" AND scope_id="+placeholder(4)+" AND id="+placeholder(5), run.Status, string(payload), run.Scope.Kind, run.Scope.ID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec("DELETE FROM "+actions+" WHERE scope_kind="+placeholder(1)+" AND scope_id="+placeholder(2)+" AND id="+placeholder(3), call.Scope.Kind, call.Scope.ID, call.ID)
	if err != nil {
		t.Fatal(err)
	}
	params := make([]string, 14)
	for i := range params {
		params[i] = placeholder(i + 1)
	}
	_, err = db.Exec("INSERT INTO "+actions+" (id,scope_kind,scope_id,run_id,status,available_at,revision,created_at,payload,deployment_id,binding_id,binding_revision,skill_id,skill_version) VALUES("+strings.Join(params, ",")+")", call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.Status, time.Now(), 1, time.Now(), "{}", call.DeploymentID, call.BindingID, call.BindingRevision, call.SkillID, call.SkillVersion)
	if err != nil {
		t.Fatal(err)
	}
}
