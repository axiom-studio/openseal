package skill

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

type activationResourceStager struct {
	stage *ResourceStage
	err   error
	seen  []ResourceStageRequest
}

func (s *activationResourceStager) StageResources(_ context.Context, request ResourceStageRequest) (*ResourceStage, error) {
	s.seen = append(s.seen, request)
	return s.stage, s.err
}

func TestActivationSnapshotIsScopedStableAndSecretFree(t *testing.T) {
	compilation, err := skillopenclaw.Compile(skillopenclaw.Bundle{
		SkillMD: []byte(`---
name: research
description: Research with a deterministic tool.
command-dispatch: tool
command-tool: source_search
metadata:
  openclaw:
    skillKey: research-config
    primaryEnv: RESEARCH_TOKEN
    os: [linux]
    requires:
      bins: [curl]
      anyBins: [jq, yq]
      env: [RESEARCH_TOKEN]
      config: [research.enabled]
---
Read {baseDir}/references/policy.md before searching.
`),
		Files:  []skillopenclaw.File{{Path: "references/policy.md", Content: []byte("Preserve provenance.")}},
		Source: skillopenclaw.Source{Registry: "https://registry.test", Reference: "publisher/research", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	definitionVersion := compilation.Definition.Version
	bind := func(catalog *Catalog, deployment string, credentialID string) {
		t.Helper()
		if err := catalog.Register(context.Background(), compilation.Definition); err != nil {
			t.Fatal(err)
		}
		if err := catalog.Bind(context.Background(), &Binding{
			ID: "research", Scope: scope, DeploymentID: deployment, SkillID: "research", SkillVersion: definitionVersion,
			AllowedActions: []string{"invoke"}, EnablePrompt: true, MaximumRisk: RiskLevelExternal,
			Credentials: map[string]CredentialReference{"RESEARCH_TOKEN": {Kind: "environment-secret", ID: credentialID}},
			Config:      map[string]interface{}{"resultLimit": float64(20)}, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}

	catalog := NewCatalog()
	bind(catalog, "researcher-a", "credential-a")
	host := HostCapabilityState{
		OperatingSystem: "linux", Executables: map[string]bool{"curl": true, "jq": true},
		Configuration: map[string]interface{}{"research": map[string]interface{}{"enabled": true}},
		ResourceRoots: map[string]string{"research@" + definitionVersion: "/sandbox/skills/research"},
		Adapters: map[string]AdapterCapability{
			"tool": {State: AdapterStateAvailable}, "sandbox": {State: AdapterStateAvailable},
		}, Revision: "host-7",
	}
	first, err := catalog.Activate(context.Background(), scope, "researcher-a", host)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Activate(context.Background(), scope, "researcher-a", host)
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotID == "" || first.SnapshotID != second.SnapshotID || len(first.Skills) != 1 || len(first.Unavailable) != 0 {
		t.Fatalf("unstable activation snapshot: first=%#v second=%#v", first, second)
	}
	activated := first.Skills[0]
	if activated.Name != "research" || activated.Description != "Research with a deterministic tool." || activated.ConfigurationKey != "research-config" || len(activated.Actions) != 1 || activated.ResourceRoot != "/sandbox/skills/research" ||
		activated.Prompt == nil || strings.Contains(activated.Prompt.Instructions, "{baseDir}") || !strings.Contains(activated.Prompt.Instructions, "/sandbox/skills/research/references/policy.md") {
		t.Fatalf("activation lost compiled semantics: %#v", activated)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "credential-a") || strings.Contains(string(encoded), "RESEARCH_TOKEN") {
		t.Fatalf("activation snapshot exposed credential metadata: %s", encoded)
	}

	restarted := NewCatalog()
	bind(restarted, "researcher-a", "different-opaque-id")
	restored, err := restarted.Activate(context.Background(), scope, "researcher-a", host)
	if err != nil || restored.SnapshotID != first.SnapshotID {
		t.Fatalf("equivalent restart changed model-visible snapshot: %#v, %v", restored, err)
	}
	host.Revision = "host-8"
	refreshed, err := restarted.Activate(context.Background(), scope, "researcher-a", host)
	if err != nil || refreshed.SnapshotID == first.SnapshotID {
		t.Fatalf("host revision did not refresh snapshot: %#v, %v", refreshed, err)
	}
}

func TestActivationReportsStructuredUnavailabilityPerDeployment(t *testing.T) {
	definition := &Definition{
		ID: "gated", Version: "1.0.0", Name: "gated", Actions: map[string]Action{},
		Prompt: &PromptModule{Instructions: "Read {baseDir}/guide.md", UserInvocable: true},
		Requirements: Requirements{
			OperatingSystems: []string{"darwin"}, Executables: []string{"tool"}, AnyExecutables: []string{"one", "two"},
			Environment: []string{"TOKEN"}, Configuration: []string{"feature.enabled"},
		},
	}
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &Binding{
		ID: "gated", Scope: scope, DeploymentID: "agent", SkillID: "gated", SkillVersion: "1.0.0",
		EnablePrompt: true, MaximumRisk: RiskLevelRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{OperatingSystem: "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Skills) != 0 || len(snapshot.Unavailable) != 1 || len(snapshot.Unavailable[0].Reasons) != 6 {
		t.Fatalf("structured availability = %#v", snapshot)
	}
	codes := make(map[string]bool)
	for _, reason := range snapshot.Unavailable[0].Reasons {
		codes[reason.Code] = true
	}
	for _, code := range []string{"operating_system_unavailable", "executable_missing", "any_executable_missing", "environment_missing", "configuration_missing", "resource_root_missing"} {
		if !codes[code] {
			t.Fatalf("missing availability reason %q in %#v", code, snapshot.Unavailable[0].Reasons)
		}
	}
}

func TestActivationStagesDeclaredResourcesAndPinsStageIdentity(t *testing.T) {
	compilation, err := skillopenclaw.Compile(skillopenclaw.Bundle{
		SkillMD: []byte("---\nname: staged\ndescription: staged resources\n---\nRead {baseDir}/references/guide.md."),
		Files:   []skillopenclaw.File{{Path: "references/guide.md", Content: []byte("trusted")}},
		Source:  skillopenclaw.Source{Reference: "test/staged", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), compilation.Definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &Binding{
		ID: "binding", Scope: scope, DeploymentID: "agent", SkillID: "staged", SkillVersion: compilation.Definition.Version,
		EnablePrompt: true, MaximumRisk: RiskLevelRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	stager := &activationResourceStager{stage: &ResourceStage{Root: "/sandbox/staged", Revision: "resource-7", Adapter: "sandbox/v1"}}
	host := HostCapabilityState{OperatingSystem: "linux", ResourceStager: stager, Adapters: map[string]AdapterCapability{
		AdapterResourceStaging: {State: AdapterStateAvailable, Version: "sandbox/v1"},
	}}
	first, err := catalog.Activate(context.Background(), scope, "agent", host)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Activate(context.Background(), scope, "agent", host)
	if err != nil {
		t.Fatal(err)
	}
	if first.SnapshotID != second.SnapshotID || len(first.Skills) != 1 || len(stager.seen) != 2 {
		t.Fatalf("staged activation is not stable: first=%#v second=%#v seen=%#v", first, second, stager.seen)
	}
	activated := first.Skills[0]
	if activated.ResourceRoot != "/sandbox/staged" || activated.ResourceRevision != "resource-7" || activated.ResourceAdapter != "sandbox/v1" ||
		activated.Prompt == nil || !strings.Contains(activated.Prompt.Instructions, "/sandbox/staged/references/guide.md") {
		t.Fatalf("staged resource identity was not projected: %#v", activated)
	}
	if request := stager.seen[0]; request.SourceDigest != compilation.SourceDigest || request.BindingID != "binding" || len(request.Resources) != 1 {
		t.Fatalf("staging request lost immutable provenance: %#v", request)
	}

	stager.stage.Revision = "resource-8"
	refreshed, err := catalog.Activate(context.Background(), scope, "agent", host)
	if err != nil || refreshed.SnapshotID == first.SnapshotID {
		t.Fatalf("resource revision did not refresh activation identity: %#v, %v", refreshed, err)
	}

	failed, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{
		OperatingSystem: "linux", ResourceStager: &activationResourceStager{err: errors.New("provider included sensitive detail")},
		Adapters: map[string]AdapterCapability{AdapterResourceStaging: {State: AdapterStateAvailable}},
	})
	if err != nil || len(failed.Skills) != 0 || len(failed.Unavailable) != 1 || failed.Unavailable[0].Reasons[0].Code != "resource_staging_failed" ||
		strings.Contains(failed.Unavailable[0].Reasons[0].Message, "sensitive") {
		t.Fatalf("failed staging was not safely represented: %#v, %v", failed, err)
	}
}

func TestActivationRequiresTruthfulAdapterStates(t *testing.T) {
	catalog := NewCatalog()
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	definition := &Definition{
		ID: "resource", Version: "1", Name: "resource", Actions: map[string]Action{},
		Prompt:    &PromptModule{Instructions: "Read {baseDir}/guide.md", UserInvocable: true},
		Resources: []Resource{{Path: "guide.md", Kind: "reference", Digest: strings.Repeat("a", 64), Size: 1}},
		Source:    &SourceProvenance{Format: "test", Digest: strings.Repeat("b", 64)},
	}
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(context.Background(), &Binding{ID: "resource", Scope: scope, DeploymentID: "agent", SkillID: "resource", SkillVersion: "1", EnablePrompt: true, MaximumRisk: RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	stager := &activationResourceStager{stage: &ResourceStage{Root: "/sandbox/resource", Revision: "one", Adapter: "sandbox/v1"}}
	snapshot, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{OperatingSystem: "linux", ResourceStager: stager, Adapters: map[string]AdapterCapability{
		AdapterResourceStaging: {State: AdapterStateUnavailable, Reason: "artifact transport is not configured"},
	}})
	if err != nil || len(stager.seen) != 0 || len(snapshot.Unavailable) != 1 || snapshot.Unavailable[0].Reasons[0].Code != "resource_staging_unavailable" {
		t.Fatalf("unavailable adapter was not enforced: snapshot=%#v seen=%#v err=%v", snapshot, stager.seen, err)
	}
	if _, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{Adapters: map[string]AdapterCapability{
		AdapterRemoteNode: {State: AdapterStateDegraded},
	}}); err == nil || !strings.Contains(err.Error(), "requires a reason") {
		t.Fatalf("ambiguous adapter state was accepted: %v", err)
	}
}

func TestBindingConfigurationRejectsRawSecretFields(t *testing.T) {
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), &Definition{
		ID: "prompt", Version: "1", Name: "prompt", Actions: map[string]Action{},
		Prompt: &PromptModule{Instructions: "Work safely.", UserInvocable: true},
	}); err != nil {
		t.Fatal(err)
	}
	err := catalog.Bind(context.Background(), &Binding{
		ID: "prompt", Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent",
		SkillID: "prompt", SkillVersion: "1", EnablePrompt: true, MaximumRisk: RiskLevelRead,
		Config: map[string]interface{}{"provider": map[string]interface{}{"apiKey": "raw-secret"}}, Revision: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "opaque credential reference") {
		t.Fatalf("raw secret-like binding config should fail, got %v", err)
	}
}

func TestAlwaysAvailableSkipsHostRequirementGates(t *testing.T) {
	catalog := NewCatalog()
	definition := &Definition{
		ID: "always", Version: "1", Name: "always", Actions: map[string]Action{},
		Prompt:       &PromptModule{Instructions: "Always available.", AlwaysActive: true, UserInvocable: true},
		Requirements: Requirements{AlwaysAvailable: true, OperatingSystems: []string{"darwin"}, Executables: []string{"missing"}, Environment: []string{"TOKEN"}},
	}
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "local", ID: "one"}
	if err := catalog.Bind(context.Background(), &Binding{ID: "always", Scope: scope, DeploymentID: "agent", SkillID: "always", SkillVersion: "1", EnablePrompt: true, MaximumRisk: RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{OperatingSystem: "linux"})
	if err != nil || len(snapshot.Skills) != 1 || len(snapshot.Unavailable) != 0 {
		t.Fatalf("always-available skill was gated: %#v, %v", snapshot, err)
	}
}
