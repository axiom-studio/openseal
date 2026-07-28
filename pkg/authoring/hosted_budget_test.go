package authoring

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestHostedRunbookBudgetRejectsBrowserCatalogThatCannotStartOrFinish(t *testing.T) {
	candidate := hostedBrowserBudgetCandidate(runbook.BudgetAllocation{
		MaxAttempts: 3, MaxTurns: 3, MaxActions: 3,
		MaxInputTokens: 16000, MaxOutputTokens: 128, MaxTotalTokens: 20000,
	})
	issues := validateHostedRunbookBudgets(&candidate, hostedBrowserBudgetCatalog())
	for _, code := range []string{
		"hosted_budget_attempts_insufficient", "hosted_budget_turns_insufficient",
		"hosted_budget_input_insufficient", "hosted_budget_output_insufficient", "hosted_budget_total_insufficient",
	} {
		if !hasValidationCode(issues, code) {
			t.Fatalf("missing %s in issues %#v", code, issues)
		}
	}
}

func TestHostedRunbookBudgetAcceptsCompleteBrowserWorkflowEnvelope(t *testing.T) {
	candidate := hostedBrowserBudgetCandidate(runbook.BudgetAllocation{
		MaxAttempts: 4, MaxTurns: 4, MaxActions: 3,
		MaxInputTokens: 100000, MaxOutputTokens: 1024, MaxTotalTokens: 101024,
	})
	if issues := validateHostedRunbookBudgets(&candidate, hostedBrowserBudgetCatalog()); len(issues) != 0 {
		t.Fatalf("complete Browser budget issues = %#v", issues)
	}
}

func TestHostedExecutionAttestationStaysOutOfProviderCatalog(t *testing.T) {
	catalog := hostedBrowserBudgetCatalog()
	compact := compactPromptCapabilityCatalog(catalog)
	if compact.HostedExecution != nil || compact.Skills["skill-browser"].HostedModelInputTokens != 0 {
		t.Fatalf("hosted execution attestation leaked into provider catalog: %#v", compact)
	}
}

func hostedBrowserBudgetCandidate(budget runbook.BudgetAllocation) WorkforceCandidate {
	targetID := literalActionValue("agent/browser")
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{
		{
			ID: "agent/scheduler", SystemPrompt: "Start the reviewed routine on schedule.",
			Runbook: &runbook.Definition{Steps: map[string]runbook.Step{
				"work": {Kind: runbook.StepDelegate, Delegate: &runbook.DelegateStep{
					AgentID: targetID, Goal: literalActionValue("Use the Browser safely."), Mode: runbook.DelegateReason, Budget: &budget,
				}},
			}},
		},
		{
			ID: "agent/browser", SystemPrompt: "Browse the authorized source and perform the reviewed operation.",
			SkillRequirements: []agent.SkillRequirement{{
				SkillID: "skill-browser", RequiredActions: []string{"browser-open", "browser-snapshot", "browser-commit"},
			}},
		},
	}}
}

func hostedBrowserBudgetCatalog() CapabilityCatalog {
	return CapabilityCatalog{
		HostedExecution: &HostedExecutionCapability{
			ProtocolVersion: HostedExecutionProtocolV1, BaseInputTokens: 6400, MinimumOutputTokens: 64,
		},
		Skills: map[string]SkillCapability{
			"skill-browser": {
				ID: "skill-browser", Actions: []string{"browser-open", "browser-snapshot", "browser-commit"},
				HostedModelInputTokens: 14000,
			},
		},
	}
}
