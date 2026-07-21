// Package progression derives evidence-backed authority recommendations and
// turns them into ordinary governed Agent or Team definition amendments.
// Recommendations never mutate active definitions or deployments.
package progression

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

var (
	ErrInvalidEvaluation   = errors.New("authority progression evaluation is invalid")
	ErrPolicyCeiling       = errors.New("authority progression exceeds the host policy ceiling")
	ErrRecommendationStale = errors.New("authority progression recommendation is stale")
	ErrNoChange            = errors.New("no-change recommendations do not create amendments")
)

type OwnerKind string
type Direction string

const (
	OwnerAgent OwnerKind = "agent"
	OwnerTeam  OwnerKind = "team"

	DirectionPromote  Direction = "promote"
	DirectionNoChange Direction = "no_change"
	DirectionRegress  Direction = "regress"
)

// CriterionOutcome is retained, concise evaluation evidence. Summary is an
// auditable conclusion, not hidden model reasoning.
type CriterionOutcome struct {
	CriterionID  string   `json:"criterionId"`
	Required     bool     `json:"required,omitempty"`
	Passed       bool     `json:"passed"`
	Score        float64  `json:"score"`
	Weight       float64  `json:"weight,omitempty"`
	Summary      string   `json:"summary"`
	EvidenceRefs []string `json:"evidenceRefs"`
}

type EvaluationPolicy struct {
	MinimumCriteria     int     `json:"minimumCriteria"`
	PromotionThreshold  float64 `json:"promotionThreshold"`
	RegressionThreshold float64 `json:"regressionThreshold"`
	RequireEvidence     bool    `json:"requireEvidence,omitempty"`
	AllowPromotion      bool    `json:"allowPromotion,omitempty"`
	AllowRegression     bool    `json:"allowRegression,omitempty"`
}

type EvaluationRequest struct {
	Scope                      capability.ScopeReference `json:"scope"`
	OwnerKind                  OwnerKind                 `json:"ownerKind"`
	DeploymentID               string                    `json:"deploymentId"`
	ExpectedDeploymentRevision int64                     `json:"expectedDeploymentRevision"`
	BaseVersion                string                    `json:"baseVersion"`
	BaseDigest                 string                    `json:"baseDigest"`
	Policy                     EvaluationPolicy          `json:"policy"`
	Outcomes                   []CriterionOutcome        `json:"outcomes"`
}

// Recommendation is deterministic for the same scoped deployment revision,
// base definition, policy, outcomes, and evidence. Its key is safe to use as
// the idempotency key of the resulting governed amendment proposal.
type Recommendation struct {
	ID                         string                    `json:"id"`
	Scope                      capability.ScopeReference `json:"scope"`
	OwnerKind                  OwnerKind                 `json:"ownerKind"`
	DeploymentID               string                    `json:"deploymentId"`
	ExpectedDeploymentRevision int64                     `json:"expectedDeploymentRevision"`
	BaseVersion                string                    `json:"baseVersion"`
	BaseDigest                 string                    `json:"baseDigest"`
	Direction                  Direction                 `json:"direction"`
	AggregateScore             float64                   `json:"aggregateScore"`
	RequiredCriteriaPassed     bool                      `json:"requiredCriteriaPassed"`
	Rationale                  string                    `json:"rationale"`
	EvidenceRefs               []string                  `json:"evidenceRefs"`
	Policy                     EvaluationPolicy          `json:"policy"`
	Outcomes                   []CriterionOutcome        `json:"outcomes"`
	EvaluationDigest           string                    `json:"evaluationDigest"`
	IdempotencyKey             string                    `json:"idempotencyKey"`
}

// Evaluate is pure and deterministic. Required failures always recommend a
// regression when policy permits; thresholds apply only after that fail-safe.
func Evaluate(request EvaluationRequest) (*Recommendation, error) {
	request.Scope.Kind, request.Scope.ID = strings.TrimSpace(request.Scope.Kind), strings.TrimSpace(request.Scope.ID)
	request.DeploymentID, request.BaseVersion, request.BaseDigest = strings.TrimSpace(request.DeploymentID), strings.TrimSpace(request.BaseVersion), strings.TrimSpace(request.BaseDigest)
	policy := request.Policy
	if request.Scope.Kind == "" || request.Scope.ID == "" || request.DeploymentID == "" || request.ExpectedDeploymentRevision < 1 || request.BaseVersion == "" || request.BaseDigest == "" ||
		(request.OwnerKind != OwnerAgent && request.OwnerKind != OwnerTeam) || policy.MinimumCriteria < 1 || len(request.Outcomes) < policy.MinimumCriteria ||
		policy.RegressionThreshold < 0 || policy.PromotionThreshold > 1 || policy.RegressionThreshold >= policy.PromotionThreshold {
		return nil, ErrInvalidEvaluation
	}
	outcomes := append([]CriterionOutcome(nil), request.Outcomes...)
	seen := make(map[string]bool, len(outcomes))
	evidence := make([]string, 0)
	weighted, weights := 0.0, 0.0
	requiredPassed := true
	for index := range outcomes {
		outcome := &outcomes[index]
		outcome.CriterionID, outcome.Summary = strings.TrimSpace(outcome.CriterionID), strings.TrimSpace(outcome.Summary)
		outcome.EvidenceRefs = normalized(outcome.EvidenceRefs)
		if outcome.CriterionID == "" || seen[outcome.CriterionID] || outcome.Summary == "" || outcome.Score < 0 || outcome.Score > 1 || outcome.Weight < 0 || policy.RequireEvidence && len(outcome.EvidenceRefs) == 0 {
			return nil, ErrInvalidEvaluation
		}
		seen[outcome.CriterionID] = true
		weight := outcome.Weight
		if weight == 0 {
			weight = 1
		}
		weighted, weights = weighted+outcome.Score*weight, weights+weight
		if outcome.Required && !outcome.Passed {
			requiredPassed = false
		}
		evidence = append(evidence, outcome.EvidenceRefs...)
	}
	sort.Slice(outcomes, func(i, j int) bool { return outcomes[i].CriterionID < outcomes[j].CriterionID })
	aggregate := weighted / weights
	direction := DirectionNoChange
	switch {
	case !requiredPassed || aggregate <= policy.RegressionThreshold:
		if policy.AllowRegression {
			direction = DirectionRegress
		}
	case aggregate >= policy.PromotionThreshold:
		if policy.AllowPromotion {
			direction = DirectionPromote
		}
	}
	rationale := fmt.Sprintf("Evaluation recommends %s: aggregate %.3f across %d criteria; required criteria passed: %t.", direction, aggregate, len(outcomes), requiredPassed)
	canonical := request
	canonical.Outcomes = outcomes
	payload, _ := json.Marshal(canonical)
	digestBytes := sha256.Sum256(payload)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	identityBytes := sha256.Sum256([]byte("openseal.authority-progression.v1\x00" + digest))
	identity := hex.EncodeToString(identityBytes[:])
	return &Recommendation{
		ID: "progression:" + identity, Scope: request.Scope, OwnerKind: request.OwnerKind, DeploymentID: request.DeploymentID,
		ExpectedDeploymentRevision: request.ExpectedDeploymentRevision, BaseVersion: request.BaseVersion, BaseDigest: request.BaseDigest,
		Direction: direction, AggregateScore: aggregate, RequiredCriteriaPassed: requiredPassed, Rationale: rationale,
		EvidenceRefs: normalized(evidence), Policy: policy, Outcomes: outcomes, EvaluationDigest: digest, IdempotencyKey: "progression:" + identity,
	}, nil
}

type AgentPolicyCeiling struct {
	MaximumRisk       capability.RiskLevel `json:"maximumRisk"`
	MaxConcurrentRuns int                  `json:"maxConcurrentRuns"`
	AllowedSkillIDs   []string             `json:"allowedSkillIds,omitempty"`
	BudgetCeilings    map[string]float64   `json:"budgetCeilings,omitempty"`
}

type TeamPolicyCeiling struct {
	MaximumRisk            capability.RiskLevel `json:"maximumRisk"`
	MaximumDelegationDepth int                  `json:"maximumDelegationDepth"`
	MaximumConcurrency     int                  `json:"maximumConcurrency"`
	MaximumSpeakers        int                  `json:"maximumSpeakers,omitempty"`
	AllowedSkillIDs        []string             `json:"allowedSkillIds,omitempty"`
	AllowSharedWrite       bool                 `json:"allowSharedWrite,omitempty"`
}

type ProposeAgentRequest struct {
	Recommendation *Recommendation
	Candidate      *agent.AgentDefinition
	Ceiling        AgentPolicyCeiling
	ProposerType   string
	ProposerID     string
}

type ProposeTeamRequest struct {
	Recommendation *Recommendation
	Candidate      *team.Definition
	Ceiling        TeamPolicyCeiling
	ProposerType   string
	ProposerID     string
}

type Service struct {
	agents *agent.Registry
	teams  *team.Registry
}

func NewService(agents *agent.Registry, teams *team.Registry) *Service {
	return &Service{agents: agents, teams: teams}
}

func (s *Service) RecommendAgent(ctx context.Context, scope capability.ScopeReference, deploymentID string, expectedRevision int64, policy EvaluationPolicy, outcomes []CriterionOutcome) (*Recommendation, error) {
	if s == nil || s.agents == nil {
		return nil, errors.New("Agent progression registry is not configured")
	}
	deployment, err := s.agents.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	if deployment.Revision != expectedRevision {
		return nil, agent.ErrRevisionConflict
	}
	definition, err := s.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if err := validateDefinitionOutcomes(definition.Evaluations, outcomes); err != nil {
		return nil, err
	}
	return Evaluate(EvaluationRequest{Scope: scope, OwnerKind: OwnerAgent, DeploymentID: deployment.ID, ExpectedDeploymentRevision: deployment.Revision, BaseVersion: definition.Version, BaseDigest: definition.Digest, Policy: policy, Outcomes: outcomes})
}

func (s *Service) RecommendTeam(ctx context.Context, scope capability.ScopeReference, deploymentID string, expectedRevision int64, policy EvaluationPolicy, outcomes []CriterionOutcome) (*Recommendation, error) {
	if s == nil || s.teams == nil {
		return nil, errors.New("Team progression registry is not configured")
	}
	deployment, err := s.teams.GetDeployment(ctx, scope, deploymentID)
	if err != nil {
		return nil, err
	}
	if deployment.Revision != expectedRevision {
		return nil, team.ErrRevisionConflict
	}
	definition, err := s.teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if err := validateDefinitionOutcomes(definition.Evaluations, outcomes); err != nil {
		return nil, err
	}
	return Evaluate(EvaluationRequest{Scope: scope, OwnerKind: OwnerTeam, DeploymentID: deployment.ID, ExpectedDeploymentRevision: deployment.Revision, BaseVersion: definition.Version, BaseDigest: definition.Digest, Policy: policy, Outcomes: outcomes})
}

func (s *Service) ProposeAgent(ctx context.Context, request ProposeAgentRequest) (*agent.DefinitionAmendment, error) {
	if s == nil || s.agents == nil || request.Recommendation == nil || request.Candidate == nil {
		return nil, ErrInvalidEvaluation
	}
	recommendation := request.Recommendation
	if err := validateRecommendation(recommendation); err != nil {
		return nil, err
	}
	if recommendation.OwnerKind != OwnerAgent {
		return nil, ErrInvalidEvaluation
	}
	if recommendation.Direction == DirectionNoChange {
		return nil, ErrNoChange
	}
	deployment, err := s.agents.GetDeployment(ctx, recommendation.Scope, recommendation.DeploymentID)
	if err != nil {
		return nil, err
	}
	if deployment.Revision != recommendation.ExpectedDeploymentRevision || deployment.ActiveVersion != recommendation.BaseVersion {
		return nil, ErrRecommendationStale
	}
	base, err := s.agents.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if base.Digest != recommendation.BaseDigest {
		return nil, ErrRecommendationStale
	}
	if err := validateAgentCeiling(request.Candidate, request.Ceiling); err != nil {
		return nil, err
	}
	if recommendation.Direction == DirectionRegress && agentAuthorityWidened(base, request.Candidate) {
		return nil, ErrPolicyCeiling
	}
	return s.agents.ProposeAmendment(ctx, agent.ProposeAmendmentRequest{
		Scope: recommendation.Scope, DeploymentID: recommendation.DeploymentID, Candidate: request.Candidate,
		ProposerType: request.ProposerType, ProposerID: request.ProposerID, Rationale: recommendation.Rationale,
		EvidenceRefs: recommendation.EvidenceRefs, IdempotencyKey: recommendation.IdempotencyKey,
		ExpectedDeploymentRevision: recommendation.ExpectedDeploymentRevision,
	})
}

func (s *Service) ProposeTeam(ctx context.Context, request ProposeTeamRequest) (*team.DefinitionAmendment, error) {
	if s == nil || s.teams == nil || request.Recommendation == nil || request.Candidate == nil {
		return nil, ErrInvalidEvaluation
	}
	recommendation := request.Recommendation
	if err := validateRecommendation(recommendation); err != nil {
		return nil, err
	}
	if recommendation.OwnerKind != OwnerTeam {
		return nil, ErrInvalidEvaluation
	}
	if recommendation.Direction == DirectionNoChange {
		return nil, ErrNoChange
	}
	deployment, err := s.teams.GetDeployment(ctx, recommendation.Scope, recommendation.DeploymentID)
	if err != nil {
		return nil, err
	}
	if deployment.Revision != recommendation.ExpectedDeploymentRevision || deployment.ActiveVersion != recommendation.BaseVersion {
		return nil, ErrRecommendationStale
	}
	base, err := s.teams.GetDefinition(ctx, deployment.DefinitionID, deployment.ActiveVersion)
	if err != nil {
		return nil, err
	}
	if base.Digest != recommendation.BaseDigest {
		return nil, ErrRecommendationStale
	}
	if err := validateTeamCeiling(request.Candidate, request.Ceiling); err != nil {
		return nil, err
	}
	if recommendation.Direction == DirectionRegress && teamAuthorityWidened(base, request.Candidate) {
		return nil, ErrPolicyCeiling
	}
	return s.teams.ProposeAmendment(ctx, team.ProposeAmendmentRequest{
		Scope: recommendation.Scope, DeploymentID: recommendation.DeploymentID, Candidate: request.Candidate,
		ProposerType: request.ProposerType, ProposerID: request.ProposerID, Rationale: recommendation.Rationale,
		EvidenceRefs: recommendation.EvidenceRefs, IdempotencyKey: recommendation.IdempotencyKey,
		ExpectedDeploymentRevision: recommendation.ExpectedDeploymentRevision,
	})
}

func validateRecommendation(value *Recommendation) error {
	if value == nil {
		return ErrInvalidEvaluation
	}
	derived, err := Evaluate(EvaluationRequest{
		Scope: value.Scope, OwnerKind: value.OwnerKind, DeploymentID: value.DeploymentID,
		ExpectedDeploymentRevision: value.ExpectedDeploymentRevision, BaseVersion: value.BaseVersion, BaseDigest: value.BaseDigest,
		Policy: value.Policy, Outcomes: value.Outcomes,
	})
	if err != nil {
		return err
	}
	if derived.ID != value.ID || derived.Direction != value.Direction || derived.AggregateScore != value.AggregateScore ||
		derived.RequiredCriteriaPassed != value.RequiredCriteriaPassed || derived.EvaluationDigest != value.EvaluationDigest ||
		derived.IdempotencyKey != value.IdempotencyKey || derived.Rationale != value.Rationale || !equalStrings(derived.EvidenceRefs, value.EvidenceRefs) {
		return ErrInvalidEvaluation
	}
	return nil
}

func validateDefinitionOutcomes(criteria []workforce.EvaluationCriterion, outcomes []CriterionOutcome) error {
	if len(criteria) == 0 || len(outcomes) != len(criteria) {
		return ErrInvalidEvaluation
	}
	declared := make(map[string]bool, len(criteria))
	for _, criterion := range criteria {
		id := strings.TrimSpace(criterion.ID)
		if id == "" || declared[id] {
			return ErrInvalidEvaluation
		}
		declared[id] = criterion.Required
	}
	seen := make(map[string]bool, len(outcomes))
	for _, outcome := range outcomes {
		id := strings.TrimSpace(outcome.CriterionID)
		required, ok := declared[id]
		if !ok || seen[id] || outcome.Required != required {
			return ErrInvalidEvaluation
		}
		seen[id] = true
	}
	return nil
}

func validateAgentCeiling(candidate *agent.AgentDefinition, ceiling AgentPolicyCeiling) error {
	if riskRank(ceiling.MaximumRisk) < 0 || ceiling.MaxConcurrentRuns < 1 || riskRank(candidate.Authority.MaximumRisk) > riskRank(ceiling.MaximumRisk) || candidate.Authority.MaxConcurrentRuns > ceiling.MaxConcurrentRuns || !subset(candidate.Authority.AllowedSkillIDs, ceiling.AllowedSkillIDs) {
		return ErrPolicyCeiling
	}
	for name, amount := range candidate.Authority.BudgetCeilings {
		limit, ok := ceiling.BudgetCeilings[name]
		if !ok || amount > limit {
			return ErrPolicyCeiling
		}
	}
	return nil
}

func validateTeamCeiling(candidate *team.Definition, ceiling TeamPolicyCeiling) error {
	if riskRank(ceiling.MaximumRisk) < 0 || ceiling.MaximumDelegationDepth < 0 || ceiling.MaximumConcurrency < 0 || riskRank(candidate.Approvals.MaximumRisk) > riskRank(ceiling.MaximumRisk) || candidate.Delegation.MaximumDepth > ceiling.MaximumDelegationDepth || candidate.Delegation.MaximumConcurrent > ceiling.MaximumConcurrency || ceiling.MaximumSpeakers > 0 && candidate.Coordination.MaximumSpeakersPerRound > ceiling.MaximumSpeakers || candidate.SharedContext.AllowMemberWrite && !ceiling.AllowSharedWrite {
		return ErrPolicyCeiling
	}
	for _, role := range candidate.Roles {
		for _, grant := range role.SkillGrants {
			if !subset([]string{grant.SkillID}, ceiling.AllowedSkillIDs) || riskRank(grant.MaximumRisk) > riskRank(ceiling.MaximumRisk) {
				return ErrPolicyCeiling
			}
		}
	}
	return nil
}

func agentAuthorityWidened(base, candidate *agent.AgentDefinition) bool {
	if riskRank(candidate.Authority.MaximumRisk) > riskRank(base.Authority.MaximumRisk) || candidate.Authority.MaxConcurrentRuns > base.Authority.MaxConcurrentRuns || !subset(candidate.Authority.AllowedSkillIDs, base.Authority.AllowedSkillIDs) {
		return true
	}
	for name, amount := range candidate.Authority.BudgetCeilings {
		if amount > base.Authority.BudgetCeilings[name] {
			return true
		}
	}
	return false
}

func teamAuthorityWidened(base, candidate *team.Definition) bool {
	if riskRank(candidate.Approvals.MaximumRisk) > riskRank(base.Approvals.MaximumRisk) || candidate.Delegation.MaximumDepth > base.Delegation.MaximumDepth || candidate.Delegation.MaximumConcurrent > base.Delegation.MaximumConcurrent || candidate.SharedContext.AllowMemberWrite && !base.SharedContext.AllowMemberWrite {
		return true
	}
	baseGrants := make(map[string]team.RoleSkillGrant)
	for _, role := range base.Roles {
		for _, grant := range role.SkillGrants {
			baseGrants[role.ID+"\x00"+grant.ExactIdentity().Key()] = grant
		}
	}
	for _, role := range candidate.Roles {
		for _, grant := range role.SkillGrants {
			old, ok := baseGrants[role.ID+"\x00"+grant.ExactIdentity().Key()]
			if !ok || riskRank(grant.MaximumRisk) > riskRank(old.MaximumRisk) || !subset(grant.AllowedActions, old.AllowedActions) {
				return true
			}
		}
	}
	return false
}

func riskRank(value capability.RiskLevel) int {
	switch value {
	case capability.RiskLevelRead:
		return 0
	case capability.RiskLevelWrite:
		return 1
	case capability.RiskLevelExternal:
		return 2
	case capability.RiskLevelProduction:
		return 3
	case capability.RiskLevelDestructive:
		return 4
	}
	return -1
}
func subset(values, allowed []string) bool {
	set := make(map[string]bool, len(allowed))
	for _, value := range allowed {
		set[strings.TrimSpace(value)] = true
	}
	for _, value := range values {
		if !set[strings.TrimSpace(value)] {
			return false
		}
	}
	return true
}
func normalized(values []string) []string {
	set := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !set[value] {
			set[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
