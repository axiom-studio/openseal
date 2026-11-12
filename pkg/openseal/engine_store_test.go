package openseal

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestDefaultStoreServesRunAuditsAndSkillSources(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	if engine.runbookAudits == nil {
		t.Fatal("default engine has no Run execution audit")
	}
	if _, err := engine.SkillSourceContentProvider(); err != nil {
		t.Fatalf("default engine has no skill source store: %v", err)
	}
}

func TestNewOpensAndClosesStoreOnlyWithoutStoreOption(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if engine.ownedStore == nil || engine.Store() != models.KernelStore(engine.ownedStore) {
		t.Fatal("engine without a store option does not run on its own in-memory store")
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if engine.ownedStore != nil {
		t.Fatal("Close kept the in-memory store")
	}

	store := kerneltest.NewStore(t)
	engine, err = New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	if engine.ownedStore != nil {
		t.Fatal("engine with a full store opened an in-memory store")
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListAgentRuns(context.Background(), models.AgentRunFilter{Scope: Scope{Kind: "local", ID: "test"}}); err != nil {
		t.Fatalf("Close closed the caller's store: %v", err)
	}
}

func TestWithStoreRejectsNil(t *testing.T) {
	if _, err := New(WithStore(nil)); err == nil {
		t.Fatal("nil store accepted")
	}
}

// kernelOnlyStore hides every interface beyond models.KernelStore, as a
// runtime-only embedding's store would.
type kernelOnlyStore struct{ models.KernelStore }

func TestPartialStoreKeepsRegistriesInMemory(t *testing.T) {
	store := kernelOnlyStore{kerneltest.NewStore(t)}
	engine, err := New(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })

	if engine.Store() != models.KernelStore(store) {
		t.Fatal("engine does not run on the given store")
	}
	if engine.ownedStore == nil {
		t.Fatal("no in-memory store for the registries the store lacks")
	}
	if engine.agents == nil || engine.teams == nil || engine.skills == nil || engine.progression == nil {
		t.Fatal("registries missing for a partial store")
	}
	if engine.dependencies != nil || engine.conversations != nil || engine.conversationChanges != nil ||
		engine.embeds != nil || engine.spaces != nil || engine.collaboration != nil || engine.artifacts != nil ||
		engine.skillReferenceUpgrades != nil || engine.skillSources != nil {
		t.Fatal("a service the store does not implement was wired")
	}
}

func TestSkillCatalogOptionWinsOverStoreInAnyOrder(t *testing.T) {
	for _, catalogFirst := range []bool{true, false} {
		catalog := skill.NewCatalog()
		options := []Option{WithStore(kerneltest.NewStore(t)), WithSkillCatalog(catalog)}
		if catalogFirst {
			options[0], options[1] = options[1], options[0]
		}
		engine, err := New(options...)
		if err != nil {
			t.Fatal(err)
		}
		if engine.skills != catalog {
			t.Fatalf("catalogFirst=%v: the store's catalog replaced WithSkillCatalog", catalogFirst)
		}
		engine.Close()
	}
}
