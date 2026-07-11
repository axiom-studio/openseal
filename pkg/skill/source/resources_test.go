package source

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelskill "github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skill/resourcefs"
)

func stageRequestForCandidate(candidate Candidate) kernelskill.ResourceStageRequest {
	return kernelskill.ResourceStageRequest{
		Scope: kernelskill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent", BindingID: "research",
		SkillID: candidate.Compilation.Definition.ID, SkillVersion: candidate.Compilation.Definition.Version,
		SourceDigest: candidate.Compilation.SourceDigest, Resources: candidate.Compilation.Definition.Resources,
	}
}

func TestEffectiveSourceArtifactStagesIntoFilesystemSandbox(t *testing.T) {
	sourceRoot := filepath.Join(t.TempDir(), "skills")
	skillRoot := filepath.Join(sourceRoot, "research")
	if err := os.MkdirAll(filepath.Join(skillRoot, "references"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), []byte("---\nname: research\ndescription: research\n---\nRead {baseDir}/references/guide.md."), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillRoot, "references", "guide.md"), []byte("preserve evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewCatalog().Discover(context.Background(), []Root{{ID: "workspace", Kind: RootWorkspace, Path: sourceRoot}})
	if err != nil || len(snapshot.Effective) != 1 {
		t.Fatalf("source discovery = %#v, %v", snapshot, err)
	}
	provider, err := NewArtifactProvider(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	stager, err := resourcefs.New(filepath.Join(t.TempDir(), "sandbox"), provider)
	if err != nil {
		t.Fatal(err)
	}
	candidate := snapshot.Effective[0]
	stage, err := stager.StageResources(context.Background(), stageRequestForCandidate(candidate))
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(stage.Root, "references", "guide.md"))
	if err != nil || string(content) != "preserve evidence" {
		t.Fatalf("staged effective resource = %q, %v", content, err)
	}
	if _, err := provider.ReadResource(context.Background(), candidate.Digest, "SKILL.md"); err == nil {
		t.Fatal("SKILL.md must not be exposed through the supporting-resource provider")
	}
	snapshot.Effective[0].Compilation.Definition.Resources = append(snapshot.Effective[0].Compilation.Definition.Resources, capability.Resource{
		Path: "references/missing.md", Kind: "reference",
		Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Size: 1,
	})
	if _, err := NewArtifactProvider(snapshot); err == nil {
		t.Fatal("missing retained source resource was accepted")
	}
}
