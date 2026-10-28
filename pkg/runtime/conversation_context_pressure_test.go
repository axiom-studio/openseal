package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type contextPressurePayload struct {
	Messages []agentConversationPromptMessage `json:"messages"`
	Request  *ConversationCompactionRequest   `json:"historyCompaction"`
	Omitted  int                              `json:"historyOmittedMessages"`
	Guidance string                           `json:"contextGuidance"`
	Current  agentConversationPromptMessage   `json:"currentMessage"`
}

func decodeContextPressurePayload(t *testing.T, goal string) contextPressurePayload {
	t.Helper()
	start := strings.Index(goal, "\n\n{")
	if start < 0 {
		t.Fatal("missing prompt payload")
	}
	var payload contextPressurePayload
	if err := json.Unmarshal([]byte(goal[start+2:]), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// contextPressureChat is a chat with count earlier messages of size bytes and
// a trigger, scheduled for a turn.
func contextPressureChat(t *testing.T, name string, count, size int, runner func(context.Context, TurnExecutionContext) (*TurnOutcome, error)) (TurnRunner, TurnExecutionContext, *ConversationRunTurnRunner, *Conversation, *ChannelMessage) {
	t.Helper()
	store := NewMemoryStore()
	service := NewConversationService(store)
	scope := Scope{Kind: "tenant", ID: name}
	c, _, err := service.CreateConversation(t.Context(), CreateConversationRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Title: "long chat", IdempotencyKey: name})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		c, _ = service.GetConversation(t.Context(), scope, c.ID)
		postConversationRunTestMessage(t, service, c, ConversationParticipantUser, MessageIntentUpdate, fmt.Sprintf("original-%d ", i)+strings.Repeat("x", size), fmt.Sprint(i))
	}
	c, _ = service.GetConversation(t.Context(), scope, c.ID)
	trigger := postConversationRunTestMessage(t, service, c, ConversationParticipantUser, MessageIntentQuestion, "What did I say first?", "trigger")
	resolver := TurnRunnerResolverFunc(func(context.Context, *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DeploymentID: "agent", Runner: TurnRunnerFunc(runner)}, nil
	})
	conversations, err := NewConversationRunTurnRunner(store, conversationRunTestCoordinator(t, service), ConversationRunTurnRunnerConfig{AgentTurns: resolver})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, _, err := mustConversationRunScheduler(t, store).ScheduleMessage(t.Context(), scope, c.ID, trigger.ID)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := conversations.ResolveTurnRunner(t.Context(), scheduled.Run)
	if err != nil {
		t.Fatal(err)
	}
	return binding.Runner, TurnExecutionContext{Run: scheduled.Run}, conversations, c, trigger
}

func contextPressureFailure(estimated, limit int64) error {
	return &TurnHostFailure{Code: TurnHostContextExceeded, Message: "This conversation is too long for test-model.", ContextPressure: &HostedContextPressure{EstimatedTokens: estimated, LimitTokens: limit}}
}

func TestContextPressureShrinksHistoryAndRetriesOnce(t *testing.T) {
	var calls []contextPressurePayload
	var inputs []TurnExecutionContext
	runner, input, conversations, c, trigger := contextPressureChat(t, "pressure-shrinks", 40, 2000, func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		payload := decodeContextPressurePayload(t, input.Run.Goal)
		calls = append(calls, payload)
		inputs = append(inputs, input)
		if len(calls) == 1 {
			// About 80 KiB of history; ask for about 30 KiB less.
			return nil, contextPressureFailure(40000, 43000)
		}
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "You said original-0."},
			ContinuationCheckpoint: map[string]interface{}{conversationSummaryCheckpoint: map[string]interface{}{"basis": payload.Request.Basis, "text": "The user listed forty originals."}}}, nil
	})
	outcome, err := runner.RunTurn(t.Context(), input)
	if err != nil || outcome == nil || outcome.NextRunStatus != AgentRunStatusCompleted {
		t.Fatalf("outcome = %#v, err = %v", outcome, err)
	}
	if len(calls) != 2 {
		t.Fatalf("turn was sent %d times, want 2", len(calls))
	}
	if !inputs[0].contextReducible || inputs[0].contextPressure != nil || inputs[1].contextReducible {
		t.Fatalf("first reducible = %v, retry reducible = %v", inputs[0].contextReducible, inputs[1].contextReducible)
	}
	first, retry := calls[0], calls[1]
	if retry.Current.ID != trigger.ID || retry.Omitted == 0 || len(retry.Messages)+retry.Omitted != len(first.Messages) {
		t.Fatalf("first sent %d messages, retry %d with %d omitted", len(first.Messages), len(retry.Messages), retry.Omitted)
	}
	// Each message is about 2.3 KiB in the model input with its metadata.
	removed := 0
	for _, message := range first.Messages[:retry.Omitted] {
		projected, _ := json.Marshal(message)
		escaped, _ := json.Marshal(string(projected))
		removed += len(escaped) + 1
	}
	if want := (40000 - 43000*3/4) * 4; removed < want || removed > want+2500 {
		t.Fatalf("removed %d bytes, want about %d", removed, want)
	}
	if !strings.HasPrefix(retry.Messages[0].Content, fmt.Sprintf("original-%d ", retry.Omitted)) || !strings.HasPrefix(retry.Messages[len(retry.Messages)-1].Content, "original-39 ") {
		t.Fatal("retry did not keep the latest history")
	}
	if !strings.Contains(retry.Guidance, "read_conversation_history") {
		t.Fatalf("guidance does not explain omitted history: %q", retry.Guidance)
	}
	// The retry asks for a summary of the history it kept, so the next turn
	// starts smaller.
	if retry.Request == nil || retry.Request.SourceMessageIDs[0] != retry.Messages[0].ID || retry.Request.ThroughSequence >= trigger.Sequence {
		t.Fatalf("retry summary request = %#v", retry.Request)
	}
	saved := conversations.summaries.get(c.Scope, c.ID, conversationViewerKey(ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent"}}))
	if saved == nil || saved.ThroughSequence != retry.Request.ThroughSequence {
		t.Fatalf("summary from the retry was not kept: %#v", saved)
	}
}

func TestContextPressureRequestsSummaryWhenNoneWasDue(t *testing.T) {
	calls := 0
	var retry contextPressurePayload
	runner, input, _, _, trigger := contextPressureChat(t, "pressure-summary", 8, 3000, func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		payload := decodeContextPressurePayload(t, input.Run.Goal)
		if calls == 1 {
			if payload.Request != nil {
				t.Fatal("eight messages already asked for a summary")
			}
			return nil, contextPressureFailure(9000, 9000)
		}
		retry = payload
		return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "Done."}}, nil
	})
	if _, err := runner.RunTurn(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	// 2250 tokens (9000 bytes) must go: three earlier messages leave, and the
	// five kept are summarized for later turns.
	if retry.Current.ID != trigger.ID || retry.Omitted != 3 || len(retry.Messages) != 5 || retry.Request == nil || len(retry.Request.SourceMessageIDs) != 5 {
		t.Fatalf("retry = %d messages, %d omitted, request %#v", len(retry.Messages), retry.Omitted, retry.Request)
	}
}

func TestContextPressureThatPersistsFailsWithTheHostMessage(t *testing.T) {
	calls := 0
	runner, input, _, _, _ := contextPressureChat(t, "pressure-fails", 6, 500, func(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		if calls == 2 {
			payload := decodeContextPressurePayload(t, input.Run.Goal)
			// Nothing but the trigger is left.
			if len(payload.Messages) != 0 || payload.Omitted != 6 || payload.Current.ID == "" {
				t.Fatalf("retry sent %#v", payload.Messages)
			}
			return nil, NewTurnHostFailure(TurnHostContextExceeded, "This conversation is too long for test-model. Start a new chat to keep going.", false)
		}
		return nil, contextPressureFailure(500000, 100000)
	})
	_, err := runner.RunTurn(t.Context(), input)
	var failure *TurnHostFailure
	if !errors.As(err, &failure) || failure.Code != TurnHostContextExceeded || !strings.HasPrefix(failure.Error(), "This conversation is too long for test-model") {
		t.Fatalf("err = %v", err)
	}
	if calls != 2 {
		t.Fatalf("turn was sent %d times, want 2", calls)
	}
}

func TestContextPressureWithoutReducibleTurnIsNotRetried(t *testing.T) {
	calls := 0
	runner, input, _, _, _ := contextPressureChat(t, "pressure-other-failure", 4, 100, func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
		calls++
		return nil, NewTurnHostFailure("provider_unavailable", "Model provider is temporarily unavailable.", false)
	})
	if _, err := runner.RunTurn(t.Context(), input); err == nil || calls != 1 {
		t.Fatalf("err = %v after %d calls", err, calls)
	}
}

func TestShrinkConversationHistoryOrder(t *testing.T) {
	messages := make([]*ChannelMessage, 11)
	for i := range messages {
		messages[i] = &ChannelMessage{ID: fmt.Sprintf("m%d", i+1), Sequence: int64(i + 1), Content: strings.Repeat("x", 1000)}
	}
	trigger := messages[10]
	size := conversationPromptMessageBytes(messages[0])

	plan := conversationHistoryPlan{Messages: messages}
	if shorten := shrinkConversationHistory(&plan, trigger, 3*size, 50*size); shorten || plan.Omitted != 3 || plan.Messages[0].ID != "m4" || len(plan.Request.SourceMessageIDs) != 7 {
		t.Fatalf("earlier messages first: shorten %v, %+v", shorten, plan)
	}
	plan = conversationHistoryPlan{Messages: messages}
	if shorten := shrinkConversationHistory(&plan, trigger, 9*size, 2*size); !shorten || plan.Omitted != 7 || plan.Messages[0].ID != "m8" {
		t.Fatalf("then earlier action results, then the last four: shorten %v, %+v", shorten, plan)
	}
	plan = conversationHistoryPlan{Messages: messages}
	if shrinkConversationHistory(&plan, trigger, 100*size, 0); plan.Omitted != 10 || len(plan.Messages) != 1 || plan.Messages[0] != trigger || plan.Request != nil {
		t.Fatalf("the trigger always stays: %+v", plan)
	}
}

func TestPressuredModelInputShortensEarlierActionResults(t *testing.T) {
	long := strings.Repeat("evidence ", 400)
	checkpoint := map[string]interface{}{
		actionHistoryCheckpointKey: []interface{}{
			map[string]interface{}{"actionCallId": "call-1", "status": "succeeded", "result": map[string]interface{}{"url": "https://example.com/a", "text": long}},
			map[string]interface{}{"actionCallId": "call-2", "status": "succeeded", "result": map[string]interface{}{"text": "short"}},
			map[string]interface{}{"actionCallId": "call-3", "status": "succeeded", "result": map[string]interface{}{"text": long}},
		},
		"lastAction": map[string]interface{}{"actionCallId": "call-3", "status": "succeeded", "result": map[string]interface{}{"text": long}},
	}
	request := HostedTurnRequest{Goal: "answer", ContinuationCheckpoint: checkpoint}
	plain, err := MarshalHostedTurnModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	request.ContextPressure = &HostedContextPressure{EstimatedTokens: 10, LimitTokens: 10}
	pressured, err := MarshalHostedTurnModelInput(request)
	if err != nil {
		t.Fatal(err)
	}
	if saved := pressuredActionHistorySavings(checkpoint); saved <= 0 || len(plain)-len(pressured) != saved {
		t.Fatalf("plain %d, pressured %d, predicted saving %d", len(plain), len(pressured), saved)
	}
	var view struct {
		Checkpoint map[string]interface{} `json:"continuationCheckpoint"`
	}
	if err := json.Unmarshal(pressured, &view); err != nil {
		t.Fatal(err)
	}
	history := view.Checkpoint[actionHistoryCheckpointKey].([]interface{})
	first := history[0].(map[string]interface{})["result"].(map[string]interface{})
	if text, _ := first["text"].(string); len(text) > HostedTurnPressuredActionResultBytes+8 || first["url"] != "https://example.com/a" || first["evidenceRef"] != "action-call:call-1" {
		t.Fatalf("earlier result = %#v", first)
	}
	if history[1].(map[string]interface{})["result"].(map[string]interface{})["text"] != "short" {
		t.Fatal("a short earlier result was rewritten")
	}
	last := view.Checkpoint["lastAction"].(map[string]interface{})["result"].(map[string]interface{})
	if last["text"] != long {
		t.Fatal("the current action result was shortened")
	}
	if checkpoint[actionHistoryCheckpointKey].([]interface{})[0].(map[string]interface{})["result"].(map[string]interface{})["text"] != long {
		t.Fatal("the durable checkpoint was changed")
	}
	if strings.Contains(string(pressured), "contextPressure") || strings.Contains(string(pressured), "contextReducible") {
		t.Fatal("context hints entered model input")
	}
}

type hostedTurnHostFunc func(context.Context, HostedTurnRequest) (*HostedTurnResponse, error)

func (f hostedTurnHostFunc) ExecuteHostedTurn(ctx context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	return f(ctx, request)
}

func TestHostedTurnRequestCarriesContextHints(t *testing.T) {
	var seen []HostedTurnRequest
	host := hostedTurnHostFunc(func(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
		seen = append(seen, request)
		return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "test-model",
			NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Done."}, nil
	})
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "definition", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	run := &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "7"}, AssignedAgentID: "agent", Goal: "Answer"}
	turn := &AgentTurn{ID: "turn", RunID: run.ID}
	pressure := &HostedContextPressure{EstimatedTokens: 120, LimitTokens: 100}
	for _, input := range []TurnExecutionContext{{Run: run, Turn: turn}, {Run: run, Turn: turn, contextReducible: true}, {Run: run, Turn: turn, contextPressure: pressure}} {
		if _, err := runner.RunTurn(t.Context(), input); err != nil {
			t.Fatal(err)
		}
	}
	if seen[0].ContextReducible || seen[0].ContextPressure != nil || !seen[1].ContextReducible || seen[2].ContextReducible ||
		seen[2].ContextPressure == nil || *seen[2].ContextPressure != *pressure {
		t.Fatalf("requests = %+v", seen)
	}
}
