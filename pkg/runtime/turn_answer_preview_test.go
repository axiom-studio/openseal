package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTurnAnswerPreviewProjectionDoesNotExposePrivatePayload(t *testing.T) {
	event := &ActivityEvent{EventType: "turn.answer_preview", Visibility: ActivityVisibilityScope, Payload: map[string]interface{}{
		"attemptId": "attempt", "sequence": uint64(3), "text": "Public answer", "reset": false, "reasoning": "private", "arguments": map[string]interface{}{"token": "secret"},
	}}
	// Match the database's float64 representation of JSON numbers.
	data, _ := json.Marshal(event)
	if err := json.Unmarshal(data, event); err != nil {
		t.Fatal(err)
	}
	projection := projectActivityEvent(event, false)
	if projection.AnswerPreview == nil || projection.AnswerPreview.Text != "Public answer" || projection.AnswerPreview.Sequence != 3 || projection.Payload != nil {
		t.Fatalf("projection=%+v", projection)
	}
	data, _ = json.Marshal(projection)
	if strings.Contains(string(data), "private") || strings.Contains(string(data), "secret") {
		t.Fatal("private payload escaped")
	}
	event.Visibility = ActivityVisibilityPrivate
	if projectActivityEvent(event, false).AnswerPreview != nil {
		t.Fatal("private preview escaped")
	}
	event.Visibility = ActivityVisibilityScope
	event.Payload["sequence"] = 1.5
	if projectActivityEvent(event, false).AnswerPreview != nil {
		t.Fatal("invalid sequence accepted")
	}
}

func TestTurnAnswerPreviewDurableScopedAndLeaseBound(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "7"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Reply", Source: RunSourceObjective})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewTurnCoordinator(store, store, store)
	var captured *AgentTurn
	_, err = coordinator.Advance(t.Context(), AdvanceAgentRunRequest{Scope: scope, RunID: run.ID, WorkerID: "worker"}, TurnRunnerFunc(func(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
		captured = input.Turn
		for _, preview := range []TurnAnswerPreview{{AttemptID: "attempt", Sequence: 1, Text: "Hello"}, {AttemptID: "attempt", Sequence: 2, Reset: true}} {
			if err := ReportTurnAnswerPreview(ctx, preview); err != nil {
				return nil, err
			}
		}
		events, err := store.ListActivity(ctx, ActivityFilter{Scope: scope, RunID: run.ID, EventTypes: []string{"turn.answer_preview"}})
		if err != nil || len(events) != 2 {
			t.Fatalf("events=%v err=%v", events, err)
		}
		for _, event := range events {
			if event.TurnID != input.Turn.ID || event.Scope != scope || event.Payload["attemptId"] != "attempt" {
				t.Fatalf("wrong identity: %+v", event)
			}
		}
		current, err := store.GetAgentTurn(ctx, scope, input.Turn.ID)
		if err != nil || current.Status != AgentTurnStatusRunning {
			t.Fatal("preview waited for completion")
		}
		foreign, err := store.ListActivity(ctx, ActivityFilter{Scope: Scope{Kind: "tenant", ID: "other"}, RunID: run.ID})
		if err != nil || len(foreign) > 0 {
			t.Fatal("cross-tenant preview exposed")
		}
		return &TurnOutcome{OutputSummary: "Hello", NextRunStatus: AgentRunStatusCompleted}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	lateCtx := coordinator.observeTurnAnswer(t.Context(), scope, run, captured, "worker")
	if err := ReportTurnAnswerPreview(lateCtx, TurnAnswerPreview{AttemptID: "late", Sequence: 1, Text: "Too late"}); !errors.Is(err, ErrTurnLeaseHeld) {
		t.Fatalf("late preview accepted: %v", err)
	}
}

func TestTurnAnswerPreviewRejectsInvalidFramesAndCancellation(t *testing.T) {
	count := 0
	ctx, cancel := context.WithCancel(WithTurnAnswerPreview(t.Context(), func(context.Context, TurnAnswerPreview) error { count++; return nil }))
	for _, p := range []TurnAnswerPreview{
		{Sequence: 1, Text: "missing attempt"},
		{AttemptID: "a", Text: "zero sequence"},
		{AttemptID: "a", Sequence: 1, Text: strings.Repeat("x", (64<<10)+1)},
		{AttemptID: "a", Sequence: 1, Text: string([]byte{0xff})},
		{AttemptID: "a", Sequence: 1, Text: "hidden", Reset: true},
	} {
		if err := ReportTurnAnswerPreview(ctx, p); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
	if count != 0 {
		t.Fatal("invalid preview escaped")
	}
	cancel()
	if err := ReportTurnAnswerPreview(ctx, TurnAnswerPreview{AttemptID: "a", Sequence: 1, Text: "late"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
