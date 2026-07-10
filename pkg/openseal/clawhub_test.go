package openseal

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

type facadeClawHubRegistry struct{ archive []byte }

func (r *facadeClawHubRegistry) SearchSkills(context.Context, ClawHubSearchRequest) (*ClawHubSkillPage, error) {
	return &ClawHubSkillPage{Items: []ClawHubSkillSummary{{Slug: "research", Name: "Research"}}}, nil
}
func (r *facadeClawHubRegistry) ExploreSkills(context.Context, ClawHubExploreRequest) (*ClawHubSkillPage, error) {
	return &ClawHubSkillPage{}, nil
}
func (r *facadeClawHubRegistry) InspectSkill(_ context.Context, ref ClawHubSkillReference) (*ClawHubSkillDetail, error) {
	return &ClawHubSkillDetail{SkillSummary: ClawHubSkillSummary{Slug: ref.Slug, Name: "Research"}, Version: "1.0.0", Owner: ref.Owner}, nil
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
	return &ClawHubVerification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass", Slug: ref.Slug, PublisherHandle: ref.Owner, Version: "1.0.0"}, nil
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
