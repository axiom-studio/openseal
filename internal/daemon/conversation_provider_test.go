package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func TestDesktopParticipationProviderBoundsRepliesAndRetainsUsage(t *testing.T) {
	for _, mode := range []string{"reply", "quiet", "invalid-form", "incomplete", "missing-usage", "negative-usage", "overage", "http-error", "redirect", "removed-member", "oversized-trigger", "history-trim"} {
		t.Run(mode, func(t *testing.T) {
			catalog, query := participationFixture()
			source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 2)
			if err != nil {
				t.Fatal(err)
			}
			var calls, redirects atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirects.Add(1) }))
			defer other.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Authorization") != "Bearer synthetic-private-key" {
					t.Error("missing authorization")
				}
				var payload struct {
					MaxTokens int `json:"max_completion_tokens"`
					Messages  []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
					t.Error(err)
				}
				if payload.MaxTokens != 100 {
					t.Errorf("output allowance not enforced: %d", payload.MaxTokens)
				}
				if len(payload.Messages) != 2 || strings.Contains(payload.Messages[1].Content, "synthetic-private-key") {
					t.Error("invalid or credential-bearing context")
				}
				if mode == "history-trim" {
					var input struct {
						Omitted int `json:"olderMessagesOmitted"`
						Trigger struct {
							Content string `json:"content"`
						} `json:"trigger"`
					}
					if err := json.Unmarshal([]byte(payload.Messages[1].Content), &input); err != nil {
						t.Error(err)
					}
					if input.Omitted == 0 || input.Trigger.Content != "Review the evidence" {
						t.Errorf("history trimming lost trigger or disclosure: %#v", input)
					}
				}
				if mode == "http-error" {
					w.WriteHeader(429)
					w.Write([]byte("synthetic-private-key"))
					return
				}
				if mode == "redirect" {
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
					return
				}
				form := map[string]interface{}{"wantsToSpeak": true, "content": "The evidence is incomplete; verify the source.", "roleRelevant": true, "hasNewInformation": true}
				if mode == "quiet" {
					form["wantsToSpeak"] = false
					form["content"] = ""
				}
				if mode == "invalid-form" {
					form["proposedAction"] = map[string]string{"name": "unoffered"}
				}
				args, _ := json.Marshal(form)
				finish := "tool_calls"
				if mode == "incomplete" {
					finish = "length"
				}
				envelope := map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": finish, "message": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"type": "function", "function": map[string]interface{}{"name": "submit_channel_contribution", "arguments": string(args)}}}}}}, "usage": map[string]int{"prompt_tokens": 40, "completion_tokens": 20}}
				if mode == "missing-usage" {
					delete(envelope, "usage")
				}
				if mode == "negative-usage" {
					envelope["usage"] = map[string]int{"prompt_tokens": -1, "completion_tokens": 20}
				}
				if mode == "overage" {
					envelope["usage"] = map[string]int{"prompt_tokens": 40, "completion_tokens": 101}
				}
				json.NewEncoder(w).Encode(envelope)
			}))
			defer server.Close()
			host, err := NewProviderTurnHost(server.URL, "synthetic-private-key", "synthetic-model")
			if err != nil {
				t.Fatal(err)
			}
			provider, err := NewDesktopParticipationProvider(host, source)
			if err != nil {
				t.Fatal(err)
			}
			trigger := &runtime.ChannelMessage{ID: "trigger", Scope: query.Conversation.Scope, ConversationID: query.Conversation.ID, Intent: runtime.MessageIntentQuestion, Content: "Review the evidence", Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceParticipants, Participants: []runtime.ConversationParticipant{{Type: runtime.ConversationParticipantAgent, ID: "a"}}}}
			input := runtime.ParticipationProposalContext{Conversation: query.Conversation, Trigger: trigger, Participant: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "a"}, SemanticRoles: []string{"a"}, Budget: &runtime.ParticipationProposalBudget{InputTokens: 10000, OutputTokens: 100}}
			if mode == "oversized-trigger" {
				trigger.Content = strings.Repeat("x", 20000)
			}
			if mode == "history-trim" {
				for range 50 {
					input.RecentMessages = append(input.RecentMessages, &runtime.ChannelMessage{Content: strings.Repeat("old context ", 100)})
				}
			}
			if mode == "removed-member" {
				catalog.onTeamRead = func(n int) {
					if n == 2 {
						catalog.agents["a"].RolloutStatus = agent.RolloutPaused
					}
				}
			}
			result, err := provider.ProposeParticipationWithUsage(t.Context(), input)
			success := mode == "reply" || mode == "quiet" || mode == "missing-usage" || mode == "history-trim"
			if success && err != nil || !success && err == nil {
				t.Fatalf("unexpected result: %#v %v", result, err)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-private-key") {
				t.Fatal("private provider error leaked")
			}
			if mode == "oversized-trigger" {
				if calls.Load() != 0 || result.Usage != (runtime.TurnUsage{}) {
					t.Fatal("oversized context reached provider or was charged")
				}
				return
			}
			if calls.Load() != 1 || redirects.Load() != 0 {
				t.Fatalf("unexpected HTTP calls: %d redirects=%d", calls.Load(), redirects.Load())
			}
			if result.Usage.InputTokens <= 0 || result.Usage.OutputTokens <= 0 {
				t.Fatalf("attempt lost usage: %#v", result.Usage)
			}
			if mode == "invalid-form" || mode == "incomplete" || mode == "removed-member" {
				if result.Usage.InputTokens != 40 || result.Usage.OutputTokens != 20 {
					t.Fatalf("failure lost measured usage: %#v", result.Usage)
				}
			}
			if mode == "missing-usage" && (result.Usage.OutputTokens != 100 || result.Usage.InputTokens <= 40 || result.Usage.InputTokens > 10000) {
				t.Fatalf("missing usage did not charge bounded estimate: %#v", result.Usage)
			}
			if mode == "quiet" {
				proposal := result.Proposal
				proposal.ID, proposal.RoundID = "proposal", "round"
				proposal.Participant = input.Participant
				if err := proposal.Validate(); err != nil {
					t.Fatalf("quiet proposal invalid: %v", err)
				}
			}
			if mode == "overage" && result.Usage.OutputTokens != 101 {
				t.Fatal("overage was hidden")
			}
			if success && mode != "quiet" {
				if result.Proposal.Audience.Kind != runtime.ConversationAudienceChannel || result.Proposal.BroadcastToChannel || result.Proposal.ReplyToMessageID != "trigger" || result.Proposal.ProposedAction != nil {
					t.Fatalf("reply escaped its audience or gained authority: %#v", result.Proposal)
				}
				if mode == "quiet" && result.Proposal.WantsToSpeak {
					t.Fatal("quiet proposal became a message")
				}
			} else if result.Proposal.WantsToSpeak {
				t.Fatal("failed proposal can publish")
			}
		})
	}
}

func TestDesktopParticipationProviderRequiresBudgetAndCanonicalIdentity(t *testing.T) {
	catalog, query := participationFixture()
	source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 2)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	host, err := NewProviderTurnHost(server.URL, "synthetic-key", "model")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewDesktopParticipationProvider(host, source)
	if err != nil {
		t.Fatal(err)
	}
	input := runtime.ParticipationProposalContext{Conversation: query.Conversation, Trigger: &runtime.ChannelMessage{ID: "trigger", Scope: query.Conversation.Scope, ConversationID: query.Conversation.ID}, Participant: runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "a"}, SemanticRoles: []string{"a"}}
	if _, err := provider.ProposeParticipationWithUsage(t.Context(), input); err == nil {
		t.Fatal("unbounded invocation accepted")
	}
	input.Budget = &runtime.ParticipationProposalBudget{InputTokens: 10000, OutputTokens: 100}
	input.SemanticRoles = []string{"forged"}
	if _, err := provider.ProposeParticipationWithUsage(t.Context(), input); err == nil {
		t.Fatal("forged role accepted")
	}
	if calls.Load() != 0 {
		t.Fatal("unauthorized request reached provider")
	}
}

func TestDesktopParticipationRoundPreservesPrivateThreadAndChargesRun(t *testing.T) {
	catalog, query := participationFixture()
	source, err := NewDesktopConversationParticipants(catalog, query.Conversation.Scope, 2)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		args := `{"wantsToSpeak":true,"content":"Review the source before drawing a conclusion.","roleRelevant":true,"hasNewInformation":true}`
		json.NewEncoder(w).Encode(map[string]interface{}{"choices": []interface{}{map[string]interface{}{"finish_reason": "tool_calls", "message": map[string]interface{}{"tool_calls": []interface{}{map[string]interface{}{"type": "function", "function": map[string]string{"name": "submit_channel_contribution", "arguments": args}}}}}}, "usage": map[string]int{"prompt_tokens": 40, "completion_tokens": 20}})
	}))
	defer server.Close()
	host, err := NewProviderTurnHost(server.URL, "synthetic-key", "model")
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewDesktopParticipationProvider(host, source)
	if err != nil {
		t.Fatal(err)
	}
	store := runtime.NewMemoryStore()
	service := runtime.NewConversationService(store)
	scope := query.Conversation.Scope
	channel, _, err := service.CreateConversation(t.Context(), runtime.CreateConversationRequest{Scope: scope, Owner: query.Conversation.Owner, Title: "Research", IdempotencyKey: "channel"})
	if err != nil {
		t.Fatal(err)
	}
	enabled := true
	channel, err = service.UpdateConversation(t.Context(), runtime.UpdateConversationRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, ParticipationEnabled: &enabled})
	if err != nil {
		t.Fatal(err)
	}
	user := runtime.ConversationParticipant{Type: runtime.ConversationParticipantUser, ID: "operator"}
	posted, err := service.PostChannelMessage(t.Context(), runtime.PostChannelMessageRequest{Scope: scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision, Sender: user, Intent: runtime.MessageIntentQuestion, Content: "Review the evidence privately", Audience: runtime.ConversationAudience{Kind: runtime.ConversationAudienceParticipants, Participants: []runtime.ConversationParticipant{{Type: runtime.ConversationParticipantAgent, ID: "a"}}}, RequiresResponse: true, IdempotencyKey: "private-question"})
	if err != nil {
		t.Fatal(err)
	}
	config := runtime.DefaultConversationCoordinatorConfig()
	config.MaximumParticipants = 2
	config.MaximumConcurrency = 2
	config.ProposalBudget = runtime.ParticipationProposalBudget{InputTokens: 10000, OutputTokens: 100}
	coordinator, err := runtime.NewConversationCoordinator(service, source, provider, config)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := runtime.NewConversationRunScheduler(store, store, runtime.ConversationRunSchedulerConfig{RequireParticipationOptIn: true, Budget: &runtime.BudgetPolicy{MaxTurns: 4, MaxTotalTokens: 100000}})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, channel.ID, posted.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := runtime.NewConversationRunTurnRunner(store, coordinator, runtime.ConversationRunTurnRunnerConfig{RequireParticipationOptIn: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runtime.NewTurnCoordinator(store, store, store).Advance(t.Context(), runtime.AdvanceAgentRunRequest{Scope: scope, RunID: scheduled.Run.ID, WorkerID: "worker"}, runner)
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.Status != runtime.AgentRunStatusCompleted || result.Run.BudgetUsage.InputTokens != 40 || result.Run.BudgetUsage.OutputTokens != 20 || calls.Load() != 1 {
		t.Fatalf("round not settled correctly: %#v calls=%d", result, calls.Load())
	}
	for _, viewer := range []struct {
		participant runtime.ConversationParticipant
		count       int
	}{
		{user, 2}, {runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "a"}, 2}, {runtime.ConversationParticipant{Type: runtime.ConversationParticipantAgent, ID: "z"}, 0},
	} {
		messages, err := service.ListChannelMessages(t.Context(), runtime.ChannelMessageFilter{Scope: scope, ConversationID: channel.ID, Viewer: &runtime.ConversationViewer{Participant: viewer.participant}})
		if err != nil || len(messages) != viewer.count {
			t.Fatalf("thread visibility for %v: count=%d want=%d err=%v", viewer.participant, len(messages), viewer.count, err)
		}
	}
}
