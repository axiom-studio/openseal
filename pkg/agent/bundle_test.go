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
	installed.Definition.Channels[0].EndpointID = "conversation-endpoint:source-host"
	installed.Definition.Authority.ApprovalDestinations[0].EndpointID = "conversation-endpoint:source-host"
	identity := capability.NewSkillIdentity("skill-slack", "2.2.12", "https://github.com/axiom-studio/skills::skill-slack")
	request := BundleExportRequest{
		Definition: installed.Definition, Deployment: installed.Deployment,
		EndpointIDs: map[string]string{"conversation-endpoint:source-host": "approvals"},
		Metadata:    BundleMetadata{ID: "rowan-greenwood", Tags: []string{"woodworking", "assistant"}},
		Manifest:    ManifestMetadata{ID: "rowan-greenwood", Tags: []string{"woodworking"}},
		Skills: []BundleSkillRequirement{{RequirementID: "skill-slack", Identity: identity, Policy: BundleSkillPolicy{
			AllowedActions: []string{"send-approval"}, MaximumRisk: capability.RiskLevelRead,
		}}},
		Credentials: []BundleCredentialNeed{{Name: "skill-slack.slack_bot_token", BindingKey: "slack_bot_token", Kind: "slack_bot_token", RequiredBy: []string{"skill-slack"}}},
		Endpoints:   []BundleEndpointNeed{{ID: "approvals", Name: "Approval channel", Provider: "slack", Mode: capability.ConversationEndpointChannel, Adapter: identity, AdapterID: "interactions", Enabled: true}},
	}
	originalConstraint := installed.Definition.SkillRequirements[0].VersionConstraint
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
	if got := first.Agent.Spec.SkillRequirements[0].VersionConstraint; got != identity.Version {
		t.Fatalf("exported manifest Skill version = %q, want reviewed identity %q", got, identity.Version)
	}
	if got := installed.Definition.SkillRequirements[0].VersionConstraint; got != originalConstraint {
		t.Fatalf("export mutated source Skill constraint: got %q, want %q", got, originalConstraint)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"vault://", "binding:source-host", "conversation-endpoint:source-host", "tenant/7", "agent:rowan\"", "ingressRoute", "callback", "approvalId", "runHistory"} {
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
		Skills: []BundleSkillRequirement{{RequirementID: "skill-slack", Identity: identity, Policy: BundleSkillPolicy{
			AllowedActions: []string{"send-approval"}, MaximumRisk: capability.RiskLevelRead,
		}}},
		Endpoints: []BundleEndpointNeed{{ID: "approvals", Name: "Approvals", Provider: "slack", Mode: capability.ConversationEndpointChannel, Adapter: identity, AdapterID: "interactions"}},
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
	bundle.Agent.Spec.SkillRequirements[0].VersionConstraint = "2.2.11"
	bundle.Digest, err = bundleDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err = bundle.Validate(); err == nil || !strings.Contains(err.Error(), "exact identity") {
		t.Fatalf("inconsistent Skill identity error = %v", err)
	}
	bundle.Agent.Spec.SkillRequirements[0].VersionConstraint = identity.Version
	bundle.Digest, err = bundleDigest(bundle)
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

func TestBundleInstallationPreviewAndCompilerRequireExactTargetMappings(t *testing.T) {
	installed, err := InstallManifest(t.Context(), registryManifestInstaller{registry: NewRegistry()}, manifestInstallationFixture("1.0.0"))
	if err != nil {
		t.Fatal(err)
	}
	identity := capability.NewSkillIdentity("skill-slack", "2.2.12", "source::slack")
	bundle, err := ExportBundle(BundleExportRequest{
		Definition: installed.Definition, Deployment: installed.Deployment,
		Metadata: BundleMetadata{ID: "rowan"}, Manifest: ManifestMetadata{ID: "rowan"},
		Skills: []BundleSkillRequirement{{RequirementID: "skill-slack", Identity: identity, Policy: BundleSkillPolicy{
			AllowedActions: []string{"send-approval"}, MaximumRisk: capability.RiskLevelRead,
		}}},
		Credentials: []BundleCredentialNeed{{Name: "skill-slack.slack", BindingKey: "slack", Kind: "slack_bot_token", RequiredBy: []string{"skill-slack"}}},
		Endpoints:   []BundleEndpointNeed{{ID: "approvals", Name: "Approvals", Provider: "slack", Mode: capability.ConversationEndpointChannel, Adapter: identity, AdapterID: "interactions", Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	preview, err := PreviewBundleInstallation(bundle, BundlePlacement{})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Ready || len(preview.Requirements) != 4 {
		t.Fatalf("empty target preview = %#v", preview)
	}
	placement := BundlePlacement{
		DeploymentID: "agent:rowan", Environment: "default",
		Skills:      map[string]BundleSkillPlacement{"skill-slack": {Identity: identity, BindingID: "binding:slack"}},
		Credentials: map[string]capability.CredentialReference{"skill-slack.slack": {Kind: "slack_bot_token", ID: "vault://target/slack"}},
		Endpoints: map[string]BundleEndpointPlacement{"approvals": {
			Provider: "slack", Adapter: identity, BindingID: "binding:slack", InstallationID: "workspace:T1", Address: "channel:C1",
		}},
	}
	preview, err = PreviewBundleInstallation(bundle, placement)
	if err != nil || !preview.Ready {
		t.Fatalf("resolved target preview = %#v, %v", preview, err)
	}
	plan, err := CompileBundleInstallation(BundleInstallationRequest{
		Bundle: bundle, Scope: capability.ScopeReference{Kind: "tenant", ID: "9"}, Placement: placement,
		ActorType: "user", ActorID: "42", Reason: "import elsewhere", IdempotencyKey: "import-rowan-v1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Manifest.Deployment.ID != "agent:rowan" || len(plan.Bindings) != 1 || plan.Bindings[0].Credentials["slack"].ID != "vault://target/slack" || len(plan.Endpoints) != 1 || plan.Endpoints[0].Placement.Address != "channel:C1" {
		t.Fatalf("compiled installation plan = %#v", plan)
	}
	if strings.Contains(plan.Manifest.Manifest.Metadata.ID, "tenant") || plan.Manifest.Deployment.DefinitionID != "" {
		t.Fatalf("compiled installation leaked source identity: %#v", plan.Manifest)
	}
	placement.Endpoints["approvals"] = BundleEndpointPlacement{Provider: "slack", Adapter: identity, BindingID: "different", Address: "channel:C1"}
	preview, err = PreviewBundleInstallation(bundle, placement)
	if err != nil || preview.Ready {
		t.Fatalf("mismatched endpoint binding was accepted: %#v, %v", preview, err)
	}
}
