package openseal

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

type workforceFixtureGenerator struct{}

func (workforceFixtureGenerator) Generate(context.Context, WorkforceAuthoringRequest) ([]byte, error) {
	return []byte(`{"candidate":{"agents":[],"assignments":[]},"questions":["What should this Team own?"]}`), nil
}

func TestEngineExposesDurableWorkforceChangeSetsOnlyWithPersistentSupport(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err := New(WithPersistentStore(store), WithWorkforceAuthoringGenerator(workforceFixtureGenerator{}))
	if err != nil {
		t.Fatal(err)
	}
	if !engine.WorkforceAuthoringAvailable() || !engine.WorkforceChangeSetsAvailable() {
		t.Fatal("durable workforce authoring was not exposed")
	}
	created, replayed, err := engine.CreateWorkforceChangeSet(t.Context(), CreateWorkforceChangeSetRequest{
		Scope: SkillScope{Kind: "workspace", ID: "local"}, Prompt: "Create a Team", Catalog: WorkforceCapabilityCatalog{},
		Placement: WorkforceChangeSetPlacement{TeamDeploymentID: "team-live"}, Actor: WorkforceChangeSetActor{Type: "user", ID: "local"},
		IdempotencyKey: "create-team",
	})
	if err != nil || replayed || created.Status != WorkforceChangeSetBlocked {
		t.Fatalf("created = %#v, replayed = %t, err = %v", created, replayed, err)
	}
	restored, err := engine.GetWorkforceChangeSet(t.Context(), created.Scope, created.ID)
	if err != nil || restored.CandidateDigest != created.CandidateDigest {
		t.Fatalf("restored = %#v, err = %v", restored, err)
	}
}
