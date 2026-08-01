package authoring

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestCompileAuthoringIntentOwnsScheduledRunbookStructure(t *testing.T) {
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"skill-browser": {
			ID: "skill-browser", Version: "2.0.0", Actions: []string{"navigate", "snapshot", "commit"},
			ActionRisks: map[string]capability.RiskLevel{"navigate": capability.RiskLevelRead, "snapshot": capability.RiskLevelRead, "commit": capability.RiskLevelExternal},
			MaximumRisk: capability.RiskLevelExternal,
		},
	}}
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent,
		Name: "Reddit Researcher", Purpose: "Find and discuss relevant engineering posts", Activation: WorkforceActivationActive,
		Agents: []AuthoringAgentIntent{{
			Key: "reddit-researcher", Name: "Reddit Researcher", Purpose: "Research relevant communities",
			Behavior:   "Read the full discussion, contribute useful context, and report the result.",
			Skills:     []AuthoringSkillIntent{{CatalogID: "skill-browser", Actions: []string{"navigate", "snapshot", "commit"}, Required: true}},
			Objectives: []AuthoringObjectiveIntent{{Key: "community-research", Title: "Research communities", Outcome: "Find and discuss relevant engineering posts", Priority: 1}},
			Operations: []AuthoringOperationIntent{{
				Key: "hourly-research", Name: "Hourly community research", Goal: "Review new posts and contribute when useful",
				ObjectiveKey: "community-research", Wake: AuthoringWakeSchedule, Schedule: "every hour",
				SkillCatalogIDs: []string{"skill-browser"}, Approval: AuthoringApprovalByPolicy, ReportProgress: true,
			}},
		}},
	}
	generated, err := CompileAuthoringIntent(intent, GenerateRequest{Mode: ModeCreate, Prompt: "Run every hour", Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	if len(generated.Candidate.Agents) != 1 {
		t.Fatalf("candidate Agents = %#v", generated.Candidate.Agents)
	}
	definition := generated.Candidate.Agents[0]
	if definition.Runbook == nil || definition.Runbook.Triggers["hourly-research"].Kind != runbook.TriggerSchedule ||
		definition.Runbook.Steps["hourly-research"].Delegate == nil || definition.Runbook.Steps["hourly-research"].Delegate.ResultPath != "/results/hourly-research" {
		t.Fatalf("compiler-owned Runbook = %#v", definition.Runbook)
	}
	if definition.Authority.MaximumRisk != capability.RiskLevelExternal || len(definition.Authority.StandingGrants) != 0 {
		t.Fatalf("compiled authority = %#v", definition.Authority)
	}
	if issues := validateCandidate(&generated.Candidate, nil); len(issues) > 0 {
		t.Fatalf("compiled candidate issues = %#v", issues)
	}
}

func TestCompileAuthoringIntentBuildsTeamRolesAndAssignments(t *testing.T) {
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceTeam,
		Name: "Release Team", Purpose: "Prepare and review releases", Activation: WorkforceActivationInactive,
		Agents: []AuthoringAgentIntent{
			{Key: "author", Name: "Release Author", Purpose: "Draft release material", Behavior: "Draft accurate release material."},
			{Key: "reviewer", Name: "Release Reviewer", Purpose: "Review release material", Behavior: "Review claims against evidence."},
		},
		Team: &AuthoringTeamIntent{
			Key: "release-team", Name: "Release Team", Purpose: "Prepare and review releases",
			Roles: []AuthoringRoleIntent{
				{Key: "authoring", Name: "Authoring", Purpose: "Draft release material", AgentKeys: []string{"author"}, CanSpeakInChannels: true},
				{Key: "review", Name: "Review", Purpose: "Review release material", AgentKeys: []string{"reviewer"}, CanSpeakInChannels: true},
			},
		},
	}
	generated, err := CompileAuthoringIntent(intent, GenerateRequest{Mode: ModeCreate, Prompt: "Create a two-Agent release Team"})
	if err != nil {
		t.Fatal(err)
	}
	if generated.Candidate.Team == nil || len(generated.Candidate.Team.Roles) != 2 || len(generated.Candidate.Assignments) != 2 {
		t.Fatalf("compiled Team = %#v assignments=%#v", generated.Candidate.Team, generated.Candidate.Assignments)
	}
	if issues := validateCandidate(&generated.Candidate, nil); len(issues) > 0 {
		t.Fatalf("compiled candidate issues = %#v", issues)
	}
}

func TestCompileAuthoringIntentMapsClarificationWithoutModelOwnedWireEnums(t *testing.T) {
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent,
		Name: "Researcher", Purpose: "Research sources", Activation: WorkforceActivationInactive,
		Agents:         []AuthoringAgentIntent{{Key: "researcher", Name: "Researcher", Purpose: "Research sources", Behavior: "Research only permitted sources."}},
		Clarifications: []AuthoringClarification{{Key: "report-format", Question: "Which report format should be used?", WhyNeeded: "The requested output format is ambiguous.", Choices: []string{"PDF", "Markdown"}}},
	}
	generated, err := CompileAuthoringIntent(intent, GenerateRequest{Mode: ModeCreate, Prompt: "Create a researcher"})
	if err != nil {
		t.Fatal(err)
	}
	question := generated.UnresolvedQuestions[0]
	if question.Category != RefinementCategoryOther || question.Answer.Kind != RefinementAnswerSingleSelect || len(question.Blocking) != 1 || question.Blocking[0] != RefinementBlocksCandidate {
		t.Fatalf("compiler-owned refinement = %#v", question)
	}
	if err := validateRefinementQuestions(generated.UnresolvedQuestions); err != nil {
		t.Fatal(err)
	}
}
