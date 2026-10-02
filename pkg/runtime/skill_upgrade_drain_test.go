package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillReferenceUpgradeDrainsQueuedCallsAndLiveReceiptsAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		catalog, proposal, plan := skillUpgradeDrainFixture(t, store)
		service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
		apply := func() error {
			_, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Adopt compatible Skill version"})
			return err
		}
		if err := apply(); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("queued call was not drained: %v", err)
		}
		assertSkillUpgradePin(t, catalog, proposal.Call, "1.0.0", 1)
		worker := NewActionWorker(store, catalog, nil, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			return map[string]interface{}{"channel": "provider-conversation"}, nil
		}))
		result, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "action-worker", time.Minute)
		if err != nil || result == nil || result.Call.Status != ActionCallStatusSucceeded {
			t.Fatalf("execute original version: %#v %v", result, err)
		}
		if err := apply(); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("live receipt was invalidated: %v", err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(KernelStore)
			catalog = skill.NewCatalogWithStore(store.(skill.CatalogStore))
			service = NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
			if err := apply(); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
				t.Fatalf("restart lost live receipt fence: %v", err)
			}
		}
		run, err := store.GetAgentRun(t.Context(), proposal.Call.Scope, proposal.Call.RunID)
		if err != nil {
			t.Fatal(err)
		}
		activity := NewRunActivityService(store, store)
		run, _, err = activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusRunning, Summary: "Finish task"})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err = activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusCompleted, Summary: "Task finished"}); err != nil {
			t.Fatal(err)
		}
		if err := apply(); err != nil {
			t.Fatalf("drained upgrade: %v", err)
		}
		assertSkillUpgradePin(t, catalog, proposal.Call, "1.1.0", 2)
		call, err := store.GetActionCall(t.Context(), proposal.Call.Scope, proposal.Call.ID)
		if err != nil || call.SkillVersion != "1.0.0" || call.BindingRevision != 1 || call.Status != ActionCallStatusSucceeded {
			t.Fatalf("historical receipt mutated: %#v %v", call, err)
		}
	})
}

func skillUpgradeDrainFixture(t *testing.T, store KernelStore) (*skill.Catalog, *ActionProposalResult, *SkillReferenceUpgradePlan) {
	t.Helper()
	catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if err := catalog.Register(t.Context(), &skill.Definition{ID: "follow-up", Version: version, Name: "Follow up", Transport: skill.TransportReference{Kind: "http", Endpoint: "https://skill.invalid"}, Actions: map[string]skill.Action{"read": {Name: "read", Description: "Read messages", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	scope := Scope{Kind: "tenant", ID: "drain"}
	if err := catalog.Bind(t.Context(), &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "follow-up", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	portfolio := NewPortfolioService(store)
	run, err := portfolio.CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Read and follow up", Source: RunSourceManual})
	if err != nil {
		t.Fatal(err)
	}
	run, err = store.ClaimNextAgentRun(t.Context(), AgentRunClaim{Scope: scope, WorkerID: "chat-worker", Now: time.Now().UTC().Add(time.Second), LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || run == nil {
		t.Fatalf("claim run: %#v %v", run, err)
	}
	coordinator := NewActionCoordinator(store, store, catalog, ActionPolicyEvaluatorFunc(func(context.Context, ActionPolicyInput) (ActionPolicyDecision, error) {
		return ActionPolicyDecision{Disposition: ActionDispositionAllow}, nil
	}))
	proposal, err := coordinator.Propose(t.Context(), ProposeActionRequest{Scope: scope, RunID: run.ID, WorkerID: "chat-worker", DeploymentID: "agent", BindingID: "account", BindingRevision: 1, SkillID: "follow-up", SkillVersion: "1.0.0", Action: "read", Arguments: map[string]interface{}{}, Summary: "Read messages"})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog).Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0"})
	if err != nil || plan.ApprovalRequired {
		t.Fatalf("compatible plan: %#v %v", plan, err)
	}
	return catalog, proposal, plan
}

func assertSkillUpgradePin(t *testing.T, catalog *skill.Catalog, call *ActionCall, version string, revision int64) {
	t.Helper()
	binding, err := catalog.GetBinding(t.Context(), skill.ScopeReference(call.Scope), call.DeploymentID, call.BindingID)
	if err != nil || binding == nil || binding.SkillVersion != version || binding.Revision != revision {
		t.Fatalf("binding pin: %#v %v", binding, err)
	}
}

func TestSkillRuntimeUsageScopesAndRetentionAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		catalog, proposal, _ := skillUpgradeDrainFixture(t, store)
		usage := store.(SkillRuntimeUsageStore)
		filter := SkillRuntimeUsageFilter{Scope: proposal.Call.Scope, SkillID: proposal.Call.SkillID, SkillVersion: proposal.Call.SkillVersion}
		busy, err := usage.HasSkillRuntimeUsage(t.Context(), filter)
		if err != nil || !busy {
			t.Fatalf("runtime usage: %v %v", busy, err)
		}
		for _, other := range []SkillRuntimeUsageFilter{
			{Scope: Scope{Kind: "tenant", ID: "other"}, SkillID: filter.SkillID, SkillVersion: filter.SkillVersion},
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
		// The worker retires a stale/revoked call instead of issuing side effects.
		worker := NewActionWorker(store, catalog, nil, ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			t.Fatal("revoked call dispatched")
			return nil, nil
		}))
		if _, err := worker.RunOnce(t.Context(), filter.Scope, "retire-worker", time.Minute); err != nil {
			t.Fatal(err)
		}
		refs, err = usage.ListReferencedSkillRuntimeVersions(t.Context(), filter.Scope)
		if err != nil || len(refs) != 0 {
			t.Fatalf("disabled drained binding retained runtime: %#v %v", refs, err)
		}
	})
}

func TestSkillUpgradeDrainQueriesUseIndexedMetadataWithoutPayloads(t *testing.T) {
	store, err := NewSQLiteStore(t.TempDir() + "/metadata.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Malformed action payloads make an accidental decode fail immediately.
	_, err = store.db.Exec(`INSERT INTO action_calls(id,scope_kind,scope_id,run_id,status,available_at,revision,created_at,payload,deployment_id,binding_id,binding_revision,skill_id,skill_version) VALUES ('active','tenant','metadata','run','ready',CURRENT_TIMESTAMP,1,CURRENT_TIMESTAMP,'not-json','agent','account',1,'skill','1')`)
	if err != nil {
		t.Fatal(err)
	}
	filter := SkillRuntimeUsageFilter{Scope: Scope{Kind: "tenant", ID: "metadata"}, SkillID: "skill", SkillVersion: "1", DeploymentID: "agent", BindingID: "account"}
	if busy, err := store.HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
		t.Fatalf("metadata query decoded payload: %v %v", busy, err)
	}
	refs, err := store.ListReferencedSkillRuntimeVersions(t.Context(), filter.Scope)
	if err != nil || len(refs) != 1 || refs[0].SourceIdentity != "" {
		t.Fatalf("metadata retention: %#v %v", refs, err)
	}
	for _, selected := range []SkillRuntimeUsageFilter{filter, {Scope: filter.Scope, SkillID: filter.SkillID, SkillVersion: filter.SkillVersion}} {
		query, args := skillRuntimeUsageQuery(selected, "action_calls", "agent_runs", false)
		plan := sqliteSkillUsageExplain(t, store.db, query, args...)
		activeIndex := "idx_action_calls_skill_runtime_active"
		if selected.BindingID != "" {
			activeIndex = "idx_action_calls_skill_binding_active"
		}
		for _, index := range []string{activeIndex, "idx_agent_runs_skill_runtime_active", "idx_action_calls_skill_run_receipt"} {
			if !strings.Contains(plan, index) {
				t.Fatalf("missing %s: %s", index, plan)
			}
		}
		if strings.Contains(plan, "SCAN a") || strings.Contains(plan, "SCAN r") {
			t.Fatalf("unbounded action/run scan: %s", plan)
		}
	}
	plan := sqliteSkillUsageExplain(t, store.db, listReferencedSkillRuntimeVersionsQuery("skill_bindings", "action_calls", "agent_runs", false), filter.Scope.Kind, filter.Scope.ID)
	for _, index := range []string{"idx_agent_runs_skill_runtime_active", "idx_action_calls_skill_run_receipt"} {
		if !strings.Contains(plan, index) {
			t.Fatalf("retention missing %s: %s", index, plan)
		}
	}
	if !strings.Contains(plan, "idx_action_calls_skill_runtime_active") && !strings.Contains(plan, "idx_action_calls_skill_binding_active") {
		t.Fatalf("retention does not use a partial unfinished action index: %s", plan)
	}
	if strings.Contains(plan, "SCAN a") || strings.Contains(plan, "SCAN r") {
		t.Fatalf("retention scans terminal history: %s", plan)
	}
}

func sqliteSkillUsageExplain(t *testing.T, db *sql.DB, query string, args ...interface{}) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

func TestSkillUpgradeDrainMigrationProjectsLegacyMetadata(t *testing.T) {
	db, err := sql.Open("sqlite3", t.TempDir()+"/legacy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE action_calls(id TEXT,scope_kind TEXT,scope_id TEXT,run_id TEXT,status TEXT,payload TEXT);CREATE TABLE agent_runs(id TEXT,scope_kind TEXT,scope_id TEXT,status TEXT);CREATE TABLE skill_bindings(scope_kind TEXT,scope_id TEXT,deployment_id TEXT,id TEXT,revision INTEGER,skill_id TEXT,skill_version TEXT,payload TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	call := ActionCall{ID: "old", Scope: Scope{Kind: "tenant", ID: "one"}, RunID: "run", DeploymentID: "agent", BindingID: "account", BindingRevision: 7, SkillID: "skill", SkillVersion: "2", Status: ActionCallStatusReady}
	payload, _ := json.Marshal(call)
	if _, err = db.Exec(`INSERT INTO action_calls VALUES(?,?,?,?,?,?)`, call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.Status, string(payload)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = migrateSkillUpgradeDrainSQLite(db); err != nil {
			t.Fatal(err)
		}
	}
	var deployment, binding, id, version string
	var revision int64
	if err = db.QueryRow(`SELECT deployment_id,binding_id,binding_revision,skill_id,skill_version FROM action_calls`).Scan(&deployment, &binding, &revision, &id, &version); err != nil {
		t.Fatal(err)
	}
	if deployment != call.DeploymentID || binding != call.BindingID || revision != 7 || id != call.SkillID || version != call.SkillVersion {
		t.Fatalf("legacy projection: %s %s %d %s %s", deployment, binding, revision, id, version)
	}
	call.ID = "rolling-writer"
	payload, _ = json.Marshal(call)
	if _, err = db.Exec(`INSERT INTO action_calls(id,scope_kind,scope_id,run_id,status,payload) VALUES(?,?,?,?,?,?)`, call.ID, call.Scope.Kind, call.Scope.ID, call.RunID, call.Status, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow(`SELECT deployment_id,binding_id,binding_revision,skill_id,skill_version FROM action_calls WHERE id=?`, call.ID).Scan(&deployment, &binding, &revision, &id, &version); err != nil {
		t.Fatal(err)
	}
	if deployment != call.DeploymentID || binding != call.BindingID || revision != 7 || id != call.SkillID || version != call.SkillVersion {
		t.Fatalf("rolling writer metadata was missing: %s %s %d %s %s", deployment, binding, revision, id, version)
	}
}

func TestSkillUpgradeDrainProjectsRollingVersionWritesAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		var db *sql.DB
		actions := "action_calls"
		postgres := false
		switch value := store.(type) {
		case *MemoryStore:
			t.Skip("MemoryStore is process local and cannot share a rolling-version writer")
		case *SQLiteStore:
			db = value.db
		case *PostgresStore:
			db = value.db
			postgres = true
			actions = value.table(actions)
		}
		scope := Scope{Kind: "tenant", ID: "rolling"}
		run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Rolling-version write", Source: RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		call := ActionCall{ID: "old-writer", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "account", BindingRevision: 7, SkillID: "skill", SkillVersion: "1", Status: ActionCallStatusReady}
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
		filter := SkillRuntimeUsageFilter{Scope: scope, SkillID: call.SkillID, SkillVersion: call.SkillVersion, DeploymentID: call.DeploymentID, BindingID: call.BindingID}
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("rolling writer escaped drain: %v %v", busy, err)
		}
		// The conditional update projection covers restored legacy rows as well.
		_, err = db.Exec("UPDATE "+actions+" SET deployment_id='',skill_id='',skill_version='',payload="+placeholder(1)+" WHERE id="+placeholder(2), string(payload), call.ID)
		if err != nil {
			t.Fatal(err)
		}
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("rolling update escaped drain: %v %v", busy, err)
		}
	})
}

func TestSkillUpgradeDrainFencesLegacyWritersAgainstExistingAuthority(t *testing.T) {
	for _, invalid := range []string{"stale_revision", "stale_version", "disabled", "different_skill"} {
		t.Run(invalid, func(t *testing.T) {
			eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
				store := fixture.store.(KernelStore)
				var db *sql.DB
				actions := "action_calls"
				postgres := false
				switch value := store.(type) {
				case *MemoryStore:
					t.Skip("MemoryStore cannot share rolling-version writers")
				case *SQLiteStore:
					db = value.db
				case *PostgresStore:
					db = value.db
					postgres = true
					actions = value.table(actions)
				}
				scope := Scope{Kind: "tenant", ID: "legacy-fence"}
				run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Fence old writer", Source: RunSourceManual})
				if err != nil {
					t.Fatal(err)
				}
				binding := &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "skill", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}
				if err = store.(skill.CatalogStore).SaveSkillBinding(t.Context(), binding, 0); err != nil {
					t.Fatal(err)
				}
				call := ActionCall{ID: "old-writer", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "account", BindingRevision: 1, SkillID: "skill", SkillVersion: "1", Status: ActionCallStatusReady}
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

func TestSkillUpgradeCompatibilityIncludesExecutableAdapterContracts(t *testing.T) {
	base := &skill.Definition{ID: "skill", Version: "1", Actions: map[string]skill.Action{"read": {Name: "read", InputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead}}, ConversationAdapters: map[string]skill.ConversationAdapter{"conversation": {Name: "Before", Provider: "provider", SubjectEvidence: []capability.ConversationSubjectEvidence{{Action: "read", SubjectPath: "channel"}}}}, CallbackAdapters: map[string]skill.CallbackAdapter{"callback": {Name: "Before", Provider: "provider", EventTypes: []string{"received"}}}}
	binding := &skill.Binding{AllowedActions: []string{"read"}, EnabledConversationAdapters: []string{"conversation"}, EnabledCallbackAdapters: []string{"callback"}}
	for _, test := range []struct {
		name   string
		change func(*skill.Definition)
		code   string
	}{
		{"operation policy", func(d *skill.Definition) {
			a := d.Actions["read"]
			a.ExternalOperationPolicy = skill.ExternalOperationRequired
			d.Actions["read"] = a
		}, "action_contract_changed"},
		{"action transport", func(d *skill.Definition) {
			a := d.Actions["read"]
			a.Transport = &skill.TransportReference{Kind: "http", Endpoint: "changed"}
			d.Actions["read"] = a
		}, "action_contract_changed"},
		{"subject receipt", func(d *skill.Definition) {
			a := d.ConversationAdapters["conversation"]
			a.SubjectEvidence = []capability.ConversationSubjectEvidence{{Action: "read", SubjectPath: "other"}}
			d.ConversationAdapters["conversation"] = a
		}, "conversation_adapter_contract_changed"},
		{"callback acquisition", func(d *skill.Definition) {
			a := d.CallbackAdapters["callback"]
			a.EventTypes = []string{"other"}
			d.CallbackAdapters["callback"] = a
		}, "callback_adapter_contract_changed"},
		{"descriptions only", func(d *skill.Definition) {
			a := d.ConversationAdapters["conversation"]
			a.Name = "After"
			a.Description = "Revised copy"
			d.ConversationAdapters["conversation"] = a
			a2 := d.CallbackAdapters["callback"]
			a2.Name = "After"
			d.CallbackAdapters["callback"] = a2
		}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := cloneUpgradeTestDefinition(t, base)
			test.change(target)
			findings := append(compareUpgradeContracts(base, target, binding.AllowedActions, nil), compareUpgradeAdapterContracts(base, target, binding)...)
			if test.code == "" {
				if len(findings) != 0 {
					t.Fatalf("copy triggered approval: %#v", findings)
				}
				return
			}
			found := false
			for _, finding := range findings {
				found = found || finding.Code == test.code
			}
			if !found {
				t.Fatalf("missing %s: %#v", test.code, findings)
			}
		})
	}
	unchanged := cloneUpgradeTestDefinition(t, base)
	if !reflect.DeepEqual(base, unchanged) {
		t.Fatal("comparison mutated original")
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
	filter := SkillRuntimeUsageFilter{Scope: Scope{Kind: "tenant", ID: "one"}, SkillID: "skill", SkillVersion: "1", DeploymentID: "agent", BindingID: "account"}
	query, args := skillRuntimeUsageQuery(filter, `"kernel"."action_calls"`, `"kernel"."agent_runs"`, true)
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
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		scope := Scope{Kind: "tenant", ID: "statuses"}
		run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Check status metadata", Source: RunSourceManual})
		if err != nil {
			t.Fatal(err)
		}
		call := &ActionCall{ID: "call", Scope: scope, RunID: run.ID, DeploymentID: "agent", BindingID: "account", BindingRevision: 1, SkillID: "skill", SkillVersion: "1"}
		filter := SkillRuntimeUsageFilter{Scope: scope, SkillID: call.SkillID, SkillVersion: call.SkillVersion, DeploymentID: call.DeploymentID, BindingID: call.BindingID}
		statuses := []ActionCallStatus{ActionCallStatusReady, ActionCallStatusWaitingApproval, ActionCallStatusRunning, ActionCallStatusCompensating, ActionCallStatusSucceeded, ActionCallStatusDenied, ActionCallStatusFailed, ActionCallStatusCanceled, ActionCallStatusCompensated}
		for _, runStatus := range []AgentRunStatus{AgentRunStatusRunning, AgentRunStatusPaused, AgentRunStatusWaitingForEvent, AgentRunStatusCompleted, AgentRunStatusFailed, AgentRunStatusCanceled} {
			run.Status = runStatus
			for _, status := range statuses {
				call.Status = status
				seedSkillUsageMetadata(t, store, run, call)
				want := unfinishedSkillActionStatus(status) || (status == ActionCallStatusSucceeded && !isTerminalAgentRunStatus(runStatus))
				busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter)
				if err != nil || busy != want {
					t.Fatalf("%s action / %s run: busy=%v want=%v error=%v", status, runStatus, busy, want, err)
				}
			}
		}
		call.BindingID, call.BindingRevision = "", 0
		call.Status, run.Status = ActionCallStatusReady, AgentRunStatusRunning
		seedSkillUsageMetadata(t, store, run, call)
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("unbound legacy call escaped exact binding drain: %v %v", busy, err)
		}
		call.Status = ActionCallStatusSucceeded
		seedSkillUsageMetadata(t, store, run, call)
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
			t.Fatalf("live unbound legacy receipt escaped binding drain: %v %v", busy, err)
		}
		run.Status = AgentRunStatusCompleted
		seedSkillUsageMetadata(t, store, run, call)
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), filter); err != nil || busy {
			t.Fatalf("completed unbound legacy receipt retained binding drain: %v %v", busy, err)
		}
	})
}

// Status-table coverage uses the store's metadata commit boundaries without
// decoding deliberately absent action outputs. Lifecycle behavior itself is
// exercised through Coordinator/Worker in the drain regression above.
func seedSkillUsageMetadata(t *testing.T, store KernelStore, run *AgentRun, call *ActionCall) {
	t.Helper()
	if memory, ok := store.(*MemoryStore); ok {
		memory.mu.Lock()
		memory.saveMemoryAgentRunLocked(portfolioKey(run.Scope, run.ID), run)
		memory.saveMemoryActionCallLocked(portfolioKey(call.Scope, call.ID), call)
		memory.mu.Unlock()
		return
	}
	var db *sql.DB
	postgres := false
	actions, runs := "action_calls", "agent_runs"
	switch value := store.(type) {
	case *SQLiteStore:
		db = value.db
	case *PostgresStore:
		db = value.db
		postgres = true
		actions, runs = value.table(actions), value.table(runs)
	default:
		t.Fatal("unsupported usage store")
	}
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
