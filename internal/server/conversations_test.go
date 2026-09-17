package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestTeamChannelAPIExposesOnlyDurableNaturalChannelFacts(t *testing.T) {
	store := runtime.NewMemoryStore()
	server := NewServer(store, zap.NewNop().Sugar())
	createBody := `{
		"scope":{"kind":"tenant","id":"one"},
		"owner":{"type":"team","id":"engineering"},
		"title":"Release coordination"
	}`
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations", createBody, "release-v1")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var conversation runtime.Conversation
	if err := json.NewDecoder(created.Body).Decode(&conversation); err != nil {
		t.Fatal(err)
	}
	replayed := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations", createBody, "release-v1")
	if replayed.Code != http.StatusOK {
		t.Fatalf("replay status = %d, body = %s", replayed.Code, replayed.Body.String())
	}
	listed := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/conversations?scopeKind=tenant&scopeId=one&ownerType=team&ownerId=engineering&status=active", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), conversation.ID) {
		t.Fatalf("list status = %d, body = %s", listed.Code, listed.Body.String())
	}

	questionBody := `{
		"scope":{"kind":"tenant","id":"one"},
		"expectedRevision":1,
		"sender":{"type":"user","id":"operator"},
		"intent":"question",
		"content":"Is it ready?",
		"audience":{"kind":"channel"},
		"requiresResponse":true,
		"references":[{"kind":"run","id":"release-run"}]
	}`
	questionResponse := performAgentRunRequest(t, server.Handler(), http.MethodPost,
		"/api/v1/conversations/"+conversation.ID+"/messages", questionBody, "question-v1")
	if questionResponse.Code != http.StatusCreated {
		t.Fatalf("question status = %d, body = %s", questionResponse.Code, questionResponse.Body.String())
	}
	var question runtime.ChannelMessageCommitResult
	if err := json.NewDecoder(questionResponse.Body).Decode(&question); err != nil {
		t.Fatal(err)
	}
	roundBody := `{
		"scope":{"kind":"tenant","id":"one"},
		"expectedRevision":2,
		"triggerMessageId":"` + question.Message.ID + `",
		"proposals":[
			{
				"participant":{"type":"agent","id":"developer"},
				"wantsToSpeak":true,
				"intent":"answer",
				"content":"It is ready and smoke evidence passed.",
				"audience":{"kind":"channel"},
				"replyToMessageId":"` + question.Message.ID + `",
				"resolvesMessageId":"` + question.Message.ID + `",
				"signals":{"answersOpenQuestion":true,"hasNewInformation":true,"roleRelevant":true,"hasEvidence":true}
			},
			{"participant":{"type":"agent","id":"quiet"},"wantsToSpeak":false}
		]
	}`
	roundResponse := performAgentRunRequest(t, server.Handler(), http.MethodPost,
		"/api/v1/conversations/"+conversation.ID+"/participation-rounds", roundBody, "round-v1")
	if roundResponse.Code != http.StatusCreated {
		t.Fatalf("round status = %d, body = %s", roundResponse.Code, roundResponse.Body.String())
	}
	var round runtime.ParticipationRoundResult
	if err := json.NewDecoder(roundResponse.Body).Decode(&round); err != nil {
		t.Fatal(err)
	}
	if len(round.Messages) != 1 || len(round.Round.Arbitration.Decisions) != 2 {
		t.Fatalf("round = %#v", round)
	}
	messages := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/conversations/"+conversation.ID+"/messages?scopeKind=tenant&scopeId=one", "", "")
	if messages.Code != http.StatusOK || !strings.Contains(messages.Body.String(), `"sequence":2`) {
		t.Fatalf("messages status = %d, body = %s", messages.Code, messages.Body.String())
	}
	audit := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/conversations/"+conversation.ID+"/participation-rounds/"+round.Round.ID+"?scopeKind=tenant&scopeId=one", "", "")
	if audit.Code != http.StatusOK || !strings.Contains(audit.Body.String(), `"requested_silence"`) {
		t.Fatalf("audit status = %d, body = %s", audit.Code, audit.Body.String())
	}
	changesResponse := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/conversations/"+conversation.ID+"/changes?scopeKind=tenant&scopeId=one", "", "")
	if changesResponse.Code != http.StatusOK {
		t.Fatalf("changes status = %d, body = %s", changesResponse.Code, changesResponse.Body.String())
	}
	var changes runtime.ConversationChangeSet
	if err := json.NewDecoder(changesResponse.Body).Decode(&changes); err != nil {
		t.Fatal(err)
	}
	if !changes.HasChanges || len(changes.Messages) != 2 || len(changes.Rounds) != 1 ||
		changes.Rounds[0].Round.ConversationRevision != 3 || changes.Cursor == "" {
		t.Fatalf("changes = %#v", changes)
	}
	unchangedResponse := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/conversations/"+conversation.ID+"/changes?scopeKind=tenant&scopeId=one&cursor="+changes.Cursor, "", "")
	if unchangedResponse.Code != http.StatusOK || !strings.Contains(unchangedResponse.Body.String(), `"hasChanges":false`) {
		t.Fatalf("unchanged status = %d, body = %s", unchangedResponse.Code, unchangedResponse.Body.String())
	}

	cursorBody := `{"scope":{"kind":"tenant","id":"one"},"participant":{"type":"agent","id":"developer"},"deliveredSequence":2,"readSequence":2}`
	cursor := performAgentRunRequest(t, server.Handler(), http.MethodPut,
		"/api/v1/conversations/"+conversation.ID+"/cursor", cursorBody, "")
	if cursor.Code != http.StatusOK || !strings.Contains(cursor.Body.String(), `"readSequence":2`) {
		t.Fatalf("cursor status = %d, body = %s", cursor.Code, cursor.Body.String())
	}
	presenceBody := `{"scope":{"kind":"tenant","id":"one"},"participant":{"type":"agent","id":"developer"},"state":"working","summary":"Following rollout","runId":"release-run","ttlSeconds":30}`
	presence := performAgentRunRequest(t, server.Handler(), http.MethodPut,
		"/api/v1/conversations/"+conversation.ID+"/presence", presenceBody, "")
	if presence.Code != http.StatusOK {
		t.Fatalf("presence status = %d, body = %s", presence.Code, presence.Body.String())
	}
	var lease runtime.ConversationPresence
	if err := json.NewDecoder(presence.Body).Decode(&lease); err != nil {
		t.Fatal(err)
	}
	presenceList := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/conversations/"+conversation.ID+"/presence?scopeKind=tenant&scopeId=one", "", "")
	if presenceList.Code != http.StatusOK || !strings.Contains(presenceList.Body.String(), `"working"`) {
		t.Fatalf("presence list status = %d, body = %s", presenceList.Code, presenceList.Body.String())
	}
	releaseBody := `{"scope":{"kind":"tenant","id":"one"},"participant":{"type":"agent","id":"developer"},"leaseId":"` + lease.LeaseID + `"}`
	released := performAgentRunRequest(t, server.Handler(), http.MethodDelete,
		"/api/v1/conversations/"+conversation.ID+"/presence", releaseBody, "")
	if released.Code != http.StatusNoContent {
		t.Fatalf("release status = %d, body = %s", released.Code, released.Body.String())
	}

	wrongScope := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/conversations/"+conversation.ID+"?scopeKind=tenant&scopeId=other", "", "")
	if wrongScope.Code != http.StatusNotFound {
		t.Fatalf("wrong scope status = %d, body = %s", wrongScope.Code, wrongScope.Body.String())
	}
	invalid := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations",
		strings.TrimSuffix(createBody, "}")+`,"fake":true}`, "invalid-v1")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status = %d, body = %s", invalid.Code, invalid.Body.String())
	}
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	for _, expected := range []string{`"channels"`, `"coordinate"`, `"presence"`, `"audit"`, `"changes"`} {
		if !strings.Contains(capabilities.Body.String(), expected) {
			t.Fatalf("capabilities missing %s: %s", expected, capabilities.Body.String())
		}
	}
}

func TestDesktopChannelAuthorshipAndHistoryWindow(t *testing.T) {
	store := runtime.NewMemoryStore()
	server := NewServer(store, zap.NewNop().Sugar())
	server.SetDesktopConversationScope(runtime.Scope{Kind: "local", ID: "default"})
	body := `{"scope":{"kind":"local","id":"default"},"owner":{"type":"team","id":"research"},"title":"Evidence"}`
	foreign := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations", strings.Replace(body, `"default"`, `"other"`, 1), "foreign")
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("foreign create: %d %s", foreign.Code, foreign.Body.String())
	}
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations", body, "create")
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	var conversation runtime.Conversation
	if err := json.NewDecoder(created.Body).Decode(&conversation); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/conversations/" + conversation.ID + "/messages"
	message := `{"scope":{"kind":"local","id":"default"},"expectedRevision":1,"sender":{"type":"user","id":"local-operator"},"intent":"update","content":"First note","audience":{"kind":"channel"}}`
	forged := performAgentRunRequest(t, server.Handler(), http.MethodPost, path, strings.Replace(message, `"type":"user"`, `"type":"agent"`, 1), "forged")
	if forged.Code != http.StatusForbidden {
		t.Fatalf("forged: %d", forged.Code)
	}
	foreignMessage := performAgentRunRequest(t, server.Handler(), http.MethodPost, path, strings.Replace(message, `"default"`, `"other"`, 1), "foreign-message")
	if foreignMessage.Code != http.StatusForbidden {
		t.Fatalf("foreign message: %d", foreignMessage.Code)
	}
	first := performAgentRunRequest(t, server.Handler(), http.MethodPost, path, message, "first")
	if first.Code != http.StatusCreated {
		t.Fatal(first.Body.String())
	}
	secondBody := strings.Replace(strings.Replace(message, `"expectedRevision":1`, `"expectedRevision":2`, 1), "First note", "Second note", 1)
	second := performAgentRunRequest(t, server.Handler(), http.MethodPost, path, secondBody, "second")
	if second.Code != http.StatusCreated {
		t.Fatal(second.Body.String())
	}
	replay := performAgentRunRequest(t, server.Handler(), http.MethodPost, path, message, "first")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"replayed":true`) {
		t.Fatal(replay.Body.String())
	}
	old := performAgentRunRequest(t, server.Handler(), http.MethodGet, path+"?scopeKind=local&scopeId=default&order=desc&beforeSequence=2&limit=1", "", "")
	if old.Code != http.StatusOK || !strings.Contains(old.Body.String(), "First note") || strings.Contains(old.Body.String(), "Second note") {
		t.Fatalf("history: %d %s", old.Code, old.Body.String())
	}
	invalid := performAgentRunRequest(t, server.Handler(), http.MethodGet, path+"?scopeKind=local&scopeId=default&beforeSequence=-1", "", "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor: %d", invalid.Code)
	}
}

func TestDesktopChannelLifecyclePreservesHistoryAndChecksRevision(t *testing.T) {
	server := NewServer(runtime.NewMemoryStore(), zap.NewNop().Sugar())
	server.SetDesktopConversationScope(runtime.Scope{Kind: "local", ID: "default"})
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations", `{"scope":{"kind":"local","id":"default"},"owner":{"type":"team","id":"research"},"title":"Evidence"}`, "lifecycle")
	var conversation runtime.Conversation
	if err := json.NewDecoder(created.Body).Decode(&conversation); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/conversations/" + conversation.ID
	message := `{"scope":{"kind":"local","id":"default"},"expectedRevision":1,"sender":{"type":"user","id":"local-operator"},"intent":"update","content":"Preserve this evidence","audience":{"kind":"channel"}}`
	posted := performAgentRunRequest(t, server.Handler(), http.MethodPost, path+"/messages", message, "evidence")
	if posted.Code != http.StatusCreated {
		t.Fatal(posted.Body.String())
	}
	update := `{"scope":{"kind":"local","id":"default"},"expectedRevision":2,"title":"July evidence"}`
	forbidden := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, strings.Replace(update, `"default"`, `"foreign"`, 1), "")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("scope: %d", forbidden.Code)
	}
	renamed := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, update, "")
	if renamed.Code != http.StatusOK || !strings.Contains(renamed.Body.String(), `"title":"July evidence"`) {
		t.Fatal(renamed.Body.String())
	}
	stale := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, update, "")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d", stale.Code)
	}
	invalid := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, `{"scope":{"kind":"local","id":"default"},"expectedRevision":3,"status":"deleted"}`, "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status: %d", invalid.Code)
	}
	archive := `{"scope":{"kind":"local","id":"default"},"expectedRevision":3,"status":"archived"}`
	archived := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, archive, "")
	if archived.Code != http.StatusOK || !strings.Contains(archived.Body.String(), `"archivedAt"`) {
		t.Fatal(archived.Body.String())
	}
	denied := performAgentRunRequest(t, server.Handler(), http.MethodPost, path+"/messages", strings.Replace(message, `"expectedRevision":1`, `"expectedRevision":4`, 1), "new-post")
	if denied.Code != http.StatusBadRequest {
		t.Fatalf("archived post: %d", denied.Code)
	}
	replay := performAgentRunRequest(t, server.Handler(), http.MethodPost, path+"/messages", message, "evidence")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), `"replayed":true`) {
		t.Fatal(replay.Body.String())
	}
	history := performAgentRunRequest(t, server.Handler(), http.MethodGet, path+"/messages?scopeKind=local&scopeId=default", "", "")
	if history.Code != http.StatusOK || !strings.Contains(history.Body.String(), "Preserve this evidence") {
		t.Fatal(history.Body.String())
	}
	restored := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, `{"scope":{"kind":"local","id":"default"},"expectedRevision":4,"status":"active"}`, "")
	if restored.Code != http.StatusOK || strings.Contains(restored.Body.String(), `"archivedAt"`) {
		t.Fatal(restored.Body.String())
	}
	after := performAgentRunRequest(t, server.Handler(), http.MethodPost, path+"/messages", strings.Replace(message, `"expectedRevision":1`, `"expectedRevision":5`, 1), "restored-post")
	if after.Code != http.StatusCreated {
		t.Fatal(after.Body.String())
	}
	empty := performAgentRunRequest(t, server.Handler(), http.MethodPatch, path, `{"scope":{"kind":"local","id":"default"},"expectedRevision":6}`, "")
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty patch: %d", empty.Code)
	}
}

func TestDesktopReadPositionsBindLocalReaderAndPreserveMonotonicHistory(t *testing.T) {
	server := NewServer(runtime.NewMemoryStore(), zap.NewNop().Sugar())
	server.SetDesktopConversationScope(runtime.Scope{Kind: "local", ID: "default"})
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/conversations", `{"scope":{"kind":"local","id":"default"},"owner":{"type":"team","id":"research"},"title":"Reading evidence"}`, "read-channel")
	var channel runtime.Conversation
	if err := json.NewDecoder(created.Body).Decode(&channel); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/conversations/" + channel.ID
	posted := performAgentRunRequest(t, server.Handler(), http.MethodPost, path+"/messages", `{"scope":{"kind":"local","id":"default"},"expectedRevision":1,"sender":{"type":"user","id":"local-operator"},"intent":"update","content":"Evidence to read","audience":{"kind":"channel"}}`, "read-note")
	if posted.Code != http.StatusCreated {
		t.Fatal(posted.Body.String())
	}
	body := `{"scope":{"kind":"local","id":"default"},"participant":{"type":"user","id":"local-operator"},"deliveredSequence":1,"readSequence":1}`
	for _, invalid := range []string{strings.Replace(body, `"user"`, `"agent"`, 1), strings.Replace(body, `"local-operator"`, `"other-user"`, 1), strings.Replace(body, `"default"`, `"foreign"`, 1)} {
		response := performAgentRunRequest(t, server.Handler(), http.MethodPut, path+"/cursor", invalid, "")
		if response.Code != http.StatusForbidden {
			t.Fatalf("forged receipt: %d %s", response.Code, response.Body.String())
		}
	}
	saved := performAgentRunRequest(t, server.Handler(), http.MethodPut, path+"/cursor", body, "")
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"readSequence":1`) {
		t.Fatal(saved.Body.String())
	}
	stale := performAgentRunRequest(t, server.Handler(), http.MethodPut, path+"/cursor", body, "")
	if stale.Code != http.StatusConflict {
		t.Fatalf("stale revision: %d", stale.Code)
	}
	regression := strings.Replace(strings.Replace(body, `"readSequence":1`, `"readSequence":0`, 1), `"deliveredSequence":1`, `"expectedRevision":1,"deliveredSequence":1`, 1)
	denied := performAgentRunRequest(t, server.Handler(), http.MethodPut, path+"/cursor", regression, "")
	if denied.Code != http.StatusConflict {
		t.Fatalf("regression: %d", denied.Code)
	}
	restored := performAgentRunRequest(t, server.Handler(), http.MethodGet, path+"/cursor?scopeKind=local&scopeId=default&participantType=user&participantId=local-operator", "", "")
	if restored.Code != http.StatusOK || !strings.Contains(restored.Body.String(), `"readSequence":1`) {
		t.Fatal(restored.Body.String())
	}
	caps := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if !strings.Contains(caps.Body.String(), `"receipts"`) {
		t.Fatal("durable receipts not advertised")
	}
}
