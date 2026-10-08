package skill

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestSkillManifestRoundTripPreservesCompleteDefinition(t *testing.T) {
	definition := testSkillDefinition()
	definition.Installers = []capability.Installer{{ID: "oci", Kind: "oci", Package: "example/skill:1.0.0"}}
	definition.Category = "research"
	definition.Tags = []string{"evidence", "web"}
	manifest, err := NewManifest(definition)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeManifestYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifestYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Definition.ID != definition.ID || len(decoded.Definition.Installers) != 1 ||
		decoded.Definition.Installers[0].Package != "example/skill:1.0.0" || decoded.Definition.Category != "research" ||
		len(decoded.Definition.Tags) != 2 {
		t.Fatalf("round trip lost canonical definition fields: %#v", decoded.Definition)
	}
}

func TestSkillManifestRejectsLegacyUnknownAndIncompleteDocuments(t *testing.T) {
	for name, document := range map[string]string{
		"legacy": `apiVersion: skills.axiom.dev/v1
kind: Skill
metadata:
  id: legacy
`,
		"unknown": `apiVersion: openseal.dev/v1alpha1
kind: SkillDefinition
definition:
  id: example
  version: 1.0.0
  name: Example
  actions: {}
  transport:
    kind: tool
  legacyExecutor: grpc
`,
		"duplicate": `apiVersion: openseal.dev/v1alpha1
apiVersion: openseal.dev/v1alpha1
kind: SkillDefinition
definition: {}
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeManifestYAML([]byte(document)); err == nil {
				t.Fatal("invalid Skill manifest was accepted")
			}
		})
	}
}

func TestClaimsSkillManifestRequiresCanonicalIdentity(t *testing.T) {
	if !ClaimsManifest(map[string]interface{}{"apiVersion": ManifestAPIVersion, "kind": ManifestKind}) {
		t.Fatal("canonical Skill manifest was not recognized")
	}
	if ClaimsManifest(map[string]interface{}{"apiVersion": "skills.axiom.dev/v1", "kind": "Skill"}) {
		t.Fatal("legacy manifest was misclassified as canonical")
	}
	if _, err := DecodeManifestYAML([]byte("apiVersion: [")); err == nil || !strings.Contains(err.Error(), "canonical Skill YAML") {
		t.Fatalf("malformed YAML error = %v", err)
	}
}

func TestSkillManifestActionReviewRoundTripsAndValidates(t *testing.T) {
	definition := testSkillDefinition()
	action := definition.Actions["deploy"]
	action.Review = ActionReviewAlways
	definition.Actions["deploy"] = action
	manifest, err := NewManifest(definition)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeManifestYAML(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "review: always") {
		t.Fatalf("manifest YAML omits the review floor:\n%s", data)
	}
	decoded, err := DecodeManifestYAML(data)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.Definition.Actions["deploy"].AlwaysReview() {
		t.Fatalf("round trip lost review: always: %#v", decoded.Definition.Actions["deploy"])
	}
	for name, mutate := range map[string]func(*Action){
		"unknown value":   func(a *Action) { a.Review = "sometimes" },
		"no side effects": func(a *Action) { a.SideEffect, a.Risk, a.Idempotency = SideEffectRead, RiskLevelRead, IdempotencyNone },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := testSkillDefinition()
			action := invalid.Actions["deploy"]
			action.Review = ActionReviewAlways
			mutate(&action)
			invalid.Actions["deploy"] = action
			if err := validateDefinition(invalid); err == nil || !strings.Contains(err.Error(), "review") {
				t.Fatalf("invalid review declaration error = %v", err)
			}
		})
	}
}
