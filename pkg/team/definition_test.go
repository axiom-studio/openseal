package team

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestDeploymentRosterCarriesEachMembersProfile(t *testing.T) {
	definition := validDefinition()
	deployment := &Deployment{
		ID: "growth-team", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"},
		DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: DeploymentActive, Revision: 1,
		Roster: []RosterAssignment{
			{ID: "researcher", AgentDeploymentID: "agent-research", DisplayName: "Researcher", Purpose: "Collects evidence", Approver: true, Lead: true},
			{ID: "cartographer", AgentDeploymentID: "agent-community", DisplayName: "Cartographer", Purpose: "Maps communities", ChannelParticipation: ChannelObserveOnly},
		},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := deployment.Validate(definition); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		Definition *Definition `json:"definition"`
		Deployment *Deployment `json:"deployment"`
	}{definition, deployment})
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{`"roles"`, `"roleId"`, `"approverRoleIds"`, `"leadRoleId"`, `"minimumMembers"`, `"requiredDefinitionIds"`} {
		if strings.Contains(string(encoded), retired) {
			t.Fatalf("team JSON still carries %s: %s", retired, encoded)
		}
	}
	var restored Deployment
	if err := json.Unmarshal(encoded[strings.Index(string(encoded), `"deployment":`)+len(`"deployment":`):len(encoded)-1], &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Roster[0].Purpose != "Collects evidence" || !restored.Roster[0].Approver || !restored.Roster[0].Lead ||
		restored.Roster[1].ChannelParticipation != ChannelObserveOnly {
		t.Fatalf("restored roster = %#v", restored.Roster)
	}
}

func TestDeploymentFailsClosedOnInvalidRoster(t *testing.T) {
	definition := validDefinition()
	deployment := &Deployment{
		ID: "growth-team", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"},
		DefinitionID: definition.ID, ActiveVersion: definition.Version, Status: DeploymentActive, Revision: 1,
		Roster: []RosterAssignment{
			{ID: "one", AgentDeploymentID: "agent-research"},
			{ID: "two", AgentDeploymentID: "agent-research"},
		},
	}
	if err := deployment.Validate(definition); err == nil {
		t.Fatal("one Agent deployment must not occupy multiple Team assignments")
	}
	deployment.Roster = []RosterAssignment{{ID: "one", AgentDeploymentID: "agent-research", ChannelParticipation: "interrupt_everyone"}}
	if err := deployment.Validate(definition); err == nil {
		t.Fatal("unknown channel participation should fail closed")
	}
	deployment.Roster = nil
	if err := deployment.Validate(definition); !errors.Is(err, ErrRosterMinimum) {
		t.Fatalf("a team without members: %v", err)
	}
	deployment.Status = DeploymentArchived
	if err := deployment.Validate(definition); err != nil {
		t.Fatalf("an archived team keeps its roster as it was: %v", err)
	}
}

func validDefinition() *Definition {
	return &Definition{
		ID: "market-research", Version: "1.0.0", DisplayName: "Market research", Purpose: "Discover evidence-backed customer needs",
		Routing:    RoutingPolicy{Notes: "Researchers own customer interviews"},
		Delegation: DelegationPolicy{MaximumDepth: 3, MaximumConcurrent: 6, AllowPeerDelegation: true, RequireAcceptance: true},
		Approvals:  ApprovalPolicy{MaximumRisk: capability.RiskLevelExternal},
		Digest:     "sha256:definition", CreatedAt: time.Now().UTC(),
	}
}
