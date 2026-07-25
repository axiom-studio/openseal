package authoring

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const liveReleaseNotesPrompt = "Create one release-notes agent with one objective to turn completed development work into release notes. Require approval before any external publication."
const shippedSREPrompt = "Create one SRE Agent that watches Kubernetes events, investigates safely, and asks before production changes."

func TestCompilerRejectsLiveReleaseNotesCandidateThatDropsExplicitCommitments(t *testing.T) {
	candidate := releaseNotesCandidate(nil, "")
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate, Commitments: releaseNotesCommitments()})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: liveReleaseNotesPrompt})
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || !hasValidationCode(result.Validation, "prompt_objective_mismatch") || hasValidationCode(result.Validation, "prompt_approval_mismatch") {
		t.Fatalf("omitted live commitments = %#v", result)
	}
	if got := result.Candidate.Agents[0].Authority.RequireApprovalAt; got != capability.RiskLevelWrite {
		t.Fatalf("deterministically repaired approval threshold = %q", got)
	}
	if result.Commitments.AgentCount == nil || *result.Commitments.AgentCount != 1 || len(result.Commitments.ObjectiveCounts) != 1 ||
		result.Commitments.ObjectiveCounts[0].OwnerType != CommitmentOwnerAgent || result.Commitments.ObjectiveCounts[0].Count != 1 ||
		len(result.Commitments.ApprovalRequirements) != 1 || result.Commitments.ApprovalRequirements[0].RequireApprovalAt != capability.RiskLevelWrite {
		t.Fatalf("extracted live commitments = %#v", result.Commitments)
	}
}

func TestCompilerDeterministicallyRepairsShippedSingleAgentSREApproval(t *testing.T) {
	agentCount, teamCount := 1, 0
	candidate := releaseNotesCandidate(nil, "")
	candidate.Agents[0].ID = "sre-agent"
	candidate.Agents[0].DisplayName = "SRE Agent"
	candidate.Agents[0].Purpose = "Watch Kubernetes events and investigate safely"
	candidate.Agents[0].SystemPrompt = "Watch Kubernetes events, investigate safely, and ask before production changes."
	payload, _ := json.Marshal(GenerationResponse{
		Candidate:   candidate,
		Commitments: PromptCommitments{AgentCount: &agentCount, TeamCount: &teamCount},
	})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: shippedSREPrompt})
	if err != nil || !result.Valid {
		t.Fatalf("shipped SRE candidate = %#v, err = %v", result, err)
	}
	if len(result.Candidate.Agents) != 1 || result.Candidate.Team != nil {
		t.Fatalf("shipped SRE topology = %#v", result.Candidate)
	}
	definition := result.Candidate.Agents[0]
	if definition.Authority.MaximumRisk != capability.RiskLevelDestructive || definition.Authority.RequireApprovalAt != capability.RiskLevelWrite {
		t.Fatalf("shipped SRE authority = %#v", definition.Authority)
	}
	if len(result.Commitments.ApprovalRequirements) != 1 || result.Commitments.ApprovalRequirements[0].RequireApprovalAt != capability.RiskLevelWrite {
		t.Fatalf("shipped SRE commitments = %#v", result.Commitments)
	}
}

func TestCompilerRepairsLiveReleaseNotesCommitmentsOnce(t *testing.T) {
	generated, _ := json.Marshal(GenerationResponse{Candidate: releaseNotesCandidate(nil, ""), Commitments: PromptCommitments{}})
	repairedCandidate := releaseNotesCandidate([]workforce.ObjectiveTemplate{{
		ID: "release-notes", Title: "Release notes", Goal: "Turn completed development work into release notes", Priority: 1,
	}}, capability.RiskLevelWrite)
	repaired, _ := json.Marshal(GenerationResponse{Candidate: repairedCandidate, Commitments: releaseNotesCommitments()})
	generator := &repairingGenerator{generated: generated, repaired: repaired}
	compiler, _ := NewCompiler(generator)
	result, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: liveReleaseNotesPrompt})
	if err != nil || !result.Valid || generator.repairs != 1 {
		t.Fatalf("repaired live candidate = %#v, repairs = %d, err = %v", result, generator.repairs, err)
	}
	if generator.lastError == nil || !strings.Contains(generator.lastError.Error(), "prompt_commitment_missing") ||
		!strings.Contains(generator.lastError.Error(), "prompt_objective_mismatch") || strings.Contains(generator.lastError.Error(), "prompt_approval_mismatch") {
		t.Fatalf("repair diagnostic = %v", generator.lastError)
	}
}

func TestCompilerValidatesMultiAgentTeamCountsObjectivePlacementAndInactivity(t *testing.T) {
	candidate := threeAgentTeamCandidate()
	agentCount, teamCount := 3, 1
	commitments := PromptCommitments{
		AgentCount: &agentCount, TeamCount: &teamCount, Activation: ActivationCommitmentInactive,
		ObjectiveCounts: []ObjectiveCountCommitment{{OwnerType: CommitmentOwnerTeam, OwnerID: candidate.Team.ID, Count: 2}},
	}
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate, Commitments: commitments})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create exactly three Agents and one Team with exactly two Team objectives. Do not activate.",
	})
	if err != nil || !result.Valid || result.Commitments.AgentCount == nil || *result.Commitments.AgentCount != 3 ||
		result.Commitments.TeamCount == nil || *result.Commitments.TeamCount != 1 || result.Commitments.Activation != ActivationCommitmentInactive ||
		result.Candidate.Activation != WorkforceActivationInactive {
		t.Fatalf("multi-Agent commitments = %#v, err = %v", result, err)
	}
	if !containsString(result.Assumptions, "Atomic apply remains inactive by creating non-executing resources; activation requires a separate governed command.") {
		t.Fatalf("inactive assumption = %#v", result.Assumptions)
	}

	broken := candidate
	broken.Agents = broken.Agents[:2]
	broken.Assignments = broken.Assignments[:2]
	broken.Team.Roles[0].MinimumMembers = 2
	broken.Team.Roles[0].MaximumMembers = 2
	broken.Team.ObjectiveTemplates = broken.Team.ObjectiveTemplates[:1]
	payload, _ = json.Marshal(GenerationResponse{Candidate: broken, Commitments: commitments})
	compiler, _ = NewCompiler(staticGenerator{payload: payload})
	result, err = compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create exactly three Agents and one Team with exactly two Team objectives. Do not activate.",
	})
	if err != nil || result.Valid || !hasValidationCode(result.Validation, "prompt_count_mismatch") || !hasValidationCode(result.Validation, "prompt_objective_mismatch") {
		t.Fatalf("broken multi-Agent commitments = %#v, err = %v", result, err)
	}
}

func TestCommitmentExtractionDoesNotClaimOpenEndedSemanticEquivalence(t *testing.T) {
	candidate := releaseNotesCandidate(nil, capability.RiskLevelWrite)
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate, Commitments: PromptCommitments{}})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Review a report that mentions three agents and one objective from an earlier system.",
	})
	if err != nil || !result.Valid || result.Commitments.AgentCount != nil || len(result.Commitments.ObjectiveCounts) != 0 {
		t.Fatalf("open-ended prose was overclaimed = %#v, err = %v", result, err)
	}
}

func TestGenerationResponseRejectsUnknownCommitmentFields(t *testing.T) {
	_, err := decodeGenerationResponse([]byte(`{"candidate":{"agents":[]},"commitments":{"agentCount":1,"semanticIntent":"trust me"}}`))
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown commitment field error = %v", err)
	}
}

func TestSpecificCommitmentDoesNotNarrowAGroupPrompt(t *testing.T) {
	agentCount := 3
	extracted := PromptCommitments{
		AgentCount: &agentCount,
		ApprovalRequirements: []ApprovalCommitment{{
			OwnerType: CommitmentOwnerAgent, RequireApprovalAt: capability.RiskLevelWrite,
		}},
	}
	declared := PromptCommitments{ApprovalRequirements: []ApprovalCommitment{{
		OwnerType: CommitmentOwnerAgent, OwnerID: "agent-1", RequireApprovalAt: capability.RiskLevelWrite,
	}}}
	issues := validateDeclaredCommitmentCoverage(declared, extracted)
	if !hasValidationCode(issues, "prompt_commitment_missing") {
		t.Fatalf("specific declaration silently narrowed a group commitment: %#v", issues)
	}
}

func FuzzExplicitPromptCommitmentExtraction(f *testing.F) {
	for _, seed := range []string{
		liveReleaseNotesPrompt,
		"Create exactly three Agents and one Team with exactly two Team objectives. Do not activate.",
		"Review a report that mentions three agents and one objective from an earlier system.",
		"Create 1001 agents; require approval before posting.",
		"\x00don't ACTIVATE — create two agents 🚀",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, prompt string) {
		first := extractExplicitPromptCommitments(prompt)
		second := extractExplicitPromptCommitments(prompt)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("extraction is not deterministic: %#v != %#v", first, second)
		}
		for _, count := range []*int{first.AgentCount, first.TeamCount} {
			if count != nil && (*count < 0 || *count > maximumExplicitCommitmentCount) {
				t.Fatalf("out-of-range count extracted: %d", *count)
			}
		}
		for _, objective := range first.ObjectiveCounts {
			if objective.Count < 0 || objective.Count > maximumExplicitCommitmentCount {
				t.Fatalf("out-of-range objective count extracted: %d", objective.Count)
			}
		}
		if first.Activation != "" && first.Activation != ActivationCommitmentInactive {
			t.Fatalf("unsupported activation commitment extracted: %q", first.Activation)
		}
		for _, approval := range first.ApprovalRequirements {
			if approval.OwnerType != CommitmentOwnerAgent || approval.RequireApprovalAt != capability.RiskLevelWrite {
				t.Fatalf("unsupported approval commitment extracted: %#v", approval)
			}
		}
	})
}

func releaseNotesCandidate(objectives []workforce.ObjectiveTemplate, approvalAt capability.RiskLevel) WorkforceCandidate {
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "release-notes", Version: "1", DisplayName: "Release notes", Purpose: "Turn completed development work into release notes",
		SystemPrompt:       "Draft factual release notes from completed development work.",
		Authority:          agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelDestructive, MaxConcurrentRuns: 1, RequireApprovalAt: approvalAt},
		ObjectiveTemplates: objectives,
	}}}
}

func releaseNotesCommitments() PromptCommitments {
	agentCount := 1
	return PromptCommitments{
		AgentCount:           &agentCount,
		ObjectiveCounts:      []ObjectiveCountCommitment{{OwnerType: CommitmentOwnerAgent, OwnerID: "release-notes", Count: 1}},
		ApprovalRequirements: []ApprovalCommitment{{OwnerType: CommitmentOwnerAgent, OwnerID: "release-notes", RequireApprovalAt: capability.RiskLevelWrite}},
	}
}

func threeAgentTeamCandidate() WorkforceCandidate {
	agents := make([]*agent.AgentDefinition, 0, 3)
	assignments := make([]Assignment, 0, 3)
	requiredDefinitions := make([]string, 0, 3)
	for index := 1; index <= 3; index++ {
		id := "agent-" + string(rune('0'+index))
		agents = append(agents, &agent.AgentDefinition{
			ID: id, Version: "1", DisplayName: "Agent " + string(rune('0'+index)), Purpose: "Own a distinct part of the work", SystemPrompt: "Collaborate calmly.",
			Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		})
		requiredDefinitions = append(requiredDefinitions, id)
		assignments = append(assignments, Assignment{ID: "assignment-" + id, RoleID: "member", AgentDefinitionID: id})
	}
	definition := &team.Definition{
		ID: "three-agent-team", Version: "1", DisplayName: "Three Agent Team", Purpose: "Coordinate three Agents",
		Roles:        []team.RoleSlot{{ID: "member", DisplayName: "Member", Purpose: "Contribute role-relevant work", MinimumMembers: 3, MaximumMembers: 3, RequiredDefinitionIDs: requiredDefinitions}},
		Coordination: team.CoordinationPolicy{}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{
			{ID: "first", Title: "First objective", Goal: "Complete the first outcome", Priority: 1},
			{ID: "second", Title: "Second objective", Goal: "Complete the second outcome", Priority: 2},
		},
	}
	return WorkforceCandidate{Agents: agents, Team: definition, Assignments: assignments}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
