package sourceartifact_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealruntime "github.com/axiom-studio/openseal/pkg/runtime"
	kernelskill "github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/openclaw"
	"github.com/axiom-studio/openseal/pkg/skill/resourcefs"
	"github.com/axiom-studio/openseal/pkg/skill/sourceartifact"
)

func TestSourceArtifactSQLiteRestartRoundTripIsolationConcurrencyStagingAndGC(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "kernel.db")
	store, err := opensealruntime.NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	service, err := sourceartifact.NewService(store)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	bundle := representativeBundle()
	compilation, err := openclaw.Compile(bundle)
	if err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var created atomic.Int32
	errorsFound := make(chan error, workers)
	var wait sync.WaitGroup
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, wasCreated, importErr := service.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{
				Scope: scope, Compilation: compilation, ReferenceID: "install:registry/research", ReferenceKind: "installation",
			})
			if wasCreated {
				created.Add(1)
			}
			errorsFound <- importErr
		}()
	}
	wait.Wait()
	close(errorsFound)
	for importErr := range errorsFound {
		if importErr != nil {
			t.Fatalf("concurrent import failed: %v", importErr)
		}
	}
	if created.Load() != 1 {
		t.Fatalf("concurrent created count=%d", created.Load())
	}
	if _, wasCreated, err := service.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{
		Scope: scope, Compilation: compilation, ReferenceID: "catalog:research", ReferenceKind: "catalog",
	}); err != nil || wasCreated {
		t.Fatalf("second retention reference created=%t err=%v", wasCreated, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := opensealruntime.NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, _ := sourceartifact.NewService(reopened)
	exported, err := restarted.ExportOpenClaw(ctx, scope, compilation.SourceDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(exported, bundle) {
		t.Fatalf("restart export changed source bundle\n got: %#v\nwant: %#v", exported, bundle)
	}
	recompiled, err := openclaw.Compile(exported)
	if err != nil || recompiled.SourceDigest != compilation.SourceDigest || !reflect.DeepEqual(recompiled.Definition, compilation.Definition) {
		t.Fatalf("recompile digest=%q definitionEqual=%t err=%v", recompiled.SourceDigest, reflect.DeepEqual(recompiled.Definition, compilation.Definition), err)
	}

	stager, err := resourcefs.New(filepath.Join(t.TempDir(), "staged"), restarted)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := stager.StageResources(ctx, kernelskill.ResourceStageRequest{
		Scope: scope, DeploymentID: "agent-one", BindingID: "research", SkillID: compilation.Definition.ID,
		SkillVersion: compilation.Definition.Version, SourceDigest: compilation.SourceDigest, Resources: compilation.Definition.Resources,
	})
	if err != nil || stage.Root == "" {
		t.Fatalf("restart resource stage=%#v err=%v", stage, err)
	}
	if _, err := restarted.ExportOpenClaw(ctx, capability.ScopeReference{Kind: "tenant", ID: "two"}, compilation.SourceDigest); !errors.Is(err, sourceartifact.ErrNotFound) {
		t.Fatalf("cross-tenant export error=%v", err)
	}
	if _, err := restarted.ExportOpenClaw(ctx, scope, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); !errors.Is(err, sourceartifact.ErrNotFound) {
		t.Fatalf("missing export error=%v", err)
	}

	stored, err := restarted.Get(ctx, scope, compilation.SourceDigest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.ImportSourceArtifact(ctx, stored, &sourceartifact.Reference{
		Scope: scope, Digest: stored.Digest, ID: "catalog:research", Kind: "catalog", CreatedAt: time.Now().UTC(),
		Origin: sourceartifact.Origin{Registry: "https://untrusted.example"},
	}); err != nil {
		t.Fatalf("retention owner provenance refresh error=%v", err)
	}
	refreshed, err := restarted.ExportOpenClawForReference(ctx, scope, compilation.SourceDigest, "catalog:research")
	if err != nil || refreshed.Source.Registry != "https://untrusted.example" {
		t.Fatalf("refreshed reference origin=%#v err=%v", refreshed.Source, err)
	}
	corrupt := sourceartifact.CloneArtifact(stored)
	corrupt.Files[1].Content[0] ^= 0xff
	if _, err := reopened.ImportSourceArtifact(ctx, corrupt, &sourceartifact.Reference{
		Scope: scope, Digest: corrupt.Digest, ID: "corrupt", Kind: "test", CreatedAt: time.Now().UTC(),
	}); err == nil {
		t.Fatal("content mutation was accepted")
	}

	if err := restarted.Release(ctx, scope, compilation.SourceDigest, "install:registry/research"); err != nil {
		t.Fatal(err)
	}
	held, err := restarted.GarbageCollect(ctx, time.Now().UTC().Add(time.Hour), 0, 10)
	if err != nil || held.DeletedArtifacts != 0 {
		t.Fatalf("referenced GC=%#v err=%v", held, err)
	}
	if err := restarted.Release(ctx, scope, compilation.SourceDigest, "catalog:research"); err != nil {
		t.Fatal(err)
	}
	collected, err := restarted.GarbageCollect(ctx, time.Now().UTC().Add(time.Hour), 0, 10)
	if err != nil || collected.DeletedArtifacts != 1 {
		t.Fatalf("unreferenced GC=%#v err=%v", collected, err)
	}
	if _, err := restarted.Get(ctx, scope, compilation.SourceDigest); !errors.Is(err, sourceartifact.ErrNotFound) {
		t.Fatalf("collected artifact error=%v", err)
	}

	expires := time.Now().UTC().Add(time.Minute)
	if _, created, err := restarted.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{
		Scope: scope, Compilation: compilation, ReferenceID: "inspection", ReferenceKind: "temporary", ReferenceExpiresAt: &expires,
	}); err != nil || !created {
		t.Fatalf("temporary reimport created=%t err=%v", created, err)
	}
	expired, err := restarted.GarbageCollect(ctx, expires.Add(time.Minute), 0, 10)
	if err != nil || expired.ExpiredReferences != 1 || expired.DeletedArtifacts != 1 {
		t.Fatalf("expired GC=%#v err=%v", expired, err)
	}
}

func TestSourceArtifactDeduplicatesBytesAndDisambiguatesPublisherProvenance(t *testing.T) {
	ctx := context.Background()
	store, err := opensealruntime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, _ := sourceartifact.NewService(store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	aliceBundle := representativeBundle()
	aliceBundle.Source.Publisher = "alice"
	aliceBundle.Source.Reference = "@alice/research"
	alice, err := openclaw.Compile(aliceBundle)
	if err != nil {
		t.Fatal(err)
	}
	bobBundle := representativeBundle()
	bobBundle.Source.Publisher = "bob"
	bobBundle.Source.Reference = "@bob/research"
	bob, err := openclaw.Compile(bobBundle)
	if err != nil {
		t.Fatal(err)
	}
	if alice.SourceDigest != bob.SourceDigest {
		t.Fatalf("byte-identical sources have different content digests: %s != %s", alice.SourceDigest, bob.SourceDigest)
	}
	if _, created, err := service.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{Scope: scope, Compilation: alice, ReferenceID: "install:alice", ReferenceKind: "installation"}); err != nil || !created {
		t.Fatalf("alice import created=%t err=%v", created, err)
	}
	if _, created, err := service.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{Scope: scope, Compilation: bob, ReferenceID: "install:bob", ReferenceKind: "installation"}); err != nil || created {
		t.Fatalf("bob import created=%t err=%v", created, err)
	}
	if _, err := service.ExportOpenClaw(ctx, scope, alice.SourceDigest); !errors.Is(err, sourceartifact.ErrAmbiguousOrigin) {
		t.Fatalf("ambiguous digest-only export error=%v", err)
	}
	for _, testCase := range []struct {
		name      string
		reference string
		want      openclaw.Bundle
	}{
		{name: "alice", reference: "install:alice", want: aliceBundle},
		{name: "bob", reference: "install:bob", want: bobBundle},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			exported, err := service.ExportOpenClawForReference(ctx, scope, alice.SourceDigest, testCase.reference)
			if err != nil || !reflect.DeepEqual(exported, testCase.want) {
				t.Fatalf("publisher export equal=%t err=%v", reflect.DeepEqual(exported, testCase.want), err)
			}
		})
	}
}

func TestSourceArtifactReferenceMovesAtomicallyAcrossSkillUpdate(t *testing.T) {
	ctx := context.Background()
	store, err := opensealruntime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, _ := sourceartifact.NewService(store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	firstBundle := representativeBundle()
	first, _ := openclaw.Compile(firstBundle)
	secondBundle := representativeBundle()
	secondBundle.Source.Version = "2.0.0"
	secondBundle.Files[0].Content = []byte("cite sources and preserve counter-evidence\n")
	second, _ := openclaw.Compile(secondBundle)
	const referenceID = "install:registry/research"
	if _, created, err := service.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{Scope: scope, Compilation: first, ReferenceID: referenceID, ReferenceKind: "installation"}); err != nil || !created {
		t.Fatalf("first import created=%t err=%v", created, err)
	}
	if _, created, err := service.ImportOpenClaw(ctx, sourceartifact.ImportOpenClawRequest{Scope: scope, Compilation: second, ReferenceID: referenceID, ReferenceKind: "installation"}); err != nil || !created {
		t.Fatalf("updated import created=%t err=%v", created, err)
	}
	report, err := service.GarbageCollect(ctx, time.Now().UTC().Add(time.Hour), 0, 10)
	if err != nil || report.DeletedArtifacts != 1 {
		t.Fatalf("old version collection=%#v err=%v", report, err)
	}
	if _, err := service.ExportOpenClaw(ctx, scope, first.SourceDigest); !errors.Is(err, sourceartifact.ErrNotFound) {
		t.Fatalf("old source export error=%v", err)
	}
	if exported, err := service.ExportOpenClaw(ctx, scope, second.SourceDigest); err != nil || !reflect.DeepEqual(exported, secondBundle) {
		t.Fatalf("updated source export equal=%t err=%v", reflect.DeepEqual(exported, secondBundle), err)
	}
	if err := service.Release(ctx, scope, second.SourceDigest, referenceID); err != nil {
		t.Fatal(err)
	}
}

func representativeBundle() openclaw.Bundle {
	return openclaw.Bundle{
		SkillMD: []byte("---\nname: research\ndescription: Preserve cited research evidence.\nmetadata:\n  openclaw:\n    requires:\n      bins: [curl]\n---\nRead references/guide.md, then run scripts/collect.sh when governed."),
		Files: []openclaw.File{
			{Path: "references/guide.md", Content: []byte("cite every factual claim\n")},
			{Path: "scripts/collect.sh", Content: []byte("#!/bin/sh\nset -eu\nprintf '%s\\n' collected\n")},
			{Path: "assets/template.txt", Content: []byte{0x00, 0x01, 0x7f, 0xff}},
		},
		Source: openclaw.Source{
			Registry: "https://registry.example", Publisher: "acme", Reference: "@acme/research", ExpectedName: "research", Version: "1.2.3",
			Trust: map[string]interface{}{"verified": true, "scanner": map[string]interface{}{"decision": "pass"}},
		},
	}
}
