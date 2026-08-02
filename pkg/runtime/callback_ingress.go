package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const MaximumCallbackIngressBytes = 1 << 20

type CallbackPublicRequest struct {
	Route   string              `json:"route"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body"`
}

func (r *CallbackPublicRequest) Validate() error {
	if r == nil || !validOpaqueIdentifier(strings.TrimSpace(r.Route), 128) || r.Method != http.MethodPost ||
		len(r.Body) == 0 || len(r.Body) > MaximumCallbackIngressBytes || len(r.Headers) > 64 {
		return ErrInvalidCallbackRegistration
	}
	for name, values := range r.Headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name == "" || name == "Authorization" || name == "Cookie" || len(name) > 128 || len(values) == 0 || len(values) > 16 {
			return ErrInvalidCallbackRegistration
		}
		for _, value := range values {
			if len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
				return ErrInvalidCallbackRegistration
			}
		}
	}
	return nil
}

// NormalizedCallbackEvent is the scope-free result of Skill-owned signature
// verification. The registry stamps authoritative scope and actor facts after
// verification, so a provider body cannot select another tenant or owner.
type NormalizedCallbackEvent struct {
	ID         string                 `json:"id"`
	Type       string                 `json:"type"`
	Source     string                 `json:"source"`
	Subject    string                 `json:"subject,omitempty"`
	Severity   string                 `json:"severity,omitempty"`
	OccurredAt time.Time              `json:"occurredAt"`
	Attributes map[string]interface{} `json:"attributes,omitempty"`
	Payload    map[string]interface{} `json:"payload,omitempty"`
}

func (e NormalizedCallbackEvent) envelope(registration *CallbackRegistration) EventEnvelope {
	return EventEnvelope{
		ID: e.ID, Scope: registration.Scope, Type: e.Type, Source: e.Source,
		Subject: e.Subject, Severity: e.Severity, OccurredAt: e.OccurredAt,
		Attributes: cloneMap(e.Attributes), Payload: cloneMap(e.Payload),
		Actor: ActivityActor{Type: "callback", ID: registration.ID},
	}
}

func (e NormalizedCallbackEvent) Validate() error {
	probe := EventEnvelope{
		ID: e.ID, Scope: Scope{Kind: "tenant", ID: "callback-validation"}, Type: e.Type,
		Source: e.Source, Subject: e.Subject, Severity: e.Severity, OccurredAt: e.OccurredAt,
		Attributes: e.Attributes, Payload: e.Payload, Actor: ActivityActor{Type: "callback", ID: "callback-validation"},
	}
	return probe.Validate()
}

type CallbackHostRequest struct {
	Registration *CallbackRegistration       `json:"registration"`
	Adapter      *skill.BoundCallbackAdapter `json:"adapter"`
	Request      *CallbackPublicRequest      `json:"request"`
}

type CallbackHostResult struct {
	StatusCode  int                       `json:"statusCode"`
	ContentType string                    `json:"contentType,omitempty"`
	Body        []byte                    `json:"body,omitempty"`
	Events      []NormalizedCallbackEvent `json:"events,omitempty"`
}

func (r *CallbackHostResult) Validate() error {
	if r == nil || r.StatusCode < 200 || r.StatusCode > 599 || len(r.ContentType) > 256 ||
		strings.ContainsAny(r.ContentType, "\r\n") || len(r.Body) > MaximumCallbackIngressBytes || len(r.Events) > 100 {
		return ErrInvalidCallbackRegistration
	}
	if r.StatusCode >= 300 && len(r.Events) > 0 {
		return ErrInvalidCallbackRegistration
	}
	for index, event := range r.Events {
		if err := event.Validate(); err != nil {
			return fmt.Errorf("%w: normalized event %d: %v", ErrInvalidCallbackRegistration, index, err)
		}
	}
	return nil
}

type CallbackAdapterHost interface {
	NormalizeCallback(context.Context, CallbackHostRequest) (*CallbackHostResult, error)
}

type CallbackEventReceiptStatus string

const (
	CallbackEventPending CallbackEventReceiptStatus = "pending"
	CallbackEventApplied CallbackEventReceiptStatus = "applied"
)

type CallbackEventReceipt struct {
	ID                   string                     `json:"id"`
	Scope                Scope                      `json:"scope"`
	RegistrationID       string                     `json:"registrationId"`
	RegistrationRevision int64                      `json:"registrationRevision"`
	Event                EventEnvelope              `json:"event"`
	Status               CallbackEventReceiptStatus `json:"status"`
	Attempts             int                        `json:"attempts"`
	LastError            string                     `json:"lastError,omitempty"`
	Revision             int64                      `json:"revision"`
	CreatedAt            time.Time                  `json:"createdAt"`
	UpdatedAt            time.Time                  `json:"updatedAt"`
	AppliedAt            *time.Time                 `json:"appliedAt,omitempty"`
}

func (r *CallbackEventReceipt) Validate() error {
	if r == nil || !validOpaqueIdentifier(r.ID, 256) || r.Scope.Validate() != nil ||
		!validOpaqueIdentifier(r.RegistrationID, 256) || r.RegistrationRevision < 1 ||
		r.Event.Scope != r.Scope || r.Event.Validate() != nil || r.Attempts < 0 || r.Attempts > 1000 ||
		r.Revision < 1 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || len(r.LastError) > 2000 {
		return ErrInvalidCallbackRegistration
	}
	switch r.Status {
	case CallbackEventPending:
		if r.AppliedAt != nil {
			return ErrInvalidCallbackRegistration
		}
	case CallbackEventApplied:
		if r.AppliedAt == nil || r.AppliedAt.Before(r.CreatedAt) {
			return ErrInvalidCallbackRegistration
		}
	default:
		return ErrInvalidCallbackRegistration
	}
	return nil
}

type CallbackEventStore interface {
	ReceiveCallbackEvent(context.Context, *CallbackEventReceipt) (*CallbackEventReceipt, bool, error)
	UpdateCallbackEvent(context.Context, *CallbackEventReceipt, int64) error
}

type CallbackEventConsumer interface {
	ConsumeCallbackEvent(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error
}

type CallbackEventConsumerFunc func(context.Context, *CallbackRegistration, CallbackSubscription, EventEnvelope) error

func (f CallbackEventConsumerFunc) ConsumeCallbackEvent(ctx context.Context, registration *CallbackRegistration, subscription CallbackSubscription, event EventEnvelope) error {
	return f(ctx, registration, subscription, event)
}

type CallbackIngressResult struct {
	Response *CallbackHostResult     `json:"response"`
	Receipts []*CallbackEventReceipt `json:"receipts,omitempty"`
	Replayed int                     `json:"replayed,omitempty"`
}

type CallbackIngressService struct {
	store interface {
		CallbackRegistrationStore
		CallbackEventStore
	}
	resolver  CallbackAdapterResolver
	consumers map[string]CallbackEventConsumer
	now       func() time.Time
}

func NewCallbackIngressService(store interface {
	CallbackRegistrationStore
	CallbackEventStore
}, resolver CallbackAdapterResolver, consumers map[string]CallbackEventConsumer) *CallbackIngressService {
	copyConsumers := make(map[string]CallbackEventConsumer, len(consumers))
	for name, consumer := range consumers {
		copyConsumers[strings.TrimSpace(name)] = consumer
	}
	return &CallbackIngressService{store: store, resolver: resolver, consumers: copyConsumers, now: time.Now}
}

func (s *CallbackIngressService) Receive(ctx context.Context, request CallbackPublicRequest, host CallbackAdapterHost) (*CallbackIngressResult, error) {
	if s == nil || s.store == nil || s.resolver == nil || host == nil {
		return nil, errors.New("callback ingress is not configured")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	registration, err := s.store.GetCallbackRegistrationByIngressRoute(ctx, strings.TrimSpace(request.Route))
	if err != nil {
		return nil, err
	}
	if registration == nil {
		return nil, ErrCallbackRegistrationNotFound
	}
	if err := registration.Validate(); err != nil {
		return nil, err
	}
	if registration.Status != CallbackRegistrationActive {
		return nil, fmt.Errorf("%w: callback is not active", ErrCallbackRegistrationConflict)
	}
	ref := registration.Adapter
	adapter, err := s.resolver.ResolveCallbackAdapterBinding(
		ctx, skill.ScopeReference{Kind: registration.Scope.Kind, ID: registration.Scope.ID}, registration.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil || adapter == nil || adapter.Binding == nil ||
		adapter.Adapter.Provider != registration.Provider {
		return nil, fmt.Errorf("%w: exact callback Skill adapter is unavailable or stale", ErrCallbackRegistrationConflict)
	}
	hostResult, err := host.NormalizeCallback(ctx, CallbackHostRequest{Registration: registration, Adapter: adapter, Request: &request})
	if err != nil {
		return nil, err
	}
	if err := hostResult.Validate(); err != nil {
		return nil, err
	}
	declared := make(map[string]bool, len(adapter.Adapter.EventTypes))
	for _, eventType := range adapter.Adapter.EventTypes {
		declared[eventType] = true
	}
	accepted := make(map[string]bool, len(registration.Subscriptions))
	for _, subscription := range registration.Subscriptions {
		accepted[subscription.EventType] = true
	}
	result := &CallbackIngressResult{Response: hostResult, Receipts: make([]*CallbackEventReceipt, 0, len(hostResult.Events))}
	for _, normalized := range hostResult.Events {
		if !declared[normalized.Type] || !accepted[normalized.Type] {
			return nil, fmt.Errorf("%w: callback emitted undeclared event type %q", ErrCallbackRegistrationConflict, normalized.Type)
		}
		event := normalized.envelope(registration)
		now := s.now().UTC()
		receipt := &CallbackEventReceipt{
			ID: stableCallbackReceiptID(registration.ID, event.Source, event.ID), Scope: registration.Scope,
			RegistrationID: registration.ID, RegistrationRevision: registration.Revision, Event: event,
			Status: CallbackEventPending, Revision: 1, CreatedAt: now, UpdatedAt: now,
		}
		stored, replayed, err := s.store.ReceiveCallbackEvent(ctx, receipt)
		if err != nil {
			return nil, err
		}
		if replayed && stored.Status == CallbackEventApplied {
			result.Replayed++
			result.Receipts = append(result.Receipts, stored)
			continue
		}
		if err := s.dispatch(ctx, registration, event); err != nil {
			stored.Attempts++
			stored.LastError = boundedCallbackError(err)
			stored.Revision++
			stored.UpdatedAt = now
			if updateErr := s.store.UpdateCallbackEvent(ctx, stored, stored.Revision-1); updateErr != nil {
				return nil, updateErr
			}
			return nil, err
		}
		stored.Attempts++
		stored.LastError = ""
		stored.Status = CallbackEventApplied
		stored.Revision++
		stored.UpdatedAt = now
		stored.AppliedAt = &now
		if err := s.store.UpdateCallbackEvent(ctx, stored, stored.Revision-1); err != nil {
			return nil, err
		}
		result.Receipts = append(result.Receipts, stored)
	}
	return result, nil
}

func (s *CallbackIngressService) dispatch(ctx context.Context, registration *CallbackRegistration, event EventEnvelope) error {
	matched := 0
	for _, subscription := range registration.Subscriptions {
		if subscription.EventType != event.Type {
			continue
		}
		consumer := s.consumers[subscription.Consumer]
		if consumer == nil {
			return fmt.Errorf("callback consumer %q is unavailable", subscription.Consumer)
		}
		matched++
		if err := consumer.ConsumeCallbackEvent(ctx, registration, subscription, event); err != nil {
			return fmt.Errorf("callback consumer %s: %w", subscription.Consumer, err)
		}
	}
	if matched == 0 {
		return fmt.Errorf("callback event %q has no registered consumer", event.Type)
	}
	return nil
}

func stableCallbackReceiptID(registrationID, source, eventID string) string {
	digest := sha256.Sum256([]byte(registrationID + "\x00" + source + "\x00" + eventID))
	return "callback-" + hex.EncodeToString(digest[:16])
}

func boundedCallbackError(err error) string {
	if err == nil {
		return ""
	}
	value := strings.TrimSpace(err.Error())
	if len(value) > 2000 {
		value = value[:2000]
	}
	return value
}
