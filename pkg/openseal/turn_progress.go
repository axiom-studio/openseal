package openseal

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type TurnProgressObserver = runtime.TurnProgressObserver

func WithTurnProgress(ctx context.Context, observer TurnProgressObserver) context.Context {
	return runtime.WithTurnProgress(ctx, observer)
}
func HasTurnProgress(ctx context.Context) bool { return runtime.HasTurnProgress(ctx) }
func ReportTurnProgress(ctx context.Context, summary string) error {
	return runtime.ReportTurnProgress(ctx, summary)
}
