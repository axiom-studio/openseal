//go:build ignore

// Creates an eligible paused substitute in an isolated UI-test workspace.
package main

import (
	"context"
	"flag"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func main() {
	db := flag.String("db", "", "isolated test database")
	source := flag.String("source", "", "source deployment")
	flag.Parse()
	if *db == "" || *source == "" {
		panic("-db and -source required")
	}
	store, err := runtime.NewSQLiteStore(*db)
	check(err)
	defer store.Close()
	registry := agent.NewRegistryWithStore(store)
	ctx := context.Background()
	deployment, err := registry.GetDeployment(ctx, capability.ScopeReference{Kind: "local", ID: "default"}, *source)
	check(err)
	deployment.ID = "ui-roster-substitute"
	deployment.DisplayName = "Research substitute"
	deployment.RolloutStatus = agent.RolloutPaused
	deployment.Activation = nil
	deployment.Revision = 0
	deployment.CreatedAt = time.Time{}
	_, _, err = registry.CreateDeployment(ctx, deployment, "user", "test-fixture", "Synthetic roster verification")
	check(err)
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}
