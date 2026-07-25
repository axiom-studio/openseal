package team

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestDefinitionAndDeploymentPreserveExtensibleRoles(t *testing.T) {
	definition := validDefinition()
	definition.Roles = append(definition.Roles, RoleSlot{
		ID: "community-cartographer", DisplayName: "Community cartographer",
		Purpose: "Maps emerging communities without being a fixed built-in role", MaximumMembers: 2,
		ChannelParticipation: RoleChannelObserveOnly,
	})
	encoded, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	var restored Definition
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(); err != nil || restored.Roles[1].ID != "community-cartographer" || restored.Roles[1].ChannelParticipation != RoleChannelObserveOnly {
		t.Fatalf("restored definition = %#v, err = %v", restored, err)
	}

	deployment := &Deployment{
		ID: "growth-team", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"},
		DefinitionID: restored.ID, ActiveVersion: restored.Version, Status: DeploymentActive, Revision: 1,
		Roster: []RosterAssignment{
			{ID: "researcher", RoleID: "researcher", AgentDeploymentID: "agent-research"},
			{ID: "cartographer", RoleID: "community-cartographer", AgentDeploymentID: "agent-community"},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := deployment.Validate(&restored); err != nil {
		t.Fatal(err)
	}
	if encoded, err = json.Marshal(deployment); err != nil {
		t.Fatal(err)
	}
	var restoredDeployment Deployment
	if err := json.Unmarshal(encoded, &restoredDeployment); err != nil || restoredDeployment.Roster[1].RoleID != "community-cartographer" {
		t.Fatalf("restored deployment = %#v, err = %v", restoredDeployment, err)
	}
}

func TestRoleSkillGrantExactIdentityRoundTripsAndKeepsNativeCompatibility(t *testing.T) {
	source := capability.NewSkillIdentity("summarize", "1.0.0+source.0123456789ab", "https://clawhub.ai::@alice/summarize")
	definition := validDefinition()
	definition.Roles[0].SkillGrants = []RoleSkillGrant{
		{SkillID: "summarize", SkillVersion: "1.0.0", CatalogID: "clawhub-listing-alice", RuntimeIdentity: &source, AllowedActions: []string{"execute"}, MaximumRisk: capability.RiskLevelRead},
		{SkillID: "native", SkillVersion: "2.0.0", AllowedActions: []string{"read"}, MaximumRisk: capability.RiskLevelRead},
	}
	if err := definition.Validate(); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	var restored Definition
	if err := json.Unmarshal(payload, &restored); err != nil {
		t.Fatal(err)
	}
	if !restored.Roles[0].SkillGrants[0].ExactIdentity().Equal(source) ||
		!restored.Roles[0].SkillGrants[1].ExactIdentity().Equal(capability.NewSkillIdentity("native", "2.0.0", "")) {
		t.Fatalf("restored grants = %#v", restored.Roles[0].SkillGrants)
	}

	foreign := source
	foreign.SourceIdentity = "https://clawhub.ai::@bob/summarize"
	restored.Roles[0].SkillGrants = append(restored.Roles[0].SkillGrants, RoleSkillGrant{
		SkillID: "summarize", SkillVersion: "1.0.0", CatalogID: "clawhub-listing-bob", RuntimeIdentity: &foreign,
		AllowedActions: []string{"execute"}, MaximumRisk: capability.RiskLevelRead,
	})
	if err := restored.Validate(); err != nil {
		t.Fatalf("independent source variant should remain distinct: %v", err)
	}
}

func TestDefinitionAndDeploymentFailClosedOnInvalidAuthorityOrRoster(t *testing.T) {
	definition := validDefinition()
	definition.Approvals.ApproverRoleIDs = []string{"undeclared-leader"}
	if err := definition.Validate(); err == nil {
		t.Fatal("undeclared approval authority should fail")
	}

	definition = validDefinition()
	definition.Roles[0].ChannelParticipation = "interrupt_everyone"
	if err := definition.Validate(); err == nil {
		t.Fatal("unknown role channel participation should fail closed")
	}

	definition = validDefinition()
	definition.Roles[0].SkillGrants = []RoleSkillGrant{{SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"reply"}, MaximumRisk: capability.RiskLevelProduction}}
	if err := definition.Validate(); err == nil {
		t.Fatal("role Skill grant must not widen Team risk authority")
	}
	definition.Roles[0].SkillGrants = []RoleSkillGrant{
		{SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"search"}, MaximumRisk: capability.RiskLevelRead},
		{SkillID: "forum", SkillVersion: "1", AllowedActions: []string{"search"}, MaximumRisk: capability.RiskLevelRead},
	}
	if err := definition.Validate(); err == nil {
		t.Fatal("duplicate exact Team role Skill grants must fail")
	}

	definition = validDefinition()
	deployment := &Deployment{
		ID: "growth-team", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"},
		DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: DeploymentActive, Revision: 1,
		Roster: []RosterAssignment{
			{ID: "one", RoleID: "researcher", AgentDeploymentID: "agent-research"},
			{ID: "two", RoleID: "researcher", AgentDeploymentID: "agent-research"},
		},
	}
	if err := deployment.Validate(definition); err == nil {
		t.Fatal("one Agent deployment must not occupy multiple Team assignments")
	}

	deployment.Roster = nil
	if err := deployment.Validate(definition); err == nil {
		t.Fatal("required role member bounds should fail closed")
	}
}

func validDefinition() *Definition {
	return &Definition{
		ID: "market-research", Version: "1.0.0", DisplayName: "Market research", Purpose: "Discover evidence-backed customer needs",
		Roles:        []RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Collects and synthesizes evidence", MinimumMembers: 1, MaximumMembers: 3}},
		Coordination: CoordinationPolicy{MaximumSpeakersPerRound: 2, QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true},
		Delegation:   DelegationPolicy{MaximumDepth: 3, MaximumConcurrent: 6, AllowPeerDelegation: true, RequireAcceptance: true},
		Approvals:    ApprovalPolicy{MaximumRisk: capability.RiskLevelExternal, ApproverRoleIDs: []string{"researcher"}},
		Digest:       "sha256:definition", CreatedAt: time.Now().UTC(),
	}
}
