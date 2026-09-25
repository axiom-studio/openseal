package authoring

import (
	"errors"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestDraftLifecycleIsOwnerAndTenantScoped(t *testing.T) {
	compiler, err := NewCompiler(semanticIntentGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	actor := ChangeSetActor{Type: "user", ID: "7"}
	prepared, _, err := service.Prepare(t.Context(), CreateChangeSetRequest{
		Scope: scope, Prompt: "Create a research agent", Actor: actor, IdempotencyKey: "draft-one",
	})
	if err != nil {
		t.Fatal(err)
	}
	query := DraftChangeSetQuery{Scope: scope, Actor: actor, Limit: 10}
	if drafts, err := service.ListDrafts(t.Context(), query); err != nil || len(drafts) != 1 || drafts[0].ID != prepared.ID {
		t.Fatalf("owner drafts=%v, err=%v", drafts, err)
	}
	query.Actor.ID = "8"
	if drafts, err := service.ListDrafts(t.Context(), query); err != nil || len(drafts) != 0 {
		t.Fatalf("foreign owner drafts=%v, err=%v", drafts, err)
	}
	query.Actor = actor
	query.Scope.ID = "two"
	if drafts, err := service.ListDrafts(t.Context(), query); err != nil || len(drafts) != 0 {
		t.Fatalf("foreign tenant drafts=%v, err=%v", drafts, err)
	}
	query.Scope = scope
	if _, err := service.CancelPreparedGeneration(t.Context(), scope, prepared.ID, prepared.Revision, ChangeSetActor{Type: "user", ID: "8"}); !errors.Is(err, ErrDraftOwner) {
		t.Fatalf("foreign cancellation error=%v", err)
	}
	canceled, err := service.CancelPreparedGeneration(t.Context(), scope, prepared.ID, prepared.Revision, actor)
	if err != nil || canceled.Status != ChangeSetFailed {
		t.Fatalf("canceled=%v, err=%v", canceled, err)
	}
	if _, err := service.DiscardDraft(t.Context(), scope, prepared.ID, prepared.Revision, actor); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale discard error=%v", err)
	}
	discarded, err := service.DiscardDraft(t.Context(), scope, prepared.ID, canceled.Revision, actor)
	if err != nil || discarded.Status != ChangeSetRejected {
		t.Fatalf("discarded=%v, err=%v", discarded, err)
	}
	if drafts, err := service.ListDrafts(t.Context(), query); err != nil || len(drafts) != 0 {
		t.Fatalf("discarded draft still listed: %v, err=%v", drafts, err)
	}
}
