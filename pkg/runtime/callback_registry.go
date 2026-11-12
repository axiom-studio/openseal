package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type CreateCallbackRegistrationRequest struct {
	ID            string
	Scope         models.Scope
	Owner         models.ObjectiveOwner
	DeploymentID  string
	Name          string
	Provider      string
	Adapter       models.CallbackAdapterReference
	Subscriptions []models.CallbackSubscription
	Configuration map[string]interface{}
	Actor         models.ActivityActor
	Reason        string
}

type UpdateCallbackRegistrationRequest struct {
	ExpectedRevision int64
	Adapter          *models.CallbackAdapterReference
	Name             *string
	Subscriptions    []models.CallbackSubscription
	Configuration    map[string]interface{}
	Status           *models.CallbackRegistrationStatus
	Actor            models.ActivityActor
	Reason           string
}

type CallbackAdapterResolver interface {
	ResolveCallbackAdapter(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundCallbackAdapter, error)
	ResolveCallbackAdapterBinding(context.Context, skill.ScopeReference, string, string, string) (*skill.BoundCallbackAdapter, error)
}

type CallbackRegistry struct {
	store    models.CallbackRegistrationStore
	resolver CallbackAdapterResolver
	now      func() time.Time
	newID    func() string
}

func NewCallbackRegistry(store models.CallbackRegistrationStore, resolver CallbackAdapterResolver, opts ...models.ServiceOption) *CallbackRegistry {
	return &CallbackRegistry{store: store, resolver: resolver, now: models.ClockFrom(opts), newID: uuid.NewString}
}

// resolvesAdapters reports whether r can resolve a registration's callback
// adapter (false for a nil registry).
func (r *CallbackRegistry) resolvesAdapters() bool {
	return r != nil && r.resolver != nil
}

func (r *CallbackRegistry) Create(ctx context.Context, request CreateCallbackRegistrationRequest) (*models.CallbackRegistration, error) {
	if r == nil || r.store == nil || r.resolver == nil {
		return nil, errors.New("callback registry is not configured")
	}
	if err := validateCallbackMutation(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	now := r.now().UTC()
	value := &models.CallbackRegistration{
		ID: strings.TrimSpace(request.ID), IngressRoute: r.newID(), Scope: request.Scope, Owner: request.Owner,
		DeploymentID: strings.TrimSpace(request.DeploymentID), Name: strings.TrimSpace(request.Name),
		Provider: strings.TrimSpace(request.Provider), Adapter: request.Adapter,
		Subscriptions: cloneCallbackSubscriptions(request.Subscriptions), Configuration: maps.Clone(request.Configuration),
		Status: models.CallbackRegistrationPaused, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []models.CallbackRegistrationLifecycleEntry{{
			Revision: 1, Action: models.CallbackRegistrationCreated, Actor: request.Actor,
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
	return models.CloneCallbackRegistration(value), nil
}

func (r *CallbackRegistry) Get(ctx context.Context, scope models.Scope, id string) (*models.CallbackRegistration, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("callback registry is not configured")
	}
	value, err := r.store.GetCallbackRegistration(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, models.ErrCallbackRegistrationNotFound
	}
	return value, nil
}

func (r *CallbackRegistry) List(ctx context.Context, filter models.CallbackRegistrationFilter) ([]*models.CallbackRegistration, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("callback registry is not configured")
	}
	return r.store.ListCallbackRegistrations(ctx, filter)
}

func (r *CallbackRegistry) Update(ctx context.Context, scope models.Scope, id string, request UpdateCallbackRegistrationRequest) (*models.CallbackRegistration, error) {
	current, err := r.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Status == models.CallbackRegistrationRetired || request.ExpectedRevision != current.Revision {
		return nil, models.ErrCallbackRegistrationConflict
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
	if current.Status == models.CallbackRegistrationRetired {
		current.RetiredAt = &now
	}
	current.Revision++
	current.UpdatedAt = now
	current.Lifecycle = append(current.Lifecycle, models.CallbackRegistrationLifecycleEntry{
		Revision: current.Revision, Action: callbackLifecycleAction(previousStatus, request.Status),
		Actor: request.Actor, Reason: strings.TrimSpace(request.Reason), At: now,
	})
	if err := current.Validate(); err != nil {
		return nil, err
	}
	if current.Status == models.CallbackRegistrationActive {
		if err := r.resolve(ctx, current); err != nil {
			return nil, err
		}
	}
	if err := r.store.UpdateCallbackRegistration(ctx, current, request.ExpectedRevision); err != nil {
		return nil, err
	}
	return models.CloneCallbackRegistration(current), nil
}

func (r *CallbackRegistry) resolve(ctx context.Context, value *models.CallbackRegistration) error {
	ref := value.Adapter
	bound, err := r.resolver.ResolveCallbackAdapterBinding(
		ctx, skill.ScopeReference{Kind: value.Scope.Kind, ID: value.Scope.ID}, value.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil || bound == nil || bound.Binding == nil ||
		bound.Adapter.Provider != value.Provider || strings.TrimSpace(bound.Adapter.Transport.IngressEndpoint) == "" {
		return fmt.Errorf("%w: exact callback Skill adapter is unavailable or stale", models.ErrInvalidCallbackRegistration)
	}
	accepted := make(map[string]bool, len(bound.Adapter.EventTypes))
	for _, eventType := range bound.Adapter.EventTypes {
		accepted[eventType] = true
	}
	for _, subscription := range value.Subscriptions {
		if !accepted[subscription.EventType] {
			return fmt.Errorf("%w: adapter does not declare event type %q", models.ErrInvalidCallbackRegistration, subscription.EventType)
		}
	}
	return nil
}

func validateCallbackMutation(actor models.ActivityActor, reason string) error {
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" || strings.TrimSpace(reason) == "" || len(strings.TrimSpace(reason)) > 2000 {
		return fmt.Errorf("%w: lifecycle mutation requires an actor and concise reason", models.ErrInvalidCallbackRegistration)
	}
	return nil
}

func callbackLifecycleAction(previous models.CallbackRegistrationStatus, status *models.CallbackRegistrationStatus) models.CallbackRegistrationLifecycleAction {
	if status == nil || *status == previous {
		return models.CallbackRegistrationUpdated
	}
	switch *status {
	case models.CallbackRegistrationActive:
		return models.CallbackRegistrationActivated
	case models.CallbackRegistrationPaused:
		return models.CallbackRegistrationPausedAction
	case models.CallbackRegistrationRetired:
		return models.CallbackRegistrationRetiredAction
	default:
		return models.CallbackRegistrationUpdated
	}
}

func cloneCallbackSubscriptions(values []models.CallbackSubscription) []models.CallbackSubscription {
	if values == nil {
		return nil
	}
	result := append([]models.CallbackSubscription(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		left := result[i].EventType + "\x00" + result[i].Consumer + "\x00" + result[i].TargetID
		right := result[j].EventType + "\x00" + result[j].Consumer + "\x00" + result[j].TargetID
		return left < right
	})
	return result
}
