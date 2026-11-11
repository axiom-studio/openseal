package openseal

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

var builderCutoverTestScope = Scope{Kind: "tenant", ID: "cutover"}

// recordingCutoverHost provisions the builder on the engine it is given and
// records every call in order.
type recordingCutoverHost struct {
	mu        sync.Mutex
	engine    *Engine
	ensureErr error
	models    map[string]string
	calls     []string
}

func (h *recordingCutoverHost) record(call string) {
	h.mu.Lock()
	h.calls = append(h.calls, call)
	h.mu.Unlock()
}

func (h *recordingCutoverHost) EnsureBuilderAgent(ctx context.Context, scope Scope, requester string) error {
	h.record("ensure:" + requester)
	if h.ensureErr != nil {
		return h.ensureErr
	}
	// Idempotent like the real host: an existing builder is upgraded, here
	// only recorded.
	if present, err := h.engine.agentBuilderPresent(ctx, scope); err != nil || present {
		return err
	}
	return createCutoverBuilder(ctx, h.engine, scope)
}

func (h *recordingCutoverHost) DraftModel(_ context.Context, _ Scope, key string) (string, error) {
	h.record("model:" + key)
	return h.models[key], nil
}

func (h *recordingCutoverHost) AfterAgentBuilderCutover(context.Context) error {
	h.record("after")
	return nil
}

func (h *recordingCutoverHost) recorded() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

func createCutoverAgent(ctx context.Context, engine *Engine, scope Scope, id string) error {
	reference := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
		ID: id + "-definition", Version: "1.0.0", DisplayName: id, Purpose: "Help", SystemPrompt: "Help.",
		Authority: AgentAuthorityPolicy{MaximumRisk: skill.RiskLevelRead, MaxConcurrentRuns: 1},
	})
	if err != nil {
		return err
	}
	_, _, err = engine.CreateAgentDeployment(ctx, &AgentDeployment{
		ID: id, Scope: reference, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		RolloutStatus: "active", Environment: "default", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 1},
	}, "user", "person", "test")
	return err
}

func createCutoverBuilder(ctx context.Context, engine *Engine, scope Scope) error {
	return createCutoverAgent(ctx, engine, scope, AgentBuilderDeploymentID)
}

type cutoverTestRow struct {
	id, parentID, status, mode, prompt, name, deployment string
}

// seedCutoverDraftStore writes a v75 SQLite file (a change-set lineage, a
// review draft and an applied one) and reopens it, so migration 76 keeps
// the two leaves in the cutover table.
func seedCutoverDraftStore(t *testing.T) string {
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
	now := time.Now().UTC()
	rows := []cutoverTestRow{
		{id: "cs-create", status: "ready", mode: "create", name: "Trip planner",
			prompt: "Create the profile of exactly one defaultSpace agent from this request: one agent. Request: Help me plan trips"},
		{id: "cs-amend", parentID: "cs-create", status: "ready", mode: "amend", name: "Trip planner", prompt: "Revise. Request: Make it friendlier"},
		{id: "cs-review", status: "review", mode: "create", name: "Inbox watcher", prompt: "Request: Watch my inbox", deployment: "inbox-agent"},
		{id: "cs-applied", status: "applied", mode: "create", name: "Old", prompt: "Request: Old"},
	}
	for index, row := range rows {
		payload := map[string]interface{}{
			"id": row.id, "mode": row.mode, "prompt": row.prompt, "status": row.status,
			"actor": map[string]string{"type": "user", "id": "ana"},
			"result": map[string]interface{}{"candidate": map[string]interface{}{"agents": []map[string]interface{}{{
				"displayName": row.name, "purpose": "Plans trips", "systemPrompt": "Plan trips.", "personality": "Warm",
				"operatingPrinciples": []string{"Ask first"},
			}}}},
		}
		if row.parentID != "" {
			payload["parentId"] = row.parentID
		}
		if row.deployment != "" {
			payload["placement"] = map[string]interface{}{"agentDeploymentIds": map[string]string{"agent-1": row.deployment}}
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO workforce_change_sets VALUES (?,?,?,?,?,1,?,'d','d',?,?,?)`,
			builderCutoverTestScope.Kind, builderCutoverTestScope.ID, row.id, row.parentID, row.status, "key-"+row.id,
			now.Add(time.Duration(index)*time.Second), now, string(encoded)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func openCutoverEngine(t *testing.T, path string, options ...Option) (*Engine, *runtime.SQLiteStore) {
	t.Helper()
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(append([]Option{WithStore(store)}, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		engine.Stop()
		_ = store.Close()
	})
	return engine, store
}

func startCutoverEngine(t *testing.T, engine *Engine) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}
	engine.Stop()
}

func builderCutoverConversations(t *testing.T, engine *Engine) map[string]*runtime.ChannelMessage {
	t.Helper()
	ctx := context.Background()
	conversations, err := engine.ListConversations(ctx, runtime.ConversationFilter{
		Scope: builderCutoverTestScope, OriginKind: runtime.ConversationReferenceAgentBuilder,
	})
	if err != nil {
		t.Fatal(err)
	}
	first := map[string]*runtime.ChannelMessage{}
	for _, conversation := range conversations {
		messages, err := engine.ListChannelMessages(ctx, runtime.ChannelMessageFilter{Scope: builderCutoverTestScope, ConversationID: conversation.ID})
		if err != nil {
			t.Fatal(err)
		}
		if len(messages) != 1 {
			t.Fatalf("conversation %s has %d messages", conversation.ID, len(messages))
		}
		first[conversation.Origin.ID] = messages[0]
	}
	return first
}

func cutoverDraftIDs(t *testing.T, store *runtime.SQLiteStore) []string {
	t.Helper()
	drafts, err := store.ListAgentBuilderCutoverDrafts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, draft := range drafts {
		ids = append(ids, draft.ID)
	}
	sort.Strings(ids)
	return ids
}

func TestAgentBuilderCutoverConvertsEachDraftAtStart(t *testing.T) {
	ctx := context.Background()
	path := seedCutoverDraftStore(t)
	host := &recordingCutoverHost{models: map[string]string{"key-cs-amend": "gpt-6-luna"}}
	engine, store := openCutoverEngine(t, path, WithAgentBuilderCutoverHost(host))
	host.engine = engine

	// The placed deployment of the review draft lives in its own space.
	if err := createCutoverAgent(ctx, engine, builderCutoverTestScope, "inbox-agent"); err != nil {
		t.Fatal(err)
	}
	inbox, err := engine.CreateSpace(ctx, runtime.CreateSpaceRequest{Scope: builderCutoverTestScope, Name: "Inbox", Look: runtime.DefaultSpaceLook, CreatedBy: "ana"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.PutSpaceMember(ctx, runtime.PutSpaceMemberRequest{Scope: builderCutoverTestScope, SpaceID: inbox.ID, Type: runtime.SpaceMemberAgent, MemberID: "inbox-agent"}); err != nil {
		t.Fatal(err)
	}
	defaultSpace, _, err := engine.EnsureDefaultSpace(ctx, builderCutoverTestScope)
	if err != nil {
		t.Fatal(err)
	}
	_ = engine.DeleteSpaceMember(ctx, runtime.DeleteSpaceMemberRequest{Scope: builderCutoverTestScope, SpaceID: defaultSpace.ID, Type: runtime.SpaceMemberAgent, MemberID: "inbox-agent"})

	startCutoverEngine(t, engine)

	// The builder was missing: provisioned once, before any conversion; each
	// model was read before the after-cutover step.
	if calls := host.recorded(); !reflect.DeepEqual(calls, []string{"ensure:ana", "model:key-cs-amend", "model:key-cs-review", "after"}) {
		t.Fatalf("host calls = %v", calls)
	}
	messages := builderCutoverConversations(t, engine)
	if len(messages) != 2 {
		t.Fatalf("builder conversations = %v", messages)
	}
	amend, review := messages["cutover-cs-amend"], messages["cutover-cs-review"]
	if amend == nil || review == nil {
		t.Fatalf("conversations by origin = %v", messages)
	}
	ana := runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "ana"}
	if amend.Sender != ana || amend.Content != "Help me plan trips\nMake it friendlier" || amend.BuilderParameters == nil ||
		!reflect.DeepEqual(*amend.BuilderParameters, AgentBuilderParameters{Kind: runtime.AgentBuilderKindAgent, Name: "Trip planner", Model: "gpt-6-luna",
			Draft: &AgentBuilderDraftProfile{Purpose: "Plans trips", SystemPrompt: "Plan trips.", Personality: "Warm", OperatingPrinciples: []string{"Ask first"}}}) {
		t.Fatalf("amend message = %#v parameters %#v", amend, amend.BuilderParameters)
	}
	if review.Content != "Watch my inbox" || review.BuilderParameters.Name != "Inbox watcher" || review.BuilderParameters.Model != "" {
		t.Fatalf("review message = %#v parameters %#v", review, review.BuilderParameters)
	}
	reviewConversation, err := engine.GetConversation(ctx, builderCutoverTestScope, review.ConversationID)
	if err != nil || reviewConversation.SpaceID != inbox.ID || reviewConversation.Owner.ID != AgentBuilderDeploymentID {
		t.Fatalf("review conversation = %#v, %v; want space %s", reviewConversation, err, inbox.ID)
	}
	if ids := cutoverDraftIDs(t, store); len(ids) != 0 {
		t.Fatalf("drafts left = %v", ids)
	}

	// A second start converts nothing and does not repeat the host step.
	startCutoverEngine(t, engine)
	if calls := host.recorded(); len(calls) != 4 {
		t.Fatalf("host calls after second start = %v", calls)
	}
	if again := builderCutoverConversations(t, engine); len(again) != 2 {
		t.Fatalf("builder conversations after second start = %d", len(again))
	}
}

func TestAgentBuilderCutoverKeepsDraftsUntilTheBuilderCanBeProvisioned(t *testing.T) {
	path := seedCutoverDraftStore(t)

	// No cutover host: nothing is converted or dropped.
	engine, store := openCutoverEngine(t, path)
	startCutoverEngine(t, engine)
	if ids := cutoverDraftIDs(t, store); !reflect.DeepEqual(ids, []string{"cs-amend", "cs-review"}) {
		t.Fatalf("drafts without a host = %v", ids)
	}
	if conversations := builderCutoverConversations(t, engine); len(conversations) != 0 {
		t.Fatalf("conversations without a host = %d", len(conversations))
	}
	engine.Stop()
	_ = store.Close()

	// EnsureBuilderAgent fails: every draft is kept and the table stays.
	failing := &recordingCutoverHost{ensureErr: errors.New("provisioning is down")}
	engine, store = openCutoverEngine(t, path, WithAgentBuilderCutoverHost(failing))
	failing.engine = engine
	report, err := engine.AgentBuilderCutover(context.Background())
	if err != nil || report.Kept != 2 || report.Converted != 0 || report.Dropped {
		t.Fatalf("report with failing host = %#v, %v", report, err)
	}
	for _, call := range failing.recorded() {
		if call == "after" || strings.HasPrefix(call, "model:") {
			t.Fatalf("host calls with a missing builder = %v", failing.recorded())
		}
	}
	if ids := cutoverDraftIDs(t, store); len(ids) != 2 {
		t.Fatalf("drafts after failed provisioning = %v", ids)
	}
	engine.Stop()
	_ = store.Close()

	// The next start with a working host converts them.
	working := &recordingCutoverHost{}
	engine, store = openCutoverEngine(t, path, WithAgentBuilderCutoverHost(working))
	working.engine = engine
	startCutoverEngine(t, engine)
	if ids := cutoverDraftIDs(t, store); len(ids) != 0 {
		t.Fatalf("drafts after recovery = %v", ids)
	}
	if conversations := builderCutoverConversations(t, engine); len(conversations) != 2 {
		t.Fatalf("conversations after recovery = %d", len(conversations))
	}
	if calls := working.recorded(); calls[0] != "ensure:ana" || calls[len(calls)-1] != "after" {
		t.Fatalf("host calls after recovery = %v", calls)
	}
}

func TestAgentBuilderCutoverEnsuresAnExistingBuilderBeforeConverting(t *testing.T) {
	ctx := context.Background()
	path := seedCutoverDraftStore(t)
	host := &recordingCutoverHost{}
	engine, store := openCutoverEngine(t, path, WithAgentBuilderCutoverHost(host))
	host.engine = engine
	// Finn already exists, possibly on bindings without create_agent.
	if err := createCutoverBuilder(ctx, engine, builderCutoverTestScope); err != nil {
		t.Fatal(err)
	}

	startCutoverEngine(t, engine)

	// EnsureBuilderAgent still runs, once for the scope and before any draft
	// is converted, so the host upgrades Finn before its first builder turn.
	if calls := host.recorded(); !reflect.DeepEqual(calls, []string{"ensure:ana", "model:key-cs-amend", "model:key-cs-review", "after"}) {
		t.Fatalf("host calls = %v", calls)
	}
	if ids := cutoverDraftIDs(t, store); len(ids) != 0 {
		t.Fatalf("drafts left = %v", ids)
	}
	if conversations := builderCutoverConversations(t, engine); len(conversations) != 2 {
		t.Fatalf("builder conversations = %d", len(conversations))
	}
}

func TestAgentBuilderCutoverKeepsDraftsWhenAnExistingBuilderCannotBeUpgraded(t *testing.T) {
	ctx := context.Background()
	path := seedCutoverDraftStore(t)
	host := &recordingCutoverHost{ensureErr: errors.New("upgrade failed")}
	engine, store := openCutoverEngine(t, path, WithAgentBuilderCutoverHost(host))
	host.engine = engine
	if err := createCutoverBuilder(ctx, engine, builderCutoverTestScope); err != nil {
		t.Fatal(err)
	}
	report, err := engine.AgentBuilderCutover(ctx)
	if err != nil || report.Kept != 2 || report.Converted != 0 || report.Dropped {
		t.Fatalf("report = %#v, %v", report, err)
	}
	if calls := host.recorded(); !reflect.DeepEqual(calls, []string{"ensure:ana"}) {
		t.Fatalf("host calls = %v", calls)
	}
	if ids := cutoverDraftIDs(t, store); len(ids) != 2 {
		t.Fatalf("drafts = %v", ids)
	}
	if conversations := builderCutoverConversations(t, engine); len(conversations) != 0 {
		t.Fatalf("builder conversations = %d", len(conversations))
	}
}

func TestAgentBuilderCutoverHostIsRequired(t *testing.T) {
	if _, err := New(WithAgentBuilderCutoverHost(nil)); err == nil {
		t.Fatal("a nil cutover host was accepted")
	}
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	if report, err := engine.AgentBuilderCutover(context.Background()); err != nil || report != (AgentBuilderCutoverReport{}) {
		t.Fatalf("memory store cutover = %#v, %v", report, err)
	}
}
