package skillcatalog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
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
		CapabilityNeeds: []CapabilityNeed{
			{ID: "chat", Prompt: "Which chat tool?", WhyNeeded: "To post updates.", SkillIDs: []string{"slack"}, Priority: 1},
		},
		Diagnostics: []CatalogDiagnostic{
			{Code: CatalogDiagnosticDiscoveryTimeout, Message: "The bounded verification deadline elapsed.", Reference: "reddit-research"},
		},
		RuntimeComposition: &RuntimeCompositionCapability{
			ProtocolVersion: RuntimeCompositionProtocolV1,
			Conversation: ConversationRuntimeCapability{
				EventTypes:     []string{"conversation.message.received"},
				HandlerKinds:   []ConversationHandlerKind{ConversationHandlerAgent, ConversationHandlerRunbook, ConversationHandlerTeam},
				ReplyModes:     []ConversationReplyMode{ConversationReplyChannel, ConversationReplyProviderDefault, ConversationReplyThread},
				CanonicalReply: true,
			},
			Runbook: RunbookRuntimeCapability{
				TriggerKinds:                   []runbook.TriggerKind{runbook.TriggerEvent, runbook.TriggerSchedule},
				StepKinds:                      []runbook.StepKind{runbook.StepAction, runbook.StepEnd},
				ConversationTriggerInputSchema: CanonicalConversationTriggerInputSchema(),
			},
		},
		HostedExecution: &HostedExecutionCapability{ProtocolVersion: HostedExecutionProtocolV1, BaseInputTokens: 1, MinimumOutputTokens: 1},
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

func TestValidateCapabilityCatalogRejectsUnviableCapabilityNeeds(t *testing.T) {
	skills := map[string]SkillCapability{
		"ready":       {ID: "ready"},
		"unavailable": {ID: "unavailable", Readiness: SkillReadinessUnavailable},
		"broken":      {ID: "broken", Compatibility: []SkillCompatibility{{Requirement: "platform", Compatible: false, Evidence: "linux only"}}},
	}
	need := func(skillIDs ...string) CapabilityNeed {
		return CapabilityNeed{ID: "need", Prompt: "Pick one", WhyNeeded: "Required.", SkillIDs: skillIDs, Priority: 1}
	}
	for name, needs := range map[string][]CapabilityNeed{
		"unknown Skill":       {need("missing")},
		"unavailable Skill":   {need("unavailable")},
		"incompatible Skill":  {need("broken")},
		"no Skill choices":    {need()},
		"duplicate Skill ids": {need("ready", "ready")},
		"duplicate need ids":  {need("ready"), need("ready")},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCapabilityCatalog(CapabilityCatalog{Skills: skills, CapabilityNeeds: needs}); err == nil {
				t.Fatal("invalid capability need was accepted")
			}
		})
	}
	if err := ValidateCapabilityCatalog(CapabilityCatalog{Skills: skills, CapabilityNeeds: []CapabilityNeed{need("ready")}}); err != nil {
		t.Fatalf("viable need rejected: %v", err)
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

func TestValidateCapabilityCatalogRejectsNonCanonicalRuntimeComposition(t *testing.T) {
	valid := func() *RuntimeCompositionCapability {
		return &RuntimeCompositionCapability{
			ProtocolVersion: RuntimeCompositionProtocolV1,
			Conversation: ConversationRuntimeCapability{
				EventTypes: []string{"conversation.message.received"}, HandlerKinds: []ConversationHandlerKind{ConversationHandlerAgent},
				ReplyModes: []ConversationReplyMode{ConversationReplyThread}, CanonicalReply: true,
			},
			Runbook: RunbookRuntimeCapability{
				TriggerKinds: []runbook.TriggerKind{runbook.TriggerEvent}, StepKinds: []runbook.StepKind{runbook.StepEnd},
				ConversationTriggerInputSchema: CanonicalConversationTriggerInputSchema(),
			},
		}
	}
	if err := ValidateCapabilityCatalog(CapabilityCatalog{RuntimeComposition: valid()}); err != nil {
		t.Fatalf("valid composition rejected: %v", err)
	}
	for name, mutate := range map[string]func(*RuntimeCompositionCapability){
		"protocol":      func(value *RuntimeCompositionCapability) { value.ProtocolVersion = "v0" },
		"reply":         func(value *RuntimeCompositionCapability) { value.Conversation.CanonicalReply = false },
		"handler order": func(value *RuntimeCompositionCapability) { value.Conversation.HandlerKinds = []ConversationHandlerKind{ConversationHandlerTeam, ConversationHandlerAgent} },
		"schema":        func(value *RuntimeCompositionCapability) { value.Runbook.ConversationTriggerInputSchema = map[string]interface{}{} },
	} {
		t.Run(name, func(t *testing.T) {
			value := valid()
			mutate(value)
			if err := ValidateCapabilityCatalog(CapabilityCatalog{RuntimeComposition: value}); err == nil {
				t.Fatal("invalid runtime composition was accepted")
			}
		})
	}
	if err := ValidateCapabilityCatalog(CapabilityCatalog{HostedExecution: &HostedExecutionCapability{ProtocolVersion: HostedExecutionProtocolV1}}); err == nil {
		t.Fatal("hosted execution without token envelopes was accepted")
	}
}
