package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
	if _, _, err := registry.ActivateDefinition(context.Background(), scope, deployment.ID, "2.0.0", deployment.Revision, "agent", "operator", "self rollout"); err == nil {
		t.Fatal("an agent must not bypass the amendment workflow")
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

func TestAmendmentsRequireAllowedDiffEvaluationApprovalAndAtomicActivation(t *testing.T) {
	registry := NewRegistry()
	base := testDefinition("1.0.0", capability.RiskLevelExternal, 4)
	base.Amendments = AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}, RequiresApproval: true, ApproverPrincipals: []string{"user:admin"}}
	base.Evaluations = []EvaluationCriterion{{ID: "accuracy", Description: "No unsupported claims", Required: true}}
	registered, err := registry.RegisterDefinition(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	deployment, _, err := registry.CreateDeployment(context.Background(), &AgentDeployment{
		ID: "agent", Scope: scope, DefinitionID: base.ID, ActiveVersion: base.Version, RolloutStatus: RolloutActive,
		Environment: "prod", Capacity: DeploymentCapacity{MaxConcurrentRuns: 2},
	}, "user", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	candidate := cloneDefinition(registered)
	candidate.Version = "1.1.0"
	candidate.SystemPrompt = "Keep systems healthy and cite operational evidence."
	candidate.Digest = ""
	candidate.CreatedAt = time.Time{}
	amendment, err := registry.ProposeAmendment(context.Background(), ProposeAmendmentRequest{
		Scope: scope, DeploymentID: deployment.ID, Candidate: candidate, ProposerType: "agent", ProposerID: deployment.ID,
		Rationale: "Operational reviews need explicit evidence.", EvidenceRefs: []string{"evaluation:review-7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if amendment.Status != AmendmentEvaluating || len(amendment.Changes) != 1 || amendment.Changes[0].Field != "systemPrompt" || amendment.Candidate.Provenance.DerivedFrom != registered.Digest {
		t.Fatalf("amendment proposal = %#v", amendment)
	}
	evaluated, err := registry.SubmitAmendmentEvaluation(context.Background(), SubmitAmendmentEvaluationRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision,
		Evaluations: []AmendmentEvaluation{{CriterionID: "accuracy", Passed: true, Score: 1, Summary: "All claims remain evidence-bound", EvidenceRefs: []string{"artifact:evaluation"}}},
	})
	if err != nil || evaluated.Status != AmendmentAwaitingApproval {
		t.Fatalf("amendment evaluation = %#v, %v", evaluated, err)
	}
	if _, err := registry.ResolveAmendment(context.Background(), ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: evaluated.Revision, Approved: true,
		ActorType: "agent", ActorID: "operator", Reason: "Self-approved",
	}); err == nil {
		t.Fatal("ineligible principal should not approve an amendment")
	}
	approved, err := registry.ResolveAmendment(context.Background(), ResolveAmendmentRequest{
		Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: evaluated.Revision, Approved: true,
		ActorType: "user", ActorID: "admin", Reason: "Evaluation passed",
	})
	if err != nil || approved.Status != AmendmentApproved || approved.Decision == nil {
		t.Fatalf("amendment approval = %#v, %v", approved, err)
	}
	activated, updatedDeployment, activation, err := registry.ActivateAmendment(context.Background(), scope, amendment.ID, approved.Revision, "user", "admin", "approved amendment")
	if err != nil || activated.Status != AmendmentActivated || updatedDeployment.ActiveVersion != "1.1.0" || activation.FromVersion != "1.0.0" || activated.ActivationID != activation.ID {
		t.Fatalf("amendment activation = %#v %#v %#v, %v", activated, updatedDeployment, activation, err)
	}
	storedCandidate, err := registry.GetDefinition(context.Background(), base.ID, "1.1.0")
	if err != nil || storedCandidate.SystemPrompt != candidate.SystemPrompt {
		t.Fatalf("activated candidate = %#v, %v", storedCandidate, err)
	}
}

func TestAmendmentsFailClosedOnPolicyEvaluationAndStaleState(t *testing.T) {
	registry := NewRegistry()
	base := testDefinition("1", capability.RiskLevelRead, 2)
	base.Amendments = AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"systemPrompt"}}
	base.Evaluations = []EvaluationCriterion{{ID: "safety", Description: "Safe", Required: true}}
	registered, err := registry.RegisterDefinition(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	deployment, _, err := registry.CreateDeployment(context.Background(), &AgentDeployment{ID: "agent", Scope: scope, DefinitionID: base.ID, ActiveVersion: "1", RolloutStatus: RolloutActive, Environment: "prod", Capacity: DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	disallowed := cloneDefinition(registered)
	disallowed.Version, disallowed.Personality, disallowed.Digest, disallowed.CreatedAt = "2", "new personality", "", time.Time{}
	if _, err := registry.ProposeAmendment(context.Background(), ProposeAmendmentRequest{Scope: scope, DeploymentID: deployment.ID, Candidate: disallowed, ProposerType: "agent", ProposerID: "agent", Rationale: "change"}); err == nil {
		t.Fatal("disallowed amendment field should fail")
	}
	mixed := cloneDefinition(registered)
	mixed.Version, mixed.SystemPrompt, mixed.Personality, mixed.Digest, mixed.CreatedAt = "2", "Safer prompt.", "new personality", "", time.Time{}
	if _, err := registry.ProposeAmendment(context.Background(), ProposeAmendmentRequest{Scope: scope, DeploymentID: deployment.ID, Candidate: mixed, ProposerType: "agent", ProposerID: "agent", Rationale: "change"}); err == nil {
		t.Fatal("an omitted base field must not bypass the amendment allowlist")
	}
	candidate := cloneDefinition(registered)
	candidate.Version, candidate.SystemPrompt, candidate.Digest, candidate.CreatedAt = "2", "Safer prompt.", "", time.Time{}
	amendment, err := registry.ProposeAmendment(context.Background(), ProposeAmendmentRequest{Scope: scope, DeploymentID: deployment.ID, Candidate: candidate, ProposerType: "agent", ProposerID: "agent", Rationale: "safer"})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := registry.SubmitAmendmentEvaluation(context.Background(), SubmitAmendmentEvaluationRequest{Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Evaluations: []AmendmentEvaluation{{CriterionID: "safety", Passed: false, Summary: "Regression"}}})
	if err != nil || failed.Status != AmendmentEvaluationFailed {
		t.Fatalf("failed evaluation = %#v, %v", failed, err)
	}
	if _, _, _, err := registry.ActivateAmendment(context.Background(), scope, amendment.ID, failed.Revision, "user", "admin", ""); err == nil {
		t.Fatal("failed evaluation must not activate")
	}

	riskRegistry := NewRegistry()
	riskBase := testDefinition("1", capability.RiskLevelRead, 2)
	riskBase.Amendments = AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"authority"}}
	riskRegistered, err := riskRegistry.RegisterDefinition(context.Background(), riskBase)
	if err != nil {
		t.Fatal(err)
	}
	riskDeployment, _, err := riskRegistry.CreateDeployment(context.Background(), &AgentDeployment{ID: "risk-agent", Scope: scope, DefinitionID: riskBase.ID, ActiveVersion: "1", RolloutStatus: RolloutActive, Environment: "prod", Capacity: DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	riskCandidate := cloneDefinition(riskRegistered)
	riskCandidate.Version, riskCandidate.Authority.MaximumRisk, riskCandidate.Digest, riskCandidate.CreatedAt = "2", capability.RiskLevelProduction, "", time.Time{}
	if _, err := riskRegistry.ProposeAmendment(context.Background(), ProposeAmendmentRequest{Scope: scope, DeploymentID: riskDeployment.ID, Candidate: riskCandidate, ProposerType: "agent", ProposerID: "risk-agent", Rationale: "broader production work"}); err == nil {
		t.Fatal("risk-widening amendments without eligible approvers must fail closed")
	}
}

func testDefinition(version string, risk capability.RiskLevel, concurrency int) *AgentDefinition {
	return &AgentDefinition{
		ID: "operator", Version: version, DisplayName: "Operator", Purpose: "Operate systems", SystemPrompt: "Keep systems healthy.",
		OperatingPrinciples: []string{"Verify evidence", "Act safely"},
		SkillRequirements:   []SkillRequirement{{SkillID: "git"}, {SkillID: "kubernetes"}},
		Authority:           AuthorityPolicy{MaximumRisk: risk, AllowedSkillIDs: []string{"git", "kubernetes"}, MaxConcurrentRuns: concurrency, BudgetCeilings: map[string]float64{"usd": 100}},
		Amendments:          AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"systemPrompt", "operatingPrinciples"}, RequiresApproval: true, ApproverPrincipals: []string{"user:admin"}},
	}
}
