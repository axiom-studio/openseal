package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestDefinitionCompilationsAreImmutableScopedAndOrdered(t *testing.T) {
	registry := NewRegistry()
	registry.now = func() time.Time { return time.Date(2026, 7, 12, 8, 0, 0, 0, time.UTC) }
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	failed := &DefinitionCompilation{
		ID: "graph-v1-failed", Scope: scope, DeploymentID: "agent-39", DefinitionID: "tenant-one-agent-39", CandidateVersion: "legacy-1",
		Source: CompilationSource{Kind: "visual_graph", ID: "library-7", Version: "1", Digest: "sha256:source-1"}, Status: CompilationFailed,
		Diagnostics: []CompilationDiagnostic{{NodeID: "telegram", Path: "nodes.telegram", Code: "action.unavailable", Message: "Telegram action is unavailable"}},
	}
	first, err := registry.RecordCompilation(context.Background(), failed)
	if err != nil || first.CreatedAt.IsZero() {
		t.Fatalf("failed compilation = %#v, %v", first, err)
	}
	registry.now = func() time.Time { return first.CreatedAt.Add(time.Hour) }
	if replay, err := registry.RecordCompilation(context.Background(), failed); err != nil || replay.ID != first.ID || !replay.CreatedAt.Equal(first.CreatedAt) {
		t.Fatalf("idempotent replay = %#v, %v", replay, err)
	}
	changed := cloneCompilation(first)
	changed.Diagnostics[0].Message = "changed"
	if _, err := registry.RecordCompilation(context.Background(), changed); !errors.Is(err, ErrCompilationImmutable) {
		t.Fatalf("changed immutable record error = %v", err)
	}
	clean := &DefinitionCompilation{
		ID: "graph-v2-clean", Scope: scope, DeploymentID: "agent-39", DefinitionID: "tenant-one-agent-39", CandidateVersion: "legacy-2",
		Source: CompilationSource{Kind: "visual_graph", ID: "library-7", Version: "2", Digest: "sha256:source-2"}, TargetDigest: "sha256:target-2", Status: CompilationClean,
		CreatedAt: first.CreatedAt.Add(time.Minute),
	}
	if _, err := registry.RecordCompilation(context.Background(), clean); err != nil {
		t.Fatal(err)
	}
	values, err := registry.ListCompilations(context.Background(), scope, "agent-39")
	if err != nil || len(values) != 2 || values[0].ID != clean.ID || values[1].ID != first.ID {
		t.Fatalf("ordered compilations = %#v, %v", values, err)
	}
	if values, err := registry.ListCompilations(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, "agent-39"); err != nil || len(values) != 0 {
		t.Fatalf("cross-tenant compilations = %#v, %v", values, err)
	}
}

func TestDefinitionCompilationStatusIsTruthful(t *testing.T) {
	base := DefinitionCompilation{ID: "candidate", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent", DefinitionID: "definition", CandidateVersion: "1", Source: CompilationSource{Kind: "prompt", ID: "source", Version: "1", Digest: "sha256:source"}, CreatedAt: time.Now().UTC()}
	base.Status = CompilationClean
	if err := base.Validate(); err == nil {
		t.Fatal("clean compilation without target digest should fail")
	}
	base.Status = CompilationFailed
	base.Diagnostics = []CompilationDiagnostic{{Path: "source", Code: "compile.failed", Message: "unsupported"}}
	base.TargetDigest = "sha256:must-not-exist"
	if err := base.Validate(); err == nil {
		t.Fatal("failed compilation with target digest should fail")
	}
}
