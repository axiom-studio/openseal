package runtime

import (
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func runSkillDependencyFixture(t *testing.T, store KernelStore) (*skill.Catalog, Scope, *skill.Binding) {
	t.Helper()
	catalog := skill.NewCatalogWithStore(store.(skill.CatalogStore))
	for _, version := range []string{"1.0.0", "1.1.0"} {
		if err := catalog.Register(t.Context(), &skill.Definition{ID: "reader", Version: version, Name: "Reader",
			Source:    &skill.SourceProvenance{Identity: "native::reader", Format: "native", Digest: strings.Repeat("a", 64)},
			Transport: skill.TransportReference{Kind: "http", Endpoint: "https://skill.invalid"},
			Actions:   map[string]skill.Action{"read": {Name: "read", Description: "Read messages", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	scope := Scope{Kind: "tenant", ID: uuid.NewString()}
	binding := &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "reader", SkillVersion: "1.0.0", SourceIdentity: "native::reader", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err := catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	return catalog, scope, binding
}

func runSkillDependencyRequest(scope Scope) CreateAgentRunRequest {
	return CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Read the requested messages", Source: RunSourceSchedule,
		Context: map[string]interface{}{capabilityInvocationContextKey: map[string]interface{}{"skillId": "reader", "skillVersion": "1.0.0", "action": "read", "inputs": map[string]interface{}{"scope": map[string]interface{}{"kind": "tenant", "id": "untrusted"}}}}}
}

func runSkillDependencyMaintenanceRequest(scope Scope) SkillRuntimeMaintenanceRequest {
	return SkillRuntimeMaintenanceRequest{Scope: scope, SkillID: "reader", SourceIdentity: "native::reader", RuntimeIdentity: "installed-reader", OperationID: "upgrade-reader", FromVersion: "1.0.0", ToVersion: "1.1.0",
		TargetSourceDigest: "sha256:" + strings.Repeat("a", 64), FromSourceDigest: "sha256:" + strings.Repeat("a", 64), FromArtifact: "registry.invalid/reader@sha256:" + strings.Repeat("b", 64), ToArtifact: "registry.invalid/reader@sha256:" + strings.Repeat("c", 64),
		Owner: "controller", LeaseDuration: time.Minute, Now: time.Now().UTC(), ExpectedBindings: []SkillRuntimeMaintenanceBindingRevision{{DeploymentID: "agent", BindingID: "account", Revision: 1}}}
}

func runSkillDependencyActivity(run *AgentRun) *ActivityEvent {
	return &ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.continuation.updated", Summary: "Persist durable continuation", Actor: ActivityActor{Type: "system", ID: "worker"}, CreatedAt: time.Now().UTC()}
}

func TestRunSkillDependencyPinsQueuedInvocationBeforeFirstActionAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		catalog, scope, _ := runSkillDependencyFixture(t, store)
		run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
		if err != nil {
			t.Fatal(err)
		}
		usage := store.(SkillRuntimeUsageStore)
		filter := SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: "agent", BindingID: "account"}
		assertBusy := func() {
			t.Helper()
			if busy, err := usage.HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
				t.Fatalf("queued exact invocation lost pin before first action: %v %v", busy, err)
			}
		}
		assertBusy()
		for _, unrelated := range []SkillRuntimeUsageFilter{
			{Scope: Scope{Kind: "tenant", ID: "untrusted"}, SkillID: "reader", SkillVersion: "1.0.0"},
			{Scope: scope, SkillID: "reader", SkillVersion: "1.1.0"},
			{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: "another-agent", BindingID: "account"},
		} {
			if busy, err := usage.HasSkillRuntimeUsage(t.Context(), unrelated); err != nil || busy {
				t.Fatalf("dependency expanded foreign scope/version/deployment: %#v %v %v", unrelated, busy, err)
			}
		}
		plan, err := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog).Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0", ToSourceIdentity: "native::reader"})
		if err != nil {
			t.Fatal(err)
		}
		service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
		apply := func() error {
			_, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "controller"}, Reason: "Adopt verified runtime"})
			return err
		}
		if err := apply(); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("upgrade invalidated a queued typed invocation: %v", err)
		}
		if _, err := store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope)); !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("runtime cutover crossed queued invocation: %v", err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(KernelStore)
			usage = store.(SkillRuntimeUsageStore)
			catalog = skill.NewCatalogWithStore(store.(skill.CatalogStore))
			service = NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
			assertBusy()
		}
		// Future deterministic execution still sees its original exact version.
		runner, _ := NewCapabilityInvocationTurnRunner([]capability.ModelAction{{Name: "reader.read", SkillID: "reader", Version: "1.0.0", Action: "read", BindingID: "account", BindingRevision: 1}})
		outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: run, Turn: &AgentTurn{ID: "turn", Sequence: 1}})
		if err != nil || len(outcome.ProposedActions) != 1 {
			t.Fatalf("preserved invocation cannot continue: %#v %v", outcome, err)
		}
		original := cloneAgentRun(run)
		completed, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusCanceled, Summary: "Cancel pending work"})
		if err != nil {
			t.Fatal(err)
		}
		if busy, err := usage.HasSkillRuntimeUsage(t.Context(), filter); err != nil || busy {
			t.Fatalf("terminal Run still held runtime: %v %v", busy, err)
		}
		if err := apply(); err != nil {
			t.Fatalf("drained queued invocation prevented compatible upgrade: %v", err)
		}
		stored, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || stored.Status != completed.Status || !reflect.DeepEqual(stored.Context, original.Context) {
			t.Fatalf("historical invocation was rewritten: %#v %v", stored, err)
		}
	})
}

func TestRunSkillDependencySurvivesPausedContinuationAndDisabledBindingAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		catalog, scope, binding := runSkillDependencyFixture(t, store)
		run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
		if err != nil {
			t.Fatal(err)
		}
		updated := cloneAgentRun(run)
		updated.Context = nil
		updated.Status, updated.Revision = AgentRunStatusPaused, run.Revision+1
		if _, err := store.UpdateAgentRunWithEvent(t.Context(), updated, run.Revision, runSkillDependencyActivity(updated), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.DisableBinding(t.Context(), skill.DisableBindingRequest{Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Disconnect account"}); err != nil {
			t.Fatal(err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(KernelStore)
		}
		usage := store.(SkillRuntimeUsageStore)
		if busy, err := usage.HasSkillRuntimeUsage(t.Context(), SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0"}); err != nil || !busy {
			t.Fatalf("paused pre-call continuation lost its accepted pin: %v %v", busy, err)
		}
		refs, err := usage.ListReferencedSkillRuntimeVersions(t.Context(), scope)
		if err != nil || !reflect.DeepEqual(refs, []SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}}) {
			t.Fatalf("disabled binding removed active continuation runtime: %#v %v", refs, err)
		}
		if _, _, err := NewRunActivityService(store, store).TransitionRun(t.Context(), scope, updated.ID, RunTransitionRequest{ExpectedRevision: updated.Revision, Status: AgentRunStatusCanceled, Summary: "Cancel paused work"}); err != nil {
			t.Fatal(err)
		}
		refs, err = usage.ListReferencedSkillRuntimeVersions(t.Context(), scope)
		if err != nil || len(refs) != 0 {
			t.Fatalf("terminal continuation retained old runtime: %#v %v", refs, err)
		}
	})
}

func TestRunSkillDependencySerializesNewInvocationAndMaintenanceAcrossStores(t *testing.T) {
	for _, ordering := range []string{"run_first", "gate_first", "concurrent"} {
		t.Run(ordering, func(t *testing.T) {
			eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
				store := fixture.store.(KernelStore)
				_, scope, _ := runSkillDependencyFixture(t, store)
				var run *AgentRun
				var gate *SkillRuntimeMaintenance
				var runErr, gateErr error
				create := func() {
					run, runErr = NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
				}
				acquire := func() {
					gate, gateErr = store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope))
				}
				switch ordering {
				case "run_first":
					create()
					acquire()
				case "gate_first":
					acquire()
					create()
				default:
					start := make(chan struct{})
					var workers sync.WaitGroup
					workers.Add(2)
					go func() { defer workers.Done(); <-start; create() }()
					go func() { defer workers.Done(); <-start; acquire() }()
					close(start)
					workers.Wait()
				}
				if runErr == nil {
					if run == nil || gate != nil || !errors.Is(gateErr, ErrSkillReferenceUpgradeBusy) {
						t.Fatalf("cutover crossed accepted typed Run: run=%#v gate=%#v runErr=%v gateErr=%v", run, gate, runErr, gateErr)
					}
				} else {
					if !errors.Is(runErr, ErrSkillRuntimeMaintenance) || gateErr != nil || gate == nil || run != nil {
						t.Fatalf("new invocation/gate did not serialize: run=%#v gate=%#v runErr=%v gateErr=%v", run, gate, runErr, gateErr)
					}
					runs, err := store.ListAgentRuns(t.Context(), AgentRunFilter{Scope: scope})
					if err != nil || len(runs) != 0 {
						t.Fatalf("rejected invocation left accepted work: %#v %v", runs, err)
					}
					if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0"}); err != nil || busy {
						t.Fatalf("rejected invocation left dependency metadata: %v %v", busy, err)
					}
				}
			})
		})
	}
}

func TestRunSkillDependencySQLiteProjectionMigrationAndMetadataQueries(t *testing.T) {
	db, err := sql.Open("sqlite3", t.TempDir()+"/legacy.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE agent_runs(scope_kind TEXT,scope_id TEXT,id TEXT,status TEXT,assigned_agent_id TEXT,payload TEXT); CREATE TABLE skill_runtime_maintenance(scope_kind TEXT,scope_id TEXT,skill_id TEXT,active INTEGER)`); err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{Scope: Scope{Kind: "tenant", ID: "legacy"}, ID: "accepted", AssignedAgentID: "agent", Status: AgentRunStatusSleeping, Context: runSkillDependencyRequest(Scope{}).Context}
	payload, _ := json.Marshal(run)
	for _, status := range []string{"sleeping", "completed"} {
		if _, err := db.Exec(`INSERT INTO agent_runs VALUES('tenant','legacy',?,?,?,?)`, status, status, "agent", string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateRunSkillDependenciesSQLite(db); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_skill_dependencies`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("legacy migration retained wrong Runs: %d %v", count, err)
	}
	// A reopen must not read arbitrary historical JSON again.
	if _, err := db.Exec(`DROP TRIGGER project_run_skill_dependencies_update; UPDATE agent_runs SET payload='not-json'`); err != nil {
		t.Fatal(err)
	}
	if err := migrateRunSkillDependenciesSQLite(db); err != nil {
		t.Fatalf("reopen scanned historical payload: %v", err)
	}
	plan := sqliteSkillUsageExplain(t, db, `SELECT 1 FROM run_skill_dependencies WHERE scope_kind=? AND scope_id=? AND skill_id=? AND skill_version=? AND deployment_id IN (?,'') LIMIT 1`, "tenant", "legacy", "reader", "1.0.0", "agent")
	if !strings.Contains(plan, "idx_run_skill_dependencies_runtime") || strings.Contains(plan, "SCAN ") {
		t.Fatalf("typed dependency probe is not indexed: %s", plan)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_skill_dependencies WHERE scope_kind='tenant' AND scope_id='legacy' AND skill_id='reader' AND skill_version='1.0.0'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("projection required Run payload decode: %d %v", count, err)
	}
}

func TestRunSkillDependencySerializesNewInvocationAndBindingUpgradeAcrossStores(t *testing.T) {
	for _, ordering := range []string{"run_first", "upgrade_first", "concurrent"} {
		t.Run(ordering, func(t *testing.T) {
			eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
				store := fixture.store.(KernelStore)
				catalog, scope, _ := runSkillDependencyFixture(t, store)
				service := NewSkillReferenceUpgradeService(store.(SkillReferenceUpgradeStore), catalog)
				plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0", ToSourceIdentity: "native::reader"})
				if err != nil {
					t.Fatal(err)
				}
				var run *AgentRun
				var runErr, upgradeErr error
				create := func() {
					run, runErr = NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
				}
				upgrade := func() {
					_, upgradeErr = service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: ActivityActor{Type: "system", ID: "controller"}, Reason: "Adopt new runtime"})
				}
				switch ordering {
				case "run_first":
					create()
					upgrade()
				case "upgrade_first":
					upgrade()
					create()
				default:
					start := make(chan struct{})
					var workers sync.WaitGroup
					workers.Add(2)
					go func() { defer workers.Done(); <-start; create() }()
					go func() { defer workers.Done(); <-start; upgrade() }()
					close(start)
					workers.Wait()
				}
				if runErr == nil {
					if run == nil || !errors.Is(upgradeErr, ErrSkillReferenceUpgradeBusy) {
						t.Fatalf("binding upgrade crossed first accepted old-version invocation: run=%#v runErr=%v upgradeErr=%v", run, runErr, upgradeErr)
					}
				} else if !errors.Is(runErr, ErrRunSkillDependencyUnavailable) || upgradeErr != nil || run != nil {
					t.Fatalf("stale exact invocation was admitted after binding cutover: run=%#v runErr=%v upgradeErr=%v", run, runErr, upgradeErr)
				}
			})
		})
	}
}

func TestRunSkillDependencyRejectsNewPinUpdateDuringMaintenanceAtomicallyAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		_, scope, _ := runSkillDependencyFixture(t, store)
		request := runSkillDependencyRequest(scope)
		request.Context = nil
		run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		original, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.(SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope)); err != nil {
			t.Fatal(err)
		}
		changed := cloneAgentRun(run)
		changed.Revision++
		changed.Context = runSkillDependencyRequest(scope).Context
		if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, run.Revision, runSkillDependencyActivity(changed), nil); !errors.Is(err, ErrSkillRuntimeMaintenance) {
			t.Fatalf("new accepted pin crossed active runtime gate: %v", err)
		}
		stored, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || !reflect.DeepEqual(stored, original) {
			t.Fatalf("rejected update mutated durable Run: %#v %v", stored, err)
		}
		if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0"}); err != nil || busy {
			t.Fatalf("rejected update left dependency usage: %v %v", busy, err)
		}
	})
}

func TestRunSkillDependencyRetainsTeamGrantWithoutAssumingAssignedMemberOwnsBindingAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		_, scope, _ := runSkillDependencyFixture(t, store)
		request := runSkillDependencyRequest(scope)
		request.Owner = ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}
		if _, err := NewPortfolioService(store).CreateAgentRun(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		for _, deployment := range []string{"agent", "team"} {
			if busy, err := store.(SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: deployment, BindingID: "account"}); err != nil || !busy {
				t.Fatalf("Team-owned future action missed potential grant owner %s: %v %v", deployment, busy, err)
			}
		}
	})
}
