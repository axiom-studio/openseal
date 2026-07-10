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
	"testing"
)

type installRegistry struct {
	version      string
	archive      []byte
	verification Verification
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
	lock, err := manager.List()
	if err != nil || lock.Skills[ref.Slug].Version == nil || *lock.Skills[ref.Slug].Version != "1.0.0" {
		t.Fatalf("lockfile = %#v, %v", lock, err)
	}
	if err := manager.Pin(ref.Slug, "security review"); err != nil {
		t.Fatal(err)
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
