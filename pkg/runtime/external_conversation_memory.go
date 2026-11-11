package runtime

import (
	"bytes"
	"encoding/json"
	"reflect"
)

func externalConversationEndpointMatchesVerifiedRoute(
	endpoint *ExternalConversationEndpoint,
	route ExternalConversationVerifiedRoute,
) bool {
	if endpoint == nil || endpoint.Status != ExternalConversationEndpointActive ||
		endpoint.Provider != route.Provider || endpoint.InstallationID != route.InstallationID ||
		(!endpoint.InstallationWide && endpoint.Address != "" && endpoint.Address != route.Address) || endpoint.Adapter.SkillID != route.SkillID ||
		endpoint.Adapter.SkillVersion != route.SkillVersion ||
		endpoint.Adapter.SourceIdentity != route.SourceIdentity ||
		endpoint.Adapter.AdapterID != route.AdapterID {
		return false
	}
	// The endpoint owns the optional application pin. A missing event identity
	// must not bypass an explicit pin; unpinned endpoints are further restricted
	// to their own authenticated gateway binding by the ingress service.
	return endpoint.ApplicationID == "" || endpoint.ApplicationID == route.ApplicationID
}

func sameExternalConversationInboxIntent(existing, candidate *ExternalConversationInboxItem) bool {
	if existing == nil || candidate == nil {
		return false
	}
	before, after := existing.Adapter, candidate.Adapter
	// A provider retry after a configuration save is still the same event.
	// Replay the original receipt without replacing its execution snapshot.
	before.BindingRevision, after.BindingRevision = 0, 0
	return existing.Scope == candidate.Scope && existing.EndpointID == candidate.EndpointID &&
		existing.EndpointRevision == candidate.EndpointRevision && before == after && reflect.DeepEqual(existing.Event, candidate.Event)
}

func sameExternalConversationDeliveryIntent(existing, candidate *ExternalConversationDelivery) bool {
	return existing != nil && candidate != nil && existing.Scope == candidate.Scope &&
		existing.EndpointID == candidate.EndpointID && existing.EndpointRevision == candidate.EndpointRevision &&
		existing.Adapter == candidate.Adapter && sameExternalConversationDeliveryEffect(existing, candidate)
}

// sameExternalConversationDeliveryEffect compares the externally observable
// operation selected by an idempotency key. Endpoint and adapter revisions are
// execution snapshots, not part of that operation: once a provider confirms a
// delivery, a compatible endpoint upgrade must replay the immutable receipt
// instead of attempting the same side effect again.
func sameExternalConversationDeliveryEffect(existing, candidate *ExternalConversationDelivery) bool {
	return existing != nil && candidate != nil && existing.Scope == candidate.Scope &&
		existing.EndpointID == candidate.EndpointID && existing.Operation == candidate.Operation &&
		existing.ConversationID == candidate.ConversationID && existing.ChannelMessageID == candidate.ChannelMessageID &&
		existing.ExternalThreadID == candidate.ExternalThreadID && existing.OrderingKey == candidate.OrderingKey &&
		sameExternalConversationJSON(existing.Parameters, candidate.Parameters) &&
		sameExternalConversationJSON(existing.Correlation, candidate.Correlation) && existing.IdempotencyKey == candidate.IdempotencyKey
}

func rebindExternalConversationDelivery(existing, candidate *ExternalConversationDelivery) (*ExternalConversationDelivery, bool) {
	if existing == nil || candidate == nil || existing.Scope != candidate.Scope ||
		existing.EndpointID != candidate.EndpointID || existing.Operation != candidate.Operation ||
		existing.ConversationID != candidate.ConversationID || existing.ChannelMessageID != candidate.ChannelMessageID ||
		existing.ExternalThreadID != candidate.ExternalThreadID || existing.OrderingKey != candidate.OrderingKey ||
		!sameExternalConversationJSON(existing.Parameters, candidate.Parameters) ||
		!sameExternalConversationJSON(existing.Correlation, candidate.Correlation) || existing.IdempotencyKey != candidate.IdempotencyKey ||
		(existing.Status != ExternalConversationDeliveryPending && existing.Status != ExternalConversationDeliveryRetry &&
			existing.Status != ExternalConversationDeliveryFailed) ||
		existing.ProviderMessageID != "" || !existing.DeliveredAt.IsZero() || existing.LeaseOwner != "" || !existing.LeaseExpiresAt.IsZero() {
		return nil, false
	}
	rebound := cloneExternalConversationDelivery(existing)
	rebound.EndpointRevision = candidate.EndpointRevision
	rebound.Adapter = candidate.Adapter
	rebound.Status = ExternalConversationDeliveryPending
	rebound.AvailableAt = candidate.AvailableAt
	rebound.ErrorCode = ""
	rebound.Summary = ""
	rebound.Revision++
	rebound.UpdatedAt = candidate.UpdatedAt
	if rebound.Validate() != nil {
		return nil, false
	}
	return rebound, true
}

func sameExternalConversationJSON(left, right interface{}) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
