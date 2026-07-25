package clawhub

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
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

func TestPreviewCompilesWithoutInstallingAndPinsInstallationArtifact(t *testing.T) {
	registry := &installRegistry{
		version: "2.1.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"},
		archive: createTestZip(t, map[string]string{
			"SKILL.md": `---
name: reddit-research
description: Monitor configured communities and classify feedback.
command-dispatch: tool
command-tool: reddit_api
allowed-tools: [reddit_api]
metadata:
  openclaw:
    primaryEnv: REDDIT_API_TOKEN
---
PRIVATE_PREVIEW_PROMPT_BODY must not enter the control-plane projection.
`,
			"references/method.md": "Preserve source provenance.",
		}),
	}
	workspace := t.TempDir()
	manager, err := NewInstallManager("https://registry.test", registry, workspace)
	if err != nil {
		t.Fatal(err)
	}
	reference := SkillReference{Owner: "acme", Slug: "reddit-research"}
	preview, err := manager.Preview(context.Background(), PreviewRequest{Reference: reference})
	if err != nil {
		t.Fatal(err)
	}
	if preview.APIVersion != CompilationPreviewAPIVersion || !preview.Compatible || !preview.PromptAvailable || preview.Receipt.SourceIdentity != manager.identity(reference) ||
		preview.Receipt.Version != "2.1.0" || len(preview.Receipt.SourceDigest) != 64 || len(preview.Receipt.ArchiveSHA256) != 64 ||
		!strings.HasPrefix(preview.Receipt.CompilationDigest, "sha256:") {
		t.Fatalf("preview identity is incomplete: %#v", preview)
	}
	if action, ok := preview.Actions["invoke"]; !ok || action.Name != "invoke" || len(action.Credentials) != 1 {
		t.Fatalf("exact compiled action missing: %#v", preview.Actions)
	}
	if len(preview.CredentialRequirements) != 1 || preview.CredentialRequirements[0].Name != "REDDIT_API_TOKEN" {
		t.Fatalf("credential requirements = %#v", preview.CredentialRequirements)
	}
	encoded, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "PRIVATE_PREVIEW_PROMPT_BODY") || strings.Contains(string(encoded), "credentialReference") {
		t.Fatalf("preview leaked source content or credential references: %s", encoded)
	}
	lock, err := manager.List()
	if err != nil || len(lock.Skills) != 0 {
		t.Fatalf("preview mutated installation state: %#v, %v", lock, err)
	}

	installed, err := manager.Install(context.Background(), InstallRequest{
		Reference: reference, Version: preview.Receipt.Version, PreviewReceipt: &preview.Receipt,
	})
	if err != nil || !installed.Changed || installed.Compilation.SourceDigest != preview.Receipt.SourceDigest {
		t.Fatalf("receipt-pinned install = %#v, %v", installed, err)
	}
	tamperedReceipt := preview.Receipt
	tamperedReceipt.CompilationDigest = "sha256:" + strings.Repeat("0", 64)
	if _, err := manager.Install(context.Background(), InstallRequest{
		Reference: reference, Version: preview.Receipt.Version, Force: true, PreviewReceipt: &tamperedReceipt,
	}); !errors.Is(err, ErrCompilationPreviewMismatch) {
		t.Fatalf("tampered compilation projection was accepted: %v", err)
	}

	registry.archive = createTestZip(t, map[string]string{
		"SKILL.md": `---
name: reddit-research
description: Mutated after preview.
command-dispatch: tool
command-tool: reddit_api
---
Changed bytes.
`,
	})
	if _, err := manager.Install(context.Background(), InstallRequest{
		Reference: reference, Version: preview.Receipt.Version, Force: true, PreviewReceipt: &preview.Receipt,
	}); !errors.Is(err, ErrCompilationPreviewMismatch) {
		t.Fatalf("mutated preview artifact was accepted: %v", err)
	}
}

func TestPreviewMarksUnadaptedExternalPromptIncompatibleWithDiagnostic(t *testing.T) {
	registry := &installRegistry{
		version: "1.0.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"},
		archive: createTestZip(t, map[string]string{"SKILL.md": `---
name: external-prompt
description: Needs a governed adapter.
allowed-tools: [reddit_api]
metadata:
  openclaw:
    primaryEnv: REDDIT_API_TOKEN
---
Use the external service.
`}),
	}
	manager, err := NewInstallManager("https://registry.test", registry, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Preview(context.Background(), PreviewRequest{Reference: SkillReference{Owner: "acme", Slug: "external-prompt"}})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Compatible {
		t.Fatalf("unadapted external prompt was presented as compatible: %#v", preview)
	}
	found := false
	for _, diagnostic := range preview.Diagnostics {
		found = found || diagnostic.Code == "needs_action_adapter"
	}
	if !found {
		t.Fatalf("actionable incompatibility diagnostic missing: %#v", preview.Diagnostics)
	}
}

func TestPreviewCompilesDeclarativeRedditHelperIntoGovernedReadAction(t *testing.T) {
	registry := &installRegistry{
		version: "1.0.0", verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"},
		archive: createTestZip(t, map[string]string{
			"SKILL.md": `---
name: Reddit Keyword Search API
description: Search Reddit through JustOneAPI.
metadata:
  openclaw:
    primaryEnv: JUST_ONE_API_TOKEN
    requires:
      bins: [node]
      env: [JUST_ONE_API_TOKEN]
---
Supported operation IDs in this skill: searchRedditV1.

node {baseDir}/bin/run.mjs --operation "searchRedditV1" --token "$JUST_ONE_API_TOKEN" --params-json '{"keyword":"<keyword>"}'
`,
			"bin/run.mjs": `const manifest = {
  "baseUrl":"https://api.justoneapi.com",
  "slug":"justoneapi-reddit-search",
  "operations":[{
    "method":"GET","operationId":"searchRedditV1","path":"/api/reddit/search/v1","requestBody":null,
    "description":"Search Reddit by keyword.",
    "parameters":[
      {"name":"token","location":"query","required":true,"schemaType":"string"},
      {"name":"keyword","location":"query","required":true,"schemaType":"string"}
    ]
  }]
};`,
		}),
	}
	manager, err := NewInstallManager("https://registry.test", registry, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	preview, err := manager.Preview(context.Background(), PreviewRequest{Reference: SkillReference{Owner: "justoneapi", Slug: "justoneapi-reddit-search"}})
	if err != nil {
		t.Fatal(err)
	}
	action, ok := preview.Actions["searchRedditV1"]
	if !preview.Compatible || !ok || action.SideEffect != capability.SideEffectRead || action.Risk != capability.RiskLevelRead ||
		len(preview.CredentialRequirements) != 1 || preview.CredentialRequirements[0].Name != "JUST_ONE_API_TOKEN" {
		t.Fatalf("Reddit helper preview = %#v", preview)
	}
	for _, diagnostic := range preview.Diagnostics {
		if diagnostic.Code == "needs_action_adapter" {
			t.Fatalf("semantics-preserving helper remained unavailable: %#v", preview.Diagnostics)
		}
	}
}

func TestInstallManagerRecompilesSameArtifactIdempotentlyAcrossResolutionPaths(t *testing.T) {
	registry := &installRegistry{
		version: "1.0.0",
		verification: Verification{
			Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass", ResolvedFrom: "latest", CreatedAt: 10,
			Security: map[string]interface{}{"status": "clean", "checkedAt": float64(20)},
		},
		archive: createTestZip(t, map[string]string{"SKILL.md": "---\nname: summarize\ndescription: Summarize evidence.\n---\nSummarize carefully.\n"}),
	}
	manager, err := NewInstallManager("https://registry.test", registry, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := SkillReference{Owner: "seanford", Slug: "summarize"}
	first, err := manager.Install(context.Background(), InstallRequest{Reference: ref})
	if err != nil {
		t.Fatal(err)
	}
	registry.verification.ResolvedFrom = "version"
	registry.verification.CreatedAt = 999
	registry.verification.Security = map[string]interface{}{"status": "clean", "checkedAt": float64(1000)}
	replayed, err := manager.Install(context.Background(), InstallRequest{Reference: ref, Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Changed || first.Compilation.Definition.Version != replayed.Compilation.Definition.Version || !reflect.DeepEqual(first.Compilation.Definition, replayed.Compilation.Definition) {
		t.Fatalf("same artifact replay changed canonical definition: first=%#v replayed=%#v", first.Compilation.Definition, replayed.Compilation.Definition)
	}
}

func TestLifecycleReconciliationRollsBackAndCompletesInterruptedMutations(t *testing.T) {
	registry := &installRegistry{
		version:      "1.0.0",
		verification: Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass"},
		archive:      createTestZip(t, map[string]string{"SKILL.md": "---\nname: durable\ndescription: durable skill\n---\nOriginal."}),
	}
	workspace := t.TempDir()
	manager, err := NewInstallManager("https://registry.test", registry, workspace)
	if err != nil {
		t.Fatal(err)
	}
	ref := SkillReference{Owner: "acme", Slug: "durable"}
	installed, err := manager.Install(context.Background(), InstallRequest{Reference: ref})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := manager.List()
	if err != nil {
		t.Fatal(err)
	}
	identity := manager.identity(ref)
	original := lock.Skills[identity]

	// Crash after the old directory moved and a replacement became visible,
	// but before the lockfile commit: restart must restore the old directory.
	backup := installed.Directory + ".backup-crash"
	if err := os.Rename(installed.Directory, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(installed.Directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installed.Directory, "partial"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	newVersion := "2.0.0"
	desired := original
	desired.Version = &newVersion
	if err := manager.writeIntent(lifecycleIntent{Kind: "install", Identity: identity, Target: installed.Directory, Backup: backup, Desired: &desired}); err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewInstallManager("https://registry.test", registry, workspace)
	if err := restarted.ReconcileLifecycle(); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(installed.Directory, "SKILL.md")); err != nil || !bytes.Contains(content, []byte("Original")) {
		t.Fatalf("rollback content=%q err=%v", content, err)
	}

	// Crash after the lockfile commit but before old-directory cleanup: restart
	// must preserve the committed target and remove the leftover backup.
	backup = installed.Directory + ".backup-committed"
	if err := os.MkdirAll(backup, 0o755); err != nil {
		t.Fatal(err)
	}
	committedEntry := original
	if err := manager.writeIntent(lifecycleIntent{Kind: "install", Identity: identity, Target: installed.Directory, Backup: backup, Desired: &committedEntry}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileLifecycle(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("committed install backup remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(installed.Directory, "SKILL.md")); err != nil {
		t.Fatalf("committed install target removed: %v", err)
	}

	// Crash after uninstall moved the directory but before committing the
	// lockfile: restart must put it back.
	trash := installed.Directory + ".remove-crash"
	if err := os.Rename(installed.Directory, trash); err != nil {
		t.Fatal(err)
	}
	if err := manager.writeIntent(lifecycleIntent{Kind: "uninstall", Identity: identity, Target: installed.Directory, Trash: trash}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileLifecycle(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(installed.Directory, "SKILL.md")); err != nil {
		t.Fatalf("uninstall rollback did not restore target: %v", err)
	}

	// Crash after the lockfile commit: restart must finish deleting trash.
	if err := os.Rename(installed.Directory, trash); err != nil {
		t.Fatal(err)
	}
	committedLock, _ := manager.readLockfile()
	delete(committedLock.Skills, identity)
	if err := manager.writeLockfile(committedLock); err != nil {
		t.Fatal(err)
	}
	if err := manager.writeIntent(lifecycleIntent{Kind: "uninstall", Identity: identity, Target: installed.Directory, Trash: trash}); err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReconcileLifecycle(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(trash); !os.IsNotExist(err) {
		t.Fatalf("committed uninstall trash remains: %v", err)
	}
	if _, err := os.Stat(manager.intentPath()); !os.IsNotExist(err) {
		t.Fatalf("lifecycle intent remains: %v", err)
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
