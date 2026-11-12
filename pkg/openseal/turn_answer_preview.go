package openseal

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/models"
)

type TurnAnswerPreview = models.TurnAnswerPreview
type TurnAnswerPreviewObserver = models.TurnAnswerPreviewObserver

func WithTurnAnswerPreview(ctx context.Context, observer TurnAnswerPreviewObserver) context.Context {
	return models.WithTurnAnswerPreview(ctx, observer)
}
func HasTurnAnswerPreview(ctx context.Context) bool { return models.HasTurnAnswerPreview(ctx) }
func ReportTurnAnswerPreview(ctx context.Context, preview TurnAnswerPreview) error {
	return models.ReportTurnAnswerPreview(ctx, preview)
}
