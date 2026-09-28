package authoring

import (
	"context"
	"testing"
)

type creationLatencyGenerator struct {
	intent  AuthoringIntent
	repairs int
}

func (g *creationLatencyGenerator) GenerateIntent(context.Context, GenerateRequest) (AuthoringIntent, error) {
	return g.intent, nil
}

func (g *creationLatencyGenerator) RepairIntent(_ context.Context, _ GenerateRequest, intent AuthoringIntent, _ error) (AuthoringIntent, error) {
	g.repairs++
	return intent, nil
}

func TestSemanticCreationDoesNotRepairCompilerOwnedCommitments(t *testing.T) {
	for _, prompt := range []string{"Create exactly one agent", "Create exactly one inactive agent", "Create exactly one agent without a team"} {
		t.Run(prompt, func(t *testing.T) {
			g := &creationLatencyGenerator{intent: AuthoringIntent{SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent, Name: "Analyst", Purpose: "Analyze", Agents: []AuthoringAgentIntent{{Key: "analyst", Name: "Analyst", Purpose: "Analyze", Behavior: "Analyze supplied evidence."}}}}
			compiler, err := NewCompiler(g)
			if err != nil {
				t.Fatal(err)
			}
			result, err := compiler.Compile(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: prompt})
			if err != nil {
				t.Fatal(err)
			}
			if g.repairs != 0 || !result.Valid {
				t.Fatalf("repairs=%d result=%+v", g.repairs, result)
			}
			if result.Commitments.AgentCount == nil || *result.Commitments.AgentCount != 1 {
				t.Fatal("explicit count was lost")
			}
		})
	}
}

func TestSemanticCreationStillRejectsWrongAgentCount(t *testing.T) {
	g := &creationLatencyGenerator{intent: AuthoringIntent{SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent, Name: "Analyst", Purpose: "Analyze", Agents: []AuthoringAgentIntent{{Key: "analyst", Name: "Analyst", Purpose: "Analyze", Behavior: "Analyze supplied evidence."}}}}
	compiler, _ := NewCompiler(g)
	result, err := compiler.Compile(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: "Create exactly two agents"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || g.repairs == 0 || len(result.Validation) == 0 {
		t.Fatalf("wrong count accepted: %+v repairs=%d", result, g.repairs)
	}
}

func TestReadinessGapsDoNotConsumeModelRepair(t *testing.T) {
	for _, readiness := range []SkillReadiness{SkillReadinessNeedsBinding, SkillReadinessNeedsInstallation} {
		t.Run(string(readiness), func(t *testing.T) {
			g := &creationLatencyGenerator{intent: AuthoringIntent{SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent, Name: "Analyst", Purpose: "Analyze", Agents: []AuthoringAgentIntent{{Key: "analyst", Name: "Analyst", Purpose: "Analyze", Behavior: "Analyze supplied evidence.", Skills: []AuthoringSkillIntent{{CatalogID: "example", Required: true}}}}}}
			compiler, _ := NewCompiler(g)
			result, err := compiler.Compile(t.Context(), GenerateRequest{Mode: ModeCreate, Prompt: "Create exactly one agent", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"example": {ID: "example", Version: "1.0.0", Readiness: readiness}}}})
			if err != nil {
				t.Fatal(err)
			}
			if g.repairs != 0 || result.Valid || len(result.MissingRequirements) == 0 {
				t.Fatalf("repairs=%d result=%+v", g.repairs, result)
			}
		})
	}
}

func TestRepairFilterRetainsActualCapabilityErrors(t *testing.T) {
	missing := []MissingRequirement{{Kind: "credential", ID: "account"}, {Kind: "skill_binding", ID: "example"}, {Kind: "skill_installation", ID: "example"}, {Kind: "skill", ID: "unknown"}, {Kind: "action", ID: "unknown/action"}}
	got := providerRepairableMissingRequirements(nil, missing, GenerateRequest{})
	if len(got) != 2 || got[0].Kind != "skill" || got[1].Kind != "action" {
		t.Fatalf("repairable gaps=%+v", got)
	}
}
