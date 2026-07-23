package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

func TestSkillManifestCommandPrintsCanonicalBundledDefinition(t *testing.T) {
	var output bytes.Buffer
	if err := runSkillCommand(&output, []string{"manifest", source.SkillID}, nil); err != nil {
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
		if err := runSkillCommand(&bytes.Buffer{}, args, nil); err == nil {
			t.Fatalf("request %#v succeeded", args)
		}
	}
}

func TestSkillManifestCommandWritesRequestedFile(t *testing.T) {
	var path string
	var data []byte
	writer := func(gotPath string, gotData []byte, mode os.FileMode) error {
		path, data = gotPath, append([]byte(nil), gotData...)
		if mode != 0o644 {
			t.Fatalf("mode = %v", mode)
		}
		return nil
	}
	if err := runSkillCommand(&bytes.Buffer{}, []string{"manifest", source.SkillID, "--output", "skill.yaml"}, writer); err != nil {
		t.Fatal(err)
	}
	if path != "skill.yaml" {
		t.Fatalf("path = %q", path)
	}
	if _, err := skill.DecodeManifestYAML(data); err != nil {
		t.Fatal(err)
	}
}

func TestSkillValidateCommandChecksCanonicalManifest(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "skill.yaml")
	manifest, err := skill.NewManifest(source.SkillDefinition())
	if err != nil {
		t.Fatal(err)
	}
	data, err := skill.EncodeManifestYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runSkillCommand(&output, []string{"validate", path}, nil); err != nil {
		t.Fatal(err)
	}
	if output.String() != "valid openseal.source@1.0.4\n" {
		t.Fatalf("output = %q", output.String())
	}
	if err := os.WriteFile(path, []byte("apiVersion: legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runSkillCommand(&bytes.Buffer{}, []string{"validate", path}, nil); err == nil {
		t.Fatal("invalid manifest passed validation")
	}
}
