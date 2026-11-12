package runtime

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func runSkillDependencyFixture(t *testing.T, store models.KernelStore) (*skill.Catalog, models.Scope, *skill.Binding) {
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
	scope := models.Scope{Kind: "tenant", ID: uuid.NewString()}
	binding := &skill.Binding{ID: "account", Scope: skill.ScopeReference(scope), DeploymentID: "agent", SkillID: "reader", SkillVersion: "1.0.0", SourceIdentity: "native::reader", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1}
	if err := catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	return catalog, scope, binding
}

func runSkillDependencyRequest(scope models.Scope) models.CreateAgentRunRequest {
	return models.CreateAgentRunRequest{Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", Goal: "Read the requested messages", Source: models.RunSourceSchedule,
		Context: map[string]interface{}{models.CapabilityInvocationContextKey: map[string]interface{}{"skillId": "reader", "skillVersion": "1.0.0", "action": "read", "inputs": map[string]interface{}{"scope": map[string]interface{}{"kind": "tenant", "id": "untrusted"}}}}}
}

func runSkillDependencyMaintenanceRequest(scope models.Scope) models.SkillRuntimeMaintenanceRequest {
	return models.SkillRuntimeMaintenanceRequest{Scope: scope, SkillID: "reader", SourceIdentity: "native::reader", RuntimeIdentity: "installed-reader", OperationID: "upgrade-reader", FromVersion: "1.0.0", ToVersion: "1.1.0",
		TargetSourceDigest: "sha256:" + strings.Repeat("a", 64), FromSourceDigest: "sha256:" + strings.Repeat("a", 64), FromArtifact: "registry.invalid/reader@sha256:" + strings.Repeat("b", 64), ToArtifact: "registry.invalid/reader@sha256:" + strings.Repeat("c", 64),
		Owner: "controller", LeaseDuration: time.Minute, Now: time.Now().UTC(), ExpectedBindings: []models.SkillRuntimeMaintenanceBindingRevision{{DeploymentID: "agent", BindingID: "account", Revision: 1}}}
}

func runSkillDependencyActivity(run *models.AgentRun) *models.ActivityEvent {
	return &models.ActivityEvent{ID: uuid.NewString(), Scope: run.Scope, RunID: run.ID, EventType: "run.continuation.updated", Summary: "Persist durable continuation", Actor: models.ActivityActor{Type: "system", ID: "worker"}, CreatedAt: time.Now().UTC()}
}

func TestRunSkillDependencyPinsQueuedInvocationBeforeFirstActionAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		catalog, scope, _ := runSkillDependencyFixture(t, store)
		run, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
		if err != nil {
			t.Fatal(err)
		}
		usage := store.(models.SkillRuntimeUsageStore)
		filter := models.SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: "agent", BindingID: "account"}
		assertBusy := func() {
			t.Helper()
			if busy, err := usage.HasSkillRuntimeUsage(t.Context(), filter); err != nil || !busy {
				t.Fatalf("queued exact invocation lost pin before first action: %v %v", busy, err)
			}
		}
		assertBusy()
		for _, unrelated := range []models.SkillRuntimeUsageFilter{
			{Scope: models.Scope{Kind: "tenant", ID: "untrusted"}, SkillID: "reader", SkillVersion: "1.0.0"},
			{Scope: scope, SkillID: "reader", SkillVersion: "1.1.0"},
			{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: "another-agent", BindingID: "account"},
		} {
			if busy, err := usage.HasSkillRuntimeUsage(t.Context(), unrelated); err != nil || busy {
				t.Fatalf("dependency expanded foreign scope/version/deployment: %#v %v %v", unrelated, busy, err)
			}
		}
		plan, err := skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog).Plan(t.Context(), skillbinding.PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0", ToSourceIdentity: "native::reader"})
		if err != nil {
			t.Fatal(err)
		}
		service := skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog)
		apply := func() error {
			_, err := service.Apply(t.Context(), skillbinding.ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: models.ActivityActor{Type: "system", ID: "controller"}, Reason: "Adopt verified runtime"})
			return err
		}
		if err := apply(); !errors.Is(err, models.ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("upgrade invalidated a queued typed invocation: %v", err)
		}
		if _, err := store.(models.SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope)); !errors.Is(err, models.ErrSkillReferenceUpgradeBusy) {
			t.Fatalf("runtime cutover crossed queued invocation: %v", err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(models.KernelStore)
			usage = store.(models.SkillRuntimeUsageStore)
			catalog = skill.NewCatalogWithStore(store.(skill.CatalogStore))
			service = skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog)
			assertBusy()
		}
		// Future deterministic execution still sees its original exact version.
		producer, _ := modelstep.NewCapabilityInvocationStepProducer([]capability.ModelAction{{Name: "reader.read", SkillID: "reader", Version: "1.0.0", Action: "read", BindingID: "account", BindingRevision: 1}}, store)
		outcome, err := producer.Produce(t.Context(), agentrun.StepInput{Run: run, Step: &models.AgentTurn{ID: "step", Sequence: 1}})
		if err != nil || len(outcome.ToolCalls) != 1 {
			t.Fatalf("preserved invocation cannot continue: %#v %v", outcome, err)
		}
		original := models.CloneAgentRun(run)
		completed, _, err := activity.NewRunActivityService(store, store).TransitionRun(t.Context(), scope, run.ID, activity.RunTransitionRequest{ExpectedRevision: run.Revision, Status: models.AgentRunStatusCanceled, Summary: "Cancel pending work"})
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
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		catalog, scope, binding := runSkillDependencyFixture(t, store)
		run, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
		if err != nil {
			t.Fatal(err)
		}
		updated := models.CloneAgentRun(run)
		updated.Context = nil
		updated.Status, updated.Revision = models.AgentRunStatusPaused, run.Revision+1
		if _, err := store.UpdateAgentRunWithEvent(t.Context(), updated, run.Revision, runSkillDependencyActivity(updated), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.DisableBinding(t.Context(), skill.DisableBindingRequest{Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: binding.Revision, Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Disconnect account"}); err != nil {
			t.Fatal(err)
		}
		if fixture.reopen != nil {
			store = fixture.reopen().(models.KernelStore)
		}
		usage := store.(models.SkillRuntimeUsageStore)
		if busy, err := usage.HasSkillRuntimeUsage(t.Context(), models.SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0"}); err != nil || !busy {
			t.Fatalf("paused pre-call continuation lost its accepted pin: %v %v", busy, err)
		}
		refs, err := usage.ListReferencedSkillRuntimeVersions(t.Context(), scope)
		if err != nil || !reflect.DeepEqual(refs, []models.SkillRuntimeReference{{SkillID: "reader", SkillVersion: "1.0.0"}}) {
			t.Fatalf("disabled binding removed active continuation runtime: %#v %v", refs, err)
		}
		if _, _, err := activity.NewRunActivityService(store, store).TransitionRun(t.Context(), scope, updated.ID, activity.RunTransitionRequest{ExpectedRevision: updated.Revision, Status: models.AgentRunStatusCanceled, Summary: "Cancel paused work"}); err != nil {
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
			runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
				store := fixture.store.(models.KernelStore)
				_, scope, _ := runSkillDependencyFixture(t, store)
				var run *models.AgentRun
				var gate *models.SkillRuntimeMaintenance
				var runErr, gateErr error
				create := func() {
					run, runErr = portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
				}
				acquire := func() {
					gate, gateErr = store.(models.SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope))
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
					if run == nil || gate != nil || !errors.Is(gateErr, models.ErrSkillReferenceUpgradeBusy) {
						t.Fatalf("cutover crossed accepted typed Run: run=%#v gate=%#v runErr=%v gateErr=%v", run, gate, runErr, gateErr)
					}
				} else {
					if !errors.Is(runErr, models.ErrSkillRuntimeMaintenance) || gateErr != nil || gate == nil || run != nil {
						t.Fatalf("new invocation/gate did not serialize: run=%#v gate=%#v runErr=%v gateErr=%v", run, gate, runErr, gateErr)
					}
					runs, err := store.ListAgentRuns(t.Context(), models.AgentRunFilter{Scope: scope})
					if err != nil || len(runs) != 0 {
						t.Fatalf("rejected invocation left accepted work: %#v %v", runs, err)
					}
					if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), models.SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0"}); err != nil || busy {
						t.Fatalf("rejected invocation left dependency metadata: %v %v", busy, err)
					}
				}
			})
		})
	}
}

func TestRunSkillDependencySerializesNewInvocationAndBindingUpgradeAcrossStores(t *testing.T) {
	for _, ordering := range []string{"run_first", "upgrade_first", "concurrent"} {
		t.Run(ordering, func(t *testing.T) {
			runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
				store := fixture.store.(models.KernelStore)
				catalog, scope, _ := runSkillDependencyFixture(t, store)
				service := skillbinding.NewSkillReferenceUpgradeService(store.(models.SkillReferenceUpgradeStore), catalog)
				plan, err := service.Plan(t.Context(), skillbinding.PlanSkillReferenceUpgradeRequest{Scope: scope, DeploymentID: "agent", BindingID: "account", ToVersion: "1.1.0", ToSourceIdentity: "native::reader"})
				if err != nil {
					t.Fatal(err)
				}
				var run *models.AgentRun
				var runErr, upgradeErr error
				create := func() {
					run, runErr = portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), runSkillDependencyRequest(scope))
				}
				upgrade := func() {
					_, upgradeErr = service.Apply(t.Context(), skillbinding.ApplySkillReferenceUpgradeRequest{Plan: plan, Actor: models.ActivityActor{Type: "system", ID: "controller"}, Reason: "Adopt new runtime"})
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
					if run == nil || !errors.Is(upgradeErr, models.ErrSkillReferenceUpgradeBusy) {
						t.Fatalf("binding upgrade crossed first accepted old-version invocation: run=%#v runErr=%v upgradeErr=%v", run, runErr, upgradeErr)
					}
				} else if !errors.Is(runErr, models.ErrRunSkillDependencyUnavailable) || upgradeErr != nil || run != nil {
					t.Fatalf("stale exact invocation was admitted after binding cutover: run=%#v runErr=%v upgradeErr=%v", run, runErr, upgradeErr)
				}
			})
		})
	}
}

func TestRunSkillDependencyRejectsNewPinUpdateDuringMaintenanceAtomicallyAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		_, scope, _ := runSkillDependencyFixture(t, store)
		request := runSkillDependencyRequest(scope)
		request.Context = nil
		run, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		original, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.(models.SkillRuntimeMaintenanceStore).AcquireSkillRuntimeMaintenance(t.Context(), runSkillDependencyMaintenanceRequest(scope)); err != nil {
			t.Fatal(err)
		}
		changed := models.CloneAgentRun(run)
		changed.Revision++
		changed.Context = runSkillDependencyRequest(scope).Context
		if _, err := store.UpdateAgentRunWithEvent(t.Context(), changed, run.Revision, runSkillDependencyActivity(changed), nil); !errors.Is(err, models.ErrSkillRuntimeMaintenance) {
			t.Fatalf("new accepted pin crossed active runtime gate: %v", err)
		}
		stored, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || !reflect.DeepEqual(stored, original) {
			t.Fatalf("rejected update mutated durable Run: %#v %v", stored, err)
		}
		if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), models.SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0"}); err != nil || busy {
			t.Fatalf("rejected update left dependency usage: %v %v", busy, err)
		}
	})
}

func TestRunSkillDependencyRetainsTeamGrantWithoutAssumingAssignedMemberOwnsBindingAcrossStores(t *testing.T) {
	runStoreContractFixtures(t, func(t *testing.T, fixture runStoreContractFixture) {
		store := fixture.store.(models.KernelStore)
		_, scope, _ := runSkillDependencyFixture(t, store)
		request := runSkillDependencyRequest(scope)
		request.Owner = models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}
		if _, err := portfolio.NewPortfolioService(store).CreateAgentRun(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		for _, deployment := range []string{"agent", "team"} {
			if busy, err := store.(models.SkillRuntimeUsageStore).HasSkillRuntimeUsage(t.Context(), models.SkillRuntimeUsageFilter{Scope: scope, SkillID: "reader", SkillVersion: "1.0.0", DeploymentID: deployment, BindingID: "account"}); err != nil || !busy {
				t.Fatalf("Team-owned future action missed potential grant owner %s: %v %v", deployment, busy, err)
			}
		}
	})
}
