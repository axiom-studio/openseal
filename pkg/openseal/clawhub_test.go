package openseal

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type facadeClawHubRegistry struct {
	archive   []byte
	version   string
	verifyErr error
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
	return &ClawHubVersionPage{}, nil
}
func (r *facadeClawHubRegistry) GetVersion(context.Context, ClawHubSkillReference, string) (*ClawHubVersionDetail, error) {
	return &ClawHubVersionDetail{Version: "1.0.0"}, nil
}
func (r *facadeClawHubRegistry) GetFile(context.Context, ClawHubSkillReference, string, string, string) ([]byte, error) {
	return nil, nil
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
	if capability.APIVersion != "openseal.clawhub.lifecycle/v1" || len(capability.Operations) != 10 {
		t.Fatalf("lifecycle capability = %#v", capability)
	}
	installed, err := engine.InstallClawHubSkill(context.Background(), ClawHubInstallRequest{Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"}})
	if err != nil || installed.Version != "1.0.0" {
		t.Fatalf("install = %#v, %v", installed, err)
	}
	if err := engine.PinClawHubSkill("acme/research", "production review"); err != nil {
		t.Fatal(err)
	}
	registry.version = "2.0.0"
	report, err := engine.UpdateAllClawHubSkills(context.Background())
	if err != nil || report.APIVersion != capability.APIVersion || len(report.Results) != 1 ||
		report.Results[0].Outcome != ClawHubLifecycleOutcome("skipped") || report.Results[0].Reason != "pinned" {
		t.Fatalf("pinned update report = %#v, %v", report, err)
	}
	if err := engine.UnpinClawHubSkill("acme/research"); err != nil {
		t.Fatal(err)
	}
	report, err = engine.UpdateAllClawHubSkills(context.Background())
	if err != nil || len(report.Results) != 1 || report.Results[0].PreviousVersion != "1.0.0" ||
		report.Results[0].Version != "2.0.0" || report.Results[0].Outcome != ClawHubLifecycleOutcome("updated") {
		t.Fatalf("updated report = %#v, %v", report, err)
	}
	removed, err := engine.UninstallClawHubSkill("acme/research", false)
	if err != nil || removed.Outcome != ClawHubLifecycleOutcome("removed") || removed.PreviousVersion != "2.0.0" {
		t.Fatalf("uninstall result = %#v, %v", removed, err)
	}
	if installed, err := engine.ListInstalledClawHubSkills(); err != nil || len(installed) != 0 {
		t.Fatalf("installed after removal = %#v, %v", installed, err)
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
	definition, err := engine.GetSkillDefinition(context.Background(), "research", "1.0.0")
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
	restored, err := restarted.GetSkillDefinition(context.Background(), "research", "1.0.0")
	if err != nil || restored == nil || restored.Source.Digest != definition.Source.Digest {
		t.Fatalf("restart restore = %#v, %v", restored, err)
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
