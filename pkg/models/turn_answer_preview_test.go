package models

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
	projection := ProjectActivityEvent(event, false)
	if projection.AnswerPreview == nil || projection.AnswerPreview.Text != "Public answer" || projection.AnswerPreview.Sequence != 3 || projection.Payload != nil {
		t.Fatalf("projection=%+v", projection)
	}
	data, _ = json.Marshal(projection)
	if strings.Contains(string(data), "private") || strings.Contains(string(data), "secret") {
		t.Fatal("private payload escaped")
	}
	event.Visibility = ActivityVisibilityPrivate
	if ProjectActivityEvent(event, false).AnswerPreview != nil {
		t.Fatal("private preview escaped")
	}
	event.Visibility = ActivityVisibilityScope
	event.Payload["sequence"] = 1.5
	if ProjectActivityEvent(event, false).AnswerPreview != nil {
		t.Fatal("invalid sequence accepted")
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
