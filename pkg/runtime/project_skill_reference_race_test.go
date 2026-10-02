package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// This intercepts the last host boundary before the canonical store transaction.
// A new reference is accepted after planning and materialization have finished,
// rather than relying on a goroutine winning a timing-sensitive test race.
type projectSkillUpgradeApplyInterceptor struct {
	SkillReferenceUpgradeStore
	beforeApply func(context.Context) error
}

func (s *projectSkillUpgradeApplyInterceptor) ApplySkillReferenceUpgrade(ctx context.Context, mutation *SkillReferenceUpgradeMutation) error {
	if s.beforeApply != nil {
		if err := s.beforeApply(ctx); err != nil {
			return err
		}
	}
	return s.SkillReferenceUpgradeStore.ApplySkillReferenceUpgrade(ctx, mutation)
}

func TestSkillReferenceUpgradeRejectsProjectInsertedAfterMaterializationAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(SkillReferenceUpgradeStore)
		catalog, binding, last := seedSkillUpgradeProjectPagination(t, store, false)
		late := cloneProject(last)
		late.ID = "project-0501"
		inserted := false
		interceptor := &projectSkillUpgradeApplyInterceptor{
			SkillReferenceUpgradeStore: store,
			beforeApply: func(ctx context.Context) error {
				if inserted {
					return nil
				}
				inserted = true
				return store.CreateProject(ctx, late)
			},
		}
		service := NewSkillReferenceUpgradeService(interceptor, catalog)
		request := PlanSkillReferenceUpgradeRequest{Scope: last.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ToVersion: "1.1.0"}
		plan, err := service.Plan(t.Context(), request)
		if err != nil || len(plan.Projects) != 1 || plan.Projects[0].ID != last.ID {
			t.Fatalf("initial plan=%#v err=%v", plan, err)
		}
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{
			Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Apply original complete plan",
		}); !errors.Is(err, ErrSkillReferenceUpgradeConflict) {
			t.Fatalf("reference accepted after materialization was lost: %v", err)
		}
		if !inserted {
			t.Fatal("test did not reach the canonical transaction race window")
		}
		currentBinding, err := catalog.GetBinding(t.Context(), binding.Scope, binding.DeploymentID, binding.ID)
		if err != nil || currentBinding.SkillVersion != "1.0.0" || currentBinding.Revision != binding.Revision {
			t.Fatalf("incomplete upgrade partially advanced binding: %#v err=%v", currentBinding, err)
		}
		for _, id := range []string{last.ID, late.ID} {
			project, err := store.GetProject(t.Context(), last.Scope, id)
			if err != nil || project.Revision != 1 || project.SourceMonitors[0].SkillVersion != "1.0.0" {
				t.Fatalf("incomplete upgrade partially advanced Project %s: %#v err=%v", id, project, err)
			}
		}
		// A fresh complete plan is usable; discovering a concurrent reference
		// defers an upgrade instead of leaving it permanently blocked.
		plan, err = service.Plan(t.Context(), request)
		if err != nil || len(plan.Projects) != 2 {
			t.Fatalf("replanned references=%#v err=%v", plan, err)
		}
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{
			Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Apply refreshed complete plan",
		}); err != nil {
			t.Fatalf("refreshed complete plan failed: %v", err)
		}
		for _, id := range []string{last.ID, late.ID} {
			project, err := store.GetProject(t.Context(), last.Scope, id)
			if err != nil || project.Revision != 2 || project.SourceMonitors[0].SkillVersion != "1.1.0" {
				t.Fatalf("fresh upgrade omitted Project %s: %#v err=%v", id, project, err)
			}
		}
	})
}

func TestSkillReferenceUpgradeFencesStaleProjectAdmissionsAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(SkillReferenceUpgradeStore)
		catalog, binding, last := seedSkillUpgradeProjectPagination(t, store, false)
		service := NewSkillReferenceUpgradeService(store, catalog)
		plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{
			Scope: last.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ToVersion: "1.1.0",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{
			Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Upgrade before stale producer resumes",
		}); err != nil {
			t.Fatal(err)
		}
		staleCreate := cloneProject(last)
		staleCreate.ID = "stale-project"
		if err := store.CreateProject(t.Context(), staleCreate); !errors.Is(err, ErrProjectSkillReferenceUnavailable) {
			t.Fatalf("stale producer accepted obsolete Project reference: %v", err)
		}
		if project, err := store.GetProject(t.Context(), last.Scope, staleCreate.ID); !errors.Is(err, ErrProjectNotFound) || project != nil {
			t.Fatalf("rejected Project persisted: %#v err=%v", project, err)
		}
		current, err := store.GetProject(t.Context(), last.Scope, last.ID)
		if err != nil {
			t.Fatal(err)
		}
		staleUpdate := cloneProject(current)
		staleUpdate.Revision++
		staleUpdate.SourceMonitors[0].SkillVersion = "1.0.0"
		if err := store.UpdateProject(t.Context(), staleUpdate, current.Revision); !errors.Is(err, ErrProjectSkillReferenceUnavailable) {
			t.Fatalf("stale producer rewound Project reference: %v", err)
		}
		persisted, err := store.GetProject(t.Context(), last.Scope, last.ID)
		if err != nil || persisted.Revision != current.Revision || persisted.SourceMonitors[0].SkillVersion != "1.1.0" {
			t.Fatalf("failed admission partially changed Project: %#v err=%v", persisted, err)
		}
	})
}

func TestProjectSkillReferenceAdmissionPreservesUnchangedHistoricalMetadataAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(SkillReferenceUpgradeStore)
		catalog, binding, last := seedSkillUpgradeProjectPagination(t, store, false)
		// A binding-only reconfiguration from an older host leaves the original
		// Project as historical metadata. It does not authorize its old version.
		configured := *binding
		configured.SkillVersion = "1.1.0"
		configured.Revision++
		if err := catalog.Bind(t.Context(), &configured); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.DisableBinding(t.Context(), skill.DisableBindingRequest{
			Scope: binding.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ExpectedRevision: configured.Revision,
			Actor: skill.BindingActor{Type: "user", ID: "operator"}, Reason: "Disconnect source account",
		}); err != nil {
			t.Fatal(err)
		}
		metadata := cloneProject(last)
		metadata.Title = "Renamed historical research"
		metadata.Revision++
		if err := store.UpdateProject(t.Context(), metadata, last.Revision); err != nil {
			t.Fatalf("unchanged historical references blocked metadata editing: %v", err)
		}
		changed := cloneProject(metadata)
		changed.Revision++
		changed.SourceMonitors[0].SourcePolicyRef = "new-source-policy"
		if err := store.UpdateProject(t.Context(), changed, metadata.Revision); !errors.Is(err, ErrProjectSkillReferenceUnavailable) {
			t.Fatalf("changed monitor inherited revoked authority: %v", err)
		}
		persisted, err := store.GetProject(t.Context(), last.Scope, last.ID)
		if err != nil || persisted.Revision != metadata.Revision || persisted.Title != metadata.Title ||
			persisted.SourceMonitors[0].SkillVersion != "1.0.0" || persisted.SourceMonitors[0].SourcePolicyRef != "approved-sources" {
			t.Fatalf("historical metadata/reference changed incorrectly: %#v err=%v", persisted, err)
		}
		inactive := cloneProject(last)
		inactive.ID = "configured-inactive-project"
		inactive.SourceMonitors[0].SkillVersion = configured.SkillVersion
		if err := store.CreateProject(t.Context(), inactive); err != nil {
			t.Fatalf("exact configured inactive binding blocked authoring metadata: %v", err)
		}
		currentBinding, err := catalog.GetBinding(t.Context(), binding.Scope, binding.DeploymentID, binding.ID)
		if err != nil || !currentBinding.Disabled || currentBinding.Revision != configured.Revision+1 || currentBinding.SkillVersion != configured.SkillVersion {
			t.Fatalf("metadata authoring re-enabled or altered the binding: %#v err=%v", currentBinding, err)
		}
		if bound, err := catalog.Resolve(t.Context(), binding.Scope, binding.DeploymentID, binding.SkillID, configured.SkillVersion, "monitor"); err == nil || bound != nil {
			t.Fatalf("inactive metadata became executable authority: bound=%#v err=%v", bound, err)
		}
	})
}

func TestProjectSkillReferenceAdmissionFencesLegacySQLWritersAcrossStores(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		var db *sql.DB
		var table string
		var postgres bool
		switch concrete := fixture.store.(type) {
		case *SQLiteStore:
			db, table = concrete.db, "projects"
		case *PostgresStore:
			db, table, postgres = concrete.db, concrete.table("projects"), true
		default:
			t.Skip("rolling SQL writers do not exist for the process-local store")
		}
		store := fixture.store.(SkillReferenceUpgradeStore)
		catalog, binding, last := seedSkillUpgradeProjectPagination(t, store, false)
		legacy := cloneProject(last)
		legacy.ID = "legacy-project"
		if err := writeLegacyProjectSQL(t, db, table, postgres, legacy, false); err != nil {
			t.Fatalf("valid legacy SQL writer rejected before upgrade: %v", err)
		}
		service := NewSkillReferenceUpgradeService(store, catalog)
		plan, err := service.Plan(t.Context(), PlanSkillReferenceUpgradeRequest{
			Scope: last.Scope, DeploymentID: binding.DeploymentID, BindingID: binding.ID, ToVersion: "1.1.0",
		})
		if err != nil || len(plan.Projects) != 2 {
			t.Fatalf("legacy writer did not update the canonical reference projection: plan=%#v err=%v", plan, err)
		}
		if _, err := service.Apply(t.Context(), ApplySkillReferenceUpgradeRequest{
			Plan: plan, Actor: ActivityActor{Type: "system", ID: "upgrader"}, Reason: "Upgrade references from all deployed writers",
		}); err != nil {
			t.Fatal(err)
		}
		stale := cloneProject(legacy)
		stale.ID = "stale-legacy-project"
		if err := writeLegacyProjectSQL(t, db, table, postgres, stale, false); err == nil || !strings.Contains(err.Error(), ErrProjectSkillReferenceUnavailable.Error()) {
			t.Fatalf("legacy SQL INSERT bypassed the executable version fence: %v", err)
		}
		if rejected, err := store.GetProject(t.Context(), last.Scope, stale.ID); !errors.Is(err, ErrProjectNotFound) || rejected != nil {
			t.Fatalf("rejected legacy INSERT persisted: %#v err=%v", rejected, err)
		}
		current, err := store.GetProject(t.Context(), legacy.Scope, legacy.ID)
		if err != nil {
			t.Fatal(err)
		}
		stale = cloneProject(current)
		stale.Revision++
		stale.SourceMonitors[0].SkillVersion = "1.0.0"
		if err := writeLegacyProjectSQL(t, db, table, postgres, stale, true); err == nil || !strings.Contains(err.Error(), ErrProjectSkillReferenceUnavailable.Error()) {
			t.Fatalf("legacy SQL UPDATE bypassed the executable version fence: %v", err)
		}
		persisted, err := store.GetProject(t.Context(), legacy.Scope, legacy.ID)
		if err != nil || persisted.Revision != current.Revision || persisted.SourceMonitors[0].SkillVersion != "1.1.0" {
			t.Fatalf("rejected legacy UPDATE mutated history: %#v err=%v", persisted, err)
		}
	})
}

// Old deployed writers know only the original projects row, so this deliberately
// supplies no projection records and bypasses new Go admission helpers.
func writeLegacyProjectSQL(t *testing.T, db *sql.DB, table string, postgres bool, project *Project, update bool) error {
	t.Helper()
	payload, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	placeholder := func(index int) string {
		if postgres {
			return fmt.Sprintf("$%d", index)
		}
		return "?"
	}
	if update {
		query := "UPDATE " + table + " SET revision=" + placeholder(1) + ",payload=" + placeholder(2) +
			" WHERE scope_kind=" + placeholder(3) + " AND scope_id=" + placeholder(4) + " AND id=" + placeholder(5) + " AND revision=" + placeholder(6)
		_, err = db.ExecContext(t.Context(), query, project.Revision, string(payload), project.Scope.Kind, project.Scope.ID, project.ID, project.Revision-1)
		return err
	}
	marks := make([]string, 10)
	for index := range marks {
		marks[index] = placeholder(index + 1)
	}
	_, err = db.ExecContext(t.Context(), "INSERT INTO "+table+"(id,scope_kind,scope_id,owner_type,owner_id,status,revision,updated_at,idempotency_key_hash,payload) VALUES("+strings.Join(marks, ",")+")",
		project.ID, project.Scope.Kind, project.Scope.ID, project.Owner.Type, project.Owner.ID, project.Status, project.Revision, project.UpdatedAt, project.IdempotencyKeyHash, string(payload))
	return err
}
