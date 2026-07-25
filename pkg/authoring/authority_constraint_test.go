package authoring

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestCompilerAppliesHostApprovalConstraintDeterministically(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelDestructive)
	candidate.Agents[0].Authority.RequireApprovalAt = capability.RiskLevelProduction
	payload, err := json.Marshal(GenerationResponse{Candidate: candidate})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(staticGenerator{payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a marketing research Team.",
		Catalog: CapabilityCatalog{
			Skills: marketingCatalog().Skills,
			AuthorityConstraint: &AuthorityConstraint{
				ID: "development-baseline", Version: "2", RequireApprovalAt: capability.RiskLevelWrite,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Candidate.Agents[0].Authority.RequireApprovalAt; got != capability.RiskLevelWrite {
		t.Fatalf("approval threshold = %q, want write", got)
	}
	if len(result.Validation) != 0 {
		t.Fatalf("validation = %#v", result.Validation)
	}
}

func TestCompilerKeepsStricterAgentApprovalConstraint(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelExternal)
	candidate.Agents[0].Authority.RequireApprovalAt = capability.RiskLevelRead
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a marketing research Team.",
		Catalog: CapabilityCatalog{
			Skills: marketingCatalog().Skills,
			AuthorityConstraint: &AuthorityConstraint{
				ID: "safe-default", Version: "1.0.0", RequireApprovalAt: capability.RiskLevelWrite,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Candidate.Agents[0].Authority.RequireApprovalAt; got != capability.RiskLevelRead {
		t.Fatalf("approval threshold = %q, want stricter read threshold", got)
	}
}

func TestCompilerRejectsAuthorityAboveHostMaximum(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelProduction)
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(staticGenerator{payload: payload})
	result, err := compiler.Compile(context.Background(), GenerateRequest{
		Mode: ModeCreate, Prompt: "Create a marketing research Team.",
		Catalog: CapabilityCatalog{
			Skills: marketingCatalog().Skills,
			AuthorityConstraint: &AuthorityConstraint{
				ID: "environment-boundary", Version: "3", MaximumRisk: capability.RiskLevelExternal,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Valid || len(result.Validation) != 1 || result.Validation[0].Code != "authority_maximum_risk_exceeded" {
		t.Fatalf("result = %#v", result)
	}
	if got := result.Candidate.Agents[0].Authority.MaximumRisk; got != capability.RiskLevelProduction {
		t.Fatalf("maximum risk was silently changed to %q", got)
	}
}

func TestCurrentHostAuthorityRevalidationRejectsStaleApprovalThreshold(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelExternal)
	candidate.Agents[0].Authority.RequireApprovalAt = capability.RiskLevelProduction
	issues := ValidateCandidateAuthorityConstraint(&candidate, &AuthorityConstraint{
		ID: "current-placement", Version: "4", MaximumRisk: capability.RiskLevelExternal,
		RequireApprovalAt: capability.RiskLevelWrite,
	})
	if len(issues) != 1 || issues[0].Code != "authority_approval_threshold_exceeded" {
		t.Fatalf("issues = %#v", issues)
	}
}

func TestValidateCapabilityCatalogRejectsMalformedAuthorityConstraint(t *testing.T) {
	tests := []AuthorityConstraint{
		{Version: "1", RequireApprovalAt: capability.RiskLevelWrite},
		{ID: "policy", RequireApprovalAt: capability.RiskLevelWrite},
		{ID: "Policy With Spaces", Version: "1", RequireApprovalAt: capability.RiskLevelWrite},
		{ID: "policy", Version: "1", MaximumRisk: capability.RiskLevel("root")},
		{ID: "policy", Version: "1", RequireApprovalAt: capability.RiskLevel("sometimes")},
		{ID: "policy", Version: "version with spaces", RequireApprovalAt: capability.RiskLevelWrite},
		{ID: "policy", Version: "1", MaximumRisk: capability.RiskLevelWrite, RequireApprovalAt: capability.RiskLevelExternal},
		{ID: "policy", Version: strings.Repeat("v", 129), RequireApprovalAt: capability.RiskLevelWrite},
	}
	for _, constraint := range tests {
		if err := ValidateCapabilityCatalog(CapabilityCatalog{AuthorityConstraint: &constraint}); err == nil {
			t.Fatalf("constraint %#v unexpectedly validated", constraint)
		}
	}
}

func TestValidateCapabilityCatalogRejectsMalformedActionRisks(t *testing.T) {
	tests := []SkillCapability{
		{ID: "slack", Actions: []string{"send"}, ActionRisks: map[string]capability.RiskLevel{"delete": capability.RiskLevelDestructive}, MaximumRisk: capability.RiskLevelDestructive},
		{ID: "slack", Actions: []string{"send"}, ActionRisks: map[string]capability.RiskLevel{"send": capability.RiskLevel("sometimes")}, MaximumRisk: capability.RiskLevelDestructive},
		{ID: "slack", Actions: []string{"send"}, ActionRisks: map[string]capability.RiskLevel{"send": capability.RiskLevelExternal}, MaximumRisk: capability.RiskLevelWrite},
	}
	for _, skill := range tests {
		if err := ValidateCapabilityCatalog(CapabilityCatalog{Skills: map[string]SkillCapability{skill.ID: skill}}); err == nil {
			t.Fatalf("Skill %#v unexpectedly validated", skill)
		}
	}
}

func marketingCatalog() CapabilityCatalog {
	return CapabilityCatalog{Skills: map[string]SkillCapability{
		"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
	}}
}
