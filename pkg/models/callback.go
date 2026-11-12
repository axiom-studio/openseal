package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type CallbackEventReceiptStatus string

const (
	CallbackEventPending CallbackEventReceiptStatus = "pending"
	CallbackEventLeased  CallbackEventReceiptStatus = "leased"
	CallbackEventApplied CallbackEventReceiptStatus = "applied"
	CallbackEventFailed  CallbackEventReceiptStatus = "failed"
)

type CallbackEventReceipt struct {
	ID                   string                     `json:"id"`
	Scope                Scope                      `json:"scope"`
	RegistrationID       string                     `json:"registrationId"`
	RegistrationRevision int64                      `json:"registrationRevision"`
	Event                EventEnvelope              `json:"event"`
	Status               CallbackEventReceiptStatus `json:"status"`
	Attempts             int                        `json:"attempts"`
	AvailableAt          time.Time                  `json:"availableAt"`
	LeaseOwner           string                     `json:"leaseOwner,omitempty"`
	LeaseExpiresAt       time.Time                  `json:"leaseExpiresAt,omitempty"`
	LastError            string                     `json:"lastError,omitempty"`
	Revision             int64                      `json:"revision"`
	CreatedAt            time.Time                  `json:"createdAt"`
	UpdatedAt            time.Time                  `json:"updatedAt"`
	AppliedAt            *time.Time                 `json:"appliedAt,omitempty"`
}

// Preserve the exact signed provider data through callback receipt clones and
// database reloads. Only this durable receipt contract opts in to json.Number;
// unrelated kernel JSON decoding keeps its existing representation.
func (r *CallbackEventReceipt) UnmarshalJSON(data []byte) error {
	type plain CallbackEventReceipt
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	*r = CallbackEventReceipt(value)
	return nil
}

func (r *CallbackEventReceipt) Validate() error {
	if r == nil || !IsValidOpaqueIdentifier(r.ID, 256) || r.Scope.Validate() != nil ||
		!IsValidOpaqueIdentifier(r.RegistrationID, 256) || r.RegistrationRevision < 1 ||
		r.Event.Scope != r.Scope || r.Event.Validate() != nil || r.Attempts < 0 || r.Attempts > 1000 ||
		r.Revision < 1 || r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || len(r.LastError) > 2000 {
		return ErrInvalidCallbackRegistration
	}
	switch r.Status {
	case CallbackEventPending:
		if r.AvailableAt.IsZero() || r.LeaseOwner != "" || !r.LeaseExpiresAt.IsZero() || r.AppliedAt != nil {
			return ErrInvalidCallbackRegistration
		}
	case CallbackEventLeased:
		if r.AvailableAt.IsZero() || !IsValidOpaqueIdentifier(r.LeaseOwner, 256) || r.LeaseExpiresAt.IsZero() || r.AppliedAt != nil {
			return ErrInvalidCallbackRegistration
		}
	case CallbackEventApplied:
		if r.LeaseOwner != "" || !r.LeaseExpiresAt.IsZero() || r.AppliedAt == nil || r.AppliedAt.Before(r.CreatedAt) {
			return ErrInvalidCallbackRegistration
		}
	case CallbackEventFailed:
		if r.LeaseOwner != "" || !r.LeaseExpiresAt.IsZero() || r.AppliedAt != nil || strings.TrimSpace(r.LastError) == "" {
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
	ClaimCallbackEvent(context.Context, Scope, string, time.Time, time.Duration) (*CallbackEventReceipt, error)
	SaveClaimedCallbackEvent(context.Context, *CallbackEventReceipt, int64, string) error
}

func CloneCallbackEventReceipt(value *CallbackEventReceipt) *CallbackEventReceipt {
	if value == nil {
		return nil
	}
	var result CallbackEventReceipt
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}

var (
	ErrCallbackRegistrationNotFound = errors.New("callback registration not found")
	ErrInvalidCallbackRegistration  = errors.New("invalid callback registration")
	ErrCallbackRegistrationConflict = errors.New("callback registration conflict")
)

type CallbackRegistrationStatus string

const (
	CallbackRegistrationActive  CallbackRegistrationStatus = "active"
	CallbackRegistrationPaused  CallbackRegistrationStatus = "paused"
	CallbackRegistrationRetired CallbackRegistrationStatus = "retired"
)

type CallbackRegistrationLifecycleAction string

const (
	CallbackRegistrationCreated       CallbackRegistrationLifecycleAction = "created"
	CallbackRegistrationActivated     CallbackRegistrationLifecycleAction = "activated"
	CallbackRegistrationPausedAction  CallbackRegistrationLifecycleAction = "paused"
	CallbackRegistrationRetiredAction CallbackRegistrationLifecycleAction = "retired"
	CallbackRegistrationUpdated       CallbackRegistrationLifecycleAction = "updated"
)

type CallbackAdapterReference struct {
	SkillID         string `json:"skillId"`
	SkillVersion    string `json:"skillVersion"`
	SourceIdentity  string `json:"sourceIdentity,omitempty"`
	BindingID       string `json:"bindingId"`
	BindingRevision int64  `json:"bindingRevision"`
	AdapterID       string `json:"adapterId"`
}

func (r CallbackAdapterReference) Validate() error {
	for _, value := range []string{r.SkillID, r.SkillVersion, r.BindingID, r.AdapterID} {
		if !IsValidOpaqueIdentifier(strings.TrimSpace(value), 256) {
			return ErrInvalidCallbackRegistration
		}
	}
	if r.BindingRevision < 1 || len(r.SourceIdentity) > 2048 || strings.ContainsAny(r.SourceIdentity, "\x00\r\n") {
		return ErrInvalidCallbackRegistration
	}
	return nil
}

// CallbackSubscription selects a named, trusted kernel consumer for one
// normalized event type. TargetID is consumer-specific but never comes from
// the unverified provider request.
type CallbackSubscription struct {
	EventType string `json:"eventType"`
	Consumer  string `json:"consumer"`
	TargetID  string `json:"targetId,omitempty"`
}

type CallbackRegistrationLifecycleEntry struct {
	Revision int64                               `json:"revision"`
	Action   CallbackRegistrationLifecycleAction `json:"action"`
	Actor    ActivityActor                       `json:"actor"`
	Reason   string                              `json:"reason"`
	At       time.Time                           `json:"at"`
}

// CallbackRegistration is one durable, host-owned public callback route. The
// route is an opaque locator, never an authentication secret. Authenticity is
// established only by the exact Skill adapter and binding revision pinned
// here.
type CallbackRegistration struct {
	ID            string                               `json:"id"`
	IngressRoute  string                               `json:"ingressRoute"`
	Scope         Scope                                `json:"scope"`
	Owner         ObjectiveOwner                       `json:"owner"`
	DeploymentID  string                               `json:"deploymentId"`
	Name          string                               `json:"name"`
	Provider      string                               `json:"provider"`
	Adapter       CallbackAdapterReference             `json:"adapter"`
	Subscriptions []CallbackSubscription               `json:"subscriptions"`
	Configuration map[string]interface{}               `json:"configuration,omitempty"`
	Status        CallbackRegistrationStatus           `json:"status"`
	Revision      int64                                `json:"revision"`
	CreatedAt     time.Time                            `json:"createdAt"`
	UpdatedAt     time.Time                            `json:"updatedAt"`
	RetiredAt     *time.Time                           `json:"retiredAt,omitempty"`
	Lifecycle     []CallbackRegistrationLifecycleEntry `json:"lifecycle"`
}

func (r *CallbackRegistration) Validate() error {
	if r == nil || !IsValidOpaqueIdentifier(strings.TrimSpace(r.ID), 256) ||
		!IsValidOpaqueIdentifier(strings.TrimSpace(r.IngressRoute), 128) || r.Scope.Validate() != nil ||
		r.Owner.Validate() != nil || !IsValidAgentReference(strings.TrimSpace(r.DeploymentID), 256) ||
		strings.TrimSpace(r.Name) == "" || len(r.Name) > 160 ||
		!IsValidOpaqueIdentifier(strings.TrimSpace(r.Provider), 128) || r.Revision < 1 ||
		r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) || len(r.Lifecycle) == 0 {
		return ErrInvalidCallbackRegistration
	}
	if err := r.Adapter.Validate(); err != nil {
		return err
	}
	if err := validateCallbackSubscriptions(r.Subscriptions); err != nil {
		return err
	}
	if err := validateCallbackConfiguration(r.Configuration); err != nil {
		return err
	}
	var previous int64
	for index, entry := range r.Lifecycle {
		if entry.Revision < 1 || (index > 0 && entry.Revision <= previous) ||
			strings.TrimSpace(entry.Actor.Type) == "" || strings.TrimSpace(entry.Actor.ID) == "" ||
			strings.TrimSpace(entry.Reason) == "" || len(entry.Reason) > 2000 || entry.At.IsZero() {
			return fmt.Errorf("%w: lifecycle entry is invalid", ErrInvalidCallbackRegistration)
		}
		previous = entry.Revision
	}
	if previous != r.Revision {
		return fmt.Errorf("%w: lifecycle revision does not match", ErrInvalidCallbackRegistration)
	}
	switch r.Status {
	case CallbackRegistrationActive, CallbackRegistrationPaused:
		if r.RetiredAt != nil {
			return fmt.Errorf("%w: non-retired callback has retiredAt", ErrInvalidCallbackRegistration)
		}
	case CallbackRegistrationRetired:
		if r.RetiredAt == nil || r.RetiredAt.Before(r.CreatedAt) {
			return fmt.Errorf("%w: retired callback requires retiredAt", ErrInvalidCallbackRegistration)
		}
	default:
		return fmt.Errorf("%w: status is invalid", ErrInvalidCallbackRegistration)
	}
	return nil
}

func validateCallbackSubscriptions(values []CallbackSubscription) error {
	if len(values) == 0 || len(values) > 64 {
		return fmt.Errorf("%w: subscriptions are required", ErrInvalidCallbackRegistration)
	}
	seen := make(map[string]bool, len(values))
	previous := ""
	for _, value := range values {
		key := strings.TrimSpace(value.EventType) + "\x00" + strings.TrimSpace(value.Consumer) + "\x00" + strings.TrimSpace(value.TargetID)
		if !validCallbackEventSelector(value.EventType) || !IsValidOpaqueIdentifier(value.Consumer, 128) ||
			(value.TargetID != "" && !IsValidOpaqueIdentifier(value.TargetID, 512)) || seen[key] || (previous != "" && key < previous) {
			return fmt.Errorf("%w: subscriptions must be unique and canonically ordered", ErrInvalidCallbackRegistration)
		}
		seen[key] = true
		previous = key
	}
	return nil
}

func validCallbackEventSelector(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "*" || len(value) > 256 || strings.HasPrefix(value, ".") || strings.HasSuffix(value, ".") {
		return false
	}
	for _, part := range strings.Split(value, ".") {
		if !IsValidOpaqueIdentifier(part, 128) {
			return false
		}
	}
	return true
}

func validateCallbackConfiguration(value map[string]interface{}) error {
	if err := ValidateCredentialFreeContext(value); err != nil {
		return fmt.Errorf("%w: configuration: %v", ErrInvalidCallbackRegistration, err)
	}
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) > 64<<10 {
		return fmt.Errorf("%w: configuration must be valid JSON no larger than 64 KiB", ErrInvalidCallbackRegistration)
	}
	return nil
}

type CallbackRegistrationFilter struct {
	Scope        Scope
	DeploymentID string
	Provider     string
	Statuses     []CallbackRegistrationStatus
	Limit        int
	Offset       int
}

type CallbackRegistrationStore interface {
	CreateCallbackRegistration(context.Context, *CallbackRegistration) error
	GetCallbackRegistration(context.Context, Scope, string) (*CallbackRegistration, error)
	GetCallbackRegistrationByIngressRoute(context.Context, string) (*CallbackRegistration, error)
	ListCallbackRegistrations(context.Context, CallbackRegistrationFilter) ([]*CallbackRegistration, error)
	UpdateCallbackRegistration(context.Context, *CallbackRegistration, int64) error
}

func CloneCallbackRegistration(value *CallbackRegistration) *CallbackRegistration {
	if value == nil {
		return nil
	}
	var result CallbackRegistration
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}
