package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"
	"time"

	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func newBuilderSeedEngine(t *testing.T) *opensealkernel.Engine {
	t.Helper()
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	engine, err := opensealkernel.New(
		opensealkernel.WithPersistentStore(store),
		opensealkernel.WithSkillManagementActions(),
		opensealkernel.WithAgentManagementActions(),
		opensealkernel.WithTeamManagementActions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func builderBindingsByID(t *testing.T, engine *opensealkernel.Engine) map[string]*skill.Binding {
	t.Helper()
	bindings, err := engine.ListSkillBindings(t.Context(), skill.ScopeReference{Kind: localScope.Kind, ID: localScope.ID}, runtime.AgentBuilderDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*skill.Binding{}
	for _, binding := range bindings {
		byID[binding.ID] = binding
	}
	return byID
}

func assertBuilderBindings(t *testing.T, engine *opensealkernel.Engine) map[string]*skill.Binding {
	t.Helper()
	byID := builderBindingsByID(t, engine)
	want := map[string]struct {
		version string
		actions []string
	}{
		"bundled:agents":       {runtime.AgentManagementSkillVersion, []string{"create_agent", "list_agents", "try_agent", "update_agent"}},
		"bundled:teams":        {runtime.TeamManagementSkillVersion, []string{"create_team"}},
		"bundled:skills@1.7.0": {"1.7.0", []string{"disable_binding", "discover", "request_connection", "request_setup", "upsert_binding"}},
	}
	if len(byID) != len(want) {
		t.Fatalf("builder bindings = %v, want exactly %v", keys(byID), keys(want))
	}
	for id, expected := range want {
		binding := byID[id]
		if binding == nil || binding.Disabled || binding.SkillVersion != expected.version || binding.MaximumRisk != skill.RiskLevelWrite {
			t.Fatalf("binding %s = %+v, want enabled %s at write", id, binding, expected.version)
		}
		if actions := sortedActions(binding.AllowedActions); !slices.Equal(actions, expected.actions) {
			t.Fatalf("binding %s actions = %v, want %v", id, actions, expected.actions)
		}
	}
	return byID
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestBuilderAgentSeededOnce(t *testing.T) {
	engine := newBuilderSeedEngine(t)
	ctx := t.Context()
	if err := SeedBuilderAgent(ctx, engine, localScope); err != nil {
		t.Fatal(err)
	}
	reference := skill.ScopeReference{Kind: localScope.Kind, ID: localScope.ID}
	deployment, err := engine.GetAgentDeployment(ctx, reference, runtime.AgentBuilderDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.DisplayName != "Builder" || deployment.DefinitionID != BuilderDefinitionID || deployment.ActiveVersion != BuilderDefinitionVersion {
		t.Fatalf("builder deployment = %+v", deployment)
	}
	definition, err := engine.GetAgentDefinition(ctx, BuilderDefinitionID, BuilderDefinitionVersion)
	if err != nil {
		t.Fatal(err)
	}
	if definition.DisplayName != "Builder" || definition.Purpose != "Builds and changes agents and teams with you." ||
		definition.Amendments.AgentMayPropose || len(definition.Amendments.AllowedFields) != 0 ||
		definition.Authority.MaximumRisk != skill.RiskLevelWrite {
		t.Fatalf("builder definition = %+v", definition)
	}
	first := assertBuilderBindings(t, engine)

	// A restart changes nothing.
	if err := SeedBuilderAgent(ctx, engine, localScope); err != nil {
		t.Fatalf("second seed: %v", err)
	}
	again, err := engine.GetAgentDeployment(ctx, reference, runtime.AgentBuilderDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != deployment.Revision {
		t.Fatalf("restart changed the deployment: revision %d -> %d", deployment.Revision, again.Revision)
	}
	for id, binding := range assertBuilderBindings(t, engine) {
		if binding.Revision != first[id].Revision {
			t.Fatalf("restart rewrote binding %s: revision %d -> %d", id, first[id].Revision, binding.Revision)
		}
	}
}

func TestBuilderAgentBindingsUpgradedOnVersionChange(t *testing.T) {
	engine := newBuilderSeedEngine(t)
	ctx := t.Context()
	reference := skill.ScopeReference{Kind: localScope.Kind, ID: localScope.ID}
	owner := opensealkernel.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: runtime.AgentBuilderDeploymentID}

	// A builder seeded by an earlier release: an older definition version and
	// bindings to older bundled Skill versions.
	old := builderDefinition()
	old.Version = "0.9.0"
	if _, err := engine.RegisterAgentDefinition(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateAgentDeployment(ctx, &opensealkernel.AgentDeployment{
		ID: runtime.AgentBuilderDeploymentID, DisplayName: BuilderDisplayName, Scope: reference,
		DefinitionID: BuilderDefinitionID, ActiveVersion: old.Version, Environment: "default",
		RolloutStatus: opensealkernel.AgentRolloutActive, Revision: 1,
		Capacity: opensealkernel.AgentDeploymentCapacity{MaxConcurrentRuns: 4, MaxQueuedRuns: 32},
	}, "system", "test", "earlier release"); err != nil {
		t.Fatal(err)
	}
	agents := runtime.AgentManagementSkill()
	agents.Version = "1.4.0"
	for _, name := range []string{runtime.AgentActionCreateAgent, runtime.AgentActionUpdateAgent, runtime.AgentActionTryAgent, runtime.AgentActionListAgents} {
		delete(agents.Actions, name)
	}
	skills := runtime.SkillManagementSkill()
	skills.Version = "1.6.1"
	for _, definition := range []*skill.Definition{agents, skills} {
		if err := engine.RegisterSkill(ctx, definition); err != nil {
			t.Fatalf("register %s@%s: %v", definition.ID, definition.Version, err)
		}
	}
	for _, binding := range []*skill.Binding{
		{ID: "bundled:agents", Scope: reference, DeploymentID: owner.ID, SkillID: agents.ID, SkillVersion: agents.Version,
			AllowedActions: []string{runtime.AgentActionAmendBehavior}, MaximumRisk: skill.RiskLevelWrite},
		{ID: "bundled:skills@1.6.1", Scope: reference, DeploymentID: owner.ID, SkillID: skills.ID, SkillVersion: skills.Version,
			AllowedActions: []string{runtime.SkillActionDiscoverBinding}, MaximumRisk: skill.RiskLevelRead},
	} {
		if _, err := engine.UpsertOwnerSkillBinding(ctx, owner, skill.UpsertBindingRequest{
			Binding: binding, Actor: skill.BindingActor{Type: "system", ID: "test"}, Reason: "earlier release",
		}); err != nil {
			t.Fatalf("bind %s: %v", binding.ID, err)
		}
	}

	if err := SeedBuilderAgent(ctx, engine, localScope); err != nil {
		t.Fatal(err)
	}
	deployment, err := engine.GetAgentDeployment(ctx, reference, runtime.AgentBuilderDeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	if deployment.ActiveVersion != BuilderDefinitionVersion {
		t.Fatalf("builder active version = %s, want %s", deployment.ActiveVersion, BuilderDefinitionVersion)
	}
	byID := assertBuilderBindings(t, engine)
	if _, ok := byID["bundled:skills@1.6.1"]; ok {
		t.Fatal("the 1.6.1 skills binding was kept")
	}

	// A binding narrowed or disabled by hand is restored at the next start.
	teams := byID["bundled:teams"]
	if _, err := engine.DisableOwnerSkillBinding(ctx, owner, skill.DisableBindingRequest{
		Scope: reference, DeploymentID: owner.ID, BindingID: teams.ID, ExpectedRevision: teams.Revision,
		Actor: skill.BindingActor{Type: "user", ID: "local-operator"}, Reason: "test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := SeedBuilderAgent(ctx, engine, localScope); err != nil {
		t.Fatal(err)
	}
	assertBuilderBindings(t, engine)
}

// writeV75DaemonStore writes a daemon SQLite file as v75 left it: one ready
// change-set draft and no builder. Opening it again runs migration 76,
// which keeps the draft in the cutover table.
func writeV75DaemonStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kernel.db")
	fresh, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE workforce_change_sets (
		scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
		parent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, revision INTEGER NOT NULL,
		idempotency_key TEXT NOT NULL, request_digest TEXT NOT NULL, candidate_digest TEXT NOT NULL,
		created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
		PRIMARY KEY (scope_kind, scope_id, id), UNIQUE (scope_kind, scope_id, idempotency_key))`); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]interface{}{
		"id": "cs-trips", "mode": "create", "status": "ready",
		"prompt": "Create the profile of exactly one personal agent from this request. Request: Help me plan trips",
		"actor":  map[string]string{"type": "user", "id": "local-operator"},
		"result": map[string]interface{}{"candidate": map[string]interface{}{"agents": []map[string]interface{}{{
			"displayName": "Trip planner", "purpose": "Plans trips", "systemPrompt": "Plan trips.", "personality": "Warm",
			"operatingPrinciples": []string{"Ask first"},
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := db.Exec(`INSERT INTO workforce_change_sets VALUES (?,?,'cs-trips','','ready',1,'key-cs-trips','d','d',?,?,?)`,
		localScope.Kind, localScope.ID, now, now, string(payload)); err != nil {
		t.Fatal(err)
	}
	return path
}

// startDaemonEngine opens path the way the daemon does, with its cutover
// host, and runs one Engine.Start.
func startDaemonEngine(t *testing.T, path string) (*opensealkernel.Engine, *runtime.SQLiteStore) {
	t.Helper()
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	host := &BuilderCutoverHost{}
	engine, err := opensealkernel.New(
		opensealkernel.WithPersistentStore(store),
		opensealkernel.WithAgentBuilderCutoverHost(host),
		opensealkernel.WithSkillManagementActions(),
		opensealkernel.WithAgentManagementActions(),
		opensealkernel.WithTeamManagementActions(),
	)
	if err != nil {
		t.Fatal(err)
	}
	host.Bind(engine)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		engine.Stop()
		_ = store.Close()
	})
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return engine, store
}

func builderConversationMessages(t *testing.T, engine *opensealkernel.Engine) []*runtime.ChannelMessage {
	t.Helper()
	ctx := context.Background()
	conversations, err := engine.ListConversations(ctx, runtime.ConversationFilter{Scope: localScope, OriginKind: runtime.ConversationReferenceAgentBuilder})
	if err != nil {
		t.Fatal(err)
	}
	var messages []*runtime.ChannelMessage
	for _, conversation := range conversations {
		if conversation.Owner.ID != runtime.AgentBuilderDeploymentID {
			t.Fatalf("builder conversation %s is owned by %+v", conversation.ID, conversation.Owner)
		}
		listed, err := engine.ListChannelMessages(ctx, runtime.ChannelMessageFilter{Scope: localScope, ConversationID: conversation.ID})
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, listed...)
	}
	return messages
}

func TestBuilderCutoverHostConvertsAV75DraftAtFirstStart(t *testing.T) {
	path := writeV75DaemonStore(t)

	engine, store := startDaemonEngine(t, path)
	// The draft needed the builder, so the host seeded it in full.
	if _, err := engine.GetAgentDeployment(t.Context(), skill.ScopeReference{Kind: localScope.Kind, ID: localScope.ID}, runtime.AgentBuilderDeploymentID); err != nil {
		t.Fatalf("builder after the first start: %v", err)
	}
	assertBuilderBindings(t, engine)
	messages := builderConversationMessages(t, engine)
	if len(messages) != 1 {
		t.Fatalf("builder messages = %d, want the draft as one conversation", len(messages))
	}
	message := messages[0]
	if message.Content != "Help me plan trips" || message.BuilderParameters == nil ||
		message.BuilderParameters.Kind != runtime.AgentBuilderKindAgent || message.BuilderParameters.Name != "Trip planner" ||
		message.BuilderParameters.Model != "" || message.BuilderParameters.Draft == nil || message.BuilderParameters.Draft.Purpose != "Plans trips" {
		t.Fatalf("builder message = %#v parameters %#v", message, message.BuilderParameters)
	}
	if drafts, err := store.ListAgentBuilderCutoverDrafts(t.Context()); err != nil || len(drafts) != 0 {
		t.Fatalf("drafts left = %v, %v", drafts, err)
	}
	engine.Stop()
	_ = store.Close()

	// A restart converts nothing more.
	engine, _ = startDaemonEngine(t, path)
	if again := builderConversationMessages(t, engine); len(again) != 1 || again[0].ID != message.ID {
		t.Fatalf("builder messages after a restart = %d", len(again))
	}
	assertBuilderBindings(t, engine)
}

func TestBuilderCutoverHostRequiresBind(t *testing.T) {
	if err := (&BuilderCutoverHost{}).EnsureBuilderAgent(t.Context(), localScope, "local-operator"); err == nil {
		t.Fatal("an unbound cutover host seeded nothing and reported success")
	}
}
