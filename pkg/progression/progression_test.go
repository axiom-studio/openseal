package progression

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

func TestEvaluateIsDeterministicAndFailSafe(t *testing.T) {
	request := EvaluationRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "7"}, OwnerKind: OwnerAgent,
		DeploymentID: "agent-1", ExpectedDeploymentRevision: 4, BaseVersion: "1", BaseDigest: "sha256:base",
		Policy: EvaluationPolicy{MinimumCriteria: 2, PromotionThreshold: .8, RegressionThreshold: .4, RequireEvidence: true, AllowPromotion: true, AllowRegression: true},
		Outcomes: []CriterionOutcome{
			{CriterionID: "quality", Required: true, Passed: true, Score: .9, Summary: "Reviewed output stayed correct", EvidenceRefs: []string{"run:2", "run:1"}},
			{CriterionID: "safety", Required: true, Passed: true, Score: .8, Summary: "No unsafe action", EvidenceRefs: []string{"run:3"}},
		},
	}
	first, err := Evaluate(request)
	if err != nil || first.Direction != DirectionPromote || math.Abs(first.AggregateScore-.85) > 1e-9 {
		t.Fatalf("promotion = %#v, %v", first, err)
	}
	request.Outcomes[0], request.Outcomes[1] = request.Outcomes[1], request.Outcomes[0]
	second, err := Evaluate(request)
	if err != nil || first.ID != second.ID || !reflect.DeepEqual(first.EvidenceRefs, []string{"run:1", "run:2", "run:3"}) {
		t.Fatalf("deterministic recommendation = %#v %#v, %v", first, second, err)
	}
	request.Outcomes[0].Passed = false
	request.Outcomes[0].Score = .95
	regression, err := Evaluate(request)
	if err != nil || regression.Direction != DirectionRegress || regression.RequiredCriteriaPassed {
		t.Fatalf("required failure = %#v, %v", regression, err)
	}
	request.Outcomes[0].EvidenceRefs = nil
	if _, err := Evaluate(request); !errors.Is(err, ErrInvalidEvaluation) {
		t.Fatalf("missing evidence error = %v", err)
	}
}

func TestAgentProgressionCreatesOneGovernedAmendmentAndNeverMutatesAuthority(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "7"}
	agents := agent.NewRegistry()
	registered, err := agents.RegisterDefinition(ctx, testAgentDefinition())
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := agents.CreateDeployment(ctx, &agent.AgentDeployment{
		ID: "agent-deployment", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version,
		RolloutStatus: agent.RolloutActive, Environment: "default", Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1,
	}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(agents, nil)
	if _, err := service.RecommendAgent(ctx, scope, deployment.ID, deployment.Revision, testPolicy(), []CriterionOutcome{{CriterionID: "invented", Required: true, Passed: true, Score: 1, Summary: "Not retained", EvidenceRefs: []string{"run:1"}}}); !errors.Is(err, ErrInvalidEvaluation) {
		t.Fatalf("invented criterion error = %v", err)
	}
	recommendation, err := service.RecommendAgent(ctx, scope, deployment.ID, deployment.Revision, testPolicy(), passingOutcomes())
	if err != nil || recommendation.Direction != DirectionPromote {
		t.Fatalf("recommendation = %#v, %v", recommendation, err)
	}
	candidate := *registered
	candidate.Version, candidate.Digest = "2", ""
	candidate.Authority.MaxConcurrentRuns = 2
	ceiling := AgentPolicyCeiling{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 2}
	amendment, err := service.ProposeAgent(ctx, ProposeAgentRequest{Recommendation: recommendation, Candidate: &candidate, Ceiling: ceiling, ProposerType: "agent", ProposerID: deployment.ID})
	if err != nil || amendment.Status != agent.AmendmentAwaitingApproval || !amendment.RiskWidening || amendment.IdempotencyKey != recommendation.IdempotencyKey {
		t.Fatalf("proposal = %#v, %v", amendment, err)
	}
	unchanged, err := agents.GetDeployment(ctx, scope, deployment.ID)
	if err != nil || unchanged.ActiveVersion != "1" || unchanged.Revision != 1 {
		t.Fatalf("proposal mutated deployment = %#v, %v", unchanged, err)
	}
	retried, err := service.ProposeAgent(ctx, ProposeAgentRequest{Recommendation: recommendation, Candidate: &candidate, Ceiling: ceiling, ProposerType: "agent", ProposerID: deployment.ID})
	if err != nil || retried.ID != amendment.ID {
		t.Fatalf("idempotent retry = %#v, %v", retried, err)
	}
	drifted := candidate
	drifted.Version = "3"
	if _, err := service.ProposeAgent(ctx, ProposeAgentRequest{Recommendation: recommendation, Candidate: &drifted, Ceiling: ceiling, ProposerType: "agent", ProposerID: deployment.ID}); !errors.Is(err, agent.ErrIdempotencyConflict) {
		t.Fatalf("idempotency drift error = %v", err)
	}
	tampered := *recommendation
	tampered.Direction = DirectionRegress
	if _, err := service.ProposeAgent(ctx, ProposeAgentRequest{Recommendation: &tampered, Candidate: &candidate, Ceiling: ceiling, ProposerType: "agent", ProposerID: deployment.ID}); !errors.Is(err, ErrInvalidEvaluation) {
		t.Fatalf("tampered recommendation error = %v", err)
	}
	tampered = *recommendation
	tampered.EvidenceRefs = []string{"artifact:unrelated"}
	if _, err := service.ProposeAgent(ctx, ProposeAgentRequest{Recommendation: &tampered, Candidate: &candidate, Ceiling: ceiling, ProposerType: "agent", ProposerID: deployment.ID}); !errors.Is(err, ErrInvalidEvaluation) {
		t.Fatalf("tampered evidence error = %v", err)
	}
	tooSmall := ceiling
	tooSmall.MaxConcurrentRuns = 1
	// A modified identity is rejected before the ceiling, proving callers cannot
	// mint recommendation authority. The original recommendation still fails a
	// stricter host ceiling.
	if _, err := service.ProposeAgent(ctx, ProposeAgentRequest{Recommendation: recommendation, Candidate: &candidate, Ceiling: tooSmall, ProposerType: "agent", ProposerID: deployment.ID}); !errors.Is(err, ErrPolicyCeiling) {
		t.Fatalf("ceiling error = %v", err)
	}
	approved, err := agents.ResolveAmendment(ctx, agent.ResolveAmendmentRequest{Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Approved: true, ActorType: "user", ActorID: "admin", Reason: "within policy"})
	if err != nil {
		t.Fatal(err)
	}
	activated, active, _, err := agents.ActivateAmendment(ctx, scope, amendment.ID, approved.Revision, "user", "admin", "approved progression")
	if err != nil || activated.Status != agent.AmendmentActivated || active.ActiveVersion != "2" {
		t.Fatalf("activation = %#v %#v, %v", activated, active, err)
	}
	history, err := agents.ListAmendments(ctx, scope, deployment.ID)
	if err != nil || len(history) != 1 || history[0].Decision == nil || history[0].ActivationID == "" {
		t.Fatalf("history = %#v, %v", history, err)
	}
	rolledBack, rollback, err := agents.RollbackDefinition(ctx, scope, deployment.ID, active.Revision, "user", "admin", "evaluation regressed")
	if err != nil || rolledBack.ActiveVersion != "1" || rollback.FromVersion != "2" || rollback.ToVersion != "1" {
		t.Fatalf("rollback = %#v %#v, %v", rolledBack, rollback, err)
	}
}

func TestRegressionAndNoChangeRemainGoverned(t *testing.T) {
	request := EvaluationRequest{Scope: capability.ScopeReference{Kind: "tenant", ID: "7"}, OwnerKind: OwnerAgent, DeploymentID: "a", ExpectedDeploymentRevision: 1, BaseVersion: "1", BaseDigest: "d", Policy: testPolicy(), Outcomes: []CriterionOutcome{{CriterionID: "quality", Required: true, Passed: true, Score: .6, Summary: "Mixed", EvidenceRefs: []string{"run:1"}}}}
	neutral, err := Evaluate(request)
	if err != nil || neutral.Direction != DirectionNoChange {
		t.Fatalf("neutral = %#v, %v", neutral, err)
	}
	if _, err := NewService(agent.NewRegistry(), nil).ProposeAgent(ctx(), ProposeAgentRequest{Recommendation: neutral, Candidate: testAgentDefinition()}); !errors.Is(err, ErrNoChange) {
		t.Fatalf("no-change error = %v", err)
	}
}

func TestTeamProgressionUsesSameApprovalAndRejectionLifecycle(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "7"}
	agents := agent.NewRegistry()
	teams := team.NewRegistry(agents)
	registered, err := teams.RegisterDefinition(ctx, testTeamDefinition())
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := teams.CreateDeployment(ctx, &team.Deployment{ID: "team-deployment", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version, Status: team.DeploymentActive, Revision: 1}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(agents, teams)
	recommendation, err := service.RecommendTeam(ctx, scope, deployment.ID, 1, testPolicy(), passingOutcomes())
	if err != nil {
		t.Fatal(err)
	}
	candidate := *registered
	candidate.Version, candidate.Digest = "2", ""
	candidate.Delegation.MaximumConcurrent = 2
	ceiling := TeamPolicyCeiling{MaximumRisk: capability.RiskLevelWrite, MaximumDelegationDepth: 2, MaximumConcurrency: 2, MaximumSpeakers: 3}
	amendment, err := service.ProposeTeam(ctx, ProposeTeamRequest{Recommendation: recommendation, Candidate: &candidate, Ceiling: ceiling, ProposerType: "agent", ProposerID: "coordinator"})
	if err != nil || !amendment.RiskWidening || amendment.Status != team.AmendmentEvaluating {
		t.Fatalf("Team proposal = %#v, %v", amendment, err)
	}
	evaluated, err := teams.SubmitAmendmentEvaluation(ctx, team.SubmitAmendmentEvaluationRequest{Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: amendment.Revision, Evaluations: []team.AmendmentEvaluation{{CriterionID: "quality", Passed: true, Summary: "Review passed", EvidenceRefs: []string{"run:1"}}}})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := teams.ResolveAmendment(ctx, team.ResolveAmendmentRequest{Scope: scope, AmendmentID: amendment.ID, ExpectedRevision: evaluated.Revision, Approved: false, ActorType: "user", ActorID: "admin", Reason: "retain current scope"})
	if err != nil || rejected.Status != team.AmendmentRejected {
		t.Fatalf("rejection = %#v, %v", rejected, err)
	}
	current, _ := teams.GetDeployment(ctx, scope, deployment.ID)
	if current.ActiveVersion != "1" {
		t.Fatalf("rejection mutated active Team: %#v", current)
	}
	if _, _, _, err := teams.ActivateAmendment(ctx, scope, amendment.ID, rejected.Revision, "user", "admin", ""); err == nil {
		t.Fatal("rejected progression activated")
	}
	history, err := teams.ListAmendments(ctx, scope, deployment.ID)
	if err != nil || len(history) != 1 || history[0].Decision == nil || history[0].Decision.Approved {
		t.Fatalf("Team history = %#v, %v", history, err)
	}
}

func TestProgressionProposalRetrySurvivesSQLiteRestart(t *testing.T) {
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "7"}
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	agents := agent.NewRegistryWithStore(store)
	registered, err := agents.RegisterDefinition(ctx, testAgentDefinition())
	if err != nil {
		t.Fatal(err)
	}
	deployment, _, err := agents.CreateDeployment(ctx, &agent.AgentDeployment{ID: "durable-agent", Scope: scope, DefinitionID: registered.ID, ActiveVersion: registered.Version, RolloutStatus: agent.RolloutActive, Environment: "default", Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1}, "user", "admin", "initial")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(agents, nil)
	recommendation, err := service.RecommendAgent(ctx, scope, deployment.ID, 1, testPolicy(), passingOutcomes())
	if err != nil {
		t.Fatal(err)
	}
	candidate := *registered
	candidate.Version, candidate.Digest, candidate.Authority.MaxConcurrentRuns = "2", "", 2
	request := ProposeAgentRequest{Recommendation: recommendation, Candidate: &candidate, Ceiling: AgentPolicyCeiling{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 2}, ProposerType: "agent", ProposerID: deployment.ID}
	created, err := service.ProposeAgent(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewService(agent.NewRegistryWithStore(reopened), nil)
	replayed, err := restarted.ProposeAgent(ctx, request)
	if err != nil || replayed.ID != created.ID || replayed.RequestDigest != created.RequestDigest {
		t.Fatalf("restart replay = %#v, %v", replayed, err)
	}
}

func testPolicy() EvaluationPolicy {
	return EvaluationPolicy{MinimumCriteria: 1, PromotionThreshold: .8, RegressionThreshold: .4, RequireEvidence: true, AllowPromotion: true, AllowRegression: true}
}
func passingOutcomes() []CriterionOutcome {
	return []CriterionOutcome{{CriterionID: "quality", Required: true, Passed: true, Score: .9, Summary: "Sustained successful outcomes", EvidenceRefs: []string{"run:1", "artifact:review"}}}
}
func ctx() context.Context { return context.Background() }

func testAgentDefinition() *agent.AgentDefinition {
	return &agent.AgentDefinition{ID: "operator", Version: "1", DisplayName: "Operator", Purpose: "Operate safely", SystemPrompt: "Verify evidence before acting.", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}, Evaluations: []workforce.EvaluationCriterion{{ID: "quality", Description: "Output quality", Required: true}}, Amendments: workforce.AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"authority", "purpose"}, ApproverPrincipals: []string{"user:admin"}}}
}

func testTeamDefinition() *team.Definition {
	return &team.Definition{ID: "operations", Version: "1", DisplayName: "Operations", Purpose: "Coordinate operations", Roles: []team.RoleSlot{{ID: "operator", DisplayName: "Operator", Purpose: "Operate", ChannelParticipation: team.RoleChannelActive}}, Coordination: team.CoordinationPolicy{MaximumSpeakersPerRound: 2}, Delegation: team.DelegationPolicy{MaximumDepth: 1, MaximumConcurrent: 1}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}, Evaluations: []workforce.EvaluationCriterion{{ID: "quality", Description: "Output quality", Required: true}}, Amendments: workforce.AmendmentPolicy{AgentMayPropose: true, AllowedFields: []string{"delegation", "purpose"}, ApproverPrincipals: []string{"user:admin"}}}
}
