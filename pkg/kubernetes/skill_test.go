package kubernetes

import (
	"context"
	"runtime"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestSkillDefinitionPublishesCompleteGovernedKubernetesSurface(t *testing.T) {
	definition := SkillDefinition()
	if definition.ID != SkillID || definition.Version != SkillVersion || definition.Prompt == nil || len(definition.Actions) != 8 {
		t.Fatalf("unexpected Kubernetes Skill definition: %#v", definition)
	}
	for _, name := range []string{GetResource, ListResources, ListEvents, GetLogs} {
		action := definition.Actions[name]
		if action.Risk != skill.RiskLevelRead || action.SideEffect != skill.SideEffectRead || action.Transport == nil || action.Transport.Kind != "tool" || action.Idempotency != skill.IdempotencySupported || len(action.Credentials) != 1 || action.Credentials[0].Kind != ClusterCredentialKind {
			t.Fatalf("read action %s is not governed correctly: %#v", name, action)
		}
	}
	for _, name := range []string{RestartWorkload, ScaleWorkload, PatchResource} {
		action := definition.Actions[name]
		if action.Risk != skill.RiskLevelProduction || action.SideEffect != skill.SideEffectWrite || action.Idempotency != skill.IdempotencyRequired || action.Retry.MaxAttempts != 1 {
			t.Fatalf("production action %s is not governed correctly: %#v", name, action)
		}
	}
	deleted := definition.Actions[DeleteResource]
	if deleted.Risk != skill.RiskLevelDestructive || deleted.SideEffect != skill.SideEffectDestructive || deleted.Idempotency != skill.IdempotencyRequired {
		t.Fatalf("delete action is not destructive and idempotent: %#v", deleted)
	}
}

func TestSkillDefinitionPinsClusterThroughOpaqueBindingNotModelInput(t *testing.T) {
	ctx := context.Background()
	catalog := skill.NewCatalog()
	definition := SkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "cluster-one", Scope: skill.ScopeReference{Kind: "tenant", ID: "7"}, DeploymentID: "sre",
		SkillID: SkillID, SkillVersion: SkillVersion, AllowedActions: []string{ListEvents, RestartWorkload},
		MaximumRisk: skill.RiskLevelProduction, Credentials: map[string]skill.CredentialReference{ClusterCredentialName: {Kind: ClusterCredentialKind, ID: "cluster://tenant-7/one"}}, Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(ctx, binding.Scope, binding.DeploymentID, SkillID, SkillVersion, ListEvents)
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]interface{}{"namespace": "workloads", "kind": "Deployment", "name": "agent-host"}
	if err := catalog.ValidateInput(ctx, bound, input); err != nil {
		t.Fatal(err)
	}
	if reference := bound.Binding.Credentials[ClusterCredentialName]; reference.Kind != ClusterCredentialKind || reference.ID != "cluster://tenant-7/one" {
		t.Fatalf("opaque cluster binding was not preserved: %#v", reference)
	}
	properties := definition.Actions[ListEvents].InputSchema["properties"].(map[string]interface{})
	if _, modelVisible := properties["clusterId"]; modelVisible {
		t.Fatal("cluster identity must not be model-visible input")
	}
	if err := catalog.ValidateInput(ctx, bound, map[string]interface{}{"clusterId": 2}); err == nil {
		t.Fatal("model input must not supply or override the authorized cluster binding")
	}
}

func TestSkillActivationUsesTypedClusterCredentialWithoutHostConfiguration(t *testing.T) {
	ctx := context.Background()
	catalog := skill.NewCatalog()
	definition := SkillDefinition()
	if len(definition.Requirements.Configuration) != 0 {
		t.Fatalf("cluster authorization must not be modeled as host configuration: %#v", definition.Requirements.Configuration)
	}
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "cluster-activation", Scope: skill.ScopeReference{Kind: "tenant", ID: "7"}, DeploymentID: "sre",
		SkillID: SkillID, SkillVersion: SkillVersion, AllowedActions: []string{ListEvents},
		MaximumRisk: skill.RiskLevelRead, Credentials: map[string]skill.CredentialReference{ClusterCredentialName: {Kind: ClusterCredentialKind, ID: "cluster://tenant-7/one"}}, Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}

	snapshot, err := catalog.Activate(ctx, binding.Scope, binding.DeploymentID, skill.HostCapabilityState{
		OperatingSystem: runtime.GOOS,
		Architecture:    runtime.GOARCH,
		Revision:        "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Unavailable) != 0 {
		t.Fatalf("credential-authorized Kubernetes Skill is unavailable: %#v", snapshot.Unavailable)
	}
	if len(snapshot.Skills) != 1 || len(snapshot.Skills[0].Actions) != 1 || snapshot.Skills[0].Actions[0].Action != ListEvents {
		t.Fatalf("expected the authorized Kubernetes action in the activation snapshot: %#v", snapshot.Skills)
	}
}

func TestSkillSchemasRejectUnsafeOrUnboundedInputs(t *testing.T) {
	ctx := context.Background()
	catalog := skill.NewCatalog()
	definition := SkillDefinition()
	if err := catalog.Register(ctx, definition); err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{
		ID: "cluster", Scope: skill.ScopeReference{Kind: "tenant", ID: "7"}, DeploymentID: "sre",
		SkillID: SkillID, SkillVersion: SkillVersion, AllowedActions: []string{GetLogs, ScaleWorkload, PatchResource},
		MaximumRisk: skill.RiskLevelProduction, Credentials: map[string]skill.CredentialReference{ClusterCredentialName: {Kind: ClusterCredentialKind, ID: "cluster://tenant-7/one"}}, Revision: 1,
	}
	if err := catalog.Bind(ctx, binding); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		action string
		input  map[string]interface{}
	}{
		{GetLogs, map[string]interface{}{"podName": "api", "tailLines": 5001}},
		{ScaleWorkload, map[string]interface{}{"kind": "DaemonSet", "name": "agent", "replicas": 2}},
		{PatchResource, map[string]interface{}{"kind": "Deployment", "name": "api", "patch": map[string]interface{}{}}},
	}
	for _, test := range tests {
		bound, err := catalog.Resolve(ctx, binding.Scope, binding.DeploymentID, SkillID, SkillVersion, test.action)
		if err != nil {
			t.Fatal(err)
		}
		if err := catalog.ValidateInput(ctx, bound, test.input); err == nil {
			t.Fatalf("unsafe %s input unexpectedly validated: %#v", test.action, test.input)
		}
	}
}
