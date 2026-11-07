package skillcatalog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestValidateCapabilityCatalogAcceptsAValidCatalog(t *testing.T) {
	catalog := CapabilityCatalog{
		Skills: map[string]SkillCapability{
			"slack": {
				ID: "slack", Version: "1.0.0", Actions: []string{"read", "send"},
				ActionRisks: map[string]capability.RiskLevel{"read": capability.RiskLevelRead, "send": capability.RiskLevelExternal},
				MaximumRisk: capability.RiskLevelExternal, Readiness: SkillReadinessNeedsBinding,
				Credentials: []SkillCredential{{Name: "SLACK_TOKEN", Kind: "api_key"}},
				Compatibility: []SkillCompatibility{
					{Requirement: "credential:SLACK_TOKEN", Compatible: false, Evidence: "no binding yet"},
				},
			},
		},
		AuthorityConstraint: &AuthorityConstraint{ID: "policy", Version: "1", MaximumRisk: capability.RiskLevelExternal, RequireApprovalAt: capability.RiskLevelWrite},
		Diagnostics: []CatalogDiagnostic{
			{Code: CatalogDiagnosticDiscoveryTimeout, Message: "The bounded verification deadline elapsed.", Reference: "reddit-research"},
		},
	}
	if err := ValidateCapabilityCatalog(catalog); err != nil {
		t.Fatalf("valid catalog rejected: %v", err)
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

func TestValidateCapabilityCatalogRejectsMalformedSkills(t *testing.T) {
	tests := map[string]SkillCapability{
		"undeclared action risk": {ID: "slack", Actions: []string{"send"}, ActionRisks: map[string]capability.RiskLevel{"delete": capability.RiskLevelDestructive}, MaximumRisk: capability.RiskLevelDestructive},
		"invalid action risk":    {ID: "slack", Actions: []string{"send"}, ActionRisks: map[string]capability.RiskLevel{"send": capability.RiskLevel("sometimes")}, MaximumRisk: capability.RiskLevelDestructive},
		"risk above maximum":     {ID: "slack", Actions: []string{"send"}, ActionRisks: map[string]capability.RiskLevel{"send": capability.RiskLevelExternal}, MaximumRisk: capability.RiskLevelWrite},
		"invalid readiness":      {ID: "slack", Readiness: SkillReadiness("maybe")},
		"negative ceiling":       {ID: "slack", HostedModelInputTokens: -1},
		"credential action":      {ID: "slack", Actions: []string{"send"}, Credentials: []SkillCredential{{Name: "TOKEN", Kind: "api_key", Actions: []string{"read"}}}},
		"compatibility evidence": {ID: "slack", Compatibility: []SkillCompatibility{{Requirement: "installation"}}},
	}
	for name, skill := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCapabilityCatalog(CapabilityCatalog{Skills: map[string]SkillCapability{skill.ID: skill}}); err == nil {
				t.Fatalf("Skill %#v unexpectedly validated", skill)
			}
		})
	}
	if err := ValidateCapabilityCatalog(CapabilityCatalog{Skills: map[string]SkillCapability{"other": {ID: "slack"}}}); err == nil {
		t.Fatal("catalog key that differs from the Skill id was accepted")
	}
}

func TestValidateCapabilityCatalogRejectsInvalidDiagnostics(t *testing.T) {
	tests := map[string][]CatalogDiagnostic{
		"invalid code":      {{Code: "Timeout Now", Message: "Unavailable"}},
		"empty message":     {{Code: CatalogDiagnosticDiscoveryTimeout}},
		"multiline message": {{Code: CatalogDiagnosticDiscoveryTimeout, Message: "provider error\nraw detail"}},
		"oversized message": {{Code: CatalogDiagnosticDiscoveryTimeout, Message: strings.Repeat("x", 1025)}},
		"raw URL reference": {{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Unavailable", Reference: "https://registry.example/private?q=raw"}},
		"padded reference":  {{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Unavailable", Reference: " reddit-research "}},
		"duplicate": {
			{Code: CatalogDiagnosticDiscoveryTimeout, Message: "First", Reference: "reddit-research"},
			{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Second", Reference: "reddit-research"},
		},
	}
	tooMany := make([]CatalogDiagnostic, MaximumCatalogDiagnostics+1)
	for index := range tooMany {
		tooMany[index] = CatalogDiagnostic{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Unavailable", Reference: fmt.Sprintf("intent-%d", index)}
	}
	tests["too many"] = tooMany
	for name, diagnostics := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCapabilityCatalog(CapabilityCatalog{Diagnostics: diagnostics}); err == nil {
				t.Fatal("invalid catalog diagnostics were accepted")
			}
		})
	}
}

func TestValidateCapabilityCatalogRejectsMalformedCredentialGrants(t *testing.T) {
	for name, grants := range map[string]map[string][]capability.OAuth2GrantSummary{
		"blank credential":  {"": nil},
		"padded credential": {" TOKEN ": nil},
		"too many grants":   {"TOKEN": make([]capability.OAuth2GrantSummary, 33)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCapabilityCatalog(CapabilityCatalog{AvailableCredentialGrants: grants}); err == nil {
				t.Fatal("invalid credential grants were accepted")
			}
		})
	}
}
