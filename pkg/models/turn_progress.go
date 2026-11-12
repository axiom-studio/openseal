package models

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

// A step's origin reference ties it to the nested ActionCalls made while it
// ran (their OriginRef), such as the calls a workspace command made from the
// chat's VM, so clients can show those calls under the step.
type turnProgressOriginKey struct{}

// MaxTurnProgressOriginRefBytes bounds a step's origin reference.
const MaxTurnProgressOriginRefBytes = 128

// WithTurnProgressOrigin marks the progress reported with ctx as the step
// that ref's nested calls ran under.
func WithTurnProgressOrigin(ctx context.Context, ref string) context.Context {
	return context.WithValue(ctx, turnProgressOriginKey{}, strings.TrimSpace(ref))
}

// TurnProgressOrigin is the origin reference WithTurnProgressOrigin set, or
// "" when none or when it is too long to be one.
func TurnProgressOrigin(ctx context.Context) string {
	ref, _ := ctx.Value(turnProgressOriginKey{}).(string)
	if len(ref) > MaxTurnProgressOriginRefBytes || !utf8.ValidString(ref) {
		return ""
	}
	return ref
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
