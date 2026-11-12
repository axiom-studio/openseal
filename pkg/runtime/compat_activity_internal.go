// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/runtime/activity. Remove once in-flight branches have landed.
//
// Unexported names that runtime code still uses for declarations moved to
// pkg/runtime/activity.

package runtime

import (
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
)

// externalMessageSource forwards to activity.NewExternalMessageSource.
func externalMessageSource(endpoint *models.ExternalConversationEndpoint, event models.NormalizedExternalConversationEvent) *models.ExternalMessageSource {
	return activity.NewExternalMessageSource(endpoint, event)
}

// canonicalExternalOperationResource forwards to activity.CanonicalExternalOperationResource.
func canonicalExternalOperationResource(value string) (string, error) {
	return activity.CanonicalExternalOperationResource(value)
}

// computeExternalOperationDigest forwards to activity.ComputeExternalOperationDigest.
func computeExternalOperationDigest(scope models.Scope, owner models.ObjectiveOwner, objectiveID string, identity *ExternalOperationIdentity, idempotencyKey string) (string, error) {
	return activity.ComputeExternalOperationDigest(scope, owner, objectiveID, identity, idempotencyKey)
}
