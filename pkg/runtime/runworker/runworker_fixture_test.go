package runworker

// Copies of runtime's base test fixtures for the run worker tests, so they
// can move with the run worker code. The originals are in pkg/runtime: the
// stored Run reader of kernel_store_test.go, the
// method fixture of accepted_run_execution_test.go, the action call records
// of action_call_records_test.go, the store matrices of
// conversation_task_sql_test.go, run_store_contract_test.go and
// dependency_retirement_test.go, and the maintenance requests of
// skill_runtime_maintenance_test.go. Change them together.

import (
	"context"
	"database/sql"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun/agentruntest"
	"github.com/axiom-studio/openseal/pkg/runtime/store/postgres"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

// workerStoredRun reads a Run that the test knows exists.
func workerStoredRun(t testing.TB, store *sqlite.Store, scope models.Scope, id string) *models.AgentRun {
	t.Helper()
	run, err := store.GetAgentRun(context.Background(), scope, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return run
}

// workerAcceptedMethodFixture is an agent whose runbook waits an hour, then
// reads through the reader Skill.
func workerAcceptedMethodFixture(scope models.Scope) (*kernelagent.AgentDeployment, *kernelagent.AgentDefinition) {
	return &kernelagent.AgentDeployment{ID: "agent", Scope: skill.ScopeReference(scope), DefinitionID: "reader-agent", ActiveVersion: "1", RolloutStatus: kernelagent.RolloutActive},
		&kernelagent.AgentDefinition{ID: "reader-agent", Version: "1", Purpose: "Read messages carefully", SystemPrompt: "Read authorized messages", Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "reader-method", Version: "1", Name: "Read later", Entrypoints: map[string]string{"manual": "wait"},
			Steps: map[string]runbook.Step{
				"wait": {Kind: runbook.StepWait, Wait: &runbook.WaitStep{Duration: time.Hour, Next: "read"}},
				"read": {Kind: runbook.StepAction, Action: &runbook.ActionStep{SkillID: "reader", SkillVersion: "1.0.0", Action: "read", ResultPath: "/result", Next: "done"}},
				"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
			},
		}}
}

// workerActionCalls holds the action calls a test states for a feature that
// only lists them. It lists them as the stores do: oldest first, then by ID,
// filtered by scope, Run and status, one page at a time.
type workerActionCalls struct {
	calls []*models.ActionCall
}

// put stores call as given, replacing the call with the same ID.
func (r *workerActionCalls) put(call *models.ActionCall) {
	for index, current := range r.calls {
		if current.ID == call.ID {
			r.calls[index] = models.CloneActionCall(call)
			return
		}
	}
	r.calls = append(r.calls, models.CloneActionCall(call))
}

// get returns a copy of a stored call.
func (r *workerActionCalls) get(t testing.TB, id string) *models.ActionCall {
	t.Helper()
	for _, call := range r.calls {
		if call.ID == id {
			return models.CloneActionCall(call)
		}
	}
	t.Fatalf("no action call %s", id)
	return nil
}

func (r *workerActionCalls) ListActionCalls(_ context.Context, filter models.ActionFilter) ([]*models.ActionCall, error) {
	if err := filter.Scope.Validate(); err != nil {
		return nil, err
	}
	matched := make([]*models.ActionCall, 0)
	for _, call := range r.calls {
		if call.Scope == filter.Scope && (filter.RunID == "" || call.RunID == filter.RunID) &&
			(len(filter.Status) == 0 || slices.Contains(filter.Status, call.Status)) {
			matched = append(matched, models.CloneActionCall(call))
		}
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].CreatedAt.Equal(matched[j].CreatedAt) {
			return matched[i].CreatedAt.Before(matched[j].CreatedAt)
		}
		return matched[i].ID < matched[j].ID
	})
	offset, limit := max(filter.Offset, 0), filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset >= len(matched) {
		return []*models.ActionCall{}, nil
	}
	matched = matched[offset:]
	return matched[:min(limit, len(matched))], nil
}

// workerTaskStoreFixture is a conversation task store and a way to open it
// again on the same data.
type workerTaskStoreFixture struct {
	store  storetest.ConversationTaskStore
	reopen func(*testing.T) storetest.ConversationTaskStore
}

// forWorkerTaskStores runs test on a fresh SQLite file store and a fresh
// Postgres schema.
func forWorkerTaskStores(t *testing.T, test func(*testing.T, workerTaskStoreFixture)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "conversation-tasks.db")
		open := func(t *testing.T) storetest.ConversationTaskStore {
			store, err := sqlite.New(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
		test(t, workerTaskStoreFixture{store: open(t), reopen: open})
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "openseal_task_sql_")
		dsn := pgSchema.DSN
		schema := pgSchema.Name
		open := func(t *testing.T) storetest.ConversationTaskStore {
			store, err := postgres.New(t.Context(), dsn, postgres.WithSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		}
		store := open(t)
		t.Cleanup(func() {
			cleanup, err := sql.Open("postgres", dsn)
			if err != nil {
				return
			}
			defer cleanup.Close()
			_, _ = cleanup.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+pgSchema.Quoted()+` CASCADE`)
		})
		test(t, workerTaskStoreFixture{store: store, reopen: open})
	})
}

// workerRunStore is the run command and scheduling boundary the worker's
// store contracts use.
type workerRunStore interface {
	models.RunCommandStore
	models.AgentRunScheduleStore
}

// workerRunStoreFixture is a run store and a way to reopen it on the same
// data, as a restart would.
type workerRunStoreFixture struct {
	store  workerRunStore
	reopen func() workerRunStore
}

// forWorkerRunStores runs test on a SQLite file store and a Postgres schema.
func forWorkerRunStores(t *testing.T, test func(*testing.T, workerRunStoreFixture)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "contract.db")
		current, err := sqlite.New(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		test(t, workerRunStoreFixture{store: current, reopen: func() workerRunStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = sqlite.New(path)
			if err != nil {
				t.Fatal(err)
			}
			return current
		}})
	})
	t.Run("postgres", func(t *testing.T) {
		pgSchema := storetest.NewPostgresSchema(t, "run_store_contract_")
		schema, dsn := pgSchema.Name, pgSchema.DSN
		current, err := postgres.New(t.Context(), dsn, postgres.WithSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = current.Close() })
		test(t, workerRunStoreFixture{store: current, reopen: func() workerRunStore {
			if err := current.Close(); err != nil {
				t.Fatal(err)
			}
			current, err = postgres.New(t.Context(), dsn, postgres.WithSchema(schema))
			if err != nil {
				t.Fatal(err)
			}
			return current
		}})
	})
}

// forWorkerDependencyStores runs test on each backend's store, which must
// reconcile dependency groups atomically.
func forWorkerDependencyStores(t *testing.T, test func(*testing.T, agentruntest.DependencyStore)) {
	t.Helper()
	actiontest.ForEachStore(t, func(t *testing.T, kernel models.KernelStore) {
		store, ok := kernel.(agentruntest.DependencyStore)
		if !ok {
			t.Fatalf("%T does not implement atomic dependency reconciliation", kernel)
		}
		test(t, store)
	})
}

// workerMaintenanceRequest asks to upgrade the release Skill's runtime from
// 1.0.0 to 1.1.0 in a new tenant scope.
func workerMaintenanceRequest() models.SkillRuntimeMaintenanceRequest {
	return models.SkillRuntimeMaintenanceRequest{
		Scope: models.Scope{Kind: "tenant", ID: "maintenance-contract-" + uuid.NewString()}, SkillID: "release", SourceIdentity: "native::release",
		RuntimeIdentity: "native-release-server", OperationID: uuid.NewString(), FromVersion: "1.0.0", ToVersion: "1.1.0",
		TargetSourceDigest: "sha256:" + strings.Repeat("c", 64), FromSourceDigest: "sha256:" + strings.Repeat("d", 64),
		FromArtifact: "example.invalid/release@sha256:" + strings.Repeat("a", 64), ToArtifact: "example.invalid/release@sha256:" + strings.Repeat("b", 64),
		Owner: "upgrade-controller", LeaseDuration: time.Minute, Now: time.Now().UTC().Round(0),
	}
}

// workerMaintenanceCompletion completes gate at its desired version.
func workerMaintenanceCompletion(gate *models.SkillRuntimeMaintenance, now time.Time) models.SkillRuntimeMaintenanceCompletion {
	return models.SkillRuntimeMaintenanceCompletion{Scope: gate.Scope, SkillID: gate.SkillID, OperationID: gate.OperationID, Owner: gate.Owner,
		ExpectedRevision: gate.Revision, VerifiedVersion: gate.DesiredVersion, Now: now}
}
