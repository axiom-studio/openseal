package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

type registryManifestInstaller struct{ registry *Registry }

func (r registryManifestInstaller) ListAgentDefinitionVersions(ctx context.Context, id string) ([]*AgentDefinition, error) {
	return r.registry.ListDefinitionVersions(ctx, id)
}

func (r registryManifestInstaller) GetAgentDefinition(ctx context.Context, id, version string) (*AgentDefinition, error) {
	return r.registry.GetDefinition(ctx, id, version)
}

func (r registryManifestInstaller) RegisterAgentDefinition(ctx context.Context, definition *AgentDefinition) (*AgentDefinition, error) {
	return r.registry.RegisterDefinition(ctx, definition)
}

func (r registryManifestInstaller) GetAgentDeployment(ctx context.Context, scope capability.ScopeReference, id string) (*AgentDeployment, error) {
	return r.registry.GetDeployment(ctx, scope, id)
}

func (r registryManifestInstaller) CreateAgentDeployment(ctx context.Context, deployment *AgentDeployment, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	return r.registry.CreateDeployment(ctx, deployment, actorType, actorID, reason)
}

func (r registryManifestInstaller) ActivateAgentDefinition(ctx context.Context, scope capability.ScopeReference, id, version string, revision int64, actorType, actorID, reason string) (*AgentDeployment, *DefinitionActivation, error) {
	return r.registry.ActivateDefinition(ctx, scope, id, version, revision, actorType, actorID, reason)
}

func TestInstallManifestCreatesReplaysAndActivatesPortableAgent(t *testing.T) {
	registry := NewRegistry()
	adapter := registryManifestInstaller{registry: registry}
	request := manifestInstallationFixture("1.0.0")

	created, err := InstallManifest(t.Context(), adapter, request)
	if err != nil {
		t.Fatal(err)
	}
	if created.Replayed || created.Definition.ID != "tenant/7/rowan" || created.Deployment.ID != "agent:rowan" || created.Deployment.ActiveVersion != "1.0.0" {
		t.Fatalf("created=%#v", created)
	}
	var delegated string
	if err = json.Unmarshal(created.Definition.Runbook.Steps["work"].Delegate.AgentID.Literal, &delegated); err != nil || delegated != "agent:rowan" {
		t.Fatalf("delegated Agent = %q, %v", delegated, err)
	}
	if objective := created.Definition.Runbook.Triggers["daily"].ObjectiveID; objective != "agent:tenant/7/rowan:daily-help" {
		t.Fatalf("materialized objective = %q", objective)
	}

	replayed, err := InstallManifest(t.Context(), adapter, request)
	if err != nil || !replayed.Replayed || replayed.Deployment.Revision != created.Deployment.Revision {
		t.Fatalf("replayed=%#v err=%v", replayed, err)
	}

	next := manifestInstallationFixture("1.1.0")
	activated, err := InstallManifest(t.Context(), adapter, next)
	if err != nil || activated.Replayed || activated.Deployment.ActiveVersion != "1.1.0" || activated.Deployment.PreviousVersion != "1.0.0" {
		t.Fatalf("activated=%#v err=%v", activated, err)
	}
}

func TestInstallManifestRejectsDriftConflictsAndSecrets(t *testing.T) {
	registry := NewRegistry()
	adapter := registryManifestInstaller{registry: registry}
	request := manifestInstallationFixture("1.0.0")
	if _, err := InstallManifest(t.Context(), adapter, request); err != nil {
		t.Fatal(err)
	}

	drifted := manifestInstallationFixture("1.1.0")
	drifted.Deployment.Environment = "production"
	if _, err := InstallManifest(t.Context(), adapter, drifted); !errors.Is(err, ErrManifestInstallationConflict) {
		t.Fatalf("deployment drift error = %v", err)
	}
	versions, err := registry.ListDefinitionVersions(t.Context(), "tenant/7/rowan")
	if err != nil || len(versions) != 1 {
		t.Fatalf("drift left a partial definition: versions=%d err=%v", len(versions), err)
	}
	changed := manifestInstallationFixture("1.0.0")
	changed.Manifest.Spec.SystemPrompt = "Different immutable behavior."
	if _, err := InstallManifest(t.Context(), adapter, changed); !errors.Is(err, ErrManifestInstallationConflict) {
		t.Fatalf("definition drift error = %v", err)
	}
	secret := manifestInstallationFixture("2.0.0")
	secret.Manifest.Spec.DomainContext = map[string]interface{}{"password": "plain text secret"}
	if _, err := InstallManifest(t.Context(), adapter, secret); err == nil {
		t.Fatal("manifest secret was accepted")
	}
}

func TestRowanExampleIsInstallableAndContainsDurableWork(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "agents", "rowan-greenwood", "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	if err = json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	request := ManifestInstallationRequest{
		Manifest: &manifest,
		Deployment: &AgentDeployment{
			ID: "agent:rowan-greenwood", Scope: capability.ScopeReference{Kind: "tenant", ID: "1"},
			RolloutStatus: RolloutActive, Environment: "default", Capacity: DeploymentCapacity{MaxConcurrentRuns: 1},
		},
		ActorType: "user", ActorID: "1", IdempotencyKey: "rowan-example-v1",
	}
	result, err := InstallManifest(t.Context(), registryManifestInstaller{registry: NewRegistry()}, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Definition.ObjectiveTemplates) != 1 || result.Definition.Runbook == nil || len(result.Definition.Runbook.Triggers) != 1 {
		t.Fatalf("installed Rowan definition omitted durable work: %#v", result.Definition)
	}
}

func manifestInstallationFixture(version string) ManifestInstallationRequest {
	self, _ := json.Marshal("$self")
	goal, _ := json.Marshal("Help once.")
	return ManifestInstallationRequest{
		Manifest: &Manifest{
			APIVersion: ManifestAPIVersion, Kind: ManifestKind,
			Metadata: ManifestMetadata{ID: "rowan", Version: version, DisplayName: "Rowan", Description: "Help with woodworking."},
			Spec: ManifestSpec{
				SystemPrompt: "Give factual woodworking help.",
				SkillRequirements: []SkillRequirement{{
					SkillID: "skill-slack", VersionConstraint: ">=2.0.0 <3.0.0", RequiredActions: []string{"send-approval"},
				}},
				Authority: AuthorityPolicy{
					MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1,
					ApprovalDestinations: []ApprovalDestination{{EndpointID: "approvals"}},
				},
				ObjectiveTemplates: []ObjectiveTemplate{{ID: "daily-help", Title: "Help daily", Goal: "Give one useful answer.", Priority: 1}},
				Runbook: &runbook.Definition{
					APIVersion: runbook.APIVersion, ID: "daily-help", Version: version, Name: "Daily help",
					Entrypoints: map[string]string{"daily": "work"},
					Interfaces:  map[string]runbook.Interface{"daily": {Description: "Give one useful answer.", InputSchema: map[string]interface{}{"type": "object"}, OutputSchema: map[string]interface{}{"type": "object"}}},
					Triggers:    map[string]runbook.Trigger{"daily": {Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}, Entrypoint: "daily", ObjectiveID: "agent:$self:daily-help", MaximumConcurrent: 1}},
					Steps: map[string]runbook.Step{
						"work": {Kind: runbook.StepDelegate, Delegate: &runbook.DelegateStep{AgentID: runbook.Value{Literal: self}, Goal: runbook.Value{Literal: goal}, ResultPath: "/result", Next: "done"}},
						"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{Outputs: map[string]runbook.Value{}}},
					},
				},
				Channels: []ChannelRoute{{EndpointID: "approvals", MessageSelection: "direct_or_mentions", ReplyMode: "thread", IgnoreBots: true, Purposes: []string{"approvals"}}},
			},
		},
		Deployment: &AgentDeployment{
			ID: "agent:rowan", DisplayName: "Rowan", Scope: capability.ScopeReference{Kind: "tenant", ID: "7"},
			RolloutStatus: RolloutActive, Environment: "development", Capacity: DeploymentCapacity{MaxConcurrentRuns: 1},
		},
		ActorType: "user", ActorID: "7", Reason: "install fixture", IdempotencyKey: "rowan-baseline",
	}
}
