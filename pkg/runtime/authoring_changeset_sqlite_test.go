package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestSQLiteWorkforceChangeSetsAreConcurrentRestartSafeAndScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	value := testWorkforceChangeSet(scope, "change-one")
	var created, replayed atomic.Int32
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, replay, err := store.CreateChangeSet(context.Background(), value, "intent-one", "request-one")
			if err != nil || result == nil || result.ID != value.ID {
				t.Errorf("create = %#v, replay = %t, err = %v", result, replay, err)
				return
			}
			if replay {
				replayed.Add(1)
			} else {
				created.Add(1)
			}
		}()
	}
	wait.Wait()
	if created.Load() != 1 || replayed.Load() != 1 {
		t.Fatalf("created = %d, replayed = %d", created.Load(), replayed.Load())
	}
	if _, _, err := store.GetChangeSetByIdempotency(context.Background(), scope, "intent-one", "different"); !errors.Is(err, authoring.ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	if _, err := store.GetChangeSet(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, value.ID); !errors.Is(err, authoring.ErrChangeSetNotFound) {
		t.Fatalf("cross-scope read = %v", err)
	}
	updated := *value
	updated.Status, updated.Revision = authoring.ChangeSetReady, 2
	updated.UpdatedAt = value.UpdatedAt.Add(time.Minute)
	if persisted, err := store.UpdateChangeSet(context.Background(), &updated, 1); err != nil || persisted.Revision != 2 {
		t.Fatalf("update = %#v, err = %v", persisted, err)
	}
	stale := updated
	stale.Status, stale.Revision = authoring.ChangeSetRejected, 3
	if _, err := store.UpdateChangeSet(context.Background(), &stale, 1); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("stale update = %v", err)
	}
	modifiedCandidate := updated
	modifiedCandidate.CandidateDigest, modifiedCandidate.Revision = "changed", 3
	if _, err := store.UpdateChangeSet(context.Background(), &modifiedCandidate, 2); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatalf("candidate mutation = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(context.Background(), scope, value.ID)
	if err != nil || restored.CandidateDigest != value.CandidateDigest || restored.Status != authoring.ChangeSetReady || restored.Revision != 2 {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
}

func testWorkforceChangeSet(scope capability.ScopeReference, id string) *authoring.ChangeSet {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return &authoring.ChangeSet{
		ID: id, Scope: scope, Mode: authoring.ModeCreate, Prompt: "Create a Team", PromptDigest: "prompt",
		CandidateDigest: "candidate", Result: authoring.CompileResult{Valid: true},
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "team-live"}, Status: authoring.ChangeSetReview,
		Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}
