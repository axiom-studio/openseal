package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestExportBundleProducesDeterministicSecretFreePortableArtifact(t *testing.T) {
	installed, err := InstallManifest(t.Context(), registryManifestInstaller{registry: NewRegistry()}, manifestInstallationFixture("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	installed.Deployment.Credentials = map[string]capability.CredentialReference{
		"slack_bot_token": {Kind: "slack_bot_token", ID: "vault://source-host/slack"},
	}
	installed.Deployment.SkillBindingIDs = []string{"binding:source-host-slack"}
	identity := capability.NewSkillIdentity("skill-slack", "2.2.12", "https://github.com/axiom-studio/skills::skill-slack")
	request := BundleExportRequest{
		Definition: installed.Definition, Deployment: installed.Deployment,
		Metadata:    BundleMetadata{ID: "rowan-greenwood", Tags: []string{"woodworking", "assistant"}},
		Manifest:    ManifestMetadata{ID: "rowan-greenwood", Tags: []string{"woodworking"}},
		Skills:      []BundleSkillRequirement{{RequirementID: "skill-slack", Identity: identity}},
		Credentials: []BundleCredentialNeed{{Name: "slack_bot_token", Kind: "slack_bot_token", RequiredBy: []string{"skill-slack"}}},
		Endpoints:   []BundleEndpointNeed{{ID: "approvals", Name: "Approval channel", Provider: "slack", Mode: capability.ConversationEndpointChannel, Adapter: identity}},
	}
	first, err := ExportBundle(request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ExportBundle(request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest || !strings.HasPrefix(first.Digest, "sha256:") {
		t.Fatalf("bundle digest is not deterministic: %q != %q", first.Digest, second.Digest)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"vault://", "binding:source-host", "tenant/7", "agent:rowan\"", "ingressRoute", "callback", "approvalId", "runHistory"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("portable bundle leaked host-local value %q: %s", forbidden, encoded)
		}
	}
	if !strings.Contains(string(encoded), "agent:$self:daily-help") || !strings.Contains(string(encoded), "https://github.com/axiom-studio/skills::skill-slack") {
		t.Fatalf("portable bundle omitted behavior or provenance: %s", encoded)
	}

	yamlDocument, err := EncodeBundleYAML(first)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := DecodeBundleYAML(yamlDocument)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Digest != first.Digest {
		t.Fatalf("bundle YAML changed digest: %q != %q", restored.Digest, first.Digest)
	}
}

func TestBundleValidationRejectsMissingMappingsAndTampering(t *testing.T) {
	installed, err := InstallManifest(t.Context(), registryManifestInstaller{registry: NewRegistry()}, manifestInstallationFixture("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	identity := capability.NewSkillIdentity("skill-slack", "2.2.12", "source::slack")
	base := BundleExportRequest{
		Definition: installed.Definition, Deployment: installed.Deployment,
		Metadata: BundleMetadata{ID: "rowan"}, Manifest: ManifestMetadata{ID: "rowan"},
		Skills:    []BundleSkillRequirement{{RequirementID: "skill-slack", Identity: identity}},
		Endpoints: []BundleEndpointNeed{{ID: "approvals", Name: "Approvals", Provider: "slack", Mode: capability.ConversationEndpointChannel, Adapter: identity}},
	}
	missingSkill := base
	missingSkill.Skills = nil
	if _, err = ExportBundle(missingSkill); err == nil || !strings.Contains(err.Error(), "missing required Skill") {
		t.Fatalf("missing Skill error = %v", err)
	}
	missingEndpoint := base
	missingEndpoint.Endpoints = nil
	if _, err = ExportBundle(missingEndpoint); err == nil || !strings.Contains(err.Error(), "missing endpoint") {
		t.Fatalf("missing endpoint error = %v", err)
	}
	bundle, err := ExportBundle(base)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Metadata.DisplayName = "Tampered"
	if err = bundle.Validate(); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered bundle error = %v", err)
	}
	if _, err = DecodeBundleYAML([]byte("apiVersion: openseal.dev/agent-bundle/v1alpha1\nkind: AgentBundle\nunknown: true\n")); err == nil {
		t.Fatal("unknown bundle field was accepted")
	}
}
