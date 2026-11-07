package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

const (
	cutoverCreatePrompt = "Create the profile of exactly one personal agent from this request: keep it a single agent. Request: Help me plan trips"
	cutoverAmendPrompt  = "Revise the draft. Request: Make it friendlier"
)

// cutoverChangeSetRow is a retired change-set row as a v75 store held it.
type cutoverChangeSetRow struct {
	id, parentID, status, mode, prompt string
	agentName                          string
}

func (r cutoverChangeSetRow) payload(t *testing.T, scope Scope) string {
	t.Helper()
	value := map[string]interface{}{
		"id": r.id, "scope": scope, "mode": r.mode, "prompt": r.prompt, "status": r.status, "profileOnly": true,
		"actor": map[string]string{"type": "user", "id": "user-7"},
		"result": map[string]interface{}{"candidate": map[string]interface{}{"agents": []map[string]interface{}{{
			"id": "agent-" + r.id, "version": "1.0.0", "displayName": r.agentName, "purpose": "Plans trips\nend to end",
			"systemPrompt": "Plan trips carefully.", "personality": "Warm", "operatingPrinciples": []string{"Ask first", "Ask first", " "},
			"authority": map[string]string{"maximumRisk": "read"},
		}}}},
		"placement": map[string]interface{}{"agentDeploymentIds": map[string]string{"agent-" + r.id: "deployment-" + r.id}},
	}
	if r.parentID != "" {
		value["parentId"] = r.parentID
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// cutoverLineageRows: a create→amend lineage (leaf: the amend), an applied
// one, a review draft with a failed revision (leaf: the review draft) and a
// rejected one.
var cutoverLineageRows = []cutoverChangeSetRow{
	{id: "cs-create", status: "ready", mode: "create", prompt: cutoverCreatePrompt, agentName: "Trip planner"},
	{id: "cs-amend", parentID: "cs-create", status: "ready", mode: "amend", prompt: cutoverAmendPrompt, agentName: "Friendly trip planner"},
	{id: "cs-applied", status: "applied", mode: "create", prompt: "Request: Old agent", agentName: "Old"},
	{id: "cs-review", status: "review", mode: "create", prompt: "Request: Watch my inbox", agentName: "Inbox watcher"},
	{id: "cs-failed", parentID: "cs-review", status: "failed", mode: "amend", prompt: "Request: broken", agentName: "Broken"},
	{id: "cs-rejected", status: "rejected", mode: "create", prompt: "Request: no", agentName: "No"},
}

var builderCutoverScope = Scope{Kind: "tenant", ID: "cutover"}

// seedSQLiteChangeSets turns a current SQLite file back into a v75 one: the
// change-set table and indexes, a running workforce_authoring Run and
// deployments carrying an activation continuation.
func seedSQLiteChangeSets(t *testing.T, path string) string {
	t.Helper()
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: builderCutoverScope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "author", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	statements := []string{
		`CREATE TABLE workforce_change_sets (
			scope_kind TEXT NOT NULL, scope_id TEXT NOT NULL, id TEXT NOT NULL,
			parent_id TEXT NOT NULL DEFAULT '', status TEXT NOT NULL, revision INTEGER NOT NULL,
			idempotency_key TEXT NOT NULL, request_digest TEXT NOT NULL, candidate_digest TEXT NOT NULL,
			created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, payload TEXT NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			UNIQUE (scope_kind, scope_id, idempotency_key))`,
		`CREATE INDEX idx_workforce_change_sets_parent ON workforce_change_sets(scope_kind, scope_id, parent_id, created_at)`,
		`CREATE INDEX idx_workforce_change_sets_status ON workforce_change_sets(scope_kind, scope_id, status, updated_at)`,
		`CREATE INDEX idx_workforce_change_sets_global_recovery ON workforce_change_sets(status, scope_kind, scope_id)`,
		`CREATE INDEX idx_agent_runs_global_recovery ON agent_runs(status, available_at, lease_expires_at, scope_kind, scope_id)
			WHERE COALESCE(json_extract(payload, '$.kind'), 'agent_work') = 'workforce_authoring'`,
	}
	for _, statement := range statements {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for index, row := range cutoverLineageRows {
		if _, err := store.db.Exec(`INSERT INTO workforce_change_sets (scope_kind, scope_id, id, parent_id, status, revision, idempotency_key, request_digest, candidate_digest, created_at, updated_at, payload)
			VALUES (?,?,?,?,?,1,?,'digest','digest',?,?,?)`, builderCutoverScope.Kind, builderCutoverScope.ID, row.id, row.parentID, row.status,
			"key-"+row.id, now.Add(time.Duration(index)*time.Second), now, row.payload(t, builderCutoverScope)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE agent_runs SET status = 'running', lease_owner = 'authoring-worker',
		payload = json_set(payload, '$.kind', 'workforce_authoring', '$.status', 'running', '$.leaseOwner', 'authoring-worker') WHERE id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"agent_deployments", "team_deployments"} {
		if _, err := store.db.Exec(`INSERT INTO `+table+` (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload)
			VALUES (?,?,?,'definition-1','1',1,?,?)`, builderCutoverScope.Kind, builderCutoverScope.ID, "paused-"+table, now,
			`{"id":"paused-`+table+`","rolloutStatus":"paused","status":"paused","activation":{"changeSetId":"cs-applied"}}`); err != nil {
			t.Fatal(err)
		}
	}
	return run.ID
}

func sqliteObjectNames(t *testing.T, db *sql.DB, kind, prefix string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = ? AND name LIKE ? ORDER BY name`, kind, prefix+"%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	return names
}

func TestAgentBuilderMigrationSQLiteKeepsLeafDraftsAndRetiresChangeSets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	runID := seedSQLiteChangeSets(t, path)
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	if names := sqliteObjectNames(t, store.db, "table", "workforce_change_sets"); len(names) != 0 {
		t.Fatalf("change-set table survived: %v", names)
	}
	if names := sqliteObjectNames(t, store.db, "index", "idx_workforce_change_sets"); len(names) != 0 {
		t.Fatalf("change-set indexes survived: %v", names)
	}
	if names := sqliteObjectNames(t, store.db, "index", "idx_agent_runs_global_recovery"); len(names) != 0 {
		t.Fatalf("authoring recovery index survived: %v", names)
	}
	drafts, err := store.ListAgentBuilderCutoverDrafts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertCutoverLeafDrafts(t, drafts)

	run, err := store.GetAgentRun(ctx, builderCutoverScope, runID)
	if err != nil || run.Status != AgentRunStatusCanceled || run.LeaseOwner != "" {
		t.Fatalf("authoring run = %#v, %v; want canceled without lease", run, err)
	}
	var status, leaseOwner string
	if err := store.db.QueryRow(`SELECT status, lease_owner FROM agent_runs WHERE id = ?`, runID).Scan(&status, &leaseOwner); err != nil || status != "canceled" || leaseOwner != "" {
		t.Fatalf("authoring run columns = %q %q, %v", status, leaseOwner, err)
	}
	for _, table := range []string{"agent_deployments", "team_deployments"} {
		var activation sql.NullString
		if err := store.db.QueryRow(`SELECT json_type(payload, '$.activation') FROM `+table+` WHERE id = ?`, "paused-"+table).Scan(&activation); err != nil || activation.Valid {
			t.Fatalf("%s activation = %v, %v; want removed", table, activation, err)
		}
	}

	// A later open changes nothing.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := store.ListAgentBuilderCutoverDrafts(ctx); err != nil || !reflect.DeepEqual(again, drafts) {
		t.Fatalf("drafts after reopen = %#v, %v", again, err)
	}

	if dropped, err := store.DropAgentBuilderCutoverTable(ctx); err != nil || dropped {
		t.Fatalf("drop with drafts left = %v, %v; want kept", dropped, err)
	}
	for _, draft := range drafts {
		if err := store.DeleteAgentBuilderCutoverDraft(ctx, draft.Scope, draft.ID); err != nil {
			t.Fatal(err)
		}
	}
	if dropped, err := store.DropAgentBuilderCutoverTable(ctx); err != nil || !dropped {
		t.Fatalf("drop of empty table = %v, %v; want dropped", dropped, err)
	}
	if dropped, err := store.DropAgentBuilderCutoverTable(ctx); err != nil || dropped {
		t.Fatalf("second drop = %v, %v; want nothing to drop", dropped, err)
	}
	if left, err := store.ListAgentBuilderCutoverDrafts(ctx); err != nil || len(left) != 0 {
		t.Fatalf("drafts after drop = %#v, %v", left, err)
	}
	if err := store.DeleteAgentBuilderCutoverDraft(ctx, builderCutoverScope, "cs-amend"); err != nil {
		t.Fatalf("delete after drop = %v", err)
	}
}

func TestAgentBuilderMigrationSQLiteFreshStoreHasNoChangeSets(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, prefix := range []string{"workforce_change_sets", agentBuilderCutoverTable} {
		if names := sqliteObjectNames(t, store.db, "table", prefix); len(names) != 0 {
			t.Fatalf("fresh store has %v", names)
		}
	}
	if drafts, err := store.ListAgentBuilderCutoverDrafts(context.Background()); err != nil || len(drafts) != 0 {
		t.Fatalf("fresh drafts = %#v, %v", drafts, err)
	}
	if dropped, err := store.DropAgentBuilderCutoverTable(context.Background()); err != nil || dropped {
		t.Fatalf("fresh drop = %v, %v", dropped, err)
	}
}

// assertCutoverLeafDrafts checks the two leaves of cutoverLineageRows.
func assertCutoverLeafDrafts(t *testing.T, drafts []AgentBuilderCutoverDraft) {
	t.Helper()
	if len(drafts) != 2 || drafts[0].ID != "cs-amend" || drafts[1].ID != "cs-review" {
		t.Fatalf("leaf drafts = %#v; want cs-amend and cs-review", drafts)
	}
	amend, review := drafts[0], drafts[1]
	if !amend.Readable || amend.Scope != builderCutoverScope || amend.IdempotencyKey != "key-cs-amend" || amend.ActorType != "user" || amend.ActorID != "user-7" || amend.Mode != "amend" {
		t.Fatalf("amend draft = %#v", amend)
	}
	if !reflect.DeepEqual(amend.CutoverPrompts, []string{cutoverCreatePrompt}) || len(review.CutoverPrompts) != 0 {
		t.Fatalf("cutover prompts = %#v / %#v", amend.CutoverPrompts, review.CutoverPrompts)
	}
	if amend.Agent == nil || amend.Agent.DisplayName != "Friendly trip planner" || amend.Placement.AgentDeploymentIDs["agent-cs-amend"] != "deployment-cs-amend" {
		t.Fatalf("amend profile = %#v placement = %#v", amend.Agent, amend.Placement)
	}
	request := amend.StartRequest("gpt-6-luna", "")
	if request.Prompt != "Help me plan trips\nMake it friendlier" || request.IdempotencyKey != "cutover-cs-amend" ||
		request.Requester != (ConversationParticipant{Type: ConversationParticipantUser, ID: "user-7"}) {
		t.Fatalf("amend start request = %#v", request)
	}
	if request.Parameters.Name != "Friendly trip planner" || request.Parameters.Model != "gpt-6-luna" || request.Parameters.Kind != AgentBuilderKindAgent ||
		!reflect.DeepEqual(request.Parameters.Draft, &AgentBuilderDraftProfile{Purpose: "Plans trips end to end", SystemPrompt: "Plan trips carefully.", Personality: "Warm", OperatingPrinciples: []string{"Ask first"}}) {
		t.Fatalf("amend parameters = %#v draft %#v", request.Parameters, request.Parameters.Draft)
	}
	if err := request.Parameters.validate(); err != nil {
		t.Fatalf("amend parameters refused: %v", err)
	}
	if strings.Contains(request.Prompt, "Friendly trip planner") || strings.Contains(request.Prompt, "gpt-6-luna") {
		t.Fatalf("name or model leaked into the content: %q", request.Prompt)
	}
	if got := review.StartRequest("", "space-1"); got.Prompt != "Watch my inbox" || got.SpaceID != "space-1" || got.Parameters.Model != "" {
		t.Fatalf("review start request = %#v", got)
	}
}

func TestAgentBuilderCutoverStartRequestFitsParameters(t *testing.T) {
	draft := AgentBuilderCutoverDraft{
		Scope: builderCutoverScope, ID: "cs-1", ActorID: " user-1 ", Prompt: "Plain request without marker",
		Agent: &kernelagent.AgentDefinition{
			DisplayName: strings.Repeat("é", 60), Purpose: "Uses password: hunter2-secret daily",
			SystemPrompt: "  Behave.  ", OperatingPrinciples: []string{"Use api key: sk-1234567890abcdefghijklmnop", "Be kind"},
		},
	}
	request := draft.StartRequest("model\nwith newline", "")
	if request.Prompt != "Plain request without marker" || request.Requester.ID != "user-1" {
		t.Fatalf("request = %#v", request)
	}
	parameters := request.Parameters
	if len(parameters.Name) > maximumAgentBuilderNameBytes || !strings.HasPrefix(parameters.Name, "éé") || parameters.Model != "model with newline" {
		t.Fatalf("name %q (%d bytes) model %q", parameters.Name, len(parameters.Name), parameters.Model)
	}
	if parameters.Draft == nil || parameters.Draft.Purpose != "" || parameters.Draft.SystemPrompt != "Behave." ||
		!reflect.DeepEqual(parameters.Draft.OperatingPrinciples, []string{"Be kind"}) {
		t.Fatalf("draft = %#v", parameters.Draft)
	}
	if err := parameters.validate(); err != nil {
		t.Fatalf("fitted parameters refused: %v", err)
	}
	if bare := (AgentBuilderCutoverDraft{ID: "cs-2", Prompt: "x"}).StartRequest("", ""); bare.Parameters.Draft != nil || bare.Parameters.Name != "" {
		t.Fatalf("draft without a profile = %#v", bare.Parameters)
	}
}
