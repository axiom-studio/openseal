package authoring

import (
	"regexp"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestIndependentCreationsDoNotReuseRoleIdentity(t *testing.T) {
	intent := AuthoringIntent{SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent, Name: "Writer", Purpose: "Help write", Agents: []AuthoringAgentIntent{{Key: "writer", Name: "Writer", Purpose: "Help write", Behavior: "Write clearly."}}}
	compiler, _ := NewCompiler(semanticIntentGenerator{intent: intent})
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	req := CreateChangeSetRequest{Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a writing partner", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "first-writer"}
	first, _, err := service.Prepare(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	replay, replayed, err := service.Prepare(t.Context(), req)
	if err != nil || !replayed || replay.ID != first.ID {
		t.Fatal("prepare replay changed identity", err)
	}
	first, err = service.GeneratePrepared(t.Context(), first.Scope, first.ID, first.Revision)
	if err != nil {
		t.Fatal(err)
	}
	req.IdempotencyKey = "second-writer"
	second, _, err := service.Create(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	firstID, secondID := first.Result.Candidate.Agents[0].ID, second.Result.Candidate.Agents[0].ID
	for _, id := range []string{firstID, secondID, first.Placement.AgentDeploymentIDs[firstID], second.Placement.AgentDeploymentIDs[secondID]} {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{21}$`).MatchString(id) {
			t.Fatal("expected NanoID", id)
		}
	}
	if firstID == secondID {
		t.Fatal("separate creations reused role identity")
	}
	if first.Placement.AgentDeploymentIDs[firstID] == second.Placement.AgentDeploymentIDs[secondID] {
		t.Fatal("separate creations reused deployment identity")
	}
	req.ParentID = first.ID
	req.IdempotencyKey = "amend-writer"
	child, _, err := service.Prepare(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	child, err = service.GeneratePrepared(t.Context(), child.Scope, child.ID, child.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if child.Result.Candidate.Agents[0].ID != firstID || child.Placement.AgentDeploymentIDs[firstID] != first.Placement.AgentDeploymentIDs[firstID] {
		t.Fatal("amendment changed stable identity")
	}
}

func TestCreationIdentityKeepsPortableKeysAndLegacyScope(t *testing.T) {

	longKey := strings.Repeat("a", 64)
	definitions := []*agent.AgentDefinition{
		{ID: "opaque-identity", AuthoringKey: "writer"},
		{ID: longKey + "-first"},
		{ID: longKey + "-second"},
		{ID: "tenant/one/legacy-writer"},
	}
	keys := semanticAgentKeys(definitions)
	if keys[definitions[0].ID] != "writer" {
		t.Fatal("server namespace leaked into model role key", keys)
	}
	if keys[definitions[3].ID] != authoringPortableKey(definitions[3].ID) {
		t.Fatal("legacy role key changed", keys)
	}
	seen := map[string]bool{}
	for _, key := range keys {
		if !authoringIntentKeyPattern.MatchString(key) || seen[key] {
			t.Fatal("invalid or duplicated semantic key", key)
		}
		seen[key] = true
	}
}
