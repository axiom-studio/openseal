package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestProjectStoresAreScopedCASAndRestartSafe(t *testing.T) {
	openers := []struct {
		name string
		open func(*testing.T) (ProjectStore, func())
	}{
		{"memory", func(t *testing.T) (ProjectStore, func()) { return NewMemoryStore(), func() {} }},
		{"sqlite", func(t *testing.T) (ProjectStore, func()) {
			path := filepath.Join(t.TempDir(), "kernel.db")
			s, err := NewSQLiteStore(path)
			if err != nil {
				t.Fatal(err)
			}
			return s, func() { s.Close() }
		}},
	}
	for _, tc := range openers {
		t.Run(tc.name, func(t *testing.T) {
			store, closeStore := tc.open(t)
			defer closeStore()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "a"}
			i := projectFixture(scope)
			if err := store.CreateProject(ctx, i); err != nil {
				t.Fatal(err)
			}
			if _, err := store.GetProject(ctx, Scope{Kind: "tenant", ID: "b"}, i.ID); err != ErrProjectNotFound {
				t.Fatalf("cross-scope get=%v", err)
			}
			got, err := store.GetProject(ctx, scope, i.ID)
			if err != nil {
				t.Fatal(err)
			}
			got.Status = ProjectStatusActive
			got.Revision = 2
			if err = store.UpdateProject(ctx, got, 1); err != nil {
				t.Fatal(err)
			}
			if err = store.UpdateProject(ctx, got, 1); err != ErrProjectConflict {
				t.Fatalf("stale update=%v", err)
			}
			listed, err := store.ListProjects(ctx, ProjectFilter{Scope: scope, ObjectiveID: "objective-a"})
			if err != nil || len(listed) != 1 {
				t.Fatalf("list=%d err=%v", len(listed), err)
			}
		})
	}
}

func TestProjectServiceIdempotencyAndActivity(t *testing.T) {
	store := NewMemoryStore()
	svc := NewProjectService(store, store)
	ctx := context.Background()
	seedProjectObjectives(t, store, Scope{Kind: "tenant", ID: "a"})
	req := CreateProjectRequest{Project: projectFixture(Scope{Kind: "tenant", ID: "a"}), IdempotencyKey: "request-1", Actor: ActivityActor{Type: "user", ID: "u1"}}
	first, event, err := svc.Create(ctx, req)
	if err != nil || event == nil || event.ProjectID != first.ID {
		t.Fatalf("create event=%#v err=%v", event, err)
	}
	req.Project.ID = "different"
	second, replay, err := svc.Create(ctx, req)
	if err != nil || replay != nil || second.ID != first.ID {
		t.Fatalf("replay=%#v event=%#v err=%v", second, replay, err)
	}
	req.Project.Purpose = "different"
	if _, _, err = svc.Create(ctx, req); err != ErrProjectIdempotency {
		t.Fatalf("conflicting key=%v", err)
	}
	feed, err := store.ListActivity(ctx, ActivityFilter{Scope: first.Scope, Descending: true, Limit: 10})
	if err != nil || len(feed) != 1 || feed[0].ProjectID != first.ID {
		t.Fatalf("activity=%#v err=%v", feed, err)
	}
}

func TestProjectMayBeCreatedBeforeObjectives(t *testing.T) {
	store := NewMemoryStore()
	service := NewProjectService(store, store)
	project := projectFixture(Scope{Kind: "tenant", ID: "empty-project-tenant"})
	project.ObjectiveRefs = nil
	project.Milestones = nil
	project.SourceMonitors = nil
	project.Deliverables = nil
	request := CreateProjectRequest{Project: project, IdempotencyKey: "create-before-objectives", Actor: ActivityActor{Type: "user", ID: "owner"}}
	created, _, err := service.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.ObjectiveRefs) != 0 {
		t.Fatal("creating a project must not invent objectives")
	}
	replayed, event, err := service.Create(t.Context(), request)
	if err != nil || event != nil || replayed.ID != created.ID {
		t.Fatalf("empty-project replay: %#v %v", replayed, err)
	}
	if _, err = store.GetProject(t.Context(), Scope{Kind: "tenant", ID: "another-tenant"}, created.ID); err != ErrProjectNotFound {
		t.Fatalf("cross-tenant read: %v", err)
	}
}

func TestProjectRejectsDuplicatedStateAndSecrets(t *testing.T) {
	i := projectFixture(Scope{Kind: "tenant", ID: "a"})
	i.ObjectiveRefs = []string{"same", "same"}
	if err := i.Validate(); err == nil {
		t.Fatal("expected duplicate objective rejection")
	}
	i = projectFixture(i.Scope)
	i.Policy = map[string]interface{}{"apiKey": "secret"}
	if err := i.Validate(); err == nil {
		t.Fatal("expected secret-bearing policy rejection")
	}
}

func TestProjectConcurrentIdempotentCreateHasOneWinner(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T) (interface {
			ProjectStore
			PortfolioStore
			RunbookActivationStore
		}, func())
	}{
		{"memory", func(*testing.T) (interface {
			ProjectStore
			PortfolioStore
			RunbookActivationStore
		}, func()) {
			return NewMemoryStore(), func() {}
		}},
		{"sqlite", func(t *testing.T) (interface {
			ProjectStore
			PortfolioStore
			RunbookActivationStore
		}, func()) {
			s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "race.db"))
			if err != nil {
				t.Fatal(err)
			}
			return s, func() { s.Close() }
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store, closeStore := tc.open(t)
			defer closeStore()
			scope := Scope{Kind: "tenant", ID: "a"}
			seedProjectObjectives(t, store, scope)
			svc := NewProjectService(store, store)
			base := projectFixture(scope)
			base.ID = ""
			base.SourceMonitors = nil
			const n = 24
			ids := make(chan string, n)
			errs := make(chan error, n)
			var wg sync.WaitGroup
			for x := 0; x < n; x++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					i := cloneProject(base)
					got, _, err := svc.Create(context.Background(), CreateProjectRequest{Project: i, IdempotencyKey: "same"})
					if err == nil {
						ids <- got.ID
					}
					errs <- err
				}()
			}
			wg.Wait()
			close(ids)
			close(errs)
			unique := map[string]bool{}
			for id := range ids {
				unique[id] = true
			}
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(unique) != 1 {
				t.Fatalf("created identities=%v", unique)
			}
			listed, err := store.ListProjects(context.Background(), ProjectFilter{Scope: scope})
			if err != nil || len(listed) != 1 {
				t.Fatalf("rows=%d err=%v", len(listed), err)
			}
			conflict := cloneProject(base)
			conflict.Purpose = "different"
			if _, _, err = svc.Create(context.Background(), CreateProjectRequest{Project: conflict, IdempotencyKey: "same"}); err != ErrProjectIdempotency {
				t.Fatalf("conflict=%v", err)
			}
		})
	}
}
func TestProjectPatchPreservesIdentityAndCreationProvenance(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "a"}
	seedProjectObjectives(t, store, scope)
	svc := NewProjectService(store, store)
	created, _, err := svc.Create(context.Background(), CreateProjectRequest{Project: projectFixture(scope), IdempotencyKey: "immutable"})
	if err != nil {
		t.Fatal(err)
	}
	title := "Updated"
	status := ProjectStatusActive
	updated, _, err := svc.Patch(context.Background(), scope, created.ID, UpdateProjectRequest{ExpectedRevision: 1, Title: &title, Status: &status, Actor: ActivityActor{Type: "user", ID: "u1"}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.Scope != created.Scope || updated.Owner != created.Owner || !updated.CreatedAt.Equal(created.CreatedAt) || updated.IdempotencyKeyHash != created.IdempotencyKeyHash || updated.CreationFingerprint != created.CreationFingerprint {
		t.Fatal("patch changed immutable project provenance")
	}
	if updated.Title != title || updated.Status != status || updated.Revision != 2 {
		t.Fatalf("patch=%#v", updated)
	}
	if _, _, err = svc.Patch(context.Background(), scope, created.ID, UpdateProjectRequest{ExpectedRevision: 2}); err != ErrProjectNoChanges {
		t.Fatalf("no-op patch=%v", err)
	}
}

func TestSQLiteProjectAuditFailureRollsBackMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := Scope{Kind: "tenant", ID: "a"}
	seedProjectObjectives(t, store, scope)
	if _, err = store.db.Exec(`CREATE TRIGGER reject_project_activity BEFORE INSERT ON run_activity WHEN NEW.event_type LIKE 'project.%' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	svc := NewProjectService(store, store)
	_, _, err = svc.Create(context.Background(), CreateProjectRequest{Project: projectFixture(scope), IdempotencyKey: "atomic"})
	if err == nil {
		t.Fatal("expected audit failure")
	}
	if _, err = store.GetProject(context.Background(), scope, "project-a"); err != ErrProjectNotFound {
		t.Fatalf("project persisted without audit: %v", err)
	}
}

func TestSQLiteProjectUpgradeBackfillsIdempotencyIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE projects(id TEXT NOT NULL,scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,owner_type TEXT NOT NULL,owner_id TEXT NOT NULL,status TEXT NOT NULL,revision INTEGER NOT NULL,updated_at DATETIME NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,id))`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var count int
	if err = store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('projects') WHERE name='idempotency_key_hash'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("column=%d err=%v", count, err)
	}
}

func TestProjectStoreFiltersAndPaginatesInDeterministicOrder(t *testing.T) {
	openers := []struct {
		name string
		open func(*testing.T) (ProjectStore, func())
	}{{"memory", func(*testing.T) (ProjectStore, func()) { return NewMemoryStore(), func() {} }}, {"sqlite", func(t *testing.T) (ProjectStore, func()) {
		s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "filter.db"))
		if err != nil {
			t.Fatal(err)
		}
		return s, func() { s.Close() }
	}}}
	for _, tc := range openers {
		t.Run(tc.name, func(t *testing.T) {
			s, closeStore := tc.open(t)
			defer closeStore()
			scope := Scope{Kind: "tenant", ID: "a"}
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			for x, id := range []string{"a", "b", "c"} {
				i := projectFixture(scope)
				i.ID = id
				i.CreatedAt = base
				i.UpdatedAt = base.Add(time.Duration(x) * time.Hour)
				if id == "b" {
					i.Owner = ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent-b"}
				}
				if id == "c" {
					i.Status = ProjectStatusActive
					i.ObjectiveRefs = []string{"objective-c"}
					i.Milestones = nil
					i.SourceMonitors = nil
				}
				if err := s.CreateProject(context.Background(), i); err != nil {
					t.Fatal(err)
				}
			}
			owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}
			rows, err := s.ListProjects(context.Background(), ProjectFilter{Scope: scope, Owner: &owner, Statuses: []ProjectStatus{ProjectStatusDraft}, ObjectiveID: "objective-a", Limit: 1})
			if err != nil || len(rows) != 1 || rows[0].ID != "a" {
				t.Fatalf("filtered=%#v err=%v", rows, err)
			}
			rows, err = s.ListProjects(context.Background(), ProjectFilter{Scope: scope, Limit: 1, Offset: 1})
			if err != nil || len(rows) != 1 || rows[0].ID != "b" {
				t.Fatalf("page=%#v err=%v", rows, err)
			}
		})
	}
}

func seedProjectObjectives(t *testing.T, store interface {
	PortfolioStore
	RunbookActivationStore
}, scope Scope) {
	t.Helper()
	for _, id := range []string{"objective-a", "objective-b"} {
		now := time.Now().UTC()
		objective := &Objective{ID: id, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}, Title: id, Goal: "Verify project", Status: ObjectiveStatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now}
		if err := store.CreateObjective(context.Background(), objective); err != nil {
			t.Fatal(err)
		}
		if id == "objective-a" {
			_, err := NewRunbookActivationService(store).Create(context.Background(), CreateRunbookActivationRequest{
				ID: "monitor-a", Scope: scope, Owner: objective.Owner, ObjectiveID: id, AssignedAgentID: "researcher",
				DefinitionID: "research", DefinitionVersion: "1.0.0", TriggerID: "monitor",
				Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "monitor", Schedule: &runbook.Schedule{Cron: "0 */5 * * * *", Timezone: "UTC"}},
				Input:   map[string]interface{}{"projectId": "project-a", "sourceMonitorId": "monitor-a"}, Policy: map[string]interface{}{"sourcePolicyRef": "approved-forums"},
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func projectFixture(scope Scope) *Project {
	return &Project{ID: "project-a", Scope: scope, Title: "Launch research", Purpose: "Understand customer needs", Status: ProjectStatusDraft, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}, TeamRefs: []ResourceReference{{Kind: ResourceKindTeamDeployment, ID: "team-a", Revision: 1}}, ObjectiveRefs: []string{"objective-a", "objective-b"}, Milestones: []ProjectMilestone{{ID: "m1", Title: "Evidence review", Status: "pending", ObjectiveRefs: []string{"objective-a"}}}, Hypotheses: []ProjectHypothesis{{ID: "h1", Statement: "Onboarding is difficult", Confidence: .4, UpdatedAt: time.Now().UTC()}}, SourceMonitors: []SourceMonitorReference{{ID: "monitor-a", ObjectiveID: "objective-a", AssignedAgentID: "researcher", SkillID: "forum-reader", SkillVersion: "1.0.0", Action: "search", SourcePolicyRef: "approved-forums", Deduplication: SourceMonitorDeduplicateStableSourceAndContent}}, Deliverables: []ProjectDeliverable{{ID: "report", Title: "Cited report", Status: "planned", ArtifactRefs: []ResourceReference{{Kind: ResourceKindArtifact, ID: "report-pdf", Revision: 1}}}}, Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
}
