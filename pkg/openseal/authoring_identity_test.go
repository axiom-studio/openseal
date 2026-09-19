package openseal

import (
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestAgentNanoIDAndFormKeySurviveDraftRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identities.db")
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(WithPersistentStore(store), WithWorkforceAuthoringGenerator(sourceQualifiedWorkforceFixtureGenerator{}))
	if err != nil {
		t.Fatal(err)
	}
	request := CreateWorkforceChangeSetRequest{
		Scope: SkillScope{Kind: "tenant", ID: "one"}, Prompt: "Research", Actor: WorkforceChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "research",
		Catalog: WorkforceCapabilityCatalog{Skills: map[string]WorkforceSkillCapability{"research": {ID: "research", Version: "1.0.0", PromptAvailable: true}}},
	}
	created, _, err := engine.CreateWorkforceChangeSet(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	id := created.Result.Candidate.Agents[0].ID
	deploymentID := created.Placement.AgentDeploymentIDs[id]
	if len(id) != 21 || len(deploymentID) != 21 {
		t.Fatal("expected NanoIDs")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	engine, err = New(WithPersistentStore(store), WithWorkforceAuthoringGenerator(sourceQualifiedWorkforceFixtureGenerator{}))
	if err != nil {
		t.Fatal(err)
	}
	replay, replayed, err := engine.CreateWorkforceChangeSet(t.Context(), request)
	if err != nil || !replayed || replay.Result.Candidate.Agents[0].ID != id || replay.Placement.AgentDeploymentIDs[id] != deploymentID || replay.Result.Candidate.Agents[0].AuthoringKey != "researcher" {
		t.Fatal("identity changed on restart", err)
	}
	renamed, err := engine.RenameWorkforceDraftAgent(t.Context(), replay.Scope, replay.ID, replay.Revision, "Paper Trail", replay.Actor)
	if err != nil || renamed.Result.Candidate.Agents[0].ID != id || renamed.Placement.AgentDeploymentIDs[id] != deploymentID {
		t.Fatal("rename changed identity", err)
	}
}
