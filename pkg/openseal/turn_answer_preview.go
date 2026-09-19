package openseal

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type TurnAnswerPreview = runtime.TurnAnswerPreview
type TurnAnswerPreviewObserver = runtime.TurnAnswerPreviewObserver

func WithTurnAnswerPreview(ctx context.Context, observer TurnAnswerPreviewObserver) context.Context {
	return runtime.WithTurnAnswerPreview(ctx, observer)
}
func HasTurnAnswerPreview(ctx context.Context) bool { return runtime.HasTurnAnswerPreview(ctx) }
func ReportTurnAnswerPreview(ctx context.Context, preview TurnAnswerPreview) error {
	return runtime.ReportTurnAnswerPreview(ctx, preview)
}
