package commands

import (
	"bytes"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

func TestSkillManifestCommandPrintsCanonicalBundledDefinition(t *testing.T) {
	var output bytes.Buffer
	if err := runSkillCommand(&output, []string{"manifest", source.SkillID}); err != nil {
		t.Fatal(err)
	}
	manifest, err := skill.DecodeManifestYAML(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Definition.ID != source.SkillID || manifest.Definition.Version != source.SkillVersion ||
		len(manifest.Definition.Installers) != 1 || manifest.Definition.Installers[0].Package != source.SkillImage {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestSkillManifestCommandRejectsUnknownOrIncompleteRequests(t *testing.T) {
	for _, args := range [][]string{{}, {"manifest"}, {"manifest", "unknown"}} {
		if err := runSkillCommand(&bytes.Buffer{}, args); err == nil {
			t.Fatalf("request %#v succeeded", args)
		}
	}
}
