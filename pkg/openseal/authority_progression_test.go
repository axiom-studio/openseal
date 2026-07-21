package openseal

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestEngineExposesGovernedAgentAuthorityProgression(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "7"}
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: "reviewer", Version: "1", DisplayName: "Reviewer", Purpose: "Review work", SystemPrompt: "Cite evidence.",
		Authority:   AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Evaluations: []AgentEvaluationCriterion{{ID: "quality", Description: "Review quality", Required: true}},
		Amendments:  AgentAmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"authority"}, ApproverPrincipals: []string{"user:admin"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: "reviewer-default", Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: AgentRolloutActive, Environment: "default", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1,
	}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	policy := AuthorityProgressionEvaluationPolicy{MinimumCriteria: 1, PromotionThreshold: .8, RegressionThreshold: .4, RequireEvidence: true, AllowPromotion: true, AllowRegression: true}
	outcomes := []AuthorityProgressionCriterionOutcome{{CriterionID: "quality", Required: true, Passed: true, Score: .9, Summary: "Sustained quality", EvidenceRefs: []string{"run:1"}}}
	recommendation, err := engine.RecommendAgentAuthority(ctx, scope, deployment.ID, deployment.Revision, policy, outcomes)
	if err != nil || recommendation.Direction != AuthorityProgressionPromote {
		t.Fatalf("recommendation = %#v, %v", recommendation, err)
	}
	candidate := *definition
	candidate.Version, candidate.Digest, candidate.Authority.MaxConcurrentRuns = "2", "", 2
	amendment, err := engine.ProposeAgentAuthorityProgression(ctx, ProposeAgentAuthorityProgressionRequest{
		Recommendation: recommendation, Candidate: &candidate,
		Ceiling:      AgentAuthorityProgressionCeiling{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 2},
		ProposerType: "agent", ProposerID: deployment.ID,
	})
	if err != nil || amendment.IdempotencyKey != recommendation.IdempotencyKey || amendment.Status != AgentAmendmentEvaluating {
		t.Fatalf("amendment = %#v, %v", amendment, err)
	}
}
