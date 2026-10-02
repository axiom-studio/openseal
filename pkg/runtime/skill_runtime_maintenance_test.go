package runtime

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSkillRuntimeMaintenanceSameIntentOwnerTakeoverAndStaleCompletion(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(SkillRuntimeMaintenanceStore)
		request := skillRuntimeMaintenanceEmptyRequest()
		original, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil || original == nil || !original.Active || original.DesiredVersion != request.ToVersion || original.Phase != "upgrading" {
			t.Fatalf("initial acquisition = %#v, %v", original, err)
		}
		contender := request
		contender.Owner = "replacement-controller"
		contender.Now = request.Now.Add(time.Second)
		if gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), contender); gate != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceConflict) {
			t.Fatalf("live owner was replaced: %#v, %v", gate, err)
		}
		assertSkillRuntimeMaintenanceRecord(t, store, original)

		renew := request
		renew.Now = request.Now.Add(2 * time.Second)
		renewed, err := store.AcquireSkillRuntimeMaintenance(t.Context(), renew)
		if err != nil || renewed == nil || renewed.Owner != original.Owner || renewed.Revision != original.Revision+1 || renewed.OperationID != original.OperationID || !renewed.LeaseExpiresAt.Equal(renew.Now.Add(renew.LeaseDuration)) {
			t.Fatalf("same owner renewal = %#v, %v", renewed, err)
		}
		contender.Now = renewed.LeaseExpiresAt.Add(time.Second)
		takenOver, err := store.AcquireSkillRuntimeMaintenance(t.Context(), contender)
		if err != nil || takenOver == nil || takenOver.Owner != contender.Owner || takenOver.Revision != renewed.Revision+1 || !takenOver.Active || takenOver.CreatedAt != original.CreatedAt {
			t.Fatalf("expired-owner takeover lost the existing intent: %#v, %v", takenOver, err)
		}
		completion := skillRuntimeMaintenanceCompletion(takenOver, contender.Now.Add(time.Second))
		for _, stale := range []struct {
			name   string
			change func(*SkillRuntimeMaintenanceCompletion)
		}{
			{"old_owner", func(c *SkillRuntimeMaintenanceCompletion) { c.Owner = original.Owner }},
			{"old_revision", func(c *SkillRuntimeMaintenanceCompletion) { c.ExpectedRevision = renewed.Revision }},
			{"wrong_operation", func(c *SkillRuntimeMaintenanceCompletion) { c.OperationID = uuid.NewString() }},
			{"expired_owner", func(c *SkillRuntimeMaintenanceCompletion) { c.Now = takenOver.LeaseExpiresAt }},
		} {
			t.Run(stale.name, func(t *testing.T) {
				invalid := completion
				stale.change(&invalid)
				if gate, err := store.CompleteSkillRuntimeMaintenance(t.Context(), invalid); gate != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceConflict) {
					t.Fatalf("stale completion released the gate: %#v, %v", gate, err)
				}
				if gate, err := store.BeginSkillRuntimeMaintenanceRollback(t.Context(), invalid); gate != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceConflict) {
					t.Fatalf("stale controller changed rollback intent: %#v, %v", gate, err)
				}
				assertSkillRuntimeMaintenanceRecord(t, store, takenOver)
			})
		}
		completed, err := store.CompleteSkillRuntimeMaintenance(t.Context(), completion)
		if err != nil || completed == nil || completed.Active || completed.VerifiedVersion != request.ToVersion || completed.Revision != takenOver.Revision+1 {
			t.Fatalf("current owner could not complete: %#v, %v", completed, err)
		}
		// Replaying the same immutable maintenance operation must preserve its
		// verified terminal receipt rather than reopen the runtime.
		replayed, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil || !reflect.DeepEqual(completed, replayed) {
			t.Fatalf("completed operation reopened on replay: %#v, %v", replayed, err)
		}
	})
}

func TestSkillRuntimeMaintenanceRejectsImmutableIntentDrift(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(SkillRuntimeMaintenanceStore)
		request := skillRuntimeMaintenanceEmptyRequest()
		original, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		for _, drift := range []struct {
			name   string
			change func(*SkillRuntimeMaintenanceRequest)
		}{
			{"source", func(r *SkillRuntimeMaintenanceRequest) { r.SourceIdentity = "native::another-publisher" }},
			{"runtime", func(r *SkillRuntimeMaintenanceRequest) { r.RuntimeIdentity = "another-runtime" }},
			{"from_version", func(r *SkillRuntimeMaintenanceRequest) { r.FromVersion = "0.9.0" }},
			{"to_version", func(r *SkillRuntimeMaintenanceRequest) { r.ToVersion = "1.2.0" }},
			{"target_source", func(r *SkillRuntimeMaintenanceRequest) { r.TargetSourceDigest = "sha256:" + strings.Repeat("e", 64) }},
			{"previous_source", func(r *SkillRuntimeMaintenanceRequest) { r.FromSourceDigest = "sha256:" + strings.Repeat("e", 64) }},
			{"from_artifact", func(r *SkillRuntimeMaintenanceRequest) {
				r.FromArtifact = "example.invalid/release@sha256:" + strings.Repeat("e", 64)
			}},
			{"to_artifact", func(r *SkillRuntimeMaintenanceRequest) {
				r.ToArtifact = "example.invalid/release@sha256:" + strings.Repeat("e", 64)
			}},
			{"different_operation", func(r *SkillRuntimeMaintenanceRequest) { r.OperationID = uuid.NewString() }},
		} {
			t.Run(drift.name, func(t *testing.T) {
				invalid := request
				// Expiration permits ownership takeover, never replacement of
				// an unfinished operation's immutable upgrade intent.
				invalid.Now = original.LeaseExpiresAt.Add(time.Second)
				invalid.Owner = "replacement-controller"
				drift.change(&invalid)
				if gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), invalid); gate != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceConflict) {
					t.Fatalf("unfinished intent was overwritten: %#v, %v", gate, err)
				}
				assertSkillRuntimeMaintenanceRecord(t, store, original)
			})
		}
	})
}

func TestSkillRuntimeMaintenanceRollbackIntentSurvivesReopenAndTakeover(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(SkillRuntimeMaintenanceStore)
		request := skillRuntimeMaintenanceEmptyRequest()
		gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		rollingBack, err := store.BeginSkillRuntimeMaintenanceRollback(t.Context(), skillRuntimeMaintenanceCompletion(gate, request.Now.Add(time.Second)))
		if err != nil || rollingBack == nil || !rollingBack.Active || rollingBack.DesiredVersion != request.FromVersion || rollingBack.Phase != "rolling_back" || rollingBack.Revision != gate.Revision+1 {
			t.Fatalf("rollback intent was not durably selected: %#v, %v", rollingBack, err)
		}
		store = reopenSkillRuntimeMaintenanceStore(t, kernel)
		assertSkillRuntimeMaintenanceRecord(t, store, rollingBack)
		active, err := store.ListActiveSkillRuntimeMaintenances(t.Context(), request.Scope)
		if err != nil || len(active) != 1 || !reflect.DeepEqual(active[0], rollingBack) {
			t.Fatalf("restart lost the active rollback gate: %#v, %v", active, err)
		}
		request.Owner = "rollback-recovery-controller"
		request.Now = rollingBack.LeaseExpiresAt.Add(time.Second)
		recovered, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
		if err != nil || recovered == nil || recovered.DesiredVersion != rollingBack.DesiredVersion || recovered.Phase != rollingBack.Phase || recovered.Revision != rollingBack.Revision+1 || recovered.Owner != request.Owner {
			t.Fatalf("takeover reverted the recorded rollback intent: %#v, %v", recovered, err)
		}
		completion := skillRuntimeMaintenanceCompletion(recovered, request.Now.Add(time.Second))
		completion.VerifiedVersion = request.ToVersion
		if result, err := store.CompleteSkillRuntimeMaintenance(t.Context(), completion); result != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceConflict) {
			t.Fatalf("upgraded version released a rollback gate: %#v, %v", result, err)
		}
		assertSkillRuntimeMaintenanceRecord(t, store, recovered)
		completion.VerifiedVersion = request.FromVersion
		completed, err := store.CompleteSkillRuntimeMaintenance(t.Context(), completion)
		if err != nil || completed == nil || completed.Active || completed.VerifiedVersion != request.FromVersion || completed.Phase != "complete" {
			t.Fatalf("verified rollback did not release: %#v, %v", completed, err)
		}
		active, err = store.ListActiveSkillRuntimeMaintenances(t.Context(), request.Scope)
		if err != nil || len(active) != 0 {
			t.Fatalf("completed rollback remains active: %#v, %v", active, err)
		}
	})
}

func TestSkillRuntimeMaintenanceCompletionRequiresCanonicalBindingsAtVerifiedVersion(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
		fixture := newActionRuntimeMaintenanceFixture(t, kernel)
		store := kernel.(SkillRuntimeMaintenanceStore)
		gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), actionRuntimeMaintenanceRequest(t, fixture))
		if err != nil {
			t.Fatal(err)
		}
		completion := skillRuntimeMaintenanceCompletion(gate, fixture.now.Add(2*time.Second))
		if result, err := store.CompleteSkillRuntimeMaintenance(t.Context(), completion); result != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceConflict) {
			t.Fatalf("runtime verification ignored an enabled old binding: %#v, %v", result, err)
		}
		assertSkillRuntimeMaintenanceRecord(t, store, gate)
		if err := fixture.store.ApplySkillReferenceUpgrade(WithSkillRuntimeMaintenance(t.Context(), gate), actionBindingFenceUpgrade(fixture)); err != nil {
			t.Fatal(err)
		}
		completed, err := store.CompleteSkillRuntimeMaintenance(t.Context(), completion)
		if err != nil || completed == nil || completed.Active || completed.VerifiedVersion != gate.ToVersion {
			t.Fatalf("canonical migrated binding did not permit verified completion: %#v, %v", completed, err)
		}
	})
}

func TestSkillRuntimeMaintenanceBusyIncludesOtherVersionCallsAndLiveReceipts(t *testing.T) {
	for _, status := range []string{"ready_call", "live_succeeded_receipt"} {
		t.Run(status, func(t *testing.T) {
			forActionLifecycleStores(t, func(t *testing.T, kernel KernelStore) {
				fixture := newActionRuntimeMaintenanceFixture(t, kernel)
				otherVersion := actionRuntimeMaintenanceBinding(t, fixture, "other-version", fixture.binding.SkillID, "1.1.0", fixture.binding.SourceIdentity)
				fixture.proposal = actionRuntimeMaintenanceProposal(fixture.proposal, otherVersion)
				if _, err := kernel.CreateActionProposal(t.Context(), fixture.proposal); err != nil {
					t.Fatal(err)
				}
				if status == "live_succeeded_receipt" {
					catalogCalls, credentialCalls, dispatches := 0, 0, 0
					completed, err := actionRuntimeMaintenanceWorker(kernel, fixture, &catalogCalls, &credentialCalls, &dispatches).RunOnce(t.Context(), fixture.proposal.Call.Scope, "action-worker", time.Minute)
					if err != nil || completed == nil || completed.Call.Status != ActionCallStatusSucceeded || dispatches != 1 {
						t.Fatalf("other-version receipt fixture = %#v, %v", completed, err)
					}
				}
				store := kernel.(SkillRuntimeMaintenanceStore)
				request := actionRuntimeMaintenanceRequest(t, fixture)
				request.Now = fixture.now.Add(3 * time.Second)
				if gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request); gate != nil || !errors.Is(err, ErrSkillReferenceUpgradeBusy) {
					t.Fatalf("logical Skill gate ignored work retained from another version: %#v, %v", gate, err)
				}
				if gate, err := store.GetSkillRuntimeMaintenance(t.Context(), request.Scope, request.SkillID); gate != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceNotFound) {
					t.Fatalf("busy attempt left an active or partial gate: %#v, %v", gate, err)
				}
				if status == "live_succeeded_receipt" {
					transitionActionLifecycleRun(t, kernel, fixture.proposal.Call, AgentRunStatusCanceled, nil, fixture.now.Add(4*time.Second))
					request.Now = fixture.now.Add(5 * time.Second)
					gate, err := store.AcquireSkillRuntimeMaintenance(t.Context(), request)
					if err != nil || gate == nil || !gate.Active {
						t.Fatalf("terminal parent retained a live-work blocker: %#v, %v", gate, err)
					}
				}
			})
		})
	}
}

func skillRuntimeMaintenanceEmptyRequest() SkillRuntimeMaintenanceRequest {
	return SkillRuntimeMaintenanceRequest{
		Scope: Scope{Kind: "tenant", ID: "maintenance-contract-" + uuid.NewString()}, SkillID: "release", SourceIdentity: "native::release",
		RuntimeIdentity: "native-release-server", OperationID: uuid.NewString(), FromVersion: "1.0.0", ToVersion: "1.1.0",
		TargetSourceDigest: "sha256:" + strings.Repeat("c", 64), FromSourceDigest: "sha256:" + strings.Repeat("d", 64),
		FromArtifact: "example.invalid/release@sha256:" + strings.Repeat("a", 64), ToArtifact: "example.invalid/release@sha256:" + strings.Repeat("b", 64),
		Owner: "upgrade-controller", LeaseDuration: time.Minute, Now: time.Now().UTC().Round(0),
	}
}

func skillRuntimeMaintenanceCompletion(gate *SkillRuntimeMaintenance, now time.Time) SkillRuntimeMaintenanceCompletion {
	return SkillRuntimeMaintenanceCompletion{Scope: gate.Scope, SkillID: gate.SkillID, OperationID: gate.OperationID, Owner: gate.Owner,
		ExpectedRevision: gate.Revision, VerifiedVersion: gate.DesiredVersion, Now: now}
}

func assertSkillRuntimeMaintenanceRecord(t *testing.T, store SkillRuntimeMaintenanceStore, expected *SkillRuntimeMaintenance) {
	t.Helper()
	actual, err := store.GetSkillRuntimeMaintenance(t.Context(), expected.Scope, expected.SkillID)
	if err != nil || !reflect.DeepEqual(expected, actual) {
		t.Fatalf("maintenance record changed unexpectedly: expected=%#v actual=%#v, %v", expected, actual, err)
	}
}

func reopenSkillRuntimeMaintenanceStore(t *testing.T, kernel KernelStore) SkillRuntimeMaintenanceStore {
	t.Helper()
	switch original := kernel.(type) {
	case *MemoryStore:
		return original
	case *SQLiteStore:
		var sequence int
		var name, path string
		if err := original.db.QueryRowContext(t.Context(), "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
			t.Fatal(err)
		}
		if err := original.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		return reopened
	case *PostgresStore:
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		reopened, err := NewPostgresStore(ctx, os.Getenv("OPENSEAL_TEST_POSTGRES_DSN"), WithPostgresSchema(original.schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = reopened.Close() })
		return reopened
	default:
		t.Fatal("unsupported maintenance restart fixture backend")
		return nil
	}
}
