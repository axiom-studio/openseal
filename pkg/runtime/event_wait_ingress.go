package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// VerifiedRunEventObserver receives authenticated, credential-free connector
// observations. Services invoke it only after the exact installed adapter has
// verified the provider request. User-authored event routing does not use it.
type VerifiedRunEventObserver interface {
	Publish(context.Context, EventEnvelope) (bool, error)
}

// CallbackRunEventIntakeStore preserves a verified callback receipt and its
// workflow observation in one commit. A callback acknowledged before a wait's
// deadline must be available to that wait even if callback dispatch is delayed.
type CallbackRunEventIntakeStore interface {
	ReceiveCallbackEventWithRunEvent(context.Context, *CallbackEventReceipt, *RunEventReceipt) (*CallbackEventReceipt, bool, error)
}

type receivedRunEventObserver interface {
	PublishReceivedAt(context.Context, EventEnvelope, time.Time) (bool, error)
}

func runEventObserverForStore(store interface{}) VerifiedRunEventObserver {
	if eventStore, ok := store.(RunEventWaitStore); ok {
		return NewRunEventWaitService(eventStore)
	}
	return nil
}

// SetRunEventObserver configures verified connector intake before ingress
// starts. By default, constructors use the same persistent store when it
// implements RunEventWaitStore; no second event runtime is created.
func (s *ExternalConversationTransportService) SetRunEventObserver(observer VerifiedRunEventObserver) {
	if s != nil {
		s.eventObserver = observer
	}
}

// SetRunEventObserver configures the observer before callback workers start.
func (s *CallbackIngressService) SetRunEventObserver(observer VerifiedRunEventObserver) {
	if s != nil {
		s.eventObserver = observer
		// Custom observers own their acquisition guarantees. The constructor's
		// default observer is the one backed by the callback store transaction.
		s.atomicEventIntake = nil
	}
}

func publishVerifiedConversationEvent(
	ctx context.Context,
	observer VerifiedRunEventObserver,
	scope Scope,
	deploymentID string,
	adapter ExternalConversationAdapterReference,
	provider, installationID, applicationID string,
	event NormalizedExternalConversationEvent,
) error {
	if observer == nil {
		return nil
	}
	attributes := cloneMap(event.Attributes)
	if attributes == nil {
		attributes = make(map[string]interface{})
	}
	// These correlation and authority facts are authored by the verified
	// gateway/endpoint, never taken from adapter-supplied arbitrary attributes.
	attributes["deploymentId"] = deploymentID
	attributes["bindingId"] = adapter.BindingID
	attributes["adapterId"] = adapter.AdapterID
	attributes["provider"] = provider
	attributes["installationId"] = installationID
	attributes["applicationId"] = applicationID
	attributes["externalConversationId"] = event.ExternalConversationID
	attributes["externalThreadId"] = event.ExternalThreadID
	attributes["externalMessageId"] = event.ExternalMessageID
	attributes["externalParticipantId"] = event.ExternalParticipantID
	attributes["participantIsBot"] = event.ParticipantIsBot
	attributes["direct"] = event.Direct
	_, err := observer.Publish(ctx, EventEnvelope{
		ID: event.ID, Scope: scope, Type: event.Type,
		Source:  RunEventBindingSource(deploymentID, adapter.BindingID, adapter.AdapterID),
		Subject: event.ExternalConversationID, OccurredAt: event.OccurredAt,
		Attributes: attributes,
		Payload:    map[string]interface{}{"text": event.Text},
		Actor:      ActivityActor{Type: "connector", ID: adapter.BindingID},
	})
	if err != nil {
		return fmt.Errorf("persist verified conversation observation: %w", err)
	}
	return nil
}

func publishVerifiedCallbackEvent(
	ctx context.Context,
	observer VerifiedRunEventObserver,
	registration *CallbackRegistration,
	event EventEnvelope,
	receivedAt time.Time,
) error {
	// Adapters without resource subjects may still drive their existing
	// consumers. They cannot satisfy a resource-correlated wait until their
	// normalized contract supplies an explicit subject.
	if observer == nil || strings.TrimSpace(event.Subject) == "" {
		return nil
	}
	verified := verifiedCallbackRunEvent(registration, event)
	var err error
	if timedObserver, ok := observer.(receivedRunEventObserver); ok {
		_, err = timedObserver.PublishReceivedAt(ctx, verified, receivedAt)
	} else {
		_, err = observer.Publish(ctx, verified)
	}
	if err != nil {
		return fmt.Errorf("persist verified callback observation: %w", err)
	}
	return nil
}

func verifiedCallbackRunEvent(registration *CallbackRegistration, event EventEnvelope) EventEnvelope {
	ref := registration.Adapter
	event.Scope = registration.Scope
	event.Source = RunEventBindingSource(registration.DeploymentID, ref.BindingID, ref.AdapterID)
	event.Attributes = cloneMap(event.Attributes)
	if event.Attributes == nil {
		event.Attributes = make(map[string]interface{})
	}
	event.Attributes["deploymentId"] = registration.DeploymentID
	event.Attributes["bindingId"] = ref.BindingID
	event.Attributes["adapterId"] = ref.AdapterID
	event.Attributes["provider"] = registration.Provider
	// The same installed adapter may have several callback registrations.
	// Receipt provenance belongs to the callback registry, while immutable
	// provider observation identity remains stable across those registrations.
	event.Actor = ActivityActor{Type: "callback", ID: ref.BindingID}
	return event
}

// Both records in a combined intake must retain the same signed provider fact.
// Only host authority/provenance fields may differ during normalization.
func sameVerifiedCallbackObservation(provider, observation EventEnvelope) bool {
	expected := provider
	expected.Source = observation.Source
	expected.Attributes = cloneMap(provider.Attributes)
	if expected.Attributes == nil {
		expected.Attributes = make(map[string]interface{})
	}
	for _, key := range []string{"deploymentId", "bindingId", "adapterId", "provider"} {
		value, exists := observation.Attributes[key]
		if !exists {
			return false
		}
		expected.Attributes[key] = value
	}
	return sameCallbackProviderObservation(expected, observation)
}

func (s *CallbackIngressService) receiveVerifiedCallbackEvent(ctx context.Context, registration *CallbackRegistration, receipt *CallbackEventReceipt) (*CallbackEventReceipt, bool, error) {
	if s.eventObserver != nil && strings.TrimSpace(receipt.Event.Subject) != "" && s.atomicEventIntake != nil {
		observation, err := newRunEventReceipt(verifiedCallbackRunEvent(registration, receipt.Event), receipt.CreatedAt)
		if err != nil {
			return nil, false, err
		}
		return s.atomicEventIntake.ReceiveCallbackEventWithRunEvent(ctx, receipt, observation)
	}
	stored, replayed, err := s.store.ReceiveCallbackEvent(ctx, receipt)
	if err != nil {
		return nil, false, err
	}
	// External stores without the combined transaction receive no success
	// response until both durable writes succeed. A retry repairs a partial
	// write using the original receipt timestamp.
	if err := publishVerifiedCallbackEvent(ctx, s.eventObserver, registration, stored.Event, stored.CreatedAt); err != nil {
		return nil, false, err
	}
	return stored, replayed, nil
}
