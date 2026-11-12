package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// The skill definition identity probe contract both SQL stores run.

// SkillIdentityProbeStore is a skill catalog store under test.
type SkillIdentityProbeStore interface {
	skill.CatalogStore
	ListSkillDefinitionIdentities(context.Context, string, string) ([]capability.SkillIdentity, error)
}

// SkillIdentityProbeDefinition is a prompt skill definition, installed from
// source when source is set.
func SkillIdentityProbeDefinition(id, version, source string) *skill.Definition {
	definition := &skill.Definition{
		ID: id, Version: version, Name: "Research",
		Prompt: &skill.PromptModule{Instructions: "Use the exact installed source."},
	}
	if source != "" {
		definition.Source = &skill.SourceProvenance{Identity: source, Format: "openclaw.skill.v1"}
	}
	return definition
}

// RunSkillDefinitionIdentityProbeContract checks the identities store lists
// for its skill definitions; unrelated is an independent store, which must
// list none of them.
func RunSkillDefinitionIdentityProbeContract(t *testing.T, store, unrelated SkillIdentityProbeStore) {
	t.Helper()
	ctx := context.Background()
	writer := skill.NewCatalogWithStore(store)
	probe := func(id, version string) []capability.SkillIdentity {
		t.Helper()
		identities, err := store.ListSkillDefinitionIdentities(ctx, id, version)
		if err != nil {
			t.Fatal(err)
		}
		return identities
	}
	if got := probe("research", "1.0.0"); len(got) != 0 {
		t.Fatalf("empty store identities = %#v", got)
	}
	for _, definition := range []*skill.Definition{
		SkillIdentityProbeDefinition("research", "1.0.0", "catalog::@zoe/research"),
		SkillIdentityProbeDefinition("research", "1.0.0", ""),
		SkillIdentityProbeDefinition("researcher", "1.0.0", "catalog::@other/research"),
		SkillIdentityProbeDefinition("research", "1.0.00", "catalog::@other/research"),
	} {
		if err := writer.Register(ctx, definition); err != nil {
			t.Fatal(err)
		}
	}
	want := []capability.SkillIdentity{
		{ID: "research", Version: "1.0.0", SourceIdentity: ""},
		{ID: "research", Version: "1.0.0", SourceIdentity: "catalog::@zoe/research"},
	}
	if got := probe("research", "1.0.0"); !reflect.DeepEqual(got, want) {
		t.Fatalf("exact identity probe = %#v, want %#v", got, want)
	}
	if got := probe("missing", "1.0.0"); len(got) != 0 {
		t.Fatalf("foreign ID identities = %#v", got)
	}
	if got := probe("research", "1.0.1"); len(got) != 0 {
		t.Fatalf("foreign version identities = %#v", got)
	}
	// A different catalog can install a publisher immediately after a probe.
	// Selection metadata must never cache the former singleton variant set.
	otherWriter := skill.NewCatalogWithStore(store)
	if err := otherWriter.Register(ctx, SkillIdentityProbeDefinition("research", "1.0.0", "catalog::@alice/research")); err != nil {
		t.Fatal(err)
	}
	want = append(want[:1], capability.SkillIdentity{ID: "research", Version: "1.0.0", SourceIdentity: "catalog::@alice/research"}, want[1])
	got := probe("research", "1.0.0")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("new publisher identities = %#v, want %#v", got, want)
	}
	got[0] = capability.SkillIdentity{ID: "mutated", Version: "mutated", SourceIdentity: "mutated"}
	got = append(got, capability.SkillIdentity{ID: "injected", Version: "injected"})
	if again := probe("research", "1.0.0"); !reflect.DeepEqual(again, want) {
		t.Fatalf("returned identity mutation changed store = %#v", again)
	}
	// Metadata is global to one definition store, not tenant scoped or shared
	// across independent stores using the same declared ID and version.
	if identities, err := unrelated.ListSkillDefinitionIdentities(ctx, "research", "1.0.0"); err != nil || len(identities) != 0 {
		t.Fatalf("independent store identities = %#v, %v", identities, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if identities, err := store.ListSkillDefinitionIdentities(canceled, "research", "1.0.0"); !errors.Is(err, context.Canceled) || len(identities) != 0 {
		t.Fatalf("canceled identity probe = %#v, %v", identities, err)
	}
	expired, expire := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer expire()
	if identities, err := store.ListSkillDefinitionIdentities(expired, "research", "1.0.0"); !errors.Is(err, context.DeadlineExceeded) || len(identities) != 0 {
		t.Fatalf("expired identity probe = %#v, %v", identities, err)
	}
}
