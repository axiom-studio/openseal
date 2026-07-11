package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCatalogResolvesDeclaredNamesByRootPrecedence(t *testing.T) {
	workspace := t.TempDir()
	project := t.TempDir()
	managed := t.TempDir()
	extra := t.TempDir()
	writeSkill(t, filepath.Join(extra, "shared"), "shared", "extra", nil)
	writeSkill(t, filepath.Join(managed, "shared"), "shared", "managed", nil)
	writeSkill(t, filepath.Join(project, "group", "shared-directory"), "shared", "project", map[string]string{"references/guide.md": "project guide"})
	writeSkill(t, filepath.Join(workspace, "different-directory"), "shared", "workspace", nil)
	writeSkill(t, filepath.Join(managed, "managed-only"), "managed-only", "managed only", nil)
	writeSkill(t, filepath.Join(workspace, "too", "deep", "ignored"), "ignored", "too deep", nil)

	snapshot, err := NewCatalog().Discover(context.Background(), []Root{
		{ID: "extra", Kind: RootExtra, Path: extra},
		{ID: "managed", Kind: RootManaged, Path: managed},
		{ID: "project", Kind: RootProjectAgent, Path: project},
		{ID: "workspace", Kind: RootWorkspace, Path: workspace},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision == "" || len(snapshot.Effective) != 2 || len(snapshot.Shadowed) != 3 {
		t.Fatalf("unexpected source snapshot: %#v", snapshot)
	}
	if snapshot.Effective[0].Name != "managed-only" || snapshot.Effective[1].Name != "shared" || snapshot.Effective[1].RootID != "workspace" || snapshot.Effective[1].Description != "workspace" {
		t.Fatalf("precedence did not select workspace declaration: %#v", snapshot.Effective)
	}
	for _, shadowed := range snapshot.Shadowed {
		if shadowed.Candidate.Name == "shared" && shadowed.ShadowedBy != "workspace" {
			t.Fatalf("shadowed provenance = %#v", shadowed)
		}
	}
	for _, candidate := range snapshot.Effective {
		if candidate.Name == "ignored" {
			t.Fatal("discovery must not recurse beyond one grouping level")
		}
	}
}

func TestCatalogEnforcesSymlinkAndSupportingResourceContainment(t *testing.T) {
	root := t.TempDir()
	external := t.TempDir()
	writeSkill(t, filepath.Join(external, "linked"), "linked", "linked source", nil)
	if err := os.Symlink(filepath.Join(external, "linked"), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}

	catalog := NewCatalog()
	rejected, err := catalog.Discover(context.Background(), []Root{{ID: "workspace", Kind: RootWorkspace, Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rejected.Effective) != 0 || !hasDiagnostic(rejected, "skill.unsafe_path") {
		t.Fatalf("untrusted directory symlink was accepted: %#v", rejected)
	}
	allowed, err := catalog.Discover(context.Background(), []Root{{
		ID: "workspace", Kind: RootWorkspace, Path: root, AllowSymlinkTargets: []string{external},
	}})
	if err != nil || len(allowed.Effective) != 1 || allowed.Effective[0].Directory != filepath.Join(external, "linked") {
		t.Fatalf("explicit symlink target was not accepted: %#v, %v", allowed, err)
	}

	resourceRoot := t.TempDir()
	writeSkill(t, filepath.Join(resourceRoot, "unsafe-resource"), "unsafe-resource", "unsafe resource", nil)
	if err := os.Symlink(filepath.Join(external, "linked", "SKILL.md"), filepath.Join(resourceRoot, "unsafe-resource", "references.md")); err != nil {
		t.Fatal(err)
	}
	resources, err := catalog.Discover(context.Background(), []Root{{ID: "resources", Kind: RootWorkspace, Path: resourceRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Effective) != 0 || !hasDiagnostic(resources, "skill.load_failed") {
		t.Fatalf("supporting resource symlink was accepted: %#v", resources)
	}

	skillLinkRoot := t.TempDir()
	directory := filepath.Join(skillLinkRoot, "escaped-skill-file")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(external, "linked", "SKILL.md"), filepath.Join(directory, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	escaped, err := catalog.Discover(context.Background(), []Root{{ID: "skill-file", Kind: RootWorkspace, Path: skillLinkRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if len(escaped.Effective) != 0 || !hasDiagnostic(escaped, "skill.unsafe_path") {
		t.Fatalf("escaping SKILL.md symlink was accepted: %#v", escaped)
	}
}

func TestWatcherEmitsOnlyEffectiveSurfaceChanges(t *testing.T) {
	workspace := t.TempDir()
	managed := t.TempDir()
	workspaceSkill := filepath.Join(workspace, "research")
	managedSkill := filepath.Join(managed, "research")
	writeSkill(t, workspaceSkill, "research", "workspace v1", nil)
	writeSkill(t, managedSkill, "research", "managed v1", nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher, err := NewWatcher(NewCatalog(), []Root{
		{ID: "workspace", Kind: RootWorkspace, Path: workspace},
		{ID: "managed", Kind: RootManaged, Path: managed},
	}, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	changes := watcher.Watch(ctx)
	initial := receiveChange(t, changes, time.Second)
	if len(initial.Added) != 1 || initial.Added[0] != "research" || initial.Snapshot.Effective[0].RootID != "workspace" {
		t.Fatalf("initial change = %#v", initial)
	}

	writeSkill(t, managedSkill, "research", "managed shadow edit", nil)
	assertNoChange(t, changes, 80*time.Millisecond)
	writeSkill(t, workspaceSkill, "research", "workspace v2", nil)
	updated := receiveChange(t, changes, time.Second)
	if len(updated.Updated) != 1 || updated.Updated[0] != "research" || updated.PreviousRevision != initial.Revision {
		t.Fatalf("winner update = %#v", updated)
	}
	if err := os.RemoveAll(workspaceSkill); err != nil {
		t.Fatal(err)
	}
	promoted := receiveChange(t, changes, time.Second)
	if len(promoted.Updated) != 1 || promoted.Snapshot.Effective[0].RootID != "managed" || promoted.Snapshot.Effective[0].Description != "managed shadow edit" {
		t.Fatalf("fallback promotion = %#v", promoted)
	}
	cancel()
	for range changes {
	}
}

func TestCatalogIndexesBundleResourcesAndStableRevision(t *testing.T) {
	root := t.TempDir()
	writeSkill(t, filepath.Join(root, "reporter"), "reporter", "report", map[string]string{
		"references/policy.md": "policy", "scripts/check.sh": "#!/bin/sh\n",
	})
	catalog := NewCatalog()
	first, err := catalog.Discover(context.Background(), []Root{{ID: "workspace", Kind: RootWorkspace, Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Discover(context.Background(), []Root{{ID: "workspace", Kind: RootWorkspace, Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != second.Revision || len(first.Effective) != 1 || len(first.Effective[0].Compilation.Definition.Resources) != 2 {
		t.Fatalf("unstable or incomplete source snapshot: %#v %#v", first, second)
	}
	if !strings.HasPrefix(first.Effective[0].Compilation.Definition.Source.Reference, "workspace:workspace:") {
		t.Fatalf("source provenance missing: %#v", first.Effective[0].Compilation.Definition.Source)
	}
}

func TestCatalogPreservesInstalledClawHubOriginAndVersion(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "summarize")
	if err := os.MkdirAll(filepath.Join(skillDir, ".clawhub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSkill(t, skillDir, "summarize", "Summarize safely.", nil)
	origin := `{"version":1,"registry":"https://clawhub.ai","slug":"summarize","ownerHandle":"seanford","installedVersion":"0.1.0","fingerprint":"fp-1","archiveSha256":"` + strings.Repeat("a", 64) + `"}`
	if err := os.WriteFile(filepath.Join(skillDir, ".clawhub", "origin.json"), []byte(origin), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewCatalog().Discover(context.Background(), []Root{{ID: "managed", Kind: RootManaged, Path: root}})
	if err != nil || len(snapshot.Effective) != 1 {
		t.Fatalf("snapshot = %#v, %v", snapshot, err)
	}
	candidate := snapshot.Effective[0]
	source := candidate.Compilation.Definition.Source
	if candidate.Version != "0.1.0+source."+candidate.Digest[:12] || source == nil || source.Registry != "https://clawhub.ai" || source.Publisher != "seanford" ||
		source.Reference != "seanford/summarize" || source.ResolvedVersion != "0.1.0" || source.Trust["fingerprint"] != "fp-1" {
		t.Fatalf("installed origin was not preserved: candidate=%#v source=%#v", candidate, source)
	}
	if err := os.WriteFile(filepath.Join(skillDir, ".clawhub", "origin.json"), []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid, err := NewCatalog().Discover(context.Background(), []Root{{ID: "managed", Kind: RootManaged, Path: root}})
	if err != nil || len(invalid.Effective) != 0 || len(invalid.Diagnostics) != 1 || invalid.Diagnostics[0].Code != "source.origin_invalid" {
		t.Fatalf("invalid installed origin did not fail closed: %#v, %v", invalid, err)
	}
}

func writeSkill(t *testing.T, directory, name, description string, resources map[string]string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\nUse the skill safely.\n"
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, value := range resources {
		absolute := filepath.Join(directory, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(absolute, []byte(value), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func hasDiagnostic(snapshot *Snapshot, code string) bool {
	for _, diagnostic := range snapshot.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func receiveChange(t *testing.T, changes <-chan Change, timeout time.Duration) Change {
	t.Helper()
	select {
	case change, ok := <-changes:
		if !ok {
			t.Fatal("watcher closed before change")
		}
		return change
	case <-time.After(timeout):
		t.Fatal("timed out waiting for skill source change")
		return Change{}
	}
}

func assertNoChange(t *testing.T, changes <-chan Change, duration time.Duration) {
	t.Helper()
	select {
	case change := <-changes:
		t.Fatalf("unexpected effective source change: %#v", change)
	case <-time.After(duration):
	}
}
