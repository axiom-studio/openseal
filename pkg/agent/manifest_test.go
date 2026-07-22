package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestCompileManifestBuildsPortableDefinitionWithSafeDefaults(t *testing.T) {
	manifest := &Manifest{
		APIVersion: ManifestAPIVersion,
		Kind:       ManifestKind,
		Metadata: ManifestMetadata{
			ID: "researcher", Version: "1.0.0", DisplayName: "Market Researcher",
			Description: "Find cited customer pain points",
		},
		Spec: ManifestSpec{
			SkillRequirements: []SkillRequirement{{SkillID: "forum-reader", VersionConstraint: ">=1.0.0", RequiredActions: []string{"search"}}},
		},
	}
	definition, err := CompileManifest(manifest, "tenant-3-agent-marketplace-researcher", DefinitionProvenance{
		Source: "marketplace", Reference: "listing:42", CreatedBy: "service:installer",
	})
	if err != nil {
		t.Fatal(err)
	}
	if definition.ID != "tenant-3-agent-marketplace-researcher" || definition.Version != "1.0.0" ||
		definition.Authority.MaximumRisk != "read" || definition.Authority.MaxConcurrentRuns != 1 ||
		len(definition.Authority.AllowedSkillIDs) != 1 || definition.Authority.AllowedSkillIDs[0] != "forum-reader" {
		t.Fatalf("unexpected compiled definition: %#v", definition)
	}
	if !strings.Contains(definition.SystemPrompt, "Market Researcher") || definition.Provenance.Reference != "listing:42" {
		t.Fatalf("missing default prompt or provenance: %#v", definition)
	}
	manifest.Spec.SkillRequirements[0].SkillID = "mutated"
	if definition.SkillRequirements[0].SkillID != "forum-reader" {
		t.Fatal("compiled definition aliases mutable manifest state")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"credentials", "placement", "rolloutStatus", "environment"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("portable manifest leaked deployment field %q: %s", forbidden, encoded)
		}
	}
}

func TestCompileManifestRejectsSecretsAndInvalidIdentity(t *testing.T) {
	base := func() *Manifest {
		return &Manifest{APIVersion: ManifestAPIVersion, Kind: ManifestKind,
			Metadata: ManifestMetadata{ID: "researcher", Version: "1.0.0", DisplayName: "Researcher"}}
	}
	withSecret := base()
	withSecret.Spec.DomainContext = map[string]interface{}{"apiKey": "secret"}
	if _, err := CompileManifest(withSecret, "", DefinitionProvenance{}); err == nil {
		t.Fatal("expected model-visible secret rejection")
	}
	invalid := base()
	invalid.Metadata.ID = "Not Portable"
	if _, err := CompileManifest(invalid, "", DefinitionProvenance{}); err == nil {
		t.Fatal("expected invalid manifest identity rejection")
	}
}

func TestDecodeManifestYAMLPreservesNestedCamelCaseContract(t *testing.T) {
	manifest, err := DecodeManifestYAML([]byte(`apiVersion: openseal.dev/agent/v1alpha1
kind: Agent
metadata:
  id: researcher
  version: 1.0.0
  displayName: Researcher
spec:
  skillRequirements:
    - skillId: reddit
      versionConstraint: ">=1.0.0 <2.0.0"
      requiredActions: [search]
  authority:
    maximumRisk: read
    maxConcurrentRuns: 2
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Spec.SkillRequirements) != 1 || manifest.Spec.SkillRequirements[0].SkillID != "reddit" ||
		manifest.Spec.Authority.MaximumRisk != capability.RiskLevelRead || manifest.Spec.Authority.MaxConcurrentRuns != 2 {
		t.Fatalf("manifest=%#v", manifest)
	}
	if _, err = DecodeManifestYAML([]byte("apiVersion: openseal.dev/agent/v1alpha1\nkind: Agent\nunknown: true\n")); err == nil {
		t.Fatal("unknown portable Agent field was accepted")
	}
}
