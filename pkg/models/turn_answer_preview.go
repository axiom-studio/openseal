package models

import (
	"context"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// TurnAnswerPreview is a provisional snapshot, never a validated run output.
// A reset clears this attempt; a canonical channel message replaces previews.
// Identity of the tenant, run and turn comes from the observing execution context.
type TurnAnswerPreview struct {
	AttemptID string `json:"attemptId"`
	Sequence  uint64 `json:"sequence"`
	Text      string `json:"text"`
	Reset     bool   `json:"reset,omitempty"`
}

type TurnAnswerPreviewObserver func(context.Context, TurnAnswerPreview) error
type turnAnswerPreviewKey struct{}

func WithTurnAnswerPreview(ctx context.Context, observer TurnAnswerPreviewObserver) context.Context {
	return context.WithValue(ctx, turnAnswerPreviewKey{}, observer)
}

func HasTurnAnswerPreview(ctx context.Context) bool {
	observer, _ := ctx.Value(turnAnswerPreviewKey{}).(TurnAnswerPreviewObserver)
	return observer != nil
}

func ReportTurnAnswerPreview(ctx context.Context, preview TurnAnswerPreview) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := preview.Validate(); err != nil {
		return err
	}
	observer, _ := ctx.Value(turnAnswerPreviewKey{}).(TurnAnswerPreviewObserver)
	if observer == nil {
		return nil
	}
	return observer(ctx, preview)
}

func (preview TurnAnswerPreview) Validate() error {
	if preview.AttemptID == "" || len(preview.AttemptID) > 128 || preview.Sequence == 0 || len(preview.Text) > 64<<10 || !utf8.ValidString(preview.Text) || preview.Reset && preview.Text != "" {
		return errors.New("invalid turn answer preview")
	}
	return nil
}

// projectTurnAnswerPreview exposes only the typed public preview of a
// turn.answer_preview event, never arbitrary payload fields. It also
// reconstructs snapshots after a database JSON round-trip.
func ProjectTurnAnswerPreview(event *ActivityEvent) *TurnAnswerPreview {
	if event.EventType != "turn.answer_preview" || event.Visibility == ActivityVisibilityPrivate {
		return nil
	}
	data, err := json.Marshal(map[string]interface{}{
		"attemptId": event.Payload["attemptId"], "sequence": event.Payload["sequence"],
		"text": event.Payload["text"], "reset": event.Payload["reset"],
	})
	if err != nil {
		return nil
	}
	var preview TurnAnswerPreview
	if json.Unmarshal(data, &preview) != nil || preview.Validate() != nil {
		return nil
	}
	return &preview
}
