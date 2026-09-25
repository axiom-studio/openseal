package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestConversationCompactionKeepsOriginalsUntilAccepted(t *testing.T) {
	c := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "one"}, LastSequence: 32}
	v := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}}
	messages := make([]*ChannelMessage, 32)
	for i := range messages {
		messages[i] = &ChannelMessage{ID: fmt.Sprintf("m%d", i+1), Sequence: int64(i + 1), Content: strings.Repeat("x", 1500)}
	}
	plan := planConversationHistory(c, "m32", v, messages, nil)
	if len(plan.Messages) != 32 || plan.Request == nil || plan.Request.ThroughSequence != 20 || len(plan.Request.SourceMessageIDs) != 20 {
		t.Fatalf("bad initial plan: %+v", plan)
	}
	checkpoint := map[string]interface{}{conversationSummaryCheckpoint: map[string]interface{}{"basis": plan.Request.Basis, "text": "The user prefers terse answers [m1]. Budget is 420 [m2]."}}
	saved := acceptedConversationSummary(c, v, plan, checkpoint)
	if saved == nil {
		t.Fatal("valid checkpoint rejected")
	}
	compacted := planConversationHistory(c, "m32", v, messages, saved)
	if len(compacted.Messages) != 12 || compacted.Messages[0].ID != "m21" || compacted.Summary != saved {
		t.Fatal("recent originals were lost")
	}
	for _, bad := range []map[string]interface{}{{"basis": "wrong", "text": "memory"}, {"basis": plan.Request.Basis, "text": strings.Repeat("x", 8193)}, {"basis": plan.Request.Basis, "text": ""}} {
		if acceptedConversationSummary(c, v, plan, map[string]interface{}{conversationSummaryCheckpoint: bad}) != nil {
			t.Fatal("bad summary accepted")
		}
	}
	v.Roles = []string{"new-role"}
	if changed := planConversationHistory(c, "m32", v, messages, saved); changed.Summary != nil || len(changed.Messages) != 32 {
		t.Fatal("role change reused summary")
	}
	v.Roles = nil
	if retry := planConversationHistory(c, "m1", v, messages, saved); retry.Messages[0].ID != "m1" {
		t.Fatal("older triggering message was hidden")
	}
}

func TestConversationCompactionCountsProjectedMessageMetadata(t *testing.T) {
	c := &Conversation{ID: "chat", Scope: Scope{Kind: "tenant", ID: "one"}, LastSequence: 80}
	v := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}}
	messages := make([]*ChannelMessage, 80)
	for i := range messages {
		messages[i] = &ChannelMessage{
			ID: fmt.Sprintf("m%d", i+1), Sequence: int64(i + 1),
			Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "user"},
			Intent: MessageIntentUpdate, Content: "short reply",
			References: []ConversationReference{
				{Kind: ConversationReferenceArtifact, ID: strings.Repeat("a", 128)},
				{Kind: ConversationReferenceRun, ID: strings.Repeat("b", 128)},
			},
		}
	}
	plan := planConversationHistory(c, "m80", v, messages, nil)
	if plan.Request == nil || plan.Request.ThroughSequence != 68 || len(plan.Messages) != 80 {
		t.Fatalf("projected metadata did not trigger safe compaction: %+v", plan)
	}
	saved := acceptedConversationSummary(c, v, plan, map[string]interface{}{
		conversationSummaryCheckpoint: map[string]interface{}{"basis": plan.Request.Basis, "text": "Earlier short replies and artifact references."},
	})
	if saved == nil {
		t.Fatal("valid summary rejected")
	}
	compacted := planConversationHistory(c, "m80", v, messages, saved)
	if len(compacted.Messages) != 12 || compacted.Messages[0].ID != "m69" {
		t.Fatalf("summary did not remove covered projected history: %+v", compacted)
	}
}

func TestConversationCompactionRunsWithoutExtraModelCall(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "compact-run"}
	c, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "history", IdempotencyKey: "history"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		postConversationRunTestMessage(t, service, c, ConversationParticipantUser, MessageIntentUpdate, fmt.Sprintf("original-%d ", i)+strings.Repeat("x", 1500), fmt.Sprint(i))
		c, _ = service.GetConversation(t.Context(), scope, c.ID)
	}
	c, _ = service.GetConversation(t.Context(), scope, c.ID)
	_, err = service.PostChannelMessage(t.Context(), PostChannelMessageRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision, Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "other"}, Audience: ConversationAudience{Kind: ConversationAudienceParticipants, Participants: []ConversationParticipant{{Type: ConversationParticipantUser, ID: "other"}}}, Content: "PRIVATE_NEVER_SUMMARIZE", Intent: MessageIntentUpdate, IdempotencyKey: "private"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: "agent", Runner: TurnRunnerFunc(func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
			calls++
			if strings.Contains(input.Run.Goal, "PRIVATE_NEVER_SUMMARIZE") {
				t.Fatal("private history entered model context")
			}
			var payload struct {
				Messages []agentConversationPromptMessage `json:"messages"`
				Summary  string                           `json:"historySummary"`
				Request  *ConversationCompactionRequest   `json:"historyCompaction"`
			}
			start := strings.Index(input.Run.Goal, "\n\n{")
			if start < 0 {
				t.Fatal("missing prompt payload")
			}
			if err := json.Unmarshal([]byte(input.Run.Goal[start+2:]), &payload); err != nil {
				t.Fatal(err)
			}
			outcome := &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Answer"}}
			if calls == 1 {
				if payload.Request == nil || len(payload.Messages) < 32 {
					t.Fatal("first call dropped originals")
				}
				outcome.ContinuationCheckpoint = map[string]interface{}{conversationSummaryCheckpoint: map[string]interface{}{"basis": payload.Request.Basis, "text": "Preserve fact original-0 with its source message reference."}}
			} else {
				if payload.Summary == "" || len(payload.Messages) >= 32 {
					t.Fatal("accepted summary was not used")
				}
				for _, message := range payload.Messages {
					if strings.HasPrefix(message.Content, "original-0 ") {
						t.Fatal("summarized original still sent")
					}
				}
			}
			return outcome, nil
		})}, nil
	})
	runner, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		c, _ = service.GetConversation(t.Context(), scope, c.ID)
		trigger := postConversationRunTestMessage(t, service, c, ConversationParticipantUser, MessageIntentQuestion, "Continue", fmt.Sprintf("trigger-%d", i))
		scheduled, _, err := scheduler.ScheduleMessage(t.Context(), scope, c.ID, trigger.ID)
		if err != nil {
			t.Fatal(err)
		}
		binding, err := runner.ResolveTurnRunner(t.Context(), scheduled.Run)
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := binding.Runner.RunTurn(t.Context(), TurnExecutionContext{Run: scheduled.Run})
		if err != nil || outcome == nil {
			t.Fatalf("turn failed: %+v %v", outcome, err)
		}
		if _, exists := outcome.ContinuationCheckpoint[conversationSummaryCheckpoint]; exists {
			t.Fatal("internal summary leaked into public checkpoint")
		}
	}
	if calls != 2 {
		t.Fatalf("compaction added model calls: %d", calls)
	}
}

func TestConversationSummaryCacheIsEphemeralBoundedAndIsolated(t *testing.T) {
	var cache conversationSummaryCache
	scope := Scope{Kind: "tenant", ID: "one"}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}}
	key := conversationViewerKey(viewer)
	summary := &conversationSummary{Scope: scope, ConversationID: "chat", ViewerKey: key, ThroughSequence: 20, Basis: strings.Repeat("a", 64), Text: "working summary"}
	cache.put(summary)
	summary.Text = "mutated caller"
	got := cache.get(scope, "chat", key)
	if got == nil || got.Text != "working summary" {
		t.Fatal("cache retained caller pointer")
	}
	got.Text = "mutated reader"
	if cache.get(scope, "chat", key).Text != "working summary" {
		t.Fatal("reader mutated cache")
	}
	stale := *summary
	stale.ThroughSequence = 1
	cache.put(&stale)
	if cache.get(scope, "chat", key).ThroughSequence != 20 {
		t.Fatal("stale replacement")
	}
	if cache.get(Scope{Kind: "tenant", ID: "two"}, "chat", key) != nil || cache.get(scope, "chat", strings.Repeat("b", 64)) != nil {
		t.Fatal("cache crossed tenant/viewer boundary")
	}
	var restarted conversationSummaryCache
	if restarted.get(scope, "chat", key) != nil {
		t.Fatal("new worker inherited cache")
	}
	messages := make([]*ChannelMessage, 32)
	for i := range messages {
		messages[i] = &ChannelMessage{ID: fmt.Sprintf("m%d", i+1), Sequence: int64(i + 1), Content: strings.Repeat("x", 1500)}
	}
	plan := planConversationHistory(&Conversation{Scope: scope, ID: "chat", LastSequence: 32}, "m32", viewer, messages, restarted.get(scope, "chat", key))
	if plan.Request == nil || len(plan.Messages) != 32 {
		t.Fatal("cache loss did not rebuild from original history")
	}
	for i := 0; i < conversationSummaryCacheCapacity; i++ {
		next := *summary
		next.ConversationID = fmt.Sprintf("other-%d", i)
		cache.put(&next)
	}
	if len(cache.entries) != conversationSummaryCacheCapacity || cache.get(scope, "chat", key) != nil {
		t.Fatal("cache did not evict oldest entry")
	}
}

func TestConversationCompactionCoversLongHistoryWithoutSkipping(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "long-history"}
	c, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "long", IdempotencyKey: "long"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		postConversationRunTestMessage(t, service, c, ConversationParticipantUser, MessageIntentUpdate, fmt.Sprintf("Fact %d", i), fmt.Sprint(i))
		c, err = service.GetConversation(t.Context(), scope, c.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	viewer := ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}}
	recent, err := service.ListChannelMessages(t.Context(), ChannelMessageFilter{Scope: scope, ConversationID: c.ID, Viewer: &viewer, Descending: true, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	reverseChannelMessages(recent)
	runner := &ConversationRunTurnRunner{conversations: service}
	var saved *conversationSummary
	covered := int64(0)
	for attempt := 0; attempt < 3; attempt++ {
		history, err := runner.conversationHistoryForCompaction(t.Context(), c, viewer, recent, saved)
		if err != nil {
			t.Fatal(err)
		}
		plan := planConversationHistory(c, recent[len(recent)-1].ID, viewer, history, saved)
		if plan.Request == nil {
			t.Fatalf("backlog stopped compacting after sequence %d", covered)
		}
		byID := make(map[string]*ChannelMessage)
		for _, m := range plan.Messages {
			byID[m.ID] = m
		}
		for _, id := range plan.Request.SourceMessageIDs {
			message := byID[id]
			if message == nil || message.Sequence != covered+1 {
				t.Fatalf("coverage jumped after %d: %+v", covered, message)
			}
			covered = message.Sequence
		}
		if plan.Request.ThroughSequence != covered {
			t.Fatal("summary claimed unseen coverage")
		}
		saved = acceptedConversationSummary(c, viewer, plan, map[string]interface{}{conversationSummaryCheckpoint: map[string]interface{}{"basis": plan.Request.Basis, "text": "Retained facts with references"}})
		if saved == nil {
			t.Fatal("summary rejected")
		}
	}
	if covered != 264 {
		t.Fatalf("covered %d messages, want 264", covered)
	}
	history, err := runner.conversationHistoryForCompaction(t.Context(), c, viewer, recent, saved)
	if err != nil {
		t.Fatal(err)
	}
	final := planConversationHistory(c, recent[len(recent)-1].ID, viewer, history, saved)
	if len(final.Messages) != 36 || final.Messages[0].Sequence != 265 || final.Messages[35].Sequence != 300 {
		t.Fatal("remaining originals lost")
	}
}

func TestTeamCompactionKeepsSummariesPrivateAndInvalidatesRoles(t *testing.T) {
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: "team-summary"}
	c, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team"}, Title: "team", IdempotencyKey: "team"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		postConversationRunTestMessage(t, service, c, ConversationParticipantUser, MessageIntentUpdate, strings.Repeat("x", 1500), fmt.Sprint(i))
		c, err = service.GetConversation(t.Context(), scope, c.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	participant := ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}
	role := "developer"
	source := ConversationParticipantSourceFunc(func(context.Context, ConversationParticipantQuery) ([]ConversationParticipantBinding, error) {
		return []ConversationParticipantBinding{{Participant: participant, SemanticRoles: []string{role}}}, nil
	})
	calls := 0
	provider := ParticipationProposalProviderFunc(func(_ context.Context, input ParticipationProposalContext) (ParticipationProposal, error) {
		calls++
		if calls == 2 {
			if input.HistorySummary != "PRIVATE_WORKING_SUMMARY" || len(input.RecentMessages) >= 32 {
				t.Error("summary not reused")
			}
		} else if input.HistorySummary != "" {
			t.Error("summary survived role change")
		}
		proposal := ParticipationProposal{WantsToSpeak: false}
		if input.HistoryCompaction != nil {
			proposal.WorkingContextCheckpoint = map[string]interface{}{conversationSummaryCheckpoint: map[string]interface{}{"basis": input.HistoryCompaction.Basis, "text": "PRIVATE_WORKING_SUMMARY"}}
		}
		return proposal, nil
	})
	coordinator, err := NewConversationCoordinator(service, source, provider, DefaultConversationCoordinatorConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if i == 2 {
			role = "analyst"
		}
		c, err = service.GetConversation(t.Context(), scope, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		round, err := coordinator.Coordinate(t.Context(), ConversationCoordinationRequest{Scope: scope, ConversationID: c.ID, ExpectedRevision: c.Revision, IdempotencyKey: fmt.Sprintf("round-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range round.Round.Proposals {
			if p.WorkingContextCheckpoint != nil {
				t.Fatal("working summary persisted in proposal")
			}
		}
		data, err := json.Marshal(round)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "PRIVATE_WORKING_SUMMARY") {
			t.Fatal("working summary leaked into public round")
		}
	}
	if calls != 3 {
		t.Fatalf("model calls: %d", calls)
	}
}
