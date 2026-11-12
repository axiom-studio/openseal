package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

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

type CallbackAdapterResolver interface {
	ResolveCallbackAdapter(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundCallbackAdapter, error)
	ResolveCallbackAdapterBinding(context.Context, skill.ScopeReference, string, string, string) (*skill.BoundCallbackAdapter, error)
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
		Subscriptions: cloneCallbackSubscriptions(request.Subscriptions), Configuration: maps.Clone(request.Configuration),
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
		current.Configuration = maps.Clone(request.Configuration)
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
	bound, err := r.resolver.ResolveCallbackAdapterBinding(
		ctx, skill.ScopeReference{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil || bound == nil || bound.Binding == nil ||
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
