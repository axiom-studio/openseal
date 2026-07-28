package authoring

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
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

func TestRunbookActionContractsValidateTypedActionResultDataflow(t *testing.T) {
	catalog := browserRunbookActionContractCatalog()
	candidate := browserRunbookActionContractCandidate()
	if issues := validateRunbookActionContracts(&candidate, catalog); len(issues) != 0 {
		t.Fatalf("valid Browser Runbook dataflow issues = %#v", issues)
	}

	candidate.Agents[0].Runbook.Steps["snapshot"].Action.Arguments["sessionId"] = runbook.Value{Ref: "/results/session/missing"}
	issues := validateRunbookActionContracts(&candidate, catalog)
	if !hasValidationCode(issues, "runbook_action_reference_path_invalid") {
		t.Fatalf("missing output path issues = %#v", issues)
	}

	candidate = browserRunbookActionContractCandidate()
	start := catalog.Skills["browser"].ActionContracts["start"]
	start.OutputSchema["properties"].(map[string]interface{})["sessionId"] = map[string]interface{}{"type": "integer"}
	skill := catalog.Skills["browser"]
	skill.ActionContracts["start"] = start
	catalog.Skills["browser"] = skill
	issues = validateRunbookActionContracts(&candidate, catalog)
	if !hasValidationCode(issues, "runbook_action_reference_type_mismatch") {
		t.Fatalf("mismatched output type issues = %#v", issues)
	}

	catalog = browserRunbookActionContractCatalog()
	candidate = browserRunbookActionContractCandidate()
	startStep := candidate.Agents[0].Runbook.Steps["start"]
	startStep.Action.Next = "done"
	candidate.Agents[0].Runbook.Steps["start"] = startStep
	issues = validateRunbookActionContracts(&candidate, catalog)
	if !hasValidationCode(issues, "runbook_action_reference_unavailable") {
		t.Fatalf("out-of-order action output issues = %#v", issues)
	}

	candidate = browserRunbookActionContractCandidate()
	candidate.Agents[0].Runbook.Entrypoints["daily"] = "fork"
	candidate.Agents[0].Runbook.Steps["fork"] = runbook.Step{Kind: runbook.StepFork, Fork: &runbook.ForkStep{
		Branches: map[string]string{"with-session": "start", "without-session": "bypass"}, Join: "navigate",
	}}
	candidate.Agents[0].Runbook.Steps["bypass"] = runbook.Step{Kind: runbook.StepTransform, Transform: &runbook.TransformStep{
		Assignments: map[string]runbook.Value{"/state/bypass": literalActionValue(true)}, Next: "navigate",
	}}
	issues = validateRunbookActionContracts(&candidate, catalog)
	if !hasValidationCode(issues, "runbook_action_reference_unavailable") {
		t.Fatalf("branch-optional action output issues = %#v", issues)
	}
}

func TestCompilerRejectsInvalidRunbookActionDataflowBeforeApply(t *testing.T) {
	candidate := browserRunbookActionContractCandidate()
	navigate := candidate.Agents[0].Runbook.Steps["navigate"]
	delete(navigate.Action.Arguments, "sessionId")
	candidate.Agents[0].Runbook.Steps["navigate"] = navigate
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Run the reviewed Browser routine daily.", Catalog: browserRunbookActionContractCatalog(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || !hasValidationCode(result.Validation, "runbook_action_argument_required") {
		t.Fatalf("invalid Runbook was not rejected before apply: %#v", result.Validation)
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

func browserRunbookActionContractCatalog() CapabilityCatalog {
	stringProperty := func() map[string]interface{} { return map[string]interface{}{"type": "string"} }
	sessionInput := func(required ...string) map[string]interface{} {
		properties := map[string]interface{}{"sessionId": stringProperty()}
		for _, name := range required {
			if name != "sessionId" {
				properties[name] = stringProperty()
			}
		}
		return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": properties, "required": required}
	}
	return CapabilityCatalog{Skills: map[string]SkillCapability{
		"browser": {
			ID: "browser", Version: "1.0.0", Actions: []string{"start", "navigate", "snapshot", "close"},
			MaximumRisk: capability.RiskLevelRead, Readiness: SkillReadinessReady,
			ActionRisks: map[string]capability.RiskLevel{
				"start": capability.RiskLevelRead, "navigate": capability.RiskLevelRead,
				"snapshot": capability.RiskLevelRead, "close": capability.RiskLevelRead,
			},
			ActionContracts: map[string]SkillActionContract{
				"start": {
					InputSchema: sessionInput("profile"),
					OutputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{
						"sessionId": stringProperty(),
					}, "required": []interface{}{"sessionId"}},
				},
				"navigate": {InputSchema: sessionInput("sessionId", "url"), OutputSchema: map[string]interface{}{"type": "object"}},
				"snapshot": {InputSchema: sessionInput("sessionId"), OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"text": stringProperty()}}},
				"close":    {InputSchema: sessionInput("sessionId"), OutputSchema: map[string]interface{}{"type": "object"}},
			},
		},
	}}
}

func browserRunbookActionContractCandidate() WorkforceCandidate {
	sessionRef := func() runbook.Value { return runbook.Value{Ref: "/results/session/sessionId"} }
	return WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "agent/browser", Version: "1.0.0", DisplayName: "Browser researcher",
		Purpose: "Run a reviewed Browser routine.", SystemPrompt: "Follow the reviewed routine exactly.",
		Authority:         agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, AllowedSkillIDs: []string{"browser"}, MaxConcurrentRuns: 1},
		SkillRequirements: []agent.SkillRequirement{{SkillID: "browser", VersionConstraint: "1.0.0", RequiredActions: []string{"start", "navigate", "snapshot", "close"}}},
		Runbook: &runbook.Definition{
			APIVersion: runbook.APIVersion, ID: "browser-daily", Version: "1.0.0", Name: "Browser daily",
			Entrypoints: map[string]string{"daily": "start"},
			Interfaces: map[string]runbook.Interface{"daily": {
				Description: "Run the daily Browser routine", InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false},
			}},
			Steps: map[string]runbook.Step{
				"start": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
					SkillID: "browser", SkillVersion: "1.0.0", Action: "start",
					Arguments: map[string]runbook.Value{"profile": literalActionValue("rowan")}, ResultPath: "/results/session", Next: "navigate",
				}},
				"navigate": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
					SkillID: "browser", SkillVersion: "1.0.0", Action: "navigate",
					Arguments: map[string]runbook.Value{"sessionId": sessionRef(), "url": literalActionValue("https://example.com")}, ResultPath: "/results/navigation", Next: "snapshot",
				}},
				"snapshot": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
					SkillID: "browser", SkillVersion: "1.0.0", Action: "snapshot",
					Arguments: map[string]runbook.Value{"sessionId": sessionRef()}, ResultPath: "/results/snapshot", Next: "close",
				}},
				"close": {Kind: runbook.StepAction, Action: &runbook.ActionStep{
					SkillID: "browser", SkillVersion: "1.0.0", Action: "close",
					Arguments: map[string]runbook.Value{"sessionId": sessionRef()}, ResultPath: "/results/close", Next: "done",
				}},
				"done": {Kind: runbook.StepEnd, End: &runbook.EndStep{}},
			},
		},
	}}}
}

func literalActionValue(value interface{}) runbook.Value {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return runbook.Value{Literal: encoded}
}
