package authoring

import (
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestValidateCredentialPlacementUsesRequiredKindsAndAuthorizedOpaqueChoices(t *testing.T) {
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "sre"}, {ID: "observer"}}}
	required := map[string][]string{"sre": {"kubernetes-cluster"}}
	choices := []capability.CredentialBindingChoice{
		{Reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, DisplayName: "Development"},
		{Reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://8"}, DisplayName: "Production"},
	}
	valid := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"sre": {"kubernetes-cluster": {Kind: "kubernetes-cluster", ID: "cluster://7"}},
	}}
	if err := ValidateCredentialPlacement(candidate, required, valid, choices); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		agentID   string
		key       string
		reference capability.CredentialReference
		message   string
	}{
		{name: "requirement name is not a kind", agentID: "sre", key: "clusterId", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, message: "must match credential kind"},
		{name: "unrequired kind", agentID: "observer", key: "kubernetes-cluster", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, message: "does not require"},
		{name: "unknown agent", agentID: "other", key: "kubernetes-cluster", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"}, message: "not part of this workforce"},
		{name: "stale reference", agentID: "sre", key: "kubernetes-cluster", reference: capability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://9"}, message: "no longer authorized"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
				test.agentID: {test.key: test.reference},
			}}
			if err := ValidateCredentialPlacement(candidate, required, placement, choices); err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("error = %v, want %q", err, test.message)
			}
		})
	}
}

func TestValidateCredentialPlacementRejectsUnprovenChoiceWithoutLeakingReference(t *testing.T) {
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"sre": {"kubernetes-cluster": {Kind: "kubernetes-cluster", ID: "cluster://secret-internal-id"}},
	}}
	err := ValidateCredentialPlacement(nil, nil, placement, nil)
	if err == nil || strings.Contains(err.Error(), "secret-internal-id") {
		t.Fatalf("secret-safe unavailable error = %v", err)
	}
}

func TestValidateCredentialPlacementAcceptsAuthorizedDeploymentBinding(t *testing.T) {
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "operator"}}}
	required := map[string][]string{"operator": {agent.ModelProviderCredentialBinding}}
	choices := []capability.CredentialBindingChoice{{
		Reference:   capability.CredentialReference{Kind: "managed-secret", ID: "credential-29"},
		DisplayName: "Primary model",
		BindingKeys: []string{agent.ModelProviderCredentialBinding},
	}}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"operator": {agent.ModelProviderCredentialBinding: {Kind: "managed-secret", ID: "credential-29"}},
	}}
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err != nil {
		t.Fatal(err)
	}
	placement.CredentialReferences["operator"][agent.ModelProviderCredentialBinding] =
		capability.CredentialReference{Kind: "managed-secret", ID: "credential-other"}
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err == nil ||
		!strings.Contains(err.Error(), "unavailable or no longer authorized") {
		t.Fatalf("unadvertised deployment binding error = %v", err)
	}
}

func TestValidateCredentialPlacementRequiresOAuth2GrantCoverage(t *testing.T) {
	requirement := CredentialBindingRequirement{
		Key: "SLACK_CONNECTION", Kind: "slack-oauth",
		OAuth2: &capability.OAuth2Requirement{
			Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
			Scopes: []string{"channels:history", "chat:write"},
		},
	}
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "slack-agent"}}}
	required := map[string][]CredentialBindingRequirement{"slack-agent": {requirement}}
	reference := capability.CredentialReference{Kind: "slack-oauth", ID: "connection://slack/7"}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"slack-agent": {"SLACK_CONNECTION": reference},
	}}
	choice := capability.CredentialBindingChoice{
		Reference: reference, DisplayName: "Axiom workspace", BindingKeys: []string{"SLACK_CONNECTION"},
		OAuth2: &capability.OAuth2GrantSummary{
			Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
			Scopes: []string{"app_mentions:read", "channels:history", "chat:write"},
		},
	}
	if err := ValidateCredentialPlacementWithRequirements(candidate, required, placement, []capability.CredentialBindingChoice{choice}); err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(*capability.OAuth2GrantSummary){
		"wrong provider": func(grant *capability.OAuth2GrantSummary) { grant.Provider = "microsoft" },
		"wrong subject":  func(grant *capability.OAuth2GrantSummary) { grant.Subject = capability.OAuth2SubjectUser },
		"missing scope":  func(grant *capability.OAuth2GrantSummary) { grant.Scopes = []string{"chat:write"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidateChoice := choice
			grant := *choice.OAuth2
			grant.Scopes = append([]string(nil), choice.OAuth2.Scopes...)
			mutate(&grant)
			candidateChoice.OAuth2 = &grant
			if err := ValidateCredentialPlacementWithRequirements(candidate, required, placement, []capability.CredentialBindingChoice{candidateChoice}); err == nil ||
				!strings.Contains(err.Error(), "does not grant") {
				t.Fatalf("coverage error = %v", err)
			}
		})
	}
}

func TestRequiredCredentialBindingsDeriveActionScopedOAuth2Scopes(t *testing.T) {
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{"slack": {
		ID: "slack", Actions: []string{"read", "reply"},
		Credentials: []SkillCredential{
			{Name: "SLACK_CONNECTION", Kind: "slack-oauth", Actions: []string{"read"}, OAuth2: &capability.OAuth2Requirement{
				Provider: "slack", Subject: capability.OAuth2SubjectInstallation, Scopes: []string{"channels:history"},
			}},
			{Name: "SLACK_CONNECTION", Kind: "slack-oauth", Actions: []string{"reply"}, OAuth2: &capability.OAuth2Requirement{
				Provider: "slack", Subject: capability.OAuth2SubjectInstallation, Scopes: []string{"chat:write"},
			}},
		},
	}}}
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "slack-agent", SkillRequirements: []agent.SkillRequirement{{
			SkillID: "slack", RequiredActions: []string{"read", "reply"},
		}},
	}}}
	required := RequiredCredentialBindings(candidate, catalog)["slack-agent"]
	if len(required) != 1 || strings.Join(required[0].OAuth2.Scopes, ",") != "channels:history,chat:write" {
		t.Fatalf("action-scoped OAuth 2 requirement = %#v", required)
	}
}

func TestMissingCredentialRequirementPreservesOAuth2SetupContract(t *testing.T) {
	oauth2 := &capability.OAuth2Requirement{
		Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
		Scopes: []string{"channels:history", "chat:write"},
	}
	candidate := WorkforceCandidate{
		Activation: WorkforceActivationActive,
		Agents: []*agent.AgentDefinition{{
			ID: "slack-agent", SkillRequirements: []agent.SkillRequirement{{
				SkillID: "slack", RequiredActions: []string{"reply"},
			}},
		}},
	}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{"slack": {
		ID: "slack", Actions: []string{"reply"}, Readiness: SkillReadinessReady,
		Credentials: []SkillCredential{{
			Name: "SLACK_CONNECTION", Kind: "slack-oauth", Actions: []string{"reply"}, OAuth2: oauth2,
		}},
	}}}
	missing := missingRequirements(&candidate, catalog)
	if len(missing) != 1 || missing[0].Kind != "credential" || missing[0].ID != "SLACK_CONNECTION" ||
		missing[0].OAuth2 == nil || strings.Join(missing[0].OAuth2.Scopes, ",") != "channels:history,chat:write" {
		t.Fatalf("missing OAuth 2 setup requirement = %#v", missing)
	}

	catalog.AvailableCredentials = map[string]bool{"SLACK_CONNECTION": true}
	if missing = missingRequirements(&candidate, catalog); len(missing) != 1 {
		t.Fatalf("legacy configured boolean incorrectly satisfied OAuth 2 requirement: %#v", missing)
	}
	catalog.AvailableCredentialGrants = map[string][]capability.OAuth2GrantSummary{
		"SLACK_CONNECTION": {{
			Provider: "slack", Subject: capability.OAuth2SubjectInstallation,
			Scopes: []string{"app_mentions:read", "channels:history", "chat:write"},
		}},
	}
	if missing = missingRequirements(&candidate, catalog); len(missing) != 0 {
		t.Fatalf("authorized OAuth 2 scope superset did not satisfy setup: %#v", missing)
	}
}

func TestValidateCredentialPlacementKeepsOptionalDeploymentBindingSeparateFromSkillCredential(t *testing.T) {
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "slack-agent"}}}
	required := map[string][]string{"slack-agent": {"slack_bot_token"}}
	choices := []capability.CredentialBindingChoice{
		{
			Reference:   capability.CredentialReference{Kind: "managed-secret", ID: "36"},
			DisplayName: "Primary model",
			BindingKeys: []string{agent.ModelProviderCredentialBinding},
		},
		{
			Reference:   capability.CredentialReference{Kind: "slack_bot_token", ID: "credential://007.token"},
			DisplayName: "Slack workspace",
		},
	}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"slack-agent": {
			agent.ModelProviderCredentialBinding: {Kind: "managed-secret", ID: "36"},
			"slack_bot_token":                    {Kind: "slack_bot_token", ID: "credential://007.token"},
		},
	}}
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err != nil {
		t.Fatalf("separate deployment and Skill credentials = %v", err)
	}

	placement.CredentialReferences["slack-agent"]["unrequired_skill_token"] =
		capability.CredentialReference{Kind: "unrequired_skill_token", ID: "credential"}
	choices = append(choices, capability.CredentialBindingChoice{
		Reference: capability.CredentialReference{Kind: "unrequired_skill_token", ID: "credential"},
	})
	if err := ValidateCredentialPlacement(candidate, required, placement, choices); err == nil ||
		!strings.Contains(err.Error(), "not a required kind") {
		t.Fatalf("unrequired Skill credential error = %v", err)
	}
}

func TestStructuredSkillCredentialsUseExactNamedBindingSlots(t *testing.T) {
	skill := SkillCapability{
		ID: "security-scorecard",
		Credentials: []SkillCredential{
			{Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"score"}},
			{Name: "REPORTING_API_KEY", Kind: "environment-secret", Actions: []string{"publish"}},
			{Name: "OPTIONAL_TOKEN", Kind: "environment-secret", Optional: true},
		},
	}
	bindings := requiredSkillCredentialBindings(skill, []string{"score"})
	if len(bindings) != 1 || bindings[0].Key != "TOOLWEB_API_KEY" || bindings[0].Kind != "environment-secret" {
		t.Fatalf("score bindings = %#v", bindings)
	}
	bindings = requiredSkillCredentialBindings(skill, []string{"score", "publish"})
	if len(bindings) != 2 ||
		bindings[0].Key != "REPORTING_API_KEY" ||
		bindings[1].Key != "TOOLWEB_API_KEY" {
		t.Fatalf("independent same-kind bindings = %#v", bindings)
	}
}

func TestStructuredSkillCredentialCannotBeSatisfiedByUnrelatedSameKindSecret(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "security-reviewer",
		SkillRequirements: []agent.SkillRequirement{{
			SkillID: "security-scorecard", RequiredActions: []string{"score"},
		}},
	}}}
	catalog := CapabilityCatalog{
		Skills: map[string]SkillCapability{
			"security-scorecard": {
				ID:      "security-scorecard",
				Actions: []string{"score"},
				Credentials: []SkillCredential{{
					Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"score"},
				}},
			},
		},
		AvailableCredentials: map[string]bool{"environment-secret": true, "OTHER_TOKEN": true},
	}
	missing := missingRequirements(&candidate, catalog)
	if len(missing) != 1 || missing[0].Kind != "credential" || missing[0].ID != "TOOLWEB_API_KEY" {
		t.Fatalf("unrelated same-kind secret missing requirements = %#v", missing)
	}
	catalog.AvailableCredentials["TOOLWEB_API_KEY"] = true
	if missing = missingRequirements(&candidate, catalog); len(missing) != 0 {
		t.Fatalf("exact named credential missing requirements = %#v", missing)
	}

	required := requiredCredentials(candidate, catalog)
	if len(required["security-reviewer"]) != 1 || required["security-reviewer"][0] != "TOOLWEB_API_KEY" {
		t.Fatalf("exact required credential slots = %#v", required)
	}
}

func TestValidateCredentialPlacementRequiresExactSkillBindingChoice(t *testing.T) {
	candidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "security-reviewer"}}}
	required := map[string][]string{"security-reviewer": {"TOOLWEB_API_KEY"}}
	unrelatedChoice := []capability.CredentialBindingChoice{{
		Reference:   capability.CredentialReference{Kind: "environment-secret", ID: "credential://other"},
		DisplayName: "Other integration",
		BindingKeys: []string{"OTHER_TOKEN"},
	}}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"security-reviewer": {
			"TOOLWEB_API_KEY": {Kind: "environment-secret", ID: "credential://other"},
		},
	}}
	if err := ValidateCredentialPlacement(candidate, required, placement, unrelatedChoice); err == nil ||
		!strings.Contains(err.Error(), "must match credential kind") {
		t.Fatalf("unrelated exact binding choice error = %v", err)
	}

	exactChoice := []capability.CredentialBindingChoice{{
		Reference:   capability.CredentialReference{Kind: "environment-secret", ID: "credential://toolweb"},
		DisplayName: "ToolWeb",
		BindingKeys: []string{"TOOLWEB_API_KEY"},
	}}
	placement.CredentialReferences["security-reviewer"]["TOOLWEB_API_KEY"] =
		capability.CredentialReference{Kind: "environment-secret", ID: "credential://toolweb"}
	if err := ValidateCredentialPlacement(candidate, required, placement, exactChoice); err != nil {
		t.Fatalf("exact Skill binding choice = %v", err)
	}

	otherCandidate := &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "observer"}}}
	if err := ValidateCredentialPlacement(otherCandidate, map[string][]string{"observer": nil}, ChangeSetPlacement{
		CredentialReferences: map[string]map[string]capability.CredentialReference{
			"observer": {
				"TOOLWEB_API_KEY": {Kind: "environment-secret", ID: "credential://toolweb"},
			},
		},
	}, exactChoice); err == nil || !strings.Contains(err.Error(), "does not require") {
		t.Fatalf("unrequired exact Skill binding error = %v", err)
	}
}

func TestRequiredCredentialsIncludesRuntimeAndSkillBindingsOnlyForActiveCandidates(t *testing.T) {
	catalog := CapabilityCatalog{
		AgentCredentialRequirements: []AgentCredentialRequirement{{
			BindingKey: agent.ModelProviderCredentialBinding, DisplayName: "Model provider",
			Prompt: "Choose the model provider this Agent should use.", RequiredForActivation: true,
		}},
		Skills: map[string]SkillCapability{"posture": {
			ID: "posture", Actions: []string{"execute"},
			Credentials: []SkillCredential{{
				Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"execute"},
			}},
		}},
	}
	active := WorkforceCandidate{
		Agents: []*agent.AgentDefinition{{
			ID: "operator", SkillRequirements: []agent.SkillRequirement{{
				SkillID: "posture", RequiredActions: []string{"execute"},
			}},
		}},
		Activation: WorkforceActivationActive,
	}
	if values := requiredCredentials(active, catalog)["operator"]; len(values) != 2 ||
		values[0] != agent.ModelProviderCredentialBinding || values[1] != "TOOLWEB_API_KEY" {
		t.Fatalf("active required credentials = %#v", values)
	}
	active.Activation = WorkforceActivationInactive
	if values := requiredCredentials(active, catalog)["operator"]; len(values) != 0 {
		t.Fatalf("inactive required credentials = %#v", values)
	}
}

func TestActiveChangeSetCannotApplyWithoutRuntimeCredentialPlacement(t *testing.T) {
	changeSet := &ChangeSet{
		Result: CompileResult{Valid: true, Candidate: WorkforceCandidate{
			Agents:     []*agent.AgentDefinition{{ID: "operator"}},
			Activation: WorkforceActivationActive,
		}},
		Catalog: CapabilityCatalog{
			AgentCredentialRequirements: []AgentCredentialRequirement{{
				BindingKey: agent.ModelProviderCredentialBinding, RequiredForActivation: true,
			}},
		},
		Placement: ChangeSetPlacement{
			Environment:        "production",
			AgentDeploymentIDs: map[string]string{"operator": "operator-live"},
		},
	}
	if err := validateApplyPlacement(changeSet); err == nil ||
		!strings.Contains(err.Error(), "requires an opaque MODEL_PROVIDER credential reference") {
		t.Fatalf("missing active runtime credential error = %v", err)
	}
	changeSet.Placement.CredentialReferences = map[string]map[string]capability.CredentialReference{
		"operator": {
			agent.ModelProviderCredentialBinding: {Kind: "managed-secret", ID: "credential-29"},
		},
	}
	if err := validateApplyPlacement(changeSet); err != nil {
		t.Fatalf("placed active runtime credential = %v", err)
	}
}

func TestInactiveChangeSetApplyIgnoresStaleExecutionCredentialProjection(t *testing.T) {
	changeSet := &ChangeSet{
		Result: CompileResult{Valid: true, Candidate: WorkforceCandidate{
			Agents: []*agent.AgentDefinition{{
				ID: "operator",
				SkillRequirements: []agent.SkillRequirement{{
					SkillID: "posture", RequiredActions: []string{"execute"},
				}},
			}},
			Activation: WorkforceActivationInactive,
		}},
		Catalog: CapabilityCatalog{
			AgentCredentialRequirements: []AgentCredentialRequirement{{
				BindingKey: agent.ModelProviderCredentialBinding, RequiredForActivation: true,
			}},
			Skills: map[string]SkillCapability{"posture": {
				ID: "posture", Actions: []string{"execute"},
				Credentials: []SkillCredential{{
					Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"execute"},
				}},
			}},
		},
		// Older persisted ChangeSets may retain the credential projection they
		// received before explicit inactive intent deferred execution secrets.
		// Atomic apply derives current requirements from the reviewed candidate
		// and catalog instead of allowing that stale projection to override the
		// digest-bound lifecycle commitment.
		RequiredCredentials: map[string][]string{
			"operator": {agent.ModelProviderCredentialBinding, "TOOLWEB_API_KEY"},
		},
		Placement: ChangeSetPlacement{
			Environment:        "production",
			AgentDeploymentIDs: map[string]string{"operator": "operator-inactive"},
		},
	}
	if err := validateApplyPlacement(changeSet); err != nil {
		t.Fatalf("inactive apply with stale execution credentials = %v", err)
	}

	changeSet.Result.Candidate.Activation = WorkforceActivationActive
	if err := validateApplyPlacement(changeSet); err == nil ||
		!strings.Contains(err.Error(), "requires an opaque MODEL_PROVIDER credential reference") {
		t.Fatalf("active candidate bypassed current credential readiness: %v", err)
	}
}
