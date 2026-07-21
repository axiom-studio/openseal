package skill

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealprocess "github.com/axiom-studio/openseal/pkg/process"
)

type previewLifecycleAdapter struct {
	staged   int
	prepared int
}

func (a *previewLifecycleAdapter) StageResources(context.Context, ResourceStageRequest) (*ResourceStage, error) {
	a.staged++
	return &ResourceStage{Root: "/should/not/be/used", Revision: "1", Adapter: "test"}, nil
}

func (a *previewLifecycleAdapter) PrepareRuntime(context.Context, RuntimePreparationRequest) (*RuntimePreparationResult, error) {
	a.prepared++
	return &RuntimePreparationResult{State: RuntimePreparationReady}, nil
}

func TestBindingActivationPreviewIsExactAndSideEffectFree(t *testing.T) {
	definition := &Definition{
		ID: "writer", Version: "1.0.0", Name: "Writer", Actions: map[string]Action{},
		Prompt:    &PromptModule{Instructions: "Read {baseDir}/references/guide.md before writing."},
		Resources: []capability.Resource{{Path: "references/guide.md", Kind: "file", Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}},
		Source: &capability.SourceProvenance{
			Identity: "https://clawhub.test::publisher/writer", Format: "openclaw", Registry: "https://clawhub.test", Reference: "publisher/writer", ResolvedVersion: "1.0.0",
			Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	catalog := NewCatalog()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	binding := &Binding{
		ID: "writer", Scope: ScopeReference{Kind: "tenant", ID: "7"}, DeploymentID: "agent-1",
		SkillID: "writer", SkillVersion: "1.0.0", SourceIdentity: DefinitionSourceIdentity(definition),
		EnablePrompt: true, MaximumRisk: RiskLevelRead, Revision: 1,
	}
	unqualified := cloneBinding(binding)
	unqualified.SourceIdentity = ""
	if _, err := PreviewBindingActivation(definition, unqualified, HostCapabilityState{}); err == nil {
		t.Fatal("source-qualified definition accepted an unqualified proposed binding")
	}

	unavailable, err := catalog.PreviewBindingActivation(t.Context(), binding, HostCapabilityState{
		OperatingSystem: "linux", Adapters: map[string]AdapterCapability{AdapterResourceStaging: {State: AdapterStateUnavailable, Reason: "resource staging is disabled"}},
	})
	if err != nil || unavailable.Ready || len(unavailable.Reasons) == 0 || unavailable.Reasons[0].Code != "resource_staging_unavailable" {
		t.Fatalf("resource-dependent prompt was advertised as ready: preview=%#v err=%v", unavailable, err)
	}

	adapter := &previewLifecycleAdapter{}
	ready, err := catalog.PreviewBindingActivation(t.Context(), binding, HostCapabilityState{
		OperatingSystem: "linux", ResourceStager: adapter,
		Adapters: map[string]AdapterCapability{AdapterResourceStaging: {State: AdapterStateAvailable}},
	})
	if err != nil || !ready.Ready || !ready.RequiresResourceStaging || len(ready.Reasons) != 0 {
		t.Fatalf("configured staging adapter was not projected truthfully: preview=%#v err=%v", ready, err)
	}
	if adapter.staged != 0 || adapter.prepared != 0 {
		t.Fatalf("preview invoked lifecycle adapters: staged=%d prepared=%d", adapter.staged, adapter.prepared)
	}

	staged, err := catalog.PreviewBindingActivation(t.Context(), binding, HostCapabilityState{
		OperatingSystem: "linux", ResourceRoots: map[string]string{"writer": "/sandbox/writer"},
	})
	if err != nil || !staged.Ready || staged.RequiresResourceStaging {
		t.Fatalf("existing resource root was not accepted: preview=%#v err=%v", staged, err)
	}
}

func TestBindingActivationPreviewCoversHostAndCredentialRequirements(t *testing.T) {
	definition := &Definition{
		ID: "reader", Version: "1", Name: "Reader",
		Actions: map[string]Action{"read": {
			Name: "read", Description: "Read a governed resource", Risk: RiskLevelRead, SideEffect: SideEffectRead, Idempotency: IdempotencySupported,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			Transport:   &TransportReference{Kind: "tool", Endpoint: AdapterHTTPAction},
			Credentials: []CredentialRequirement{{Name: "api", Kind: "oauth"}},
		}},
		Requirements: Requirements{OperatingSystems: []string{"linux"}, Configuration: []string{"reader.enabled"}},
	}
	catalog := NewCatalog()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	binding := &Binding{
		ID: "reader", Scope: ScopeReference{Kind: "tenant", ID: "7"}, DeploymentID: "agent-1",
		SkillID: "reader", SkillVersion: "1", AllowedActions: []string{"read"}, MaximumRisk: RiskLevelRead, Revision: 1,
		Credentials: map[string]CredentialReference{"api": {Kind: "oauth", ID: "opaque-reference"}},
	}
	preview, err := catalog.PreviewBindingActivation(t.Context(), binding, HostCapabilityState{
		OperatingSystem: "darwin", Adapters: map[string]AdapterCapability{AdapterHTTPAction: {State: AdapterStateUnavailable, Reason: "HTTP actions are disabled"}},
	})
	if err != nil || preview.Ready || len(preview.Reasons) < 2 {
		t.Fatalf("host incompatibilities were not preserved: preview=%#v err=%v", preview, err)
	}

	ready, err := catalog.PreviewBindingActivation(t.Context(), binding, HostCapabilityState{
		OperatingSystem: "linux", Configuration: map[string]interface{}{"reader": map[string]interface{}{"enabled": true}},
		Adapters: map[string]AdapterCapability{AdapterHTTPAction: {State: AdapterStateAvailable}},
	})
	if err != nil || !ready.Ready {
		t.Fatalf("compatible exact binding was not ready: preview=%#v err=%v", ready, err)
	}
}

func TestBindingActivationPreviewDescribesRuntimePreparationWithoutInvokingIt(t *testing.T) {
	installer := capability.Installer{ID: "writer-cli", Kind: "npm", OperatingSystems: []string{"linux"}, Executables: []string{"writer-cli"}, Package: "writer-cli"}
	definition := &Definition{
		ID: "cli-writer", Version: "1", Name: "CLI writer",
		Actions: map[string]Action{"write": {
			Name: "write", Description: "Write with the governed CLI", Risk: RiskLevelRead, SideEffect: SideEffectRead,
			Idempotency: IdempotencySupported,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			Transport: &TransportReference{Kind: "tool", Endpoint: opensealprocess.TransportName, Arguments: map[string]TransportArgument{
				opensealprocess.ExecutableKey: {Literal: "writer-cli"},
				opensealprocess.InstallersKey: {Literal: []capability.Installer{installer}},
			}},
		}},
		Installers: []capability.Installer{installer},
		Source: &capability.SourceProvenance{
			Format: "openclaw", Digest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	binding := &Binding{
		ID: "cli-writer", Scope: ScopeReference{Kind: "tenant", ID: "7"}, DeploymentID: "agent-1",
		SkillID: "cli-writer", SkillVersion: "1", SourceIdentity: DefinitionSourceIdentity(definition),
		AllowedActions: []string{"write"}, MaximumRisk: RiskLevelRead, Revision: 1,
	}
	adapter := &previewLifecycleAdapter{}
	preview, err := PreviewBindingActivation(definition, binding, HostCapabilityState{
		OperatingSystem: "linux", Architecture: "amd64", RuntimePreparer: adapter,
		Adapters: map[string]AdapterCapability{
			AdapterInstaller:       {State: AdapterStateAvailable, Features: []string{"npm"}},
			AdapterPreparedRuntime: {State: AdapterStateAvailable, Version: "builder/v1"},
		},
	})
	if err != nil || !preview.Ready || !preview.RequiresRuntimePreparation {
		t.Fatalf("runtime preparation requirement was not projected: preview=%#v err=%v", preview, err)
	}
	if adapter.prepared != 0 || adapter.staged != 0 {
		t.Fatalf("preview invoked lifecycle adapters: prepared=%d staged=%d", adapter.prepared, adapter.staged)
	}
}
