//go:build ignore

// Seeds a real team catalog entry into an isolated UI-test workspace.
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
	db := flag.String("db", "", "isolated test database")
	flag.Parse()
	if *db == "" {
		panic("-db required")
	}
	store, err := runtime.NewSQLiteStore(*db)
	check(err)
	defer store.Close()
	registry := team.NewRegistryWithStore(store, agent.NewRegistryWithStore(store))
	ctx := context.Background()
	_, err = registry.RegisterDefinition(ctx, &team.Definition{
		ID: "ui-team", Version: "1", DisplayName: "Synthetic review team", Purpose: "Inspect a durable team catalog entry.",
		Roles:               []team.RoleSlot{{ID: "reviewer", DisplayName: "Reviewer", Purpose: "Check the evidence."}},
		Approvals:           team.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
		OperatingPrinciples: []string{"Report uncertainty clearly."},
	})
	check(err)
	for _, scope := range []capability.ScopeReference{{Kind: "local", ID: "default"}, {Kind: "local", ID: "other"}} {
		_, _, err = registry.CreateDeployment(ctx, &team.Deployment{
			ID: "ui-team-" + scope.ID, Scope: scope, DefinitionID: "ui-team", ActiveVersion: "1", Status: team.DeploymentDraft,
			Roster: []team.RosterAssignment{},
		}, "user", "test-fixture", "Synthetic UI verification")
		check(err)
	}
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
