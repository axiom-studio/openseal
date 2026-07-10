package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestDefinitionsAreImmutableAndDeploymentsRollForwardAndBack(t *testing.T) {
	registry := NewRegistry()
	first := testDefinition("1.0.0", capability.RiskLevelExternal, 4)
	registered, err := registry.RegisterDefinition(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	first.SystemPrompt = "mutated outside registry"
	loaded, err := registry.GetDefinition(context.Background(), "operator", "1.0.0")
	if err != nil || loaded.SystemPrompt == first.SystemPrompt || loaded.Digest == "" || registered.Digest != loaded.Digest {
		t.Fatalf("immutable definition = %#v %#v, %v", registered, loaded, err)
	}
	if _, err := registry.RegisterDefinition(context.Background(), testDefinition("1.0.0", capability.RiskLevelExternal, 4)); err == nil {
		t.Fatal("definition version overwrite should fail")
	}
	if _, err := registry.RegisterDefinition(context.Background(), testDefinition("2.0.0", capability.RiskLevelWrite, 2)); err != nil {
		t.Fatal(err)
	}

	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	maximumRisk := capability.RiskLevelWrite
	concurrency := 2
	deployment, initial, err := registry.CreateDeployment(context.Background(), &AgentDeployment{
		ID: "operator-prod", Scope: scope, DefinitionID: "operator", ActiveVersion: "1.0.0",
		Environment: "production", SkillBindingIDs: []string{"git", "kubernetes"},
		Credentials:  map[string]capability.CredentialReference{"git": {Kind: "git-token", ID: "opaque-vault-reference"}},
		Restrictions: DeploymentRestrictions{MaximumRisk: &maximumRisk, AllowedSkillIDs: []string{"git"}, MaxConcurrentRuns: &concurrency, BudgetCeilings: map[string]float64{"usd": 25}},
		Capacity:     DeploymentCapacity{MaxConcurrentRuns: 2, MaxQueuedRuns: 20}, RolloutStatus: RolloutActive,
	}, "user", "admin", "initial activation")
	if err != nil {
		t.Fatal(err)
	}
	if initial.FromVersion != "" || initial.ToVersion != "1.0.0" || deployment.Revision != 1 {
		t.Fatalf("initial activation = %#v %#v", deployment, initial)
	}
	forward, activation, err := registry.ActivateDefinition(context.Background(), scope, deployment.ID, "2.0.0", deployment.Revision, "user", "admin", "validated rollout")
	if err != nil || forward.ActiveVersion != "2.0.0" || forward.PreviousVersion != "1.0.0" || activation.FromVersion != "1.0.0" {
		t.Fatalf("forward activation = %#v %#v, %v", forward, activation, err)
	}
	rolledBack, rollback, err := registry.RollbackDefinition(context.Background(), scope, deployment.ID, forward.Revision, "user", "admin", "evaluation regression")
	if err != nil || rolledBack.ActiveVersion != "1.0.0" || rollback.FromVersion != "2.0.0" {
		t.Fatalf("rollback = %#v %#v, %v", rolledBack, rollback, err)
	}
	history, err := registry.ListActivations(context.Background(), scope, deployment.ID)
	if err != nil || len(history) != 3 || history[2].Reason != "evaluation regression" {
		t.Fatalf("activation history = %#v, %v", history, err)
	}
}

func TestDeploymentCannotWidenDefinitionAuthorityOrCrossScope(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.RegisterDefinition(context.Background(), testDefinition("1", capability.RiskLevelWrite, 2)); err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	destructive := capability.RiskLevelDestructive
	tooMany := 3
	_, _, err := registry.CreateDeployment(context.Background(), &AgentDeployment{
		ID: "unsafe", Scope: scope, DefinitionID: "operator", ActiveVersion: "1", RolloutStatus: RolloutActive, Environment: "prod",
		Restrictions: DeploymentRestrictions{MaximumRisk: &destructive, AllowedSkillIDs: []string{"undeclared"}, MaxConcurrentRuns: &tooMany, BudgetCeilings: map[string]float64{"usd": 101}},
		Capacity:     DeploymentCapacity{MaxConcurrentRuns: 3},
	}, "user", "admin", "")
	if err == nil {
		t.Fatal("authority widening should fail")
	}
	if _, err := registry.GetDeployment(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, "unsafe"); !errors.Is(err, ErrDeploymentNotFound) {
		t.Fatalf("cross-scope deployment lookup = %v", err)
	}
}

func TestDefinitionsRejectCredentialMaterial(t *testing.T) {
	definition := testDefinition("1", capability.RiskLevelRead, 1)
	definition.DomainContext = map[string]interface{}{"provider": map[string]interface{}{"apiKey": "secret"}}
	if _, err := NewRegistry().RegisterDefinition(context.Background(), definition); err == nil || !strings.Contains(err.Error(), "cannot contain credentials") {
		t.Fatalf("secret-shaped definition data should fail, got %v", err)
	}
	definition = testDefinition("1", capability.RiskLevelRead, 1)
	definition.Digest = "incorrect"
	if _, err := NewRegistry().RegisterDefinition(context.Background(), definition); err == nil {
		t.Fatal("incorrect caller-provided digest should fail")
	}
}

func TestRegistryClonesCredentialReferencesWithoutValues(t *testing.T) {
	registry := NewRegistry()
	if _, err := registry.RegisterDefinition(context.Background(), testDefinition("1", capability.RiskLevelRead, 1)); err != nil {
		t.Fatal(err)
	}
	deployment, _, err := registry.CreateDeployment(context.Background(), &AgentDeployment{
		ID: "agent", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, DefinitionID: "operator", ActiveVersion: "1",
		RolloutStatus: RolloutActive, Environment: "prod", Capacity: DeploymentCapacity{MaxConcurrentRuns: 1},
		Credentials: map[string]capability.CredentialReference{"api": {Kind: "provider-key", ID: "vault-ref"}},
	}, "user", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(deployment)
	if strings.Contains(string(encoded), "secret-value") || !strings.Contains(string(encoded), "vault-ref") {
		t.Fatalf("deployment credential representation = %s", encoded)
	}
}

func testDefinition(version string, risk capability.RiskLevel, concurrency int) *AgentDefinition {
	return &AgentDefinition{
		ID: "operator", Version: version, DisplayName: "Operator", Purpose: "Operate systems", SystemPrompt: "Keep systems healthy.",
		OperatingPrinciples: []string{"Verify evidence", "Act safely"},
		SkillRequirements:   []SkillRequirement{{SkillID: "git"}, {SkillID: "kubernetes"}},
		Authority:           AuthorityPolicy{MaximumRisk: risk, AllowedSkillIDs: []string{"git", "kubernetes"}, MaxConcurrentRuns: concurrency, BudgetCeilings: map[string]float64{"usd": 100}},
		Amendments:          AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"systemPrompt", "operatingPrinciples"}, RequiresApproval: true},
	}
}
