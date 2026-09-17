//go:build ignore

// Seeds an active speaking member and an observe-only member for isolated tests.
package main

import (
	"context"
	"flag"
	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/team"
)

func main() {
	path := flag.String("db", "", "isolated database")
	flag.Parse()
	if *path == "" {
		panic("-db required")
	}
	store, err := runtime.NewSQLiteStore(*path)
	check(err)
	defer store.Close()
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "local", ID: "default"}
	agents := agent.NewRegistryWithStore(store)
	for _, id := range []string{"speaker", "observer"} {
		_, err := agents.RegisterDefinition(ctx, &agent.AgentDefinition{ID: "ui-participation-" + id, Version: "1", DisplayName: "Channel " + id, Purpose: "Review supplied evidence", SystemPrompt: "Report uncertainty without inventing evidence.", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}})
		check(err)
		_, _, err = agents.CreateDeployment(ctx, &agent.AgentDeployment{ID: "ui-participation-" + id, Scope: scope, DefinitionID: "ui-participation-" + id, ActiveVersion: "1", RolloutStatus: agent.RolloutActive, Environment: "local", Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: 1}}, "user", "fixture", "Synthetic channel participation")
		check(err)
	}
	teams := team.NewRegistryWithStore(store, agents)
	_, err = teams.RegisterDefinition(ctx, &team.Definition{ID: "ui-participation-team", Version: "1", DisplayName: "Channel reply team", Purpose: "Review channel evidence", Roles: []team.RoleSlot{{ID: "speaker", DisplayName: "Speaker", Purpose: "Review evidence", ChannelParticipation: team.RoleChannelActive}, {ID: "observer", DisplayName: "Observer", Purpose: "Observe evidence", ChannelParticipation: team.RoleChannelObserveOnly}}, Approvals: team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}})
	check(err)
	_, _, err = teams.CreateDeployment(ctx, &team.Deployment{ID: "ui-participation-team", Scope: scope, DefinitionID: "ui-participation-team", ActiveVersion: "1", Status: team.DeploymentActive, Roster: []team.RosterAssignment{{ID: "speaker", RoleID: "speaker", AgentDeploymentID: "ui-participation-speaker"}, {ID: "observer", RoleID: "observer", AgentDeploymentID: "ui-participation-observer"}}}, "user", "fixture", "Synthetic channel participation")
	check(err)
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
