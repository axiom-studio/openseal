package authoring

import (
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

func TestRunbookActionContractsRequireWiredArguments(t *testing.T) {
	candidate := runbookActionContractCandidate(map[string]runbook.Value{
		"url": literalActionValue("https://example.com/feed"),
	})
	issues := validateRunbookActionContracts(&candidate, runbookActionContractCatalog())
	if !hasValidationCode(issues, "runbook_action_argument_required") {
		t.Fatalf("missing session argument issues = %#v", issues)
	}
}

func TestRunbookActionContractsAcceptReferencesAndValidateLiterals(t *testing.T) {
	candidate := runbookActionContractCandidate(map[string]runbook.Value{
		"sessionId": {Ref: "/results/browserSession/sessionId"},
		"url":       literalActionValue("https://example.com/feed"),
	})
	if issues := validateRunbookActionContracts(&candidate, runbookActionContractCatalog()); len(issues) != 0 {
		t.Fatalf("valid Runbook arguments issues = %#v", issues)
	}

	candidate.Agents[0].Runbook.Steps["navigate"].Action.Arguments["url"] = literalActionValue([]string{"https://example.com/feed"})
	issues := validateRunbookActionContracts(&candidate, runbookActionContractCatalog())
	if !hasValidationCode(issues, "runbook_action_literal_invalid") {
		t.Fatalf("invalid URL literal issues = %#v", issues)
	}
}

func runbookActionContractCandidate(arguments map[string]runbook.Value) WorkforceCandidate {
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "agent/reviewer",
		Runbook: &runbook.Definition{Steps: map[string]runbook.Step{
			"navigate": {
				Kind: runbook.StepAction,
				Action: &runbook.ActionStep{
					SkillID: "browser", SkillVersion: "1.0.0", Action: "navigate", Arguments: arguments,
				},
			},
		}},
	}}}
}

func runbookActionContractCatalog() CapabilityCatalog {
	return CapabilityCatalog{Skills: map[string]SkillCapability{
		"browser": {
			ID: "browser", Version: "1.0.0",
			ActionContracts: map[string]SkillActionContract{
				"navigate": {InputSchema: map[string]interface{}{
					"type": "object", "additionalProperties": false,
					"properties": map[string]interface{}{
						"sessionId": map[string]interface{}{"type": "string"},
						"url":       map[string]interface{}{"type": "string", "pattern": "^https?://"},
						"targetId":  map[string]interface{}{"type": "string"},
					},
					"required": []interface{}{"sessionId"},
					"oneOf": []interface{}{
						map[string]interface{}{"required": []interface{}{"url"}},
						map[string]interface{}{"required": []interface{}{"targetId"}},
					},
				}},
			},
		},
	}}
}

func literalActionValue(value interface{}) runbook.Value {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return runbook.Value{Literal: encoded}
}
