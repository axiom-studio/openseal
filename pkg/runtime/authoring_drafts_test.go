package runtime

import (
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"path/filepath"
	"testing"
	"time"
)

func TestDraftListingScopesOwnerTenantAndPagination(t *testing.T) {
	sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "drafts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	for name, store := range map[string]interface {
		authoring.ChangeSetStore
		authoring.DraftChangeSetStore
	}{"sqlite": sqlite, "memory": authoring.NewMemoryChangeSetStore()} {
		t.Run(name, func(t *testing.T) {
			scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
			actor := authoring.ChangeSetActor{Type: "user", ID: "7"}
			for i, id := range []string{"old", "new", "foreign-owner", "foreign-tenant", "applied", "rejected"} {
				value := &authoring.ChangeSet{ID: id, Scope: scope, Actor: actor, Revision: 1, Status: authoring.ChangeSetEvaluating, CreatedAt: time.Now().UTC(), UpdatedAt: time.Unix(int64(i), 0).UTC()}
				switch id {
				case "foreign-owner":
					value.Actor.ID = "8"
				case "foreign-tenant":
					value.Scope.ID = "two"
				case "applied":
					value.Status = authoring.ChangeSetApplied
				case "rejected":
					value.Status = authoring.ChangeSetRejected
				}
				if _, _, err := store.CreateChangeSet(t.Context(), value, id, id); err != nil {
					t.Fatal(err)
				}
			}
			q := authoring.DraftChangeSetQuery{Scope: scope, Actor: actor, Limit: 1}
			values, err := store.ListDraftChangeSets(t.Context(), q)
			if err != nil || len(values) != 1 || values[0].ID != "new" {
				t.Fatalf("first=%v err=%v", values, err)
			}
			q.Offset = 1
			values, err = store.ListDraftChangeSets(t.Context(), q)
			if err != nil || len(values) != 1 || values[0].ID != "old" {
				t.Fatalf("second=%v err=%v", values, err)
			}
			q.Offset = 2
			values, err = store.ListDraftChangeSets(t.Context(), q)
			if err != nil || len(values) != 0 {
				t.Fatalf("extra=%v err=%v", values, err)
			}
			q.Scope.ID = ""
			if _, err = store.ListDraftChangeSets(t.Context(), q); err == nil {
				t.Fatal("missing tenant accepted")
			}
			q.Scope = scope
			q.Actor.ID = ""
			if _, err = store.ListDraftChangeSets(t.Context(), q); err == nil {
				t.Fatal("missing owner accepted")
			}
		})
	}
}
