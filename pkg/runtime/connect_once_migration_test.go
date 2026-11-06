package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// connectOnceMigrationStore is what the migration-74 fixture needs from
// either SQL store.
type connectOnceMigrationStore interface {
	CreateAgentRun(context.Context, *AgentRun) error
	PublishRunEvent(context.Context, *RunEventReceipt) (bool, error)
	ClaimRunEventWaits(context.Context, ClaimRunEventWaitsRequest) ([]*RunEventWait, error)
	ProcessRunEventWait(context.Context, ProcessRunEventWaitRequest) (*RunEventWaitResult, error)
	GetRunEventWait(context.Context, Scope, string, string) (*RunEventWait, error)
	ExternalConversationGatewayStore
}

// connectOnceMigrationHarness runs the same fixture on SQLite and PostgreSQL.
// Queries use "?" placeholders and @name table references.
type connectOnceMigrationHarness struct {
	sqlite bool
	db     func() *sql.DB
	table  func(string) string
	store  func() connectOnceMigrationStore
	// rerun makes the next open apply migration 74 again.
	rerun func(t *testing.T)
}

func (h connectOnceMigrationHarness) query(text string) string {
	text = connectOnceTableReference.ReplaceAllStringFunc(text, func(reference string) string { return h.table(reference[1:]) })
	if h.sqlite {
		return text
	}
	var out strings.Builder
	n := 0
	for _, character := range text {
		if character == '?' {
			n++
			fmt.Fprintf(&out, "$%d", n)
			continue
		}
		out.WriteRune(character)
	}
	return out.String()
}

func (h connectOnceMigrationHarness) exec(t *testing.T, text string, args ...interface{}) {
	t.Helper()
	if _, err := h.db().Exec(h.query(text), args...); err != nil {
		t.Fatalf("%s: %v", text, err)
	}
}

func (h connectOnceMigrationHarness) payload(t *testing.T, table, where string, args ...interface{}) map[string]interface{} {
	t.Helper()
	column := "payload::text"
	if h.sqlite {
		column = "payload"
	}
	var payload string
	if err := h.db().QueryRow(h.query(`SELECT `+column+` FROM @`+table+` WHERE `+where), args...).Scan(&payload); err != nil {
		t.Fatalf("%s %s: %v", table, where, err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func (h connectOnceMigrationHarness) payloads(t *testing.T) map[string]string {
	t.Helper()
	column := "payload::text"
	if h.sqlite {
		column = "payload"
	}
	result := map[string]string{}
	for _, table := range []string{"run_event_waits", "action_calls", "team_definition_amendments", "workforce_change_sets", "external_conversation_gateways", "runbook_activations"} {
		rows, err := h.db().Query(h.query(`SELECT ` + column + ` FROM @` + table))
		if err != nil {
			t.Fatal(err)
		}
		index := 0
		for rows.Next() {
			var payload string
			if err := rows.Scan(&payload); err != nil {
				t.Fatal(err)
			}
			var document interface{}
			_ = json.Unmarshal([]byte(payload), &document)
			encoded, _ := json.Marshal(document)
			result[fmt.Sprintf("%s/%d", table, index)] = string(encoded)
			index++
		}
		rows.Close()
	}
	return result
}

var connectOnceTableReference = regexp.MustCompile(`@[a-z_]+`)

func pathValue(document map[string]interface{}, path ...string) interface{} {
	var value interface{} = document
	for _, key := range path {
		next, _ := value.(map[string]interface{})
		value = next[key]
	}
	return value
}

var connectOnceMigrationNow = time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)

func connectOnceTeamDocument(digest, derivedFrom string) string {
	return `{"id":"crew","version":"2","displayName":"Crew","purpose":"Help","roles":[{"id":"member","displayName":"Member","purpose":"Help","minimumMembers":1}],` +
		`"approvals":{"maximumRisk":"read"},"provenance":{"derivedFrom":"` + derivedFrom + `"},"digest":"` + digest + `"}`
}

// seedConnectOnceMigration writes records as migrations 71 to 73 left them.
func seedConnectOnceMigration(t *testing.T, h connectOnceMigrationHarness) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	now := connectOnceMigrationNow
	store := h.store()

	// Step 1: a Team run parked on its member's binding, moved to the Team by 73.
	waitRun := func(id, deployment string) {
		run := &AgentRun{ID: id, Scope: scope, RootRunID: id, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "member",
			Goal: "Wait for the reply", Source: RunSourceManual, Status: AgentRunStatusWaitingForEvent, Revision: 1,
			CreatedAt: now, UpdatedAt: now, AvailableAt: now, QueueEnteredAt: now,
			WakeCondition: &WakeCondition{Type: "event", EventWait: &RunEventWaitSpec{Key: "reply", Type: "conversation.message.received",
				Source: RunEventBindingSource(deployment, "slack", "conversations"), Subject: "C1",
				Attributes: map[string]interface{}{"deploymentId": "member", "bindingId": "slack", "adapterId": "conversations"},
				After:      now.Add(-time.Minute), Deadline: now.Add(time.Hour)}}}
		if err := store.CreateAgentRun(ctx, run); err != nil {
			t.Fatal(err)
		}
	}
	waitRun("waiting", "member")
	waitRun("unverifiable", "elsewhere")
	if h.sqlite {
		h.exec(t, `UPDATE @run_event_waits SET payload = json_set(payload, '$.umbrellaId', 'crew')`)
	} else {
		h.exec(t, `UPDATE @run_event_waits SET payload = jsonb_set(payload, '{umbrellaId}', '"crew"')`)
	}

	// Step 2: a setup-request copy that still names the member.
	h.exec(t, `INSERT INTO @skill_setup_requests (scope_kind, scope_id, id, deployment_id, conversation_id, status, revision, created_at, payload) VALUES ('tenant','one','setup-1','crew','chat','pending',1,?,?)`,
		now, `{"id":"setup-1","owner":{"type":"team","id":"crew"},"deploymentId":"crew","agentId":"member","status":"pending","revision":1}`)
	h.exec(t, `INSERT INTO @action_calls (id, scope_kind, scope_id, run_id, status, available_at, revision, created_at, payload) VALUES ('call-1','tenant','one','waiting','succeeded',?,1,?,?)`,
		now, now, `{"id":"call-1","output":{"setupRequest":{"id":"setup-1","kind":"setup","owner":{"type":"agent","id":"member"},"deploymentId":"member","agentId":"member"}}}`)

	// Step 3: amendments and change sets with digests from before 71.
	h.exec(t, `INSERT INTO @team_definitions (id, version, digest, created_at, payload) VALUES ('crew','1','sha256:base',?,?)`, now, `{"id":"crew","version":"1","digest":"sha256:base"}`)
	amendment := func(id, status, version string) {
		h.exec(t, `INSERT INTO @team_definition_amendments (id, scope_kind, scope_id, deployment_id, status, revision, updated_at, payload) VALUES (?,'tenant','one','crew',?,1,?,?)`,
			id, status, now, `{"id":"`+id+`","deploymentId":"crew","definitionId":"crew","baseVersion":"`+version+`","baseDigest":"sha256:stale","candidate":`+connectOnceTeamDocument("sha256:stale", "sha256:stale")+
				`,"proposerType":"agent","proposerId":"member","rationale":"Sharper purpose","evidenceRefs":["run:1"],"requestDigest":"sha256:stale","status":"`+status+`","revision":1}`)
	}
	amendment("amend-open", "awaiting_approval", "1")
	amendment("amend-orphan", "evaluating", "9")
	amendment("amend-done", "activated", "1")
	changeSet := func(id, status string) {
		h.exec(t, `INSERT INTO @workforce_change_sets (scope_kind, scope_id, id, status, revision, idempotency_key, request_digest, candidate_digest, created_at, updated_at, payload) VALUES ('tenant','one',?,?,1,?,'r','c',?,?,?)`,
			id, status, id, now, now, `{"id":"`+id+`","status":"`+status+`","result":{"candidate":{"team":`+connectOnceTeamDocument("sha256:stale", "")+`}}}`)
	}
	changeSet("cs-open", "proposed")
	changeSet("cs-applied", "applied")

	// Step 4: gateways that 73 marked as Agent-owned.
	gateways := NewExternalConversationGatewayService(store)
	for _, deployment := range []string{"crew", "ghost"} {
		gateway := externalConversationGatewayFixture(scope)
		gateway.DeploymentID, gateway.Owner = deployment, ObjectiveOwner{Type: OwnerTypeAgent, ID: deployment}
		if _, err := gateways.Create(ctx, CreateExternalConversationGatewayRequest{ID: "gateway-" + deployment, Name: deployment, Gateway: gateway,
			Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create"}); err != nil {
			t.Fatal(err)
		}
	}
	h.exec(t, `INSERT INTO @deployment_identities (scope_kind, scope_id, id, kind) VALUES ('tenant','one','crew','team')`)

	// Step 5: routines without a recorded creator.
	h.exec(t, `INSERT INTO @conversations (scope_kind, scope_id, id, owner_type, owner_id, status, idempotency_key, last_sequence, revision, created_at, updated_at, payload, space_id) VALUES ('tenant','one','chat','team','crew','active','chat',4,1,?,?,?,'personal')`,
		now, now, `{"scope":{"kind":"tenant","id":"one"},"id":"chat","owner":{"type":"team","id":"crew"}}`)
	message := func(id string, sequence int, sender, initiator string) {
		parts := strings.SplitN(sender, ":", 2)
		payload := `{"scope":{"kind":"tenant","id":"one"},"conversationId":"chat","id":"` + id + `","sender":{"type":"` + parts[0] + `","id":"` + parts[1] + `"}`
		if initiator != "" {
			payload += `,"initiator":{"type":"user","id":"` + initiator + `"}`
		}
		h.exec(t, `INSERT INTO @channel_messages (scope_kind, scope_id, conversation_id, id, sequence, sender_type, sender_id, intent, idempotency_key, created_at, payload) VALUES ('tenant','one','chat',?,?,?,?,'message',?,?,?)`,
			id, sequence, parts[0], parts[1], id, now, payload+`}`)
	}
	message("msg-person", 1, "user:7", "")
	message("msg-voice", 2, "service:voice", "9")
	message("msg-agent", 3, "agent:member", "")
	message("msg-handoff-source", 4, "user:11", "")
	source := &AgentRun{ID: "source-run", Scope: scope, RootRunID: "source-run", Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "lead",
		Goal: "Answer", Source: RunSourceManual, Status: AgentRunStatusQueued, Revision: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now, QueueEnteredAt: now,
		Context: map[string]interface{}{conversationRunContextConversationID: "chat", conversationRunContextTriggerID: "msg-handoff-source"}}
	if err := store.CreateAgentRun(ctx, source); err != nil {
		t.Fatal(err)
	}
	h.exec(t, `INSERT INTO @agent_requests (scope_kind, scope_id, id, kind, status, requester_type, requester_id, recipient_type, recipient_id, source_run_id, revision, created_at, updated_at, payload) VALUES ('tenant','one','request-1','handoff','accepted','agent','lead','agent','member','source-run',1,?,?,?)`,
		now, now, `{"id":"request-1","sourceRunId":"source-run"}`)
	routine := func(id, task string) {
		payload := `{"id":"` + id + `","scope":{"kind":"tenant","id":"one"}`
		if task != "" {
			payload += `,"task":` + task
		}
		h.exec(t, `INSERT INTO @runbook_activations (id, scope_kind, scope_id, owner_type, owner_id, objective_id, assigned_agent_id, status, revision, updated_at, payload) VALUES (?,'tenant','one','team','crew','objective','member','active',1,?,?)`,
			id, now, payload+`}`)
	}
	routine("routine-person", `{"goal":"g","conversationId":"chat","sourceMessageId":"msg-person"}`)
	routine("routine-voice", `{"goal":"g","conversationId":"chat","sourceMessageId":"msg-voice"}`)
	routine("routine-agent", `{"goal":"g","conversationId":"chat","sourceMessageId":"msg-agent"}`)
	routine("routine-handoff", `{"goal":"g","conversationId":"chat","sourceMessageId":"request-1"}`)
	routine("routine-missing", `{"goal":"g","conversationId":"chat","sourceMessageId":"gone"}`)
	routine("routine-stamped", `{"goal":"g","conversationId":"chat","sourceMessageId":"msg-person","createdBy":"5"}`)
	routine("routine-untasked", "")
}

func verifyConnectOnceMigration(t *testing.T, h connectOnceMigrationHarness) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}

	// Step 1.
	if wait, err := h.store().GetRunEventWait(ctx, scope, "waiting", "reply"); err != nil || wait.UmbrellaID != "member" {
		t.Fatalf("binding wait = %#v, %v; want umbrella member", wait, err)
	}
	if wait, err := h.store().GetRunEventWait(ctx, scope, "unverifiable", "reply"); err != nil || wait.UmbrellaID != "crew" {
		t.Fatalf("unverifiable wait = %#v, %v; want it left alone", wait, err)
	}

	// Step 2.
	call := h.payload(t, "action_calls", `id = 'call-1'`)
	if owner := pathValue(call, "output", "setupRequest", "owner"); fmt.Sprint(owner) != fmt.Sprint(map[string]interface{}{"type": "team", "id": "crew"}) ||
		pathValue(call, "output", "setupRequest", "deploymentId") != "crew" || pathValue(call, "output", "setupRequest", "agentId") != "member" ||
		pathValue(call, "output", "setupRequest", "kind") != "setup" {
		t.Fatalf("setup copy = %#v", call["output"])
	}

	// Step 3.
	open := h.payload(t, "team_definition_amendments", `id = 'amend-open'`)
	candidate := open["candidate"].(map[string]interface{})
	encoded, _ := json.Marshal(candidate)
	var typed kernelteam.Definition
	if err := json.Unmarshal(encoded, &typed); err != nil {
		t.Fatal(err)
	}
	if open["baseDigest"] != "sha256:base" || pathValue(candidate, "provenance", "derivedFrom") != "sha256:base" ||
		candidate["digest"] != kernelteam.DefinitionDigest(&typed) ||
		open["requestDigest"] != kernelteam.AmendmentRequestDigest("crew", "sha256:base", kernelteam.DefinitionDigest(&typed), "agent", "member", "Sharper purpose", []string{"run:1"}) {
		t.Fatalf("open amendment = %#v", open)
	}
	orphan := h.payload(t, "team_definition_amendments", `id = 'amend-orphan'`)
	if orphan["baseDigest"] != "sha256:stale" || pathValue(orphan, "candidate", "digest") == "sha256:stale" || orphan["requestDigest"] == "sha256:stale" {
		t.Fatalf("amendment without a stored base = %#v", orphan)
	}
	if done := h.payload(t, "team_definition_amendments", `id = 'amend-done'`); done["requestDigest"] != "sha256:stale" {
		t.Fatalf("activated amendment rewritten: %#v", done)
	}
	team := pathValue(h.payload(t, "workforce_change_sets", `id = 'cs-open'`), "result", "candidate", "team").(map[string]interface{})
	encoded, _ = json.Marshal(team)
	typed = kernelteam.Definition{}
	if err := json.Unmarshal(encoded, &typed); err != nil {
		t.Fatal(err)
	}
	if team["digest"] != kernelteam.DefinitionDigest(&typed) || team["digest"] == "sha256:stale" {
		t.Fatalf("open change set Team = %#v", team)
	}
	if applied := pathValue(h.payload(t, "workforce_change_sets", `id = 'cs-applied'`), "result", "candidate", "team", "digest"); applied != "sha256:stale" {
		t.Fatalf("applied change set rewritten: %v", applied)
	}

	// Step 4.
	if gateway, err := h.store().GetExternalConversationGateway(ctx, scope, "gateway-crew"); err != nil || gateway.Gateway.Owner != (ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}) {
		t.Fatalf("Team gateway = %#v, %v", gateway, err)
	}
	if gateway, err := h.store().GetExternalConversationGateway(ctx, scope, "gateway-ghost"); err != nil || gateway.Gateway.Owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: "ghost"}) {
		t.Fatalf("gateway without an identity = %#v, %v", gateway, err)
	}

	// Step 5.
	for id, want := range map[string]interface{}{
		"routine-person": "7", "routine-voice": "9", "routine-handoff": "11", "routine-stamped": "5",
		"routine-agent": nil, "routine-missing": nil,
	} {
		if got := pathValue(h.payload(t, "runbook_activations", `id = ?`, id), "task", "createdBy"); got != want {
			t.Fatalf("%s createdBy = %v, want %v", id, got, want)
		}
	}
	if task := h.payload(t, "runbook_activations", `id = 'routine-untasked'`)["task"]; task != nil {
		t.Fatalf("routine without a task gained one: %#v", task)
	}
}

// matchRepairedBindingWait shows the repaired wait is woken by its binding.
func matchRepairedBindingWait(t *testing.T, h connectOnceMigrationHarness) {
	t.Helper()
	ctx := context.Background()
	scope := Scope{Kind: "tenant", ID: "one"}
	store := h.store()
	event := EventEnvelope{ID: "reply-1", Scope: scope, Type: "conversation.message.received", Subject: "C1", OccurredAt: connectOnceMigrationNow,
		Source:     RunEventBindingSource("member", "slack", "conversations"),
		Attributes: map[string]interface{}{"deploymentId": "member", "bindingId": "slack", "adapterId": "conversations"},
		Payload:    map[string]interface{}{"text": "Done"}}
	digest, err := runEventObservationDigest(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishRunEvent(ctx, &RunEventReceipt{Event: event, ReceivedAt: connectOnceMigrationNow, Digest: digest}); err != nil {
		t.Fatal(err)
	}
	waits, err := store.ClaimRunEventWaits(ctx, ClaimRunEventWaitsRequest{Scope: scope, WorkerID: "worker", Now: connectOnceMigrationNow, LeaseDuration: time.Minute, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, wait := range waits {
		if wait.RunID != "waiting" {
			continue
		}
		result, err := store.ProcessRunEventWait(ctx, ProcessRunEventWaitRequest{Scope: scope, RunID: wait.RunID, Key: wait.Spec.Key, WorkerID: "worker", Now: connectOnceMigrationNow, LeaseExpiresAt: *wait.LeaseExpiresAt})
		if err != nil {
			t.Fatal(err)
		}
		if result.Wait.Status != RunEventWaitMatched || result.Event == nil || result.Event.ID != "reply-1" {
			t.Fatalf("repaired wait result = %#v", result)
		}
		return
	}
	t.Fatalf("repaired wait was not claimed: %#v", waits)
}

func runConnectOnceMigrationCase(t *testing.T, h connectOnceMigrationHarness, reopen func(t *testing.T)) {
	seedConnectOnceMigration(t, h)
	h.rerun(t)
	reopen(t)
	verifyConnectOnceMigration(t, h)
	before := h.payloads(t)
	h.rerun(t)
	reopen(t)
	verifyConnectOnceMigration(t, h)
	after := h.payloads(t)
	for key, value := range before {
		if after[key] != value {
			t.Fatalf("replay changed %s:\n%s\n%s", key, value, after[key])
		}
	}
	matchRepairedBindingWait(t, h)
}

func TestSQLiteConnectOnceMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connect-once.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h := connectOnceMigrationHarness{
		sqlite: true,
		db:     func() *sql.DB { return store.db },
		table:  func(name string) string { return name },
		store:  func() connectOnceMigrationStore { return store },
		rerun:  func(*testing.T) {},
	}
	runConnectOnceMigrationCase(t, h, func(t *testing.T) {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err = NewSQLiteStore(path); err != nil {
			t.Fatal(err)
		}
	})
}
