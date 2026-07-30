package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

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
		if !validOpaqueIdentifier(strings.TrimSpace(value), 256) {
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
	if r == nil || !validOpaqueIdentifier(strings.TrimSpace(r.ID), 256) ||
		!validOpaqueIdentifier(strings.TrimSpace(r.IngressRoute), 128) || r.Scope.Validate() != nil ||
		r.Owner.Validate() != nil || !validAgentReference(strings.TrimSpace(r.DeploymentID), 256) ||
		strings.TrimSpace(r.Name) == "" || len(r.Name) > 160 ||
		!validOpaqueIdentifier(strings.TrimSpace(r.Provider), 128) || r.Revision < 1 ||
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
		if !validCallbackEventSelector(value.EventType) || !validOpaqueIdentifier(value.Consumer, 128) ||
			(value.TargetID != "" && !validOpaqueIdentifier(value.TargetID, 512)) || seen[key] || (previous != "" && key < previous) {
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
		if !validOpaqueIdentifier(part, 128) {
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
	Scope    Scope
	Provider string
	Statuses []CallbackRegistrationStatus
	Limit    int
	Offset   int
}

type CreateCallbackRegistrationRequest struct {
	ID            string
	Scope         Scope
	Owner         ObjectiveOwner
	DeploymentID  string
	Name          string
	Provider      string
	Adapter       CallbackAdapterReference
	Subscriptions []CallbackSubscription
	Configuration map[string]interface{}
	Actor         ActivityActor
	Reason        string
}

type UpdateCallbackRegistrationRequest struct {
	ExpectedRevision int64
	Adapter          *CallbackAdapterReference
	Name             *string
	Subscriptions    []CallbackSubscription
	Configuration    map[string]interface{}
	Status           *CallbackRegistrationStatus
	Actor            ActivityActor
	Reason           string
}

type CallbackRegistrationStore interface {
	CreateCallbackRegistration(context.Context, *CallbackRegistration) error
	GetCallbackRegistration(context.Context, Scope, string) (*CallbackRegistration, error)
	GetCallbackRegistrationByIngressRoute(context.Context, string) (*CallbackRegistration, error)
	ListCallbackRegistrations(context.Context, CallbackRegistrationFilter) ([]*CallbackRegistration, error)
	UpdateCallbackRegistration(context.Context, *CallbackRegistration, int64) error
}

type CallbackAdapterResolver interface {
	ResolveCallbackAdapter(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundCallbackAdapter, error)
}

type CallbackRegistry struct {
	store    CallbackRegistrationStore
	resolver CallbackAdapterResolver
	now      func() time.Time
	newID    func() string
}

func NewCallbackRegistry(store CallbackRegistrationStore, resolver CallbackAdapterResolver) *CallbackRegistry {
	return &CallbackRegistry{store: store, resolver: resolver, now: time.Now, newID: uuid.NewString}
}

func (r *CallbackRegistry) Create(ctx context.Context, request CreateCallbackRegistrationRequest) (*CallbackRegistration, error) {
	if r == nil || r.store == nil || r.resolver == nil {
		return nil, errors.New("callback registry is not configured")
	}
	if err := validateCallbackMutation(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	now := r.now().UTC()
	value := &CallbackRegistration{
		ID: strings.TrimSpace(request.ID), IngressRoute: r.newID(), Scope: request.Scope, Owner: request.Owner,
		DeploymentID: strings.TrimSpace(request.DeploymentID), Name: strings.TrimSpace(request.Name),
		Provider: strings.TrimSpace(request.Provider), Adapter: request.Adapter,
		Subscriptions: cloneCallbackSubscriptions(request.Subscriptions), Configuration: cloneMap(request.Configuration),
		Status: CallbackRegistrationPaused, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []CallbackRegistrationLifecycleEntry{{
			Revision: 1, Action: CallbackRegistrationCreated, Actor: request.Actor,
			Reason: strings.TrimSpace(request.Reason), At: now,
		}},
	}
	if value.ID == "" {
		value.ID = r.newID()
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	if err := r.resolve(ctx, value); err != nil {
		return nil, err
	}
	if err := r.store.CreateCallbackRegistration(ctx, value); err != nil {
		return nil, err
	}
	return cloneCallbackRegistration(value), nil
}

func (r *CallbackRegistry) Get(ctx context.Context, scope Scope, id string) (*CallbackRegistration, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("callback registry is not configured")
	}
	value, err := r.store.GetCallbackRegistration(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrCallbackRegistrationNotFound
	}
	return value, nil
}

func (r *CallbackRegistry) List(ctx context.Context, filter CallbackRegistrationFilter) ([]*CallbackRegistration, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("callback registry is not configured")
	}
	return r.store.ListCallbackRegistrations(ctx, filter)
}

func (r *CallbackRegistry) Update(ctx context.Context, scope Scope, id string, request UpdateCallbackRegistrationRequest) (*CallbackRegistration, error) {
	current, err := r.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Status == CallbackRegistrationRetired || request.ExpectedRevision != current.Revision {
		return nil, ErrCallbackRegistrationConflict
	}
	if err := validateCallbackMutation(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	previousStatus := current.Status
	if request.Adapter != nil {
		current.Adapter = *request.Adapter
	}
	if request.Name != nil {
		current.Name = strings.TrimSpace(*request.Name)
	}
	if request.Subscriptions != nil {
		current.Subscriptions = cloneCallbackSubscriptions(request.Subscriptions)
	}
	if request.Configuration != nil {
		current.Configuration = cloneMap(request.Configuration)
	}
	if request.Status != nil {
		current.Status = *request.Status
	}
	now := r.now().UTC()
	if current.Status == CallbackRegistrationRetired {
		current.RetiredAt = &now
	}
	current.Revision++
	current.UpdatedAt = now
	current.Lifecycle = append(current.Lifecycle, CallbackRegistrationLifecycleEntry{
		Revision: current.Revision, Action: callbackLifecycleAction(previousStatus, request.Status),
		Actor: request.Actor, Reason: strings.TrimSpace(request.Reason), At: now,
	})
	if err := current.Validate(); err != nil {
		return nil, err
	}
	if current.Status == CallbackRegistrationActive {
		if err := r.resolve(ctx, current); err != nil {
			return nil, err
		}
	}
	if err := r.store.UpdateCallbackRegistration(ctx, current, request.ExpectedRevision); err != nil {
		return nil, err
	}
	return cloneCallbackRegistration(current), nil
}

func (r *CallbackRegistry) resolve(ctx context.Context, value *CallbackRegistration) error {
	ref := value.Adapter
	bound, err := r.resolver.ResolveCallbackAdapter(
		ctx, skill.ScopeReference{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.DeploymentID,
		ref.SkillID, ref.SkillVersion, ref.AdapterID,
		skill.BindingReference{ID: ref.BindingID, Revision: ref.BindingRevision},
	)
	if err != nil || bound == nil || bound.Binding == nil || bound.Binding.SourceIdentity != ref.SourceIdentity ||
		bound.Adapter.Provider != value.Provider || strings.TrimSpace(bound.Adapter.Transport.IngressEndpoint) == "" {
		return fmt.Errorf("%w: exact callback Skill adapter is unavailable or stale", ErrInvalidCallbackRegistration)
	}
	accepted := make(map[string]bool, len(bound.Adapter.EventTypes))
	for _, eventType := range bound.Adapter.EventTypes {
		accepted[eventType] = true
	}
	for _, subscription := range value.Subscriptions {
		if !accepted[subscription.EventType] {
			return fmt.Errorf("%w: adapter does not declare event type %q", ErrInvalidCallbackRegistration, subscription.EventType)
		}
	}
	return nil
}

func validateCallbackMutation(actor ActivityActor, reason string) error {
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" || strings.TrimSpace(reason) == "" || len(strings.TrimSpace(reason)) > 2000 {
		return fmt.Errorf("%w: lifecycle mutation requires an actor and concise reason", ErrInvalidCallbackRegistration)
	}
	return nil
}

func callbackLifecycleAction(previous CallbackRegistrationStatus, status *CallbackRegistrationStatus) CallbackRegistrationLifecycleAction {
	if status == nil || *status == previous {
		return CallbackRegistrationUpdated
	}
	switch *status {
	case CallbackRegistrationActive:
		return CallbackRegistrationActivated
	case CallbackRegistrationPaused:
		return CallbackRegistrationPausedAction
	case CallbackRegistrationRetired:
		return CallbackRegistrationRetiredAction
	default:
		return CallbackRegistrationUpdated
	}
}

func cloneCallbackSubscriptions(values []CallbackSubscription) []CallbackSubscription {
	if values == nil {
		return nil
	}
	result := append([]CallbackSubscription(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		left := result[i].EventType + "\x00" + result[i].Consumer + "\x00" + result[i].TargetID
		right := result[j].EventType + "\x00" + result[j].Consumer + "\x00" + result[j].TargetID
		return left < right
	})
	return result
}

func cloneCallbackRegistration(value *CallbackRegistration) *CallbackRegistration {
	if value == nil {
		return nil
	}
	var result CallbackRegistration
	encoded, _ := json.Marshal(value)
	_ = json.Unmarshal(encoded, &result)
	return &result
}
