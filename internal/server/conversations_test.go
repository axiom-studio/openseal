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
	store := runtime.NewMemoryStore(100)
	server := NewServer(nil, nil, store, zap.NewNop().Sugar())
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
	for _, expected := range []string{`"team-channels"`, `"coordinate"`, `"presence"`, `"audit"`} {
		if !strings.Contains(capabilities.Body.String(), expected) {
			t.Fatalf("capabilities missing %s: %s", expected, capabilities.Body.String())
		}
	}
}
