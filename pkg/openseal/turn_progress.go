package openseal

import (
	"context"
	"github.com/axiom-studio/openseal/pkg/models"
)

type TurnProgressObserver = models.TurnProgressObserver

func WithTurnProgress(ctx context.Context, observer TurnProgressObserver) context.Context {
	return models.WithTurnProgress(ctx, observer)
}
func HasTurnProgress(ctx context.Context) bool { return models.HasTurnProgress(ctx) }
func ReportTurnProgress(ctx context.Context, summary string) error {
	return models.ReportTurnProgress(ctx, summary)
}

// WithTurnProgressOrigin marks the progress reported with ctx as the step
// that ref's nested calls (ActionCall.OriginRef) ran under.
func WithTurnProgressOrigin(ctx context.Context, ref string) context.Context {
	return models.WithTurnProgressOrigin(ctx, ref)
}

func IsTurnCommentary(ctx context.Context) bool { return models.IsTurnCommentary(ctx) }
func ReportTurnCommentary(ctx context.Context, summary string) error {
	return models.ReportTurnCommentary(ctx, summary)
}

// IsSupportedAgentManagementSkillVersion reports the agent management contracts
// the Agent action dispatcher still executes.
func IsSupportedAgentManagementSkillVersion(version string) bool {
	return models.IsSupportedAgentManagementSkillVersion(version)
}
