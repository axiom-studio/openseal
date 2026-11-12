package runtime

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Test-only access to external conversation internals for the external
// routine offer test, which measures the offer together with the routine
// matcher (a package above the external conversation code) and so cannot
// be an in-package test of it.

func OfferConversationRoutineEvent(
	ctx context.Context,
	offerer RoutineEventOfferer,
	scope models.Scope,
	deploymentID string,
	adapter models.ExternalConversationAdapterReference,
	provider, installationID string,
	event *models.NormalizedExternalConversationEvent,
) error {
	return offerConversationRoutineEvent(ctx, offerer, scope, deploymentID, adapter, provider, installationID, event)
}
