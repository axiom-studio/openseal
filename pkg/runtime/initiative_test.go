package runtime

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestInitiativeStoresAreScopedCASAndRestartSafe(t *testing.T) {
	openers := []struct {
		name string
		open func(*testing.T) (InitiativeStore, func())
	}{
		{"memory", func(t *testing.T) (InitiativeStore, func()) { return NewMemoryStore(100), func() {} }},
		{"sqlite", func(t *testing.T) (InitiativeStore, func()) {
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
			i := initiativeFixture(scope)
			if err := store.CreateInitiative(ctx, i); err != nil {
				t.Fatal(err)
			}
			if _, err := store.GetInitiative(ctx, Scope{Kind: "tenant", ID: "b"}, i.ID); err != ErrInitiativeNotFound {
				t.Fatalf("cross-scope get=%v", err)
			}
			got, err := store.GetInitiative(ctx, scope, i.ID)
			if err != nil {
				t.Fatal(err)
			}
			got.Status = InitiativeStatusActive
			got.Revision = 2
			if err = store.UpdateInitiative(ctx, got, 1); err != nil {
				t.Fatal(err)
			}
			if err = store.UpdateInitiative(ctx, got, 1); err != ErrInitiativeConflict {
				t.Fatalf("stale update=%v", err)
			}
			listed, err := store.ListInitiatives(ctx, InitiativeFilter{Scope: scope, ObjectiveID: "objective-a"})
			if err != nil || len(listed) != 1 {
				t.Fatalf("list=%d err=%v", len(listed), err)
			}
		})
	}
}

func TestInitiativeServiceIdempotencyAndActivity(t *testing.T) {
	store := NewMemoryStore(100)
	svc := NewInitiativeService(store, store)
	ctx := context.Background()
	for _, id := range []string{"objective-a", "objective-b"} {
		now := time.Now().UTC()
		if err := store.CreateObjective(ctx, &Objective{ID: id, Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}, Title: id, Goal: "Verify initiative", Status: ObjectiveStatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	req := CreateInitiativeRequest{Initiative: initiativeFixture(Scope{Kind: "tenant", ID: "a"}), IdempotencyKey: "request-1", Actor: ActivityActor{Type: "user", ID: "u1"}}
	first, event, err := svc.Create(ctx, req)
	if err != nil || event == nil || event.InitiativeID != first.ID {
		t.Fatalf("create event=%#v err=%v", event, err)
	}
	req.Initiative.ID = "different"
	second, replay, err := svc.Create(ctx, req)
	if err != nil || replay != nil || second.ID != first.ID {
		t.Fatalf("replay=%#v event=%#v err=%v", second, replay, err)
	}
	req.Initiative.Purpose = "different"
	if _, _, err = svc.Create(ctx, req); err != ErrInitiativeIdempotency {
		t.Fatalf("conflicting key=%v", err)
	}
	feed, err := store.ListActivity(ctx, ActivityFilter{Scope: first.Scope, Descending: true, Limit: 10})
	if err != nil || len(feed) != 1 || feed[0].InitiativeID != first.ID {
		t.Fatalf("activity=%#v err=%v", feed, err)
	}
}

func TestInitiativeRejectsDuplicatedStateAndSecrets(t *testing.T) {
	i := initiativeFixture(Scope{Kind: "tenant", ID: "a"})
	i.ObjectiveRefs = []string{"same", "same"}
	if err := i.Validate(); err == nil {
		t.Fatal("expected duplicate objective rejection")
	}
	i = initiativeFixture(i.Scope)
	i.Policy = map[string]interface{}{"apiKey": "secret"}
	if err := i.Validate(); err == nil {
		t.Fatal("expected secret-bearing policy rejection")
	}
}

func TestInitiativeConcurrentIdempotentCreateHasOneWinner(t *testing.T) {
	store := NewMemoryStore(100)
	scope := Scope{Kind: "tenant", ID: "a"}
	seedInitiativeObjectives(t, store, scope)
	svc := NewInitiativeService(store, store)
	const n = 24
	ids := make(chan string, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for x := 0; x < n; x++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			i := initiativeFixture(scope)
			i.ID = ""
			got, _, err := svc.Create(context.Background(), CreateInitiativeRequest{Initiative: i, IdempotencyKey: "same"})
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
		if err != nil && err != ErrInitiativeIdempotency {
			t.Fatal(err)
		}
	}
	if len(unique) != 1 {
		t.Fatalf("created identities=%v", unique)
	}
	listed, err := store.ListInitiatives(context.Background(), InitiativeFilter{Scope: scope})
	if err != nil || len(listed) != 1 {
		t.Fatalf("rows=%d err=%v", len(listed), err)
	}
}
func TestInitiativePatchPreservesIdentityAndCreationProvenance(t *testing.T) {
	store := NewMemoryStore(100)
	scope := Scope{Kind: "tenant", ID: "a"}
	seedInitiativeObjectives(t, store, scope)
	svc := NewInitiativeService(store, store)
	created, _, err := svc.Create(context.Background(), CreateInitiativeRequest{Initiative: initiativeFixture(scope), IdempotencyKey: "immutable"})
	if err != nil {
		t.Fatal(err)
	}
	title := "Updated"
	status := InitiativeStatusActive
	updated, _, err := svc.Patch(context.Background(), scope, created.ID, UpdateInitiativeRequest{ExpectedRevision: 1, Title: &title, Status: &status, Actor: ActivityActor{Type: "user", ID: "u1"}})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != created.ID || updated.Scope != created.Scope || updated.Owner != created.Owner || !updated.CreatedAt.Equal(created.CreatedAt) || updated.IdempotencyKeyHash != created.IdempotencyKeyHash || updated.CreationFingerprint != created.CreationFingerprint {
		t.Fatal("patch changed immutable initiative provenance")
	}
	if updated.Title != title || updated.Status != status || updated.Revision != 2 {
		t.Fatalf("patch=%#v", updated)
	}
	if _, _, err = svc.Patch(context.Background(), scope, created.ID, UpdateInitiativeRequest{ExpectedRevision: 2}); err != ErrInitiativeNoChanges {
		t.Fatalf("no-op patch=%v", err)
	}
}

func TestSQLiteInitiativeAuditFailureRollsBackMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "atomic.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := Scope{Kind: "tenant", ID: "a"}
	seedInitiativeObjectives(t, store, scope)
	if _, err = store.db.Exec(`CREATE TRIGGER reject_initiative_activity BEFORE INSERT ON run_activity WHEN NEW.event_type LIKE 'initiative.%' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	svc := NewInitiativeService(store, store)
	_, _, err = svc.Create(context.Background(), CreateInitiativeRequest{Initiative: initiativeFixture(scope), IdempotencyKey: "atomic"})
	if err == nil {
		t.Fatal("expected audit failure")
	}
	if _, err = store.GetInitiative(context.Background(), scope, "initiative-a"); err != ErrInitiativeNotFound {
		t.Fatalf("initiative persisted without audit: %v", err)
	}
}

func TestSQLiteInitiativeUpgradeBackfillsIdempotencyIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE initiatives(id TEXT NOT NULL,scope_kind TEXT NOT NULL,scope_id TEXT NOT NULL,owner_type TEXT NOT NULL,owner_id TEXT NOT NULL,status TEXT NOT NULL,revision INTEGER NOT NULL,updated_at DATETIME NOT NULL,payload TEXT NOT NULL,PRIMARY KEY(scope_kind,scope_id,id))`)
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
	if err = store.db.QueryRow(`SELECT count(*) FROM pragma_table_info('initiatives') WHERE name='idempotency_key_hash'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("column=%d err=%v", count, err)
	}
}

func seedInitiativeObjectives(t *testing.T, store PortfolioStore, scope Scope) {
	t.Helper()
	for _, id := range []string{"objective-a", "objective-b"} {
		now := time.Now().UTC()
		if err := store.CreateObjective(context.Background(), &Objective{ID: id, Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}, Title: id, Goal: "Verify initiative", Status: ObjectiveStatusActive, Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
}

func initiativeFixture(scope Scope) *Initiative {
	return &Initiative{ID: "initiative-a", Scope: scope, Title: "Launch research", Purpose: "Understand customer needs", Status: InitiativeStatusDraft, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-a"}, TeamRefs: []ResourceReference{{Kind: ResourceKindTeamDeployment, ID: "team-a", Revision: 1}}, ObjectiveRefs: []string{"objective-a", "objective-b"}, RunRefs: []string{"run-a"}, Milestones: []InitiativeMilestone{{ID: "m1", Title: "Evidence review", Status: "pending", ObjectiveRefs: []string{"objective-a"}}}, Hypotheses: []InitiativeHypothesis{{ID: "h1", Statement: "Onboarding is difficult", Confidence: .4, UpdatedAt: time.Now().UTC()}}, SourceMonitors: []SourceMonitorReference{{ID: "monitor-a", SkillID: "forum-reader", ScheduleRef: "schedule-a"}}, Deliverables: []InitiativeDeliverable{{ID: "report", Title: "Cited report", Status: "planned", ArtifactRefs: []ResourceReference{{Kind: ResourceKindArtifact, ID: "report-pdf", Revision: 1}}}}, Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
}
