package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type countedConversationBackdropStore struct {
	*countedPointConversationStore
	lists   int
	filters []ChannelMessageFilter
}

func (s *countedConversationBackdropStore) ListChannelMessages(ctx context.Context, filter ChannelMessageFilter) ([]*ChannelMessage, error) {
	s.lists++
	s.filters = append(s.filters, filter)
	return s.ConversationStore.ListChannelMessages(ctx, filter)
}

func TestConversationThreadBackdropDoesNotSeekPastHiddenPage(t *testing.T) {
	for _, replyTo := range []bool{false, true} {
		t.Run(fmt.Sprintf("replyTo=%t", replyTo), func(t *testing.T) {
			store := NewMemoryStore()
			service := NewConversationService(store)
			conversation := conversationThreadStartConversation(t, store)
			older := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
				IdempotencyKey: "older-authorized", Content: "AUTHORIZED_OLDER_CONTEXT", Intent: MessageIntentUpdate,
			})
			parentID := ""
			if replyTo {
				parentID = older.ID
			}
			for index := 0; index < 24; index++ {
				conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
					IdempotencyKey: fmt.Sprintf("hidden-%02d", index), Intent: MessageIntentUpdate,
					Content: "HIDDEN_CONTEXT_MUST_NOT_APPEAR", ReplyToMessageID: parentID,
					Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "private-user"}}},
				})
			}
			root := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
				IdempotencyKey: "new-call-root", StartThread: true, ReplyToMessageID: parentID,
			})
			counted := &countedConversationBackdropStore{countedPointConversationStore: &countedPointConversationStore{ConversationStore: store}}
			viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}}
			backdrop, err := loadConversationThreadBackdrop(t.Context(), NewConversationService(counted), conversation, root, viewer)
			if err != nil || counted.lists != 1 || len(counted.filters) != 1 {
				t.Fatalf("backdrop sought past its first hidden page: lists=%d filters=%#v err=%v", counted.lists, counted.filters, err)
			}
			filter := counted.filters[0]
			if filter.Scope != conversation.Scope || filter.ConversationID != conversation.ID || filter.Limit != 16 || !filter.Descending || filter.BeforeSequence != root.Sequence || filter.ThreadRootID != parentID || filter.ChannelTimeline == replyTo {
				t.Fatalf("backdrop did not issue one bounded pre-start page: %#v", filter)
			}
			if replyTo {
				if len(backdrop) != 1 || backdrop[0].ID != older.ID || backdrop[0].Content != older.Content {
					t.Fatalf("exact authorized parent outside the page was lost: %#v", backdrop)
				}
			} else if len(backdrop) != 0 {
				t.Fatalf("backdrop sought older accessible history to fill a hidden page: %#v", backdrop)
			}
			for _, message := range backdrop {
				if strings.Contains(message.Content, "HIDDEN_CONTEXT_MUST_NOT_APPEAR") {
					t.Fatal("bounded raw page bypassed viewer authorization")
				}
			}
		})
	}
}

func TestConversationThreadBackdropIntersectsAgentAndCanonicalCallerVisibility(t *testing.T) {
	for _, replyTo := range []bool{false, true} {
		for _, serviceRoot := range []bool{false, true} {
			t.Run(fmt.Sprintf("replyTo=%t/serviceRoot=%t", replyTo, serviceRoot), func(t *testing.T) {
				store := NewMemoryStore()
				service := NewConversationService(store)
				conversation := conversationThreadStartConversation(t, store)
				agent := ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}
				caller := ConversationParticipant{Type: ConversationParticipantUser, ID: "current-caller"}
				otherUser := ConversationParticipant{Type: ConversationParticipantUser, ID: "other-user"}
				parentID := ""
				if replyTo {
					parent := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
						IdempotencyKey: "authorized-parent", Content: "AUTHORIZED_PARENT", Intent: MessageIntentUpdate,
						Sender: otherUser, Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{agent, caller}},
					})
					parentID = parent.ID
				}
				secret := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
					IdempotencyKey: "agent-only-private", Content: "OTHER_USER_AGENT_ONLY_SECRET_MUST_NOT_APPEAR", Intent: MessageIntentUpdate,
					Sender: otherUser, ReplyToMessageID: parentID,
					Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{agent}},
				})
				shared := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
					IdempotencyKey: "caller-and-agent", Content: "AUTHORIZED_CALLER_AND_AGENT_CONTEXT", Intent: MessageIntentUpdate,
					Sender: otherUser, ReplyToMessageID: parentID,
					Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{agent, caller}},
				})
				rootRequest := PostChannelMessageRequest{IdempotencyKey: "new-call-root", StartThread: true, ReplyToMessageID: parentID, RequiresResponse: true, Sender: caller}
				if serviceRoot {
					rootRequest.Sender = ConversationParticipant{Type: ConversationParticipantService, ID: "calls"}
					rootRequest.Initiator = &caller
				}
				root := conversationThreadStartPost(t, service, conversation, rootRequest)
				agentViewer := ConversationViewer{Participant: agent}
				if visible, err := service.GetVisibleChannelMessage(t.Context(), conversation.Scope, conversation.ID, secret.ID, agentViewer); err != nil || visible == nil {
					t.Fatalf("fixture did not grant the shared agent access: %#v, %v", visible, err)
				}
				callerViewer := ConversationViewer{Participant: caller}
				if _, err := service.GetVisibleChannelMessage(t.Context(), conversation.Scope, conversation.ID, secret.ID, callerViewer); err == nil {
					t.Fatal("fixture unexpectedly granted another user's private discussion to the caller")
				}
				backdrop, err := loadConversationThreadBackdrop(t.Context(), service, conversation, root, agentViewer)
				expected := []string{shared.ID}
				if replyTo {
					expected = append([]string{parentID}, expected...)
				}
				if err != nil || !reflect.DeepEqual(conversationThreadStartMessageIDs(backdrop), expected) {
					t.Fatalf("agent-only access widened the caller's backdrop: ids=%v expected=%v err=%v", conversationThreadStartMessageIDs(backdrop), expected, err)
				}
				captured := false
				resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
					return &TurnRunnerBinding{DeploymentID: agent.ID, Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
						captured = true
						if strings.Contains(input.Run.Goal, secret.Content) || !strings.Contains(input.Run.Goal, shared.Content) {
							t.Fatal("hosted prompt disclosed agent-only context or dropped context authorized to both viewers")
						}
						return &TurnOutcome{NextRunStatus: AgentRunStatusRunning}, nil
					})}, nil
				})
				runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
				if err != nil {
					t.Fatal(err)
				}
				scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), conversation.Scope, conversation.ID, root.ID)
				if err != nil {
					t.Fatal(err)
				}
				// Mutable run context cannot substitute a different backdrop caller.
				scheduled.Run.Context["modelContext"] = map[string]interface{}{"userId": otherUser.ID}
				binding, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run}); err != nil || !captured {
					t.Fatalf("capture canonical caller prompt: captured=%t err=%v", captured, err)
				}
				privateParent := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
					IdempotencyKey: "private-exact-parent", Content: "OTHER_USER_PRIVATE_EXACT_PARENT_MUST_NOT_APPEAR", Intent: MessageIntentUpdate,
					Sender: otherUser, Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{agent}},
				})
				privateRootRequest := rootRequest
				privateRootRequest.IdempotencyKey, privateRootRequest.ReplyToMessageID = "private-parent-call-root", privateParent.ID
				privateRoot := conversationThreadStartPost(t, service, conversation, privateRootRequest)
				privateBackdrop, err := loadConversationThreadBackdrop(t.Context(), service, conversation, privateRoot, agentViewer)
				if err != nil || len(privateBackdrop) != 0 {
					t.Fatalf("exact parent bypassed canonical caller visibility: backdrop=%#v err=%v", privateBackdrop, err)
				}
			})
		}
	}
}

func conversationThreadBackdropFixture(t *testing.T, service *ConversationService, conversation *Conversation, replyTo bool) (*ChannelMessage, []*ChannelMessage) {
	t.Helper()
	otherRoot := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "other-root", Intent: MessageIntentSystem})
	own := make([]*ChannelMessage, 0, 11)
	parent := ""
	for index := 1; index <= 11; index++ {
		key := fmt.Sprintf("own-context-%02d", index)
		content := "AUTHORIZED_" + key
		if index == 11 {
			content += strings.Repeat("日本語", 200)
		}
		message := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: key, Intent: MessageIntentUpdate, Content: content, ReplyToMessageID: parent,
		})
		own = append(own, message)
		if replyTo && parent == "" {
			parent = message.ID
		}
	}
	conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
		IdempotencyKey: "inaccessible-private", Intent: MessageIntentUpdate, Content: "PRIVATE_CONTEXT_MUST_NOT_APPEAR",
		Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "private-user"}}},
	})
	conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
		IdempotencyKey: "other-thread-reply", ReplyToMessageID: otherRoot.ID, Content: "OTHER_THREAD_CONTEXT_MUST_NOT_APPEAR", Intent: MessageIntentUpdate,
	})
	rootReply := ""
	expected := own[len(own)-8:]
	if replyTo {
		// Pin an older exact parent while retaining recent context from its
		// thread. The parent must survive the eight-message excerpt bound.
		rootReply = own[0].ID
		expected = append([]*ChannelMessage{own[0]}, own[len(own)-7:]...)
	}
	root := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
		IdempotencyKey: "call-root", StartThread: true, ReplyToMessageID: rootReply, RequiresResponse: true, Content: "CALL_ROOT_ONLY",
	})
	return root, expected
}

func TestConversationThreadBackdropIsBoundedAuthorizedAndStable(t *testing.T) {
	for _, replyTo := range []bool{false, true} {
		t.Run(fmt.Sprintf("replyTo=%t", replyTo), func(t *testing.T) {
			forConversationThreadStartStores(t, func(t *testing.T, store conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
				conversation := conversationThreadStartConversation(t, store)
				service := NewConversationService(store)
				root, expected := conversationThreadBackdropFixture(t, service, conversation, replyTo)
				viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}}
				backdrop, err := loadConversationThreadBackdrop(t.Context(), service, conversation, root, viewer)
				if err != nil || !reflect.DeepEqual(conversationThreadStartMessageIDs(backdrop), conversationThreadStartMessageIDs(expected)) {
					t.Fatalf("backdrop did not select its latest eight authorized originals: ids=%v expected=%v err=%v", conversationThreadStartMessageIDs(backdrop), conversationThreadStartMessageIDs(expected), err)
				}
				for _, message := range backdrop {
					if message.Sequence >= root.Sequence || len(message.Content) > 1024 || !utf8.ValidString(message.Content) {
						t.Fatalf("backdrop was not bounded pre-start UTF-8 context: %#v", message)
					}
				}
				longOriginal, err := service.GetChannelMessage(t.Context(), conversation.Scope, conversation.ID, expected[len(expected)-1].ID)
				if err != nil || len(longOriginal.Content) <= 1024 || !strings.HasPrefix(longOriginal.Content, backdrop[len(backdrop)-1].Content) {
					t.Fatalf("prompt excerpt rewrote the durable original: %#v, %v", longOriginal, err)
				}
				conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "later-main", Content: "LATER_MAIN_MUST_NOT_APPEAR", Intent: MessageIntentUpdate})
				utterance := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "future-call-utterance", ReplyToMessageID: root.ID, RequiresResponse: true})
				later, err := loadConversationThreadBackdrop(t.Context(), service, conversation, utterance, viewer)
				if err != nil || !reflect.DeepEqual(backdrop, later) {
					t.Fatalf("future call utterance moved the canonical pre-start cutoff: initial=%#v later=%#v err=%v", backdrop, later, err)
				}
				backdrop[0].Content = "changed returned excerpt"
				original, err := service.GetChannelMessage(t.Context(), conversation.Scope, conversation.ID, expected[0].ID)
				if err != nil || original.Content != expected[0].Content {
					t.Fatalf("returned backdrop shared mutable persistence state: %#v, %v", original, err)
				}
			})
		})
	}
}

func TestOrdinaryConversationHasNoThreadBackdropReads(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	conversation := conversationThreadStartConversation(t, store)
	root := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "ordinary-root"})
	reply := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "ordinary-reply", ReplyToMessageID: root.ID})
	counted := &countedConversationBackdropStore{countedPointConversationStore: &countedPointConversationStore{ConversationStore: store}}
	reader := NewConversationService(counted)
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}}
	backdrop, err := loadConversationThreadBackdrop(t.Context(), reader, conversation, root, viewer)
	if err != nil || len(backdrop) != 0 || counted.points != 0 || counted.lists != 0 {
		t.Fatalf("ordinary root acquired prompt backdrop reads: backdrop=%#v points=%d lists=%d err=%v", backdrop, counted.points, counted.lists, err)
	}
	// A canonical ordinary reply can require its root point read to determine
	// whether it belongs to a StartsThread root; it must never page old history.
	backdrop, err = loadConversationThreadBackdrop(t.Context(), reader, conversation, reply, viewer)
	if err != nil || len(backdrop) != 0 || counted.lists != 0 || counted.points > 1 {
		t.Fatalf("ordinary reply paged old conversation backdrop: backdrop=%#v points=%d lists=%d err=%v", backdrop, counted.points, counted.lists, err)
	}
}

func TestConversationThreadBackdropEntersOnlyHostedPrompt(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	conversation := conversationThreadStartConversation(t, store)
	root, expected := conversationThreadBackdropFixture(t, service, conversation, false)
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}}
	type prompt struct {
		Messages          []agentConversationPromptMessage `json:"messages"`
		ContextBackdrop   []agentConversationPromptMessage `json:"contextBackdrop"`
		HistorySummary    string                           `json:"historySummary"`
		HistoryCompaction *ConversationCompactionRequest   `json:"historyCompaction"`
	}
	captured := make([]prompt, 0, 2)
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: conversation.Owner.ID, Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
			for _, forbidden := range []string{"LATER_MAIN_MUST_NOT_APPEAR", "OTHER_THREAD_CONTEXT_MUST_NOT_APPEAR", "PRIVATE_CONTEXT_MUST_NOT_APPEAR", "CHANNEL_SUMMARY_MUST_NOT_APPEAR"} {
				if strings.Contains(input.Run.Goal, forbidden) {
					t.Fatalf("unrelated context entered rooted model goal: %s", forbidden)
				}
			}
			start := strings.Index(input.Run.Goal, "\n\n{")
			if start < 0 {
				t.Fatal("missing hosted prompt JSON")
			}
			var payload prompt
			if err := json.Unmarshal([]byte(input.Run.Goal[start+2:]), &payload); err != nil {
				t.Fatal(err)
			}
			if len(payload.ContextBackdrop) != 8 || payload.HistorySummary != "" || payload.HistoryCompaction != nil {
				t.Fatalf("rooted prompt backdrop entered transcript/compaction: %#v", payload)
			}
			for index, message := range payload.ContextBackdrop {
				if message.ID != expected[index].ID || !strings.HasPrefix(message.Content, "AUTHORIZED_") || len(message.Content) > 1024 || !utf8.ValidString(message.Content) {
					t.Fatalf("hosted backdrop lost authorized bounded original: %#v", message)
				}
			}
			for _, message := range payload.Messages {
				if strings.HasPrefix(message.Content, "AUTHORIZED_") {
					t.Fatal("pre-start backdrop became thread transcript")
				}
			}
			captured = append(captured, payload)
			return &TurnOutcome{NextRunStatus: AgentRunStatusRunning}, nil
		})}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
	if err != nil {
		t.Fatal(err)
	}
	runner.summaries.put(&conversationSummary{Scope: conversation.Scope, ConversationID: conversation.ID, ViewerKey: conversationViewerKey(viewer),
		ThroughSequence: expected[0].Sequence, Basis: strings.Repeat("a", 64), Text: "CHANNEL_SUMMARY_MUST_NOT_APPEAR"})
	scheduler := mustConversationRunScheduler(t, store)
	call := func(message *ChannelMessage) {
		t.Helper()
		scheduled, _, err := scheduler.ScheduleMessage(t.Context(), conversation.Scope, conversation.ID, message.ID)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run}); err != nil {
			t.Fatal(err)
		}
	}
	call(root)
	conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "later-main", Content: "LATER_MAIN_MUST_NOT_APPEAR", Intent: MessageIntentUpdate})
	utterance := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "future-call-utterance", ReplyToMessageID: root.ID, RequiresResponse: true})
	call(utterance)
	if len(captured) != 2 || !reflect.DeepEqual(captured[0].ContextBackdrop, captured[1].ContextBackdrop) {
		t.Fatal("future rooted message received a different pre-start backdrop")
	}
	history, err := ReadConversationHistory(t.Context(), service, conversation.Scope, conversation.Owner, conversation.ID, viewer, ConversationHistoryReadRequest{}, root.ID)
	if err != nil || !reflect.DeepEqual(conversationThreadStartHistoryIDs(history), []string{utterance.ID, root.ID}) {
		t.Fatalf("prompt backdrop widened builtin thread history: page=%#v err=%v", history, err)
	}
}
