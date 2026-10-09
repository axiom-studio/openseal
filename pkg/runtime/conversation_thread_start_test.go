package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

type conversationThreadStartTestStore interface {
	KernelStore
	ConversationStore
}

func forConversationThreadStartStores(t *testing.T, test func(*testing.T, conversationThreadStartTestStore, func(*testing.T) conversationThreadStartTestStore)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		store := NewMemoryStore()
		test(t, store, func(*testing.T) conversationThreadStartTestStore { return store })
	})
	forConversationTaskSQLStores(t, func(t *testing.T, fixture conversationTaskSQLTestFixture) {
		test(t, fixture.store.(conversationThreadStartTestStore), func(t *testing.T) conversationThreadStartTestStore {
			return fixture.reopen(t).(conversationThreadStartTestStore)
		})
	})
}

func conversationThreadStartConversation(t *testing.T, store ConversationStore) *Conversation {
	t.Helper()
	conversation, _, err := NewConversationService(store).CreateConversation(t.Context(), CreateConversationRequest{
		Scope: Scope{Kind: "tenant", ID: "thread-start"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Title: "Thread start contracts", IdempotencyKey: "thread-start",
	})
	if err != nil {
		t.Fatal(err)
	}
	return conversation
}

func conversationThreadStartPost(t *testing.T, service *ConversationService, conversation *Conversation, req PostChannelMessageRequest) *ChannelMessage {
	t.Helper()
	current, err := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	req.Scope, req.ConversationID, req.ExpectedRevision = conversation.Scope, conversation.ID, current.Revision
	if req.ID == "" {
		req.ID = req.IdempotencyKey
	}
	if req.Sender.Type == "" {
		req.Sender = ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}
	}
	if req.Intent == "" {
		req.Intent = MessageIntentQuestion
	}
	if req.Content == "" {
		req.Content = req.IdempotencyKey
	}
	if req.Audience.Kind == "" {
		req.Audience.Kind = ConversationAudienceChannel
	}
	result, err := service.PostChannelMessage(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	return result.Message
}

func TestConversationStartsThreadRootsKeepSeparateForegroundLanes(t *testing.T) {
	forConversationThreadStartStores(t, func(t *testing.T, store conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
		conversation := conversationThreadStartConversation(t, store)
		service := NewConversationService(store)
		scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
		if err != nil {
			t.Fatal(err)
		}
		schedule := func(message *ChannelMessage, thread string) *AgentRun {
			t.Helper()
			result, _, err := scheduler.ScheduleMessage(t.Context(), conversation.Scope, conversation.ID, message.ID)
			if err != nil || result == nil || result.Run == nil {
				t.Fatalf("schedule %s: %#v, %v", message.ID, result, err)
			}
			key := conversation.ID
			if thread != "" {
				key += ":thread:" + thread
			}
			actualThread, _ := result.Run.Context["threadRootMessageId"].(string)
			if result.Run.ConcurrencyKey != key || actualThread != thread {
				t.Fatalf("wrong foreground lane: %#v, expected thread %q", result.Run, thread)
			}
			return result.Run
		}
		ordinary := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "ordinary", RequiresResponse: true})
		ordinaryRun := schedule(ordinary, "")
		initiator := ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}
		rootOne := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: "call-one", StartThread: true, RequiresResponse: true,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID}, Initiator: &initiator,
			Intent: MessageIntentUpdate, ResponseMode: "spoken",
		})
		rootOneRun := schedule(rootOne, rootOne.ID)
		rootTwo := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: "call-two", StartThread: true, ReplyToMessageID: rootOne.ID, RequiresResponse: true,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: VoiceCallCoordinatorParticipantID}, Initiator: &initiator,
			Intent: MessageIntentUpdate, ResponseMode: "spoken",
		})
		rootTwoRun := schedule(rootTwo, rootTwo.ID)
		if !rootOne.StartsThread || !rootTwo.StartsThread || rootOne.ThreadRootID != "" || rootTwo.ThreadRootID != "" || rootTwo.ReplyToMessageID != rootOne.ID {
			t.Fatalf("new roots inherited an old lane or lost ancestry: %#v, %#v", rootOne, rootTwo)
		}
		one := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "utterance-one", ReplyToMessageID: rootOne.ID, RequiresResponse: true})
		oneRun := schedule(one, rootOne.ID)
		two := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "utterance-two", ReplyToMessageID: rootTwo.ID, RequiresResponse: true})
		twoRun := schedule(two, rootTwo.ID)
		nextOne := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "next-one", ReplyToMessageID: one.ID, RequiresResponse: true})
		nextOneRun := schedule(nextOne, rootOne.ID)
		if one.StartsThread || two.StartsThread || nextOne.StartsThread || one.ThreadRootID != rootOne.ID || two.ThreadRootID != rootTwo.ID || nextOne.ThreadRootID != rootOne.ID {
			t.Fatal("ordinary replies no longer inherit their canonical thread")
		}
		// Replies in each call thread are delivered to that thread's active Run.
		if oneRun.ID != rootOneRun.ID || nextOneRun.ID != rootOneRun.ID || twoRun.ID != rootTwoRun.ID || ordinaryRun.ID == rootOneRun.ID {
			t.Fatalf("thread replies started parallel Runs: %s %s %s %s", rootOneRun.ID, oneRun.ID, nextOneRun.ID, twoRun.ID)
		}
		for _, expected := range []struct {
			run       *AgentRun
			followUps []string
		}{
			{ordinaryRun, nil}, {rootOneRun, []string{one.ID, nextOne.ID}}, {rootTwoRun, []string{two.ID}},
		} {
			current, err := store.GetAgentRun(t.Context(), conversation.Scope, expected.run.ID)
			if err != nil || current == nil || current.Status != AgentRunStatusQueued || len(conversationRunFollowUps(current)) != len(expected.followUps) {
				t.Fatalf("follow-ups reached the wrong lane: run=%#v expected=%v err=%v", current, expected.followUps, err)
			}
			for index, id := range expected.followUps {
				if conversationRunFollowUps(current)[index].MessageID != id {
					t.Fatalf("follow-ups out of order: %#v", conversationRunFollowUps(current))
				}
			}
		}
		newOrdinary := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "new-ordinary", RequiresResponse: true})
		if delivered := schedule(newOrdinary, ""); delivered.ID != ordinaryRun.ID {
			t.Fatalf("ordinary follow-up started a parallel Run: %#v", delivered)
		}
		for _, run := range []*AgentRun{rootOneRun, rootTwoRun} {
			current, err := store.GetAgentRun(t.Context(), conversation.Scope, run.ID)
			if err != nil || current == nil || conversationRunHasFollowUp(current, newOrdinary.ID) {
				t.Fatalf("ordinary prompt reached a call thread: %#v, %v", current, err)
			}
		}
		timeline, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, ChannelTimeline: true, Limit: 100})
		if err != nil || !reflect.DeepEqual(conversationThreadStartMessageIDs(timeline), []string{ordinary.ID, rootOne.ID, rootTwo.ID, newOrdinary.ID}) {
			t.Fatalf("thread roots disappeared from the main timeline: ids=%v err=%v", conversationThreadStartMessageIDs(timeline), err)
		}
		thread, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, ThreadRootID: rootOne.ID, Limit: 100})
		if err != nil || !reflect.DeepEqual(conversationThreadStartMessageIDs(thread), []string{rootOne.ID, one.ID, nextOne.ID}) {
			t.Fatalf("retained ancestry merged independent threads: ids=%v err=%v", conversationThreadStartMessageIDs(thread), err)
		}
		viewer := ConversationViewer{Participant: initiator}
		page, err := ReadConversationHistory(t.Context(), service, conversation.Scope, conversation.Owner, conversation.ID, viewer, ConversationHistoryReadRequest{Limit: 2}, rootOne.ID)
		if err != nil || page == nil || !reflect.DeepEqual(conversationThreadStartHistoryIDs(page), []string{nextOne.ID, one.ID}) || page.NextBeforeSequence != one.Sequence {
			t.Fatalf("trusted local root did not page its own replies: page=%#v err=%v", page, err)
		}
		older, err := ReadConversationHistory(t.Context(), service, conversation.Scope, conversation.Owner, conversation.ID, viewer, ConversationHistoryReadRequest{BeforeSequence: page.NextBeforeSequence, Limit: 2}, rootOne.ID)
		if err != nil || older == nil || !reflect.DeepEqual(conversationThreadStartHistoryIDs(older), []string{rootOne.ID}) {
			t.Fatalf("trusted local history omitted its root or crossed lanes: page=%#v err=%v", older, err)
		}
		if _, err := ReadConversationHistory(t.Context(), service, conversation.Scope, conversation.Owner, conversation.ID, viewer, ConversationHistoryReadRequest{MessageID: two.ID}, rootOne.ID); err == nil {
			t.Fatal("exact message history escaped its trusted local root")
		}
		for _, invalidRoot := range []string{"missing-root", one.ID} {
			if _, err := ReadConversationHistory(t.Context(), service, conversation.Scope, conversation.Owner, conversation.ID, viewer, ConversationHistoryReadRequest{}, invalidRoot); err == nil {
				t.Fatalf("trusted history accepted missing/nonroot message %s", invalidRoot)
			}
		}
	})
}

func conversationThreadStartMessageIDs(messages []*ChannelMessage) []string {
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func conversationThreadStartHistoryIDs(page *ConversationHistoryReadResult) []string {
	if page == nil {
		return nil
	}
	ids := make([]string, 0, len(page.Messages))
	for _, message := range page.Messages {
		ids = append(ids, message.ID)
	}
	return ids
}

func TestConversationStartsThreadReplayJSONCloneAndRestart(t *testing.T) {
	forConversationThreadStartStores(t, func(t *testing.T, store conversationThreadStartTestStore, reopen func(*testing.T) conversationThreadStartTestStore) {
		conversation := conversationThreadStartConversation(t, store)
		service := NewConversationService(store)
		legacy := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "legacy-parent"})
		current, _ := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
		initiator := ConversationParticipant{Type: ConversationParticipantUser, ID: "user"}
		req := PostChannelMessageRequest{ID: "new-root", Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "calls"}, Initiator: &initiator, Intent: MessageIntentQuestion,
			Content: "Start a fresh call", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, StartThread: true, ReplyToMessageID: legacy.ID, IdempotencyKey: "new-root"}
		created, err := service.PostChannelMessage(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		replayed, err := service.PostChannelMessage(t.Context(), req)
		if err != nil || !replayed.Replayed || replayed.Message.ID != created.Message.ID || !replayed.Message.StartsThread || replayed.Message.ThreadRootID != "" {
			t.Fatalf("stale-revision replay lost thread intent: %#v, %v", replayed, err)
		}
		changed := req
		changed.StartThread = false
		if _, err := service.PostChannelMessage(t.Context(), changed); !errors.Is(err, ErrMessageConflict) {
			t.Fatalf("changed thread intent replay error = %v", err)
		}
		if _, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
			Sender: legacy.Sender, Intent: legacy.Intent, Content: legacy.Content, Audience: legacy.Audience,
			IdempotencyKey: legacy.IdempotencyKey, StartThread: true,
		}); !errors.Is(err, ErrMessageConflict) {
			t.Fatalf("legacy replay accepted changed thread intent: %v", err)
		}
		requestJSON, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		var requestFields map[string]json.RawMessage
		if err := json.Unmarshal(requestJSON, &requestFields); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"StartThread", "startThread", "startsThread"} {
			if _, present := requestFields[key]; present {
				t.Fatalf("host thread option became client JSON input: %s", requestJSON)
			}
		}
		var untrusted PostChannelMessageRequest
		if err := json.Unmarshal([]byte(`{"StartThread":true,"startThread":true,"startsThread":true}`), &untrusted); err != nil || untrusted.StartThread {
			t.Fatalf("client JSON authorized a host thread option: %#v, %v", untrusted, err)
		}
		payload, err := json.Marshal(created.Message)
		if err != nil {
			t.Fatal(err)
		}
		var decoded ChannelMessage
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &decoded); err != nil || !decoded.StartsThread || decoded.ReplyToMessageID != legacy.ID || decoded.ThreadRootID != "" {
			t.Fatalf("durable JSON lost thread metadata: %#v, %v", decoded, err)
		}
		if err := json.Unmarshal(payload, &fields); err != nil || string(fields["startsThread"]) != "true" {
			t.Fatalf("canonical thread JSON field missing: %s, %v", payload, err)
		}
		delete(fields, "startsThread")
		oldPayload, _ := json.Marshal(fields)
		var oldMessage ChannelMessage
		if err := json.Unmarshal(oldPayload, &oldMessage); err != nil || oldMessage.StartsThread {
			t.Fatalf("legacy JSON changed default thread routing: %#v, %v", oldMessage, err)
		}
		cloned := cloneChannelMessage(created.Message)
		if !cloned.StartsThread || cloned.Initiator == created.Message.Initiator {
			t.Fatal("channel message clone lost thread metadata or shared initiator")
		}
		cloned.StartsThread, cloned.Initiator.ID = false, "other"
		created.Message.StartsThread = false
		restored, err := reopen(t).GetChannelMessage(t.Context(), conversation.Scope, conversation.ID, req.ID)
		if err != nil || restored == nil || !restored.StartsThread || restored.ThreadRootID != "" || restored.ReplyToMessageID != legacy.ID || restored.Initiator == nil || restored.Initiator.ID != initiator.ID {
			t.Fatalf("canonical persisted thread root changed through a returned clone/restart: %#v, %v", restored, err)
		}
	})
}

func TestConversationStartsThreadPreservesPrivateAncestryForScalarAndBatchReads(t *testing.T) {
	forConversationThreadStartStores(t, func(t *testing.T, store conversationThreadStartTestStore, _ func(*testing.T) conversationThreadStartTestStore) {
		conversation := conversationThreadStartConversation(t, store)
		service := NewConversationService(store)
		allowed := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "allowed"}}
		foreign := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "foreign"}}
		private := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: "private-parent", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "private-author"},
			Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{allowed.Participant}},
		})
		directRoot := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: "direct-call-root", StartThread: true, ReplyToMessageID: private.ID,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "calls"}, Initiator: &allowed.Participant,
		})
		publicParent := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "public-parent", ReplyToMessageID: private.ID, Sender: allowed.Participant})
		root := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: "public-call-root", StartThread: true, ReplyToMessageID: publicParent.ID,
			Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "calls"}, Initiator: &allowed.Participant,
		})
		utterance := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{IdempotencyKey: "utterance", ReplyToMessageID: root.ID, Sender: allowed.Participant})
		answer := conversationThreadStartPost(t, service, conversation, PostChannelMessageRequest{
			IdempotencyKey: "answer", ReplyToMessageID: utterance.ID, Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: conversation.Owner.ID}, Intent: MessageIntentAnswer,
		})
		if root.ThreadRootID != "" || root.ReplyToMessageID != publicParent.ID || utterance.ThreadRootID != root.ID || answer.ThreadRootID != root.ID {
			t.Fatal("canonical call fixture did not preserve ancestry across its new thread boundary")
		}
		for _, message := range []*ChannelMessage{directRoot, root, utterance, answer} {
			if !CanViewChannelMessage(message, foreign) {
				t.Fatal("fixture requires public descendants whose ancestry restricts visibility")
			}
		}
		if _, err := ReadConversationHistory(t.Context(), service, conversation.Scope, conversation.Owner, conversation.ID, foreign, ConversationHistoryReadRequest{}, root.ID); err == nil {
			t.Fatal("trusted local history bypassed private root ancestry")
		}
		allowedPage, err := ReadConversationHistory(t.Context(), service, conversation.Scope, conversation.Owner, conversation.ID, allowed, ConversationHistoryReadRequest{}, root.ID)
		if err != nil || !reflect.DeepEqual(conversationThreadStartHistoryIDs(allowedPage), []string{answer.ID, utterance.ID, root.ID}) {
			t.Fatalf("authorized local history lost canonical root descendants: page=%#v err=%v", allowedPage, err)
		}
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
				point := &countedPointConversationStore{ConversationStore: store}
				var readStore ConversationStore = point
				var countedBatch *countedBatchConversationStore
				if batch {
					countedBatch = newCountedBatchConversationStore(store)
					readStore = countedBatch
				}
				reader := NewConversationService(readStore)
				for _, message := range []*ChannelMessage{directRoot, root, utterance, answer} {
					if _, err := reader.GetVisibleChannelMessage(t.Context(), conversation.Scope, conversation.ID, message.ID, foreign); !errors.Is(err, ErrChannelMessageNotFound) {
						t.Fatalf("thread boundary leaked %s: %v", message.ID, err)
					}
					if visible, err := reader.GetVisibleChannelMessage(t.Context(), conversation.Scope, conversation.ID, message.ID, allowed); err != nil || visible == nil || visible.ID != message.ID {
						t.Fatalf("authorized viewer lost %s: %#v, %v", message.ID, visible, err)
					}
				}
				point.points = 0
				if countedBatch != nil {
					countedBatch.points, countedBatch.calls = 0, 0
				}
				visible, err := reader.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, ThreadRootID: root.ID, Limit: 100, Viewer: &foreign})
				if err != nil || len(visible) != 0 {
					t.Fatalf("thread listing leaked private ancestry: %v, %v", conversationThreadStartMessageIDs(visible), err)
				}
				visible, err = reader.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: conversation.Scope, ConversationID: conversation.ID, ThreadRootID: root.ID, Limit: 100, Viewer: &allowed})
				if err != nil || !reflect.DeepEqual(conversationThreadStartMessageIDs(visible), []string{root.ID, utterance.ID, answer.ID}) {
					t.Fatalf("authorized thread listing changed: %v, %v", conversationThreadStartMessageIDs(visible), err)
				}
				if point.points > 32 || (countedBatch != nil && (countedBatch.points != 0 || countedBatch.calls > 16)) {
					t.Fatalf("small call thread retained unbounded ancestry reads: points=%d batch=%#v", point.points, countedBatch)
				}
			})
		}
	})
}

func TestConversationStartsThreadMissingCyclicAndDeepAnchorsFailClosed(t *testing.T) {
	channelMessageBatchStores(t, func(t *testing.T, kernel KernelStore) {
		store := kernel.(ConversationStore)
		conversation := conversationThreadStartConversation(t, store)
		commit := func(message *ChannelMessage) {
			t.Helper()
			channelMessageBatchCommit(t, store, &conversation, message)
		}
		commit(&ChannelMessage{ID: "missing-anchor", StartsThread: true, ReplyToMessageID: "missing-parent"})
		commit(&ChannelMessage{ID: "missing-child", ThreadRootID: "missing-anchor", ReplyToMessageID: "missing-anchor"})
		commit(&ChannelMessage{ID: "cycle-a", StartsThread: true, ReplyToMessageID: "cycle-b"})
		commit(&ChannelMessage{ID: "cycle-b", StartsThread: true, ReplyToMessageID: "cycle-a"})
		commit(&ChannelMessage{ID: "cycle-child", ThreadRootID: "cycle-a", ReplyToMessageID: "cycle-a"})
		parent := ""
		for index := 0; index < 32; index++ {
			id := fmt.Sprintf("deep-anchor-%02d", index)
			commit(&ChannelMessage{ID: id, StartsThread: true, ReplyToMessageID: parent})
			parent = id
		}
		commit(&ChannelMessage{ID: "deep-child", ThreadRootID: parent, ReplyToMessageID: parent})
		viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantUser, ID: "viewer"}}
		for _, batch := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
				point := &countedPointConversationStore{ConversationStore: store}
				var readStore ConversationStore = point
				var countedBatch *countedBatchConversationStore
				if batch {
					countedBatch = newCountedBatchConversationStore(store)
					readStore = countedBatch
				}
				reader := NewConversationService(readStore)
				if message, err := reader.GetVisibleChannelMessage(t.Context(), conversation.Scope, conversation.ID, "deep-anchor-15", viewer); err != nil || message == nil {
					t.Fatalf("16 structurally valid thread anchors were hidden: %#v, %v", message, err)
				}
				for _, id := range []string{"missing-anchor", "missing-child", "cycle-a", "cycle-b", "cycle-child", "deep-anchor-16", parent, "deep-child"} {
					point.points = 0
					if countedBatch != nil {
						countedBatch.points, countedBatch.calls = 0, 0
					}
					if _, err := reader.GetVisibleChannelMessage(t.Context(), conversation.Scope, conversation.ID, id, viewer); !errors.Is(err, ErrChannelMessageNotFound) {
						t.Fatalf("malformed StartsThread ancestry %s did not fail closed: %v", id, err)
					}
					if point.points > 40 || (countedBatch != nil && (countedBatch.points > 1 || countedBatch.calls > 20)) {
						t.Fatalf("malformed ancestry exceeded the bounded walk: points=%d batch=%#v", point.points, countedBatch)
					}
				}
			})
		}
		service := NewConversationService(store)
		current, _ := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
		if _, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
			Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
			Sender: viewer.Participant, Intent: MessageIntentQuestion, Content: "Start at the depth boundary", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
			StartThread: true, ReplyToMessageID: "deep-anchor-14", IdempotencyKey: "accepted-depth-boundary",
		}); err != nil {
			t.Fatalf("admission rejected 16 structurally valid anchors: %v", err)
		}
		for _, parentID := range []string{"missing-anchor", "cycle-a", "deep-anchor-15", parent} {
			current, _ := service.GetConversation(t.Context(), conversation.Scope, conversation.ID)
			_, err := service.PostChannelMessage(t.Context(), PostChannelMessageRequest{
				Scope: conversation.Scope, ConversationID: conversation.ID, ExpectedRevision: current.Revision,
				Sender: viewer.Participant, Intent: MessageIntentQuestion, Content: "Start a new thread", Audience: ConversationAudience{Kind: ConversationAudienceChannel},
				StartThread: true, ReplyToMessageID: parentID, IdempotencyKey: "reject-" + parentID,
			})
			if !errors.Is(err, ErrInvalidConversation) {
				t.Fatalf("host admission accepted malformed thread ancestry %s: %v", parentID, err)
			}
		}
	})
}
