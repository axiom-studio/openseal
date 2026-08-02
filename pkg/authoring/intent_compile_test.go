package authoring

import (
	"context"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

type semanticIntentGenerator struct{ intent AuthoringIntent }

func (g semanticIntentGenerator) GenerateIntent(context.Context, GenerateRequest) (AuthoringIntent, error) {
	return g.intent, nil
}

func TestCompilerUsesSemanticIntentBoundary(t *testing.T) {
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent,
		Name: "Analyst", Purpose: "Analyze evidence",
		Agents: []AuthoringAgentIntent{{Key: "analyst", Name: "Analyst", Purpose: "Analyze evidence", Behavior: "Analyze supplied evidence accurately."}},
	}
	compiler, err := NewCompiler(semanticIntentGenerator{intent: intent})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: "Create an Analyst Agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Valid || len(result.Candidate.Agents) != 1 || result.Candidate.Agents[0].ID != "analyst" {
		t.Fatalf("semantic compile result = %#v", result)
	}
}

func TestCompileAuthoringIntentOwnsLifecycleActivation(t *testing.T) {
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent,
		Name: "Analyst", Purpose: "Analyze evidence",
		Agents: []AuthoringAgentIntent{{Key: "analyst", Name: "Analyst", Purpose: "Analyze evidence", Behavior: "Analyze supplied evidence accurately."}},
	}
	created, err := CompileAuthoringIntent(intent, GenerateRequest{Mode: ModeCreate, Prompt: "Create an Analyst Agent"})
	if err != nil || created.Candidate.Activation != WorkforceActivationActive {
		t.Fatalf("created activation=%q err=%v", created.Candidate.Activation, err)
	}
	inactive, err := CompileAuthoringIntent(intent, GenerateRequest{Mode: ModeCreate, Prompt: "Create an inactive Analyst Agent"})
	if err != nil || inactive.Candidate.Activation != WorkforceActivationInactive {
		t.Fatalf("inactive activation=%q err=%v", inactive.Candidate.Activation, err)
	}
	existing := inactive.Candidate
	amended, err := CompileAuthoringIntent(intent, GenerateRequest{Mode: ModeAmend, Prompt: "Rename the Agent", Existing: &existing})
	if err != nil || amended.Candidate.Activation != WorkforceActivationInactive {
		t.Fatalf("amended activation=%q err=%v", amended.Candidate.Activation, err)
	}
}

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
		Name: "Reddit Researcher", Purpose: "Find and discuss relevant engineering posts",
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
	if budget := definition.Runbook.Steps["hourly-research"].Delegate.Budget; budget == nil || budget.MaxTotalTokens != runbook.DefaultRunTotalTokenBudget {
		t.Fatalf("delegate default budget = %#v", budget)
	}
	if budget := definition.Runbook.Triggers["hourly-research"].Budget; budget == nil || budget.MaxTotalTokens != runbook.DefaultRunTotalTokenBudget {
		t.Fatalf("trigger default budget = %#v", budget)
	}
	if definition.Authority.MaximumRisk != capability.RiskLevelExternal || len(definition.Authority.StandingGrants) != 0 {
		t.Fatalf("compiled authority = %#v", definition.Authority)
	}
	if issues := validateCandidate(&generated.Candidate, nil); len(issues) > 0 {
		t.Fatalf("compiled candidate issues = %#v", issues)
	}
}

func TestCompileAuthoringIntentUsesAuditedScheduleInsteadOfProviderParaphrase(t *testing.T) {
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent,
		Name: "Scout", Purpose: "Scout communities",
		Agents: []AuthoringAgentIntent{{
			Key: "scout", Name: "Scout", Purpose: "Scout communities", Behavior: "Find useful discussions.",
			Objectives: []AuthoringObjectiveIntent{{Key: "engagement", Title: "Community engagement", Outcome: "Engage usefully", Priority: 1}},
			Operations: []AuthoringOperationIntent{{
				Key: "scheduled-scout", Name: "Scheduled scout", Goal: "Scout every three hours",
				ObjectiveKey: "engagement", Wake: AuthoringWakeSchedule,
				Schedule: "every 3 hours between 08:00 and 20:00 local time", Approval: AuthoringApprovalByPolicy,
			}},
		}},
	}

	refinement := &RefinementContext{Answers: []RefinementResolvedAnswer{{
		QuestionID: scheduleIntentQuestionID, Source: RefinementAnswerSourceUser,
		Value: RefinementProviderAnswerValue{Text: "every 3 hours between 08:00 and 20:00 UTC"},
	}}}
	generated, err := CompileAuthoringIntent(intent, GenerateRequest{
		Mode: ModeCreate, Prompt: "Scout every three hours", Refinement: refinement,
	})
	if err != nil {
		t.Fatal(err)
	}
	trigger := generated.Candidate.Agents[0].Runbook.Triggers["scheduled-scout"]
	if trigger.Schedule == nil || trigger.Schedule.Cron != "0 00 8-20/3 * * *" || trigger.Schedule.Timezone != "UTC" {
		t.Fatalf("compiled trigger = %#v", trigger)
	}
}

func TestCompileAuthoringIntentBuildsTeamRolesAndAssignments(t *testing.T) {
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceTeam,
		Name: "Release Team", Purpose: "Prepare and review releases",
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
		Name: "Researcher", Purpose: "Research sources",
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

func TestCompileAuthoringIntentBuildsConversationAndApprovalEdges(t *testing.T) {
	catalog := slackChatbotCatalog()
	slack := catalog.Skills["slack"]
	slack.ConversationAdapters[0].InboundEventTypes = []string{capability.ConversationEventApprovalDecided, capability.ConversationEventMessageReceived}
	catalog.Skills["slack"] = slack
	intent := AuthoringIntent{
		SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent,
		Name: "Slack Helper", Purpose: "Help in a Slack channel",
		Agents: []AuthoringAgentIntent{{
			Key: "slack-helper", Name: "Slack Helper", Purpose: "Help in Slack", Behavior: "Answer channel questions accurately.",
			Objectives: []AuthoringObjectiveIntent{{Key: "help", Title: "Help people", Outcome: "Answer questions", Priority: 1}},
			Operations: []AuthoringOperationIntent{{
				Key: "answer", Name: "Answer", Goal: "Answer a question", ObjectiveKey: "help", Wake: AuthoringWakeOnDemand,
				Approval: AuthoringApprovalRequired, ApprovalDelivery: AuthoringApprovalDeliveryChannels,
				ApprovalChannelKeys: []string{"slack-help"},
			}},
		}},
		Conversations: []AuthoringChannelIntent{{
			Key: "slack-help", Name: "Slack help channel", OwnerKey: "slack-helper", Provider: "slack", Destination: "#help",
			ReceiveMessages: true, ReplyInThread: true,
		}},
	}
	generated, err := CompileAuthoringIntent(intent, GenerateRequest{Mode: ModeCreate, Prompt: slackChatbotPrompt, Catalog: catalog})
	if err != nil {
		t.Fatal(err)
	}
	form, err := ProjectWorkforceAuthoringForm(catalog, nil)
	if err != nil {
		t.Fatal(err)
	}
	if issues := CompileWorkforceAuthoringForm(&generated.Candidate, form, generated.Authoring); len(issues) > 0 {
		t.Fatalf("conversation form issues = %#v", issues)
	}
	endpoint := generated.Candidate.ConversationEndpoints[0]
	if endpoint.Handler.Kind != ConversationHandlerAgent || endpoint.Policy.ReplyMode != ConversationReplyThread || len(generated.Candidate.Agents[0].Authority.ApprovalDestinations) != 1 {
		t.Fatalf("compiled conversation = %#v authority=%#v", endpoint, generated.Candidate.Agents[0].Authority)
	}
	if issues := validateCandidate(&generated.Candidate, nil); len(issues) > 0 {
		t.Fatalf("candidate issues = %#v", issues)
	}
	if issues := validateConversationComposition(&generated.Candidate, GenerateRequest{Mode: ModeCreate, Prompt: slackChatbotPrompt, Catalog: catalog}); len(issues) > 0 {
		t.Fatalf("conversation issues = %#v", issues)
	}
}

func TestSemanticIntentRoundTripsAmendmentIdentityWithoutRuntimeJSON(t *testing.T) {
	existing := WorkforceCandidate{Activation: WorkforceActivationActive, Agents: []*agent.AgentDefinition{{
		ID: "tenant/example/researcher", Version: "2.4.9", DisplayName: "Researcher", Purpose: "Research evidence", SystemPrompt: "Research evidence accurately.",
		Authority:          agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		ObjectiveTemplates: []workforce.ObjectiveTemplate{{ID: "research", Title: "Research", Goal: "Find useful evidence", Priority: 1}},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "research-operations", Version: "2.4.9", Name: "Research operations",
			Entrypoints: map[string]string{"scan_and_report": "scan_and_report"}, Interfaces: map[string]runbook.Interface{"scan_and_report": {Description: "Scan every hour", InputSchema: map[string]interface{}{"type": "object"}}},
			Triggers: map[string]runbook.Trigger{"scan_schedule": {Kind: runbook.TriggerSchedule, Schedule: &runbook.Schedule{Cron: "0 0 * * * *", Timezone: "UTC"}, Entrypoint: "scan_and_report", ObjectiveID: "agent:tenant/example/researcher:research"}},
			Steps: map[string]runbook.Step{
				"scan_and_report": {Kind: runbook.StepDelegate, Name: "Scan", Delegate: &runbook.DelegateStep{AgentID: literalRunbookValue("tenant/example/researcher"), Goal: literalRunbookValue("Find useful evidence"), Mode: runbook.DelegateReason, ResultPath: "/results/scan", Next: "done"}},
				"done":            {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
			},
		},
	}}}
	intent := ProjectAuthoringIntent(&existing)
	if intent == nil {
		t.Fatal("semantic projection is nil")
	}
	if operation := intent.Agents[0].Operations[0]; operation.Key != "scan-and-report" || !validAuthoringIntentKey(operation.Key) {
		t.Fatalf("projected operation key = %q", operation.Key)
	}
	intent.Agents[0].Name = "Senior Researcher"
	generated, err := CompileAuthoringIntent(*intent, GenerateRequest{Mode: ModeAmend, Prompt: "Rename the Agent", Existing: &existing})
	if err != nil {
		t.Fatal(err)
	}
	definition := generated.Candidate.Agents[0]
	if definition.ID != "tenant/example/researcher" || definition.Version != "2.4.10" || definition.DisplayName != "Senior Researcher" {
		t.Fatalf("amended identity = %#v", definition)
	}
	if trigger := definition.Runbook.Triggers["scan-and-report"]; trigger.ObjectiveID != "agent:tenant/example/researcher:research" {
		t.Fatalf("amended trigger = %#v", trigger)
	}
}
