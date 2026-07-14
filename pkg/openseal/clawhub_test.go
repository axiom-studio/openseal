package openseal

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill/sourceartifact"
)

type facadeClawHubRegistry struct {
	archive     []byte
	version     string
	verifyErr   error
	filePath    string
	fileVersion string
	fileTag     string
}

func (r *facadeClawHubRegistry) resolvedVersion() string {
	if r.version != "" {
		return r.version
	}
	return "1.0.0"
}

func (r *facadeClawHubRegistry) SearchSkills(context.Context, ClawHubSearchRequest) (*ClawHubSkillPage, error) {
	return &ClawHubSkillPage{Items: []ClawHubSkillSummary{{Slug: "research", Name: "Research"}}}, nil
}
func (r *facadeClawHubRegistry) ExploreSkills(context.Context, ClawHubExploreRequest) (*ClawHubSkillPage, error) {
	return &ClawHubSkillPage{}, nil
}
func (r *facadeClawHubRegistry) InspectSkill(_ context.Context, ref ClawHubSkillReference) (*ClawHubSkillDetail, error) {
	return &ClawHubSkillDetail{SkillSummary: ClawHubSkillSummary{Slug: ref.Slug, Name: "Research"}, Version: r.resolvedVersion(), Owner: ref.Owner}, nil
}
func (r *facadeClawHubRegistry) ListVersions(context.Context, ClawHubSkillReference, int, string) (*ClawHubVersionPage, error) {
	return &ClawHubVersionPage{Items: []ClawHubVersionSummary{{Version: r.resolvedVersion()}}}, nil
}
func (r *facadeClawHubRegistry) GetVersion(context.Context, ClawHubSkillReference, string) (*ClawHubVersionDetail, error) {
	return &ClawHubVersionDetail{Version: r.resolvedVersion(), Files: []ClawHubFileEntry{{Path: "SKILL.md", Size: 12}}, Security: &ClawHubSecurityStatus{Status: "clean"}}, nil
}
func (r *facadeClawHubRegistry) GetFile(_ context.Context, _ ClawHubSkillReference, path, version, tag string) ([]byte, error) {
	r.filePath, r.fileVersion, r.fileTag = path, version, tag
	return []byte("file content"), nil
}
func (r *facadeClawHubRegistry) VerifySkill(_ context.Context, ref ClawHubSkillReference, _, _ string) (*ClawHubVerification, error) {
	if r.verifyErr != nil {
		return nil, r.verifyErr
	}
	return &ClawHubVerification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass", Slug: ref.Slug, PublisherHandle: ref.Owner, Version: r.resolvedVersion()}, nil
}

func TestEngineExposesVersionedGovernedClawHubLifecycle(t *testing.T) {
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t), version: "1.0.0"}
	engine, err := New(WithClawHubRegistry("https://registry.test", registry, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	capability := engine.ClawHubLifecycleCapabilities()
	if capability.APIVersion != "openseal.clawhub.lifecycle/v1" || len(capability.Operations) != 13 {
		t.Fatalf("lifecycle capability = %#v", capability)
	}
	installed, installReceipt, err := engine.InstallClawHubSkillLifecycle(context.Background(), ClawHubInstallRequest{Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"}})
	if err != nil || installed.Version != "1.0.0" || installReceipt.Operation != ClawHubLifecycleOperation("install") || installReceipt.Outcome != ClawHubLifecycleOutcome("installed") || !installReceipt.Changed || installReceipt.SourceIdentity != installed.SourceIdentity || installReceipt.Version != installed.Version {
		t.Fatalf("install = %#v receipt=%#v, %v", installed, installReceipt, err)
	}
	replayedInstall, replayedReceipt, err := engine.InstallClawHubSkillLifecycle(context.Background(), ClawHubInstallRequest{Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"}})
	if err != nil || replayedInstall.Changed || replayedReceipt.Changed || replayedReceipt.Outcome != ClawHubLifecycleOutcome("unchanged") {
		t.Fatalf("install replay = %#v receipt=%#v, %v", replayedInstall, replayedReceipt, err)
	}
	versions, err := engine.ListClawHubSkillVersions(context.Background(), installed.Reference, 10, "")
	if err != nil || len(versions.Items) != 1 || versions.Items[0].Version != "1.0.0" {
		t.Fatalf("versions = %#v, %v", versions, err)
	}
	version, err := engine.GetClawHubSkillVersion(context.Background(), installed.Reference, "1.0.0")
	if err != nil || version.Security == nil || version.Security.Status != "clean" || len(version.Files) != 1 {
		t.Fatalf("version detail = %#v, %v", version, err)
	}
	file, err := engine.GetClawHubSkillFile(context.Background(), installed.Reference, "1.0.0", "", "SKILL.md")
	if err != nil || string(file) != "file content" {
		t.Fatalf("file = %q, %v", file, err)
	}
	if registry.filePath != "SKILL.md" || registry.fileVersion != "1.0.0" || registry.fileTag != "" {
		t.Fatalf("registry file arguments = path %q version %q tag %q", registry.filePath, registry.fileVersion, registry.fileTag)
	}
	verification, err := engine.VerifyClawHubSkill(context.Background(), installed.Reference, "1.0.0", "")
	if err != nil || !verification.OK {
		t.Fatalf("verification = %#v, %v", verification, err)
	}
	pinned, err := engine.PinClawHubSkillLifecycle("acme/research", "production review")
	if err != nil || !pinned.Changed || pinned.Outcome != ClawHubLifecycleOutcome("pinned") || pinned.SourceIdentity == "" || pinned.Version != "1.0.0" || pinned.Reason != "production review" {
		t.Fatalf("pin receipt = %#v, %v", pinned, err)
	}
	replayedPin, err := engine.PinClawHubSkillLifecycle("acme/research", "production review")
	if err != nil || replayedPin.Changed || replayedPin.Outcome != ClawHubLifecycleOutcome("unchanged") {
		t.Fatalf("pin replay receipt = %#v, %v", replayedPin, err)
	}
	states, err := engine.ListInstalledClawHubSkillStates()
	if err != nil || len(states) != 1 || !states[0].Pinned || states[0].PinReason != "production review" || !states[0].Verified {
		t.Fatalf("installed states = %#v, %v", states, err)
	}
	registry.version = "2.0.0"
	report, err := engine.UpdateAllClawHubSkills(context.Background())
	if err != nil || report.APIVersion != capability.APIVersion || len(report.Results) != 1 ||
		report.Results[0].Outcome != ClawHubLifecycleOutcome("skipped") || report.Results[0].Reason != "pinned" {
		t.Fatalf("pinned update report = %#v, %v", report, err)
	}
	unpinned, err := engine.UnpinClawHubSkillLifecycle("acme/research")
	if err != nil || !unpinned.Changed || unpinned.Outcome != ClawHubLifecycleOutcome("unpinned") || unpinned.Version != "1.0.0" {
		t.Fatalf("unpin receipt = %#v, %v", unpinned, err)
	}
	replayedUnpin, err := engine.UnpinClawHubSkillLifecycle("acme/research")
	if err != nil || replayedUnpin.Changed || replayedUnpin.Outcome != ClawHubLifecycleOutcome("unchanged") {
		t.Fatalf("unpin replay receipt = %#v, %v", replayedUnpin, err)
	}
	updated, updateReceipt, err := engine.UpdateClawHubSkillLifecycle(context.Background(), installed.Reference.String())
	if err != nil || !updated.Changed || !updateReceipt.Changed || updateReceipt.PreviousVersion != "1.0.0" || updateReceipt.Version != "2.0.0" || updateReceipt.Outcome != ClawHubLifecycleOutcome("updated") {
		t.Fatalf("update = %#v receipt=%#v, %v", updated, updateReceipt, err)
	}
	report, err = engine.UpdateAllClawHubSkills(context.Background())
	if err != nil || len(report.Results) != 1 || report.Results[0].PreviousVersion != "2.0.0" ||
		report.Results[0].Version != "2.0.0" || report.Results[0].Outcome != ClawHubLifecycleOutcome("unchanged") {
		t.Fatalf("updated report = %#v, %v", report, err)
	}
	removed, err := engine.UninstallClawHubSkill(installed.Reference.String(), false)
	if err != nil || removed.Outcome != ClawHubLifecycleOutcome("removed") || removed.PreviousVersion != "2.0.0" {
		t.Fatalf("uninstall result = %#v, %v", removed, err)
	}
	if installed, err := engine.ListInstalledClawHubSkillStates(); err != nil || len(installed) != 0 {
		t.Fatalf("installed after removal = %#v, %v", installed, err)
	}
}

func TestRegistryOnlyClawHubEngineAdvertisesReadOnlyLifecycleWithoutWorkspace(t *testing.T) {
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t)}
	engine, err := New(WithClawHubRegistryClient(registry))
	if err != nil {
		t.Fatal(err)
	}
	capability := engine.ClawHubLifecycleCapabilities()
	if len(capability.Operations) != 5 {
		t.Fatalf("registry-only capability = %#v", capability)
	}
	for _, operation := range capability.Operations {
		if operation != ClawHubLifecycleOperation("inspect_catalog") && operation != ClawHubLifecycleOperation("inspect_versions") && operation != ClawHubLifecycleOperation("inspect_files") &&
			operation != ClawHubLifecycleOperation("inspect_security") && operation != ClawHubLifecycleOperation("verify") {
			t.Fatalf("registry-only engine advertised mutation %q", operation)
		}
	}
}

func TestClawHubLifecycleBatchDoesNotExposeRegistryErrorsOrSkillContent(t *testing.T) {
	const secret = "registry-secret-token"
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t), version: "1.0.0"}
	workspace := t.TempDir()
	engine, err := New(WithClawHubRegistry("https://registry.test", registry, workspace))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.InstallClawHubSkill(context.Background(), ClawHubInstallRequest{Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"}}); err != nil {
		t.Fatal(err)
	}
	registry.verifyErr = errors.New("upstream authorization failed: " + secret)
	report, err := engine.UpdateAllClawHubSkills(context.Background())
	if err != nil || len(report.Results) != 1 || report.Results[0].ErrorCode != ClawHubLifecycleErrorCode("unavailable") {
		t.Fatalf("safe failure report = %#v, %v", report, err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "Preserve evidence", "SKILL.md", workspace} {
		if forbidden != "" && strings.Contains(string(encoded), forbidden) {
			t.Fatalf("lifecycle result leaked %q: %s", forbidden, encoded)
		}
	}
}
func (r *facadeClawHubRegistry) DownloadArchive(context.Context, ClawHubSkillReference, string, string) (*ClawHubDownloadedArchive, error) {
	digest := sha256.Sum256(r.archive)
	return &ClawHubDownloadedArchive{Bytes: r.archive, SHA256: hex.EncodeToString(digest[:])}, nil
}

func TestEngineInstallsAndRestoresCompiledClawHubSkill(t *testing.T) {
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t)}
	workspace := t.TempDir()
	engine, err := New(WithClawHubRegistry("https://registry.test", registry, workspace))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := engine.InstallClawHubSkill(context.Background(), ClawHubInstallRequest{Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"}})
	if err != nil {
		t.Fatal(err)
	}
	definitionVersion := installed.Compilation.Definition.Version
	definition, err := engine.GetSkillDefinition(context.Background(), "research", definitionVersion)
	if err != nil || definition == nil || definition.Prompt == nil || definition.Source.Digest != installed.Compilation.SourceDigest {
		t.Fatalf("activated definition = %#v, %v", definition, err)
	}
	if err := engine.PinClawHubSkill("research", "reviewed version"); err != nil {
		t.Fatal(err)
	}
	if err := engine.UnpinClawHubSkill("research"); err != nil {
		t.Fatal(err)
	}
	results, err := engine.SearchClawHubSkills(context.Background(), ClawHubSearchRequest{Query: "research"})
	if err != nil || len(results.Items) != 1 {
		t.Fatalf("registry search = %#v, %v", results, err)
	}
	restarted, err := New(WithClawHubRegistry("https://registry.test", registry, workspace))
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.GetSkillDefinition(context.Background(), "research", definitionVersion)
	if err != nil || restored == nil || restored.Source.Digest != definition.Source.Digest {
		t.Fatalf("restart restore = %#v, %v", restored, err)
	}
}

func TestEngineInstallsBindsAndRestoresOwnerQualifiedClawHubSkills(t *testing.T) {
	ctx := context.Background()
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t)}
	workspace := t.TempDir()
	database := filepath.Join(t.TempDir(), "owner-qualified.db")
	store, err := NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(WithPersistentStore(store), WithClawHubRegistry("https://registry.test", registry, workspace))
	if err != nil {
		t.Fatal(err)
	}
	installed := make([]*ClawHubInstalledSkill, 0, 2)
	for _, owner := range []string{"alice", "bob"} {
		value, err := engine.InstallClawHubSkill(ctx, ClawHubInstallRequest{Reference: ClawHubSkillReference{Owner: owner, Slug: "research"}})
		if err != nil {
			t.Fatal(err)
		}
		if value.SourceIdentity == "" || value.Compilation.Definition.Source.Identity != value.SourceIdentity {
			t.Fatalf("installed source identity was not retained in canonical definition: %#v", value)
		}
		installed = append(installed, value)
	}
	if installed[0].SourceIdentity == installed[1].SourceIdentity {
		t.Fatalf("owner-qualified identities collided: %#v", installed)
	}
	scope := SkillScope{Kind: "tenant", ID: "one"}
	for index, value := range installed {
		if err := engine.BindSkill(ctx, &SkillBinding{
			ID: []string{"alice", "bob"}[index], Scope: scope, DeploymentID: "analyst",
			SkillID: value.Compilation.Definition.ID, SkillVersion: value.Compilation.Definition.Version,
			SourceIdentity: value.SourceIdentity, EnablePrompt: true, MaximumRisk: SkillRiskRead, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := New(WithPersistentStore(reopened), WithClawHubRegistry("https://registry.test", registry, workspace))
	if err != nil {
		t.Fatal(err)
	}
	prompts, err := restarted.ListModelSkillPrompts(ctx, scope, "analyst")
	if err != nil || len(prompts) != 2 {
		t.Fatalf("restored model prompts = %#v, %v", prompts, err)
	}
	encoded, err := json.Marshal(prompts)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range installed {
		definition, err := restarted.GetSkillDefinitionVariant(ctx, value.Compilation.Definition.ID, value.Compilation.Definition.Version, value.SourceIdentity)
		if err != nil || definition == nil || definition.Source.Identity != value.SourceIdentity {
			t.Fatalf("restored exact definition = %#v, %v", definition, err)
		}
		if strings.Contains(string(encoded), value.SourceIdentity) {
			t.Fatalf("model prompt projection leaked source identity: %s", encoded)
		}
	}
	for index, value := range installed {
		prompt, err := restarted.ResolveExactSkillPrompt(ctx, scope, "analyst", value.Compilation.Definition.ID, value.Compilation.Definition.Version, SkillBindingReference{ID: []string{"alice", "bob"}[index], Revision: 1})
		if err != nil || prompt == nil || prompt.Instructions == "" {
			t.Fatalf("restored exact prompt = %#v, %v", prompt, err)
		}
	}
	removed, err := restarted.UninstallClawHubSkill("@bob/research", false)
	if err != nil || removed.SourceIdentity != installed[1].SourceIdentity {
		t.Fatalf("exact uninstall = %#v, %v", removed, err)
	}
	states, err := restarted.ListInstalledClawHubSkillStates()
	if err != nil || len(states) != 1 || states[0].SourceIdentity != installed[0].SourceIdentity {
		t.Fatalf("peer installation after exact uninstall = %#v, %v", states, err)
	}
	alice, err := restarted.GetSkillDefinitionVariant(ctx, installed[0].Compilation.Definition.ID, installed[0].Compilation.Definition.Version, installed[0].SourceIdentity)
	if err != nil || alice == nil || alice.Source.Identity != installed[0].SourceIdentity {
		t.Fatalf("peer definition after exact uninstall = %#v, %v", alice, err)
	}
}

func TestEngineClawHubLifecycleRetainsExportsRestoresAndReleasesSourceArtifact(t *testing.T) {
	ctx := context.Background()
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t)}
	workspace := t.TempDir()
	database := filepath.Join(t.TempDir(), "kernel.db")
	scope := SkillScope{Kind: "tenant", ID: "one"}
	store, err := NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(
		WithPersistentStore(store),
		WithClawHubRegistry("https://registry.test", registry, workspace),
		WithClawHubSourceArtifactScope(scope),
	)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := engine.InstallClawHubSkill(ctx, ClawHubInstallRequest{Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"}})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := engine.ExportOpenClawSkillSource(ctx, scope, installed.Compilation.SourceDigest)
	if err != nil || !bytes.Equal(exported.SkillMD, installed.Compilation.Artifact.SkillMD) {
		t.Fatalf("installed source export=%#v err=%v", exported, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(database)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted, err := New(
		WithPersistentStore(reopened),
		WithClawHubRegistry("https://registry.test", registry, workspace),
		WithClawHubSourceArtifactScope(scope),
	)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := restarted.ExportOpenClawSkillSource(ctx, scope, installed.Compilation.SourceDigest)
	if err != nil || !reflect.DeepEqual(restored, exported) {
		t.Fatalf("restart source export equal=%t err=%v", reflect.DeepEqual(restored, exported), err)
	}
	if _, err := restarted.ExportOpenClawSkillSource(ctx, SkillScope{Kind: "tenant", ID: "two"}, installed.Compilation.SourceDigest); !errors.Is(err, sourceartifact.ErrNotFound) {
		t.Fatalf("cross-tenant source export error=%v", err)
	}
	if _, err := restarted.UninstallClawHubSkill("acme/research", false); err != nil {
		t.Fatal(err)
	}
	report, err := restarted.GarbageCollectSkillSourceArtifacts(ctx, time.Now().UTC().Add(time.Hour), 0, 10)
	if err != nil || report.DeletedArtifacts != 0 {
		t.Fatalf("uninstall source GC=%#v err=%v", report, err)
	}
	if _, err := restarted.ExportOpenClawSkillSource(ctx, scope, installed.Compilation.SourceDigest); err != nil {
		t.Fatalf("catalog definition lost retained source after uninstall: %v", err)
	}
}

func facadeSkillZip(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte("---\nname: research\ndescription: Research product pain points.\n---\nPreserve evidence and synthesize findings.")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
