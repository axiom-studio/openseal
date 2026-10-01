package runtime

import (
	"context"
	"strings"
	"unicode/utf8"
)

// TurnProgressObserver receives user-visible summaries only, never raw model
// reasoning, partial tool arguments, credentials, or the structured turn form.
// Producers call it synchronously before returning their terminal result.
type TurnProgressObserver func(context.Context, string) error
type turnProgressKey struct{}

func WithTurnProgress(ctx context.Context, observer TurnProgressObserver) context.Context {
	return context.WithValue(ctx, turnProgressKey{}, observer)
}
func HasTurnProgress(ctx context.Context) bool {
	observer, _ := ctx.Value(turnProgressKey{}).(TurnProgressObserver)
	return observer != nil
}
func ReportTurnProgress(ctx context.Context, summary string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	observer, _ := ctx.Value(turnProgressKey{}).(TurnProgressObserver)
	summary = strings.TrimSpace(summary)
	if observer == nil || summary == "" {
		return nil
	}
	if len(summary) > 4096 {
		summary = summary[:4096]
		for !utf8.ValidString(summary) {
			summary = summary[:len(summary)-1]
		}
	}
	return observer(ctx, summary)
}

// Commentary shares progress delivery and lease fencing, but has its own
// durable event kind so clients can render a chat bubble beside the trail.
type turnCommentaryKey struct{}

func IsTurnCommentary(ctx context.Context) bool {
	marked, _ := ctx.Value(turnCommentaryKey{}).(bool)
	return marked
}

func ReportTurnCommentary(ctx context.Context, summary string) error {
	return ReportTurnProgress(context.WithValue(ctx, turnCommentaryKey{}, true), summary)
}
