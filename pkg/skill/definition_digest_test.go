package skill

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDefinitionDigestSurvivesCatalogRoundTrip(t *testing.T) {
	definition := testSkillDefinition()
	definition.Source = &SourceProvenance{
		Identity: "https://example.com/skills::release", Format: "openseal.dev/v1alpha1",
		Trust: map[string]interface{}{"publisher": "example", "verified": true},
	}
	definition.Installers = []Installer{{ID: "oci", Kind: "oci", Package: "example/release:1.0.0"}}
	want, err := DefinitionDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len("sha256:")+64 || !strings.HasPrefix(want, "sha256:") {
		t.Fatalf("invalid digest format: %q", want)
	}
	manifest, err := NewManifest(definition)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Diagnostics = []ManifestDiagnostic{{Severity: "info", Code: "catalog", Message: "Catalog-only metadata"}}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]interface{}
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DefinitionDigest(&decoded.Definition)
	if err != nil || got != want {
		t.Fatalf("catalog round trip changed definition identity: got %q, want %q, error %v", got, want, err)
	}
	// Rebuild an unordered provenance map with the opposite insertion order.
	definition.Source.Trust = make(map[string]interface{})
	definition.Source.Trust["verified"] = true
	definition.Source.Trust["publisher"] = "example"
	got, err = DefinitionDigest(definition)
	if err != nil || got != want {
		t.Fatalf("map insertion order changed definition identity: got %q, want %q, error %v", got, want, err)
	}
}

func TestDefinitionDigestSeparatesSourceArtifactAndCapabilities(t *testing.T) {
	definition := testSkillDefinition()
	definition.Source = &SourceProvenance{Identity: "https://example.com/skills::release", Format: "openseal.dev/v1alpha1"}
	definition.Installers = []Installer{{ID: "oci", Kind: "oci", Package: "example/release:1.0.0"}}
	want, err := DefinitionDigest(definition)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Definition){
		"source":    func(value *Definition) { value.Source.Identity = "https://another.example/skills::release" },
		"version":   func(value *Definition) { value.Version = "2.0.0" },
		"image":     func(value *Definition) { value.Installers[0].Package = "example/release:other" },
		"transport": func(value *Definition) { value.Transport.Endpoint = "internal://other-release" },
		"action": func(value *Definition) {
			action := value.Actions["deploy"]
			action.Description = "Deploy another revision"
			value.Actions["deploy"] = action
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := cloneDefinition(definition)
			mutate(changed)
			got, err := DefinitionDigest(changed)
			if err != nil || got == want {
				t.Fatalf("different definition was not isolated: got %q, original %q, error %v", got, want, err)
			}
		})
	}
}

func TestDefinitionDigestRejectsInvalidAndNonPortableDefinitions(t *testing.T) {
	invalid := testSkillDefinition()
	invalid.Source = &SourceProvenance{Identity: "https://example.com/skills::release", Trust: map[string]interface{}{"unsupported": func() {}}}
	for name, definition := range map[string]*Definition{"nil": nil, "incomplete": {}, "nonportable": invalid} {
		t.Run(name, func(t *testing.T) {
			if digest, err := DefinitionDigest(definition); err == nil || digest != "" {
				t.Fatalf("invalid definition produced a usable identity: %q, %v", digest, err)
			}
		})
	}
}
