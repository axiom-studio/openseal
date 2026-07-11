package clawhub

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type installRegistry struct {
	version      string
	archive      []byte
	verification Verification
}

func TestInstallManagersSerializeOneSharedWorkspace(t *testing.T) {
	registry := &installRegistry{version: "1.0.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"}, archive: createTestZip(t, map[string]string{"SKILL.md": "---\nname: shared\ndescription: safe\n---\nSafe."})}
	workspace := t.TempDir()
	first, err := NewInstallManager("https://registry.test", registry, workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewInstallManager("https://registry.test", registry, workspace)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	failures := make(chan error, 2)
	var wait sync.WaitGroup
	for _, item := range []struct {
		manager *InstallManager
		ref     SkillReference
	}{{first, SkillReference{Owner: "alice", Slug: "one"}}, {second, SkillReference{Owner: "bob", Slug: "two"}}} {
		wait.Add(1)
		go func(item struct {
			manager *InstallManager
			ref     SkillReference
		}) {
			defer wait.Done()
			<-start
			_, installErr := item.manager.Install(context.Background(), InstallRequest{Reference: item.ref})
			failures <- installErr
		}(item)
	}
	close(start)
	wait.Wait()
	close(failures)
	for installErr := range failures {
		if installErr != nil {
			t.Fatal(installErr)
		}
	}
	lock, err := first.List()
	if err != nil || len(lock.Skills) != 2 {
		t.Fatalf("shared lock = %#v, %v", lock, err)
	}
}

func (r *installRegistry) SearchSkills(context.Context, SearchRequest) (*SkillPage, error) {
	return &SkillPage{}, nil
}
func (r *installRegistry) ExploreSkills(context.Context, ExploreRequest) (*SkillPage, error) {
	return &SkillPage{}, nil
}
func (r *installRegistry) InspectSkill(_ context.Context, ref SkillReference) (*SkillDetail, error) {
	return &SkillDetail{SkillSummary: SkillSummary{Slug: ref.Slug, Name: "Research"}, Version: r.version, Owner: ref.Owner}, nil
}
func (r *installRegistry) ListVersions(context.Context, SkillReference, int, string) (*VersionPage, error) {
	return &VersionPage{}, nil
}
func (r *installRegistry) GetVersion(context.Context, SkillReference, string) (*VersionDetail, error) {
	return &VersionDetail{Version: r.version}, nil
}
func (r *installRegistry) GetFile(context.Context, SkillReference, string, string, string) ([]byte, error) {
	return nil, ErrNotFound
}
func (r *installRegistry) VerifySkill(_ context.Context, ref SkillReference, _, _ string) (*Verification, error) {
	value := r.verification
	value.Slug = ref.Slug
	value.PublisherHandle = ref.Owner
	value.Version = r.version
	return &value, nil
}
func (r *installRegistry) DownloadArchive(context.Context, SkillReference, string, string) (*DownloadedArchive, error) {
	digest := sha256.Sum256(r.archive)
	return &DownloadedArchive{Bytes: append([]byte(nil), r.archive...), SHA256: hex.EncodeToString(digest[:])}, nil
}

func TestInstallManagerVerifiesCompilesPinsUpdatesAndUninstalls(t *testing.T) {
	registry := &installRegistry{
		version: "1.0.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"},
		archive: createTestZip(t, map[string]string{
			"SKILL.md": `---
name: research-helper
description: Monitor sources and summarize findings.
license: MIT-0
command-dispatch: tool
command-tool: source_monitor
metadata:
  openclaw:
    requires:
      bins: [curl]
---
Read references/method.md before monitoring.
`,
			"references/method.md": "Preserve source provenance.",
			"scripts/check.sh":     "#!/bin/sh\nexit 0\n",
		}),
	}
	workspace := t.TempDir()
	manager, err := NewInstallManager("https://registry.test", registry, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref := SkillReference{Owner: "acme", Slug: "research-helper"}
	installed, err := manager.Install(context.Background(), InstallRequest{Reference: ref})
	if err != nil {
		t.Fatal(err)
	}
	if !installed.Changed || installed.Version != "1.0.0" || installed.Compilation.Definition.Actions["invoke"].Name != "invoke" || len(installed.Compilation.Definition.Resources) != 2 {
		t.Fatalf("install did not compile first-class capability: %#v", installed)
	}
	if installed.Compilation.Definition.Source.Registry != "https://registry.test" || installed.Compilation.Definition.Source.Publisher != "acme" || installed.Origin.Fingerprint == "" {
		t.Fatalf("provenance missing: %#v", installed)
	}
	if _, err := os.Stat(filepath.Join(installed.Directory, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	reloaded, err := manager.LoadInstalled()
	if err != nil || len(reloaded) != 1 || reloaded[0].Compilation.Definition.Source.Trust["decision"] != "pass" {
		t.Fatalf("restart load = %#v, %v", reloaded, err)
	}
	if verified, err := manager.VerifyInstalled(context.Background(), ref.Slug); err != nil || verified.Version != "1.0.0" {
		t.Fatalf("verify installed = %#v, %v", verified, err)
	}
	lock, err := manager.List()
	identity := manager.identity(ref)
	if err != nil || lock.Skills[identity].Version == nil || *lock.Skills[identity].Version != "1.0.0" {
		t.Fatalf("lockfile = %#v, %v", lock, err)
	}
	if err := manager.Pin(ref.Slug, "security review"); err != nil {
		t.Fatal(err)
	}
	report := manager.UpdateAll(context.Background())
	if len(report.SkippedPinned) != 1 || report.SkippedPinned[0] != identity {
		t.Fatalf("pinned update-all report = %#v", report)
	}
	if _, err := manager.Update(context.Background(), ref.Slug); !errors.Is(err, ErrSkillPinned) {
		t.Fatalf("pinned update error = %v", err)
	}
	if err := manager.Unpin(ref.Slug); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed.Directory, "references", "method.md"), []byte("locally changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry.version = "1.1.0"
	if _, err := manager.Update(context.Background(), ref.Slug); !errors.Is(err, ErrSkillModified) {
		t.Fatalf("modified update error = %v", err)
	}
	updated, err := manager.Install(context.Background(), InstallRequest{Reference: ref, Force: true})
	if err != nil || !updated.Changed || updated.Version != "1.1.0" {
		t.Fatalf("forced update = %#v, %v", updated, err)
	}
	if err := manager.Uninstall(ref.Slug, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(updated.Directory); !os.IsNotExist(err) {
		t.Fatalf("skill directory still exists: %v", err)
	}
	lock, err = manager.List()
	if err != nil || len(lock.Skills) != 0 {
		t.Fatalf("lock after uninstall = %#v, %v", lock, err)
	}
}

func TestInstallManagerUsesExplicitSkillsDirectory(t *testing.T) {
	workspace := t.TempDir()
	skillsDirectory := filepath.Join(t.TempDir(), "mounted-skills")
	registry := &installRegistry{
		version:      "1.0.0",
		verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"},
		archive: createTestZip(t, map[string]string{
			"SKILL.md": "---\nname: weather\ndescription: Report current weather.\n---\nUse verified weather sources.",
		}),
	}
	manager, err := NewInstallManagerWithSkillsDirectory("https://registry.example", registry, workspace, skillsDirectory)
	if err != nil {
		t.Fatal(err)
	}

	installed, err := manager.Install(context.Background(), InstallRequest{Reference: SkillReference{Owner: "acme", Slug: "weather"}, Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(installed.Directory) != skillsDirectory || filepath.Base(installed.Directory) == "weather" {
		t.Fatalf("installed directory is not collision-safe: %q", installed.Directory)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".clawhub", "lock.json")); err != nil {
		t.Fatalf("workspace lockfile: %v", err)
	}
}

func TestInstallManagerPreservesOwnerQualifiedIdentityAndRejectsAmbiguousSlug(t *testing.T) {
	registry := &installRegistry{
		version: "1.0.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"},
		archive: createTestZip(t, map[string]string{"SKILL.md": "---\nname: foo\ndescription: owner-scoped skill\n---\nRun safely."}),
	}
	workspace := t.TempDir()
	manager, err := NewInstallManager("https://registry.test", registry, workspace)
	if err != nil {
		t.Fatal(err)
	}
	alice := SkillReference{Owner: "alice", Slug: "foo"}
	bob := SkillReference{Owner: "bob", Slug: "foo"}
	a, err := manager.Install(context.Background(), InstallRequest{Reference: alice})
	if err != nil {
		t.Fatal(err)
	}
	b, err := manager.Install(context.Background(), InstallRequest{Reference: bob})
	if err != nil {
		t.Fatal(err)
	}
	if a.Directory == b.Directory {
		t.Fatalf("owner-scoped installs collided at %q", a.Directory)
	}
	if a.SourceIdentity == b.SourceIdentity || a.SourceIdentity == "" || b.SourceIdentity == "" {
		t.Fatalf("source identities collided: %q and %q", a.SourceIdentity, b.SourceIdentity)
	}

	restarted, err := NewInstallManager("https://registry.test", registry, workspace)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.LoadInstalled()
	if err != nil || len(loaded) != 2 {
		t.Fatalf("restarted load = %#v, %v", loaded, err)
	}
	if loaded[0].SourceIdentity == loaded[1].SourceIdentity {
		t.Fatalf("restart lost source identity: %#v", loaded)
	}
	lock, err := restarted.List()
	if err != nil || len(lock.Skills) != 2 {
		t.Fatalf("lock = %#v, %v", lock, err)
	}
	if _, ok := lock.Skills[restarted.identity(alice)]; !ok {
		t.Fatalf("alice identity missing: %#v", lock.Skills)
	}
	if _, ok := lock.Skills[restarted.identity(bob)]; !ok {
		t.Fatalf("bob identity missing: %#v", lock.Skills)
	}

	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"update", func() error { _, err := restarted.Update(context.Background(), "foo"); return err }},
		{"verify", func() error { _, err := restarted.VerifyInstalled(context.Background(), "foo"); return err }},
		{"pin", func() error { return restarted.Pin("foo", "test") }},
		{"unpin", func() error { return restarted.Unpin("foo") }},
		{"uninstall", func() error { return restarted.Uninstall("foo", false) }},
	} {
		if err := operation.run(); !errors.Is(err, ErrAmbiguousSkill) {
			t.Fatalf("%s error = %v", operation.name, err)
		}
	}
	if err := restarted.Pin("@alice/foo", "review"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Unpin("@alice/foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.VerifyInstalled(context.Background(), "@bob/foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Update(context.Background(), "@alice/foo"); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Uninstall("@bob/foo", false); err != nil {
		t.Fatal(err)
	}
	remaining, err := restarted.LoadInstalled()
	if err != nil || len(remaining) != 1 || remaining[0].Reference != alice {
		t.Fatalf("remaining = %#v, %v", remaining, err)
	}
}

func TestInstallManagerMigratesLegacySlugLockAndDirectory(t *testing.T) {
	workspace := t.TempDir()
	skills := filepath.Join(workspace, "skills")
	legacy := filepath.Join(skills, "foo")
	if err := os.MkdirAll(filepath.Join(legacy, ".clawhub"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("---\nname: foo\ndescription: legacy skill\n---\nLegacy body.")
	if err := os.WriteFile(filepath.Join(legacy, "SKILL.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	fingerprint := fingerprintFiles(map[string][]byte{"SKILL.md": content})
	version := "1.0.0"
	origin := SkillOrigin{Version: 1, Registry: "https://registry.test", OwnerHandle: "alice", Slug: "foo", InstalledVersion: version, Fingerprint: fingerprint}
	if err := writeAtomicJSON(filepath.Join(legacy, ".clawhub", "origin.json"), origin); err != nil {
		t.Fatal(err)
	}
	legacyLock := Lockfile{Version: 1, Skills: map[string]LockEntry{"foo": {Version: &version, OwnerHandle: "alice"}}}
	if err := writeAtomicJSON(filepath.Join(workspace, ".clawhub", "lock.json"), legacyLock); err != nil {
		t.Fatal(err)
	}
	manager, err := NewInstallManager("https://registry.test", &installRegistry{}, workspace)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := manager.LoadInstalled()
	if err != nil || len(loaded) != 1 || loaded[0].Reference.String() != "@alice/foo" {
		t.Fatalf("migrated load = %#v, %v", loaded, err)
	}
	lock, err := manager.List()
	entry, ok := lock.Skills[manager.identity(SkillReference{Owner: "alice", Slug: "foo"})]
	if err != nil || lock.Version != 2 || !ok || entry.Directory == "" {
		t.Fatalf("migrated lock = %#v, %v", lock, err)
	}
	if _, err := os.Stat(filepath.Join(skills, entry.Directory)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy directory remains: %v", err)
	}

	restarted, _ := NewInstallManager("https://registry.test", &installRegistry{}, workspace)
	if _, err := restarted.LoadInstalled(); err != nil {
		t.Fatalf("restart after migration: %v", err)
	}
}

func TestInstallManagerCanonicalizesCaseAndRejectsTraversalReferences(t *testing.T) {
	registry := &installRegistry{version: "1.0.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"}, archive: createTestZip(t, map[string]string{"SKILL.md": "---\nname: foo\ndescription: safe\n---\nSafe."})}
	manager, _ := NewInstallManager("https://REGISTRY.test/", registry, t.TempDir())
	first, err := manager.Install(context.Background(), InstallRequest{Reference: SkillReference{Owner: "Alice", Slug: "Foo"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Install(context.Background(), InstallRequest{Reference: SkillReference{Owner: "alice", Slug: "foo"}})
	if err != nil || second.Changed || second.Directory != first.Directory {
		t.Fatalf("case canonicalization = %#v, %v", second, err)
	}
	for _, ref := range []SkillReference{{Owner: "..", Slug: "foo"}, {Owner: "alice", Slug: "../foo"}, {Owner: "alice/bob", Slug: "foo"}} {
		if _, err := manager.Install(context.Background(), InstallRequest{Reference: ref}); err == nil {
			t.Fatalf("accepted unsafe reference %#v", ref)
		}
	}
}

func TestInstallManagerFailsClosedOnVerificationAndUnsafeArchives(t *testing.T) {
	registry := &installRegistry{
		version: "1.0.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: false, Decision: "fail", Reasons: []string{"malicious"}},
		archive: createTestZip(t, map[string]string{"SKILL.md": "---\nname: unsafe\ndescription: unsafe\n---\nbody"}),
	}
	manager, err := NewInstallManager("https://registry.test", registry, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Install(context.Background(), InstallRequest{Reference: SkillReference{Owner: "acme", Slug: "unsafe"}}); !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("verification error = %v", err)
	}
	registry.verification = Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"}
	registry.archive = createTestZip(t, map[string]string{"../escape": "bad", "SKILL.md": "---\nname: unsafe\ndescription: unsafe\n---\nbody"})
	if _, err := manager.Install(context.Background(), InstallRequest{Reference: SkillReference{Owner: "acme", Slug: "unsafe"}}); err == nil {
		t.Fatal("unsafe archive path should fail")
	}
	if _, err := os.Stat(filepath.Join(manager.workspace, "escape")); !os.IsNotExist(err) {
		t.Fatalf("archive escaped workspace: %v", err)
	}
}

func createTestZip(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
