package authoring

import (
	"errors"
	"github.com/axiom-studio/openseal/pkg/capability"
	"testing"
)

func TestDiscardDraftOwnershipRevisionAndTerminalProtection(t *testing.T) {
	store := NewMemoryChangeSetStore()
	compiler, _ := NewCompiler(&heldCancellationGenerator{})
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	owner := ChangeSetActor{Type: "user", ID: "7"}
	for _, status := range []ChangeSetStatus{ChangeSetEvaluating, ChangeSetReady, ChangeSetApplied} {
		id := string(status)
		value := &ChangeSet{ID: id, Scope: scope, Actor: owner, Revision: 1, Status: status}
		if _, _, err := store.CreateChangeSet(t.Context(), value, id, id); err != nil {
			t.Fatal(err)
		}
	}
	checks := []struct {
		scope    capability.ScopeReference
		id       string
		revision int64
		actor    ChangeSetActor
		want     error
	}{
		{capability.ScopeReference{Kind: "tenant", ID: "other"}, "ready", 1, owner, ErrChangeSetNotFound},
		{scope, "ready", 1, ChangeSetActor{Type: "user", ID: "8"}, ErrDraftOwner},
		{scope, "ready", 2, owner, ErrChangeSetRevision},
		{scope, "applied", 1, owner, ErrChangeSetTransition},
	}
	for _, check := range checks {
		if _, err := service.DiscardDraft(t.Context(), check.scope, check.id, check.revision, check.actor); !errors.Is(err, check.want) {
			t.Fatalf("%+v: %v", check, err)
		}
	}
	old, _ := store.GetChangeSet(t.Context(), scope, "ready")
	deleted, err := service.DiscardDraft(t.Context(), scope, "ready", 1, owner)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Status != ChangeSetRejected || deleted.Revision != 2 || deleted.Lifecycle[0].Reason != "draft_deleted" || deleted.Lifecycle[0].Actor != owner {
		t.Fatalf("discard=%+v", deleted)
	}
	old.Status = ChangeSetApplied
	old.Revision = 2
	if _, err := store.UpdateChangeSet(t.Context(), old, 1); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale writer resurrected draft: %v", err)
	}
	if _, err := service.DiscardDraft(t.Context(), scope, "ready", 2, owner); !errors.Is(err, ErrChangeSetTransition) {
		t.Fatalf("terminal discard=%v", err)
	}
	drafts, err := service.ListDrafts(t.Context(), DraftChangeSetQuery{Scope: scope, Actor: owner, Limit: 50})
	if err != nil || len(drafts) != 1 || drafts[0].ID != "evaluating" {
		t.Fatalf("remaining=%+v err=%v", drafts, err)
	}
}
