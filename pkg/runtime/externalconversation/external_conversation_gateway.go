package externalconversation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type CreateExternalConversationGatewayRequest struct {
	ID      string
	Name    string
	Gateway models.ExternalConversationIngressGateway
	Status  models.ExternalConversationGatewayStatus
	Actor   models.ActivityActor
	Reason  string
}

type UpdateExternalConversationGatewayRequest struct {
	ExpectedRevision int64
	Name             *string
	Gateway          *models.ExternalConversationIngressGateway
	Status           *models.ExternalConversationGatewayStatus
	Actor            models.ActivityActor
	Reason           string
}

type ExternalConversationGatewayService struct {
	store    models.ExternalConversationGatewayStore
	resolver ExternalConversationAdapterResolver
	now      func() time.Time
	newID    func() string
}

func NewExternalConversationGatewayService(
	store models.ExternalConversationGatewayStore,
	resolver ExternalConversationAdapterResolver,
	opts ...models.ServiceOption,
) *ExternalConversationGatewayService {
	return &ExternalConversationGatewayService{store: store, resolver: resolver, now: models.ClockFrom(opts), newID: uuid.NewString}
}

func (s *ExternalConversationGatewayService) Create(
	ctx context.Context,
	request CreateExternalConversationGatewayRequest,
) (*models.ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	status := request.Status
	if status == "" {
		status = models.ExternalConversationGatewayPaused
	}
	if status != models.ExternalConversationGatewayPaused {
		return nil, fmt.Errorf("%w: gateway creation must begin paused", models.ErrInvalidExternalConversation)
	}
	now := s.now().UTC()
	if err := validateExternalConversationGatewayMutation(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	value := &models.ExternalConversationGatewayRegistration{
		ID: strings.TrimSpace(request.ID), IngressRoute: s.newID(),
		Name: strings.TrimSpace(request.Name), Gateway: request.Gateway,
		Status: status, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []models.ExternalConversationGatewayLifecycleEntry{{
			Revision: 1, Action: models.ExternalConversationGatewayCreated, Actor: request.Actor,
			Reason: strings.TrimSpace(request.Reason), At: now,
		}},
	}
	if value.ID == "" {
		value.ID = s.newID()
	}
	if err := value.Validate(); err != nil {
		return nil, err
	}
	if err := s.resolveGateway(ctx, value.Gateway); err != nil {
		return nil, err
	}
	if err := s.prepareGatewayAuthority(ctx, &value.Gateway); err != nil {
		return nil, err
	}
	if err := s.store.CreateExternalConversationGateway(ctx, value); err != nil {
		return nil, err
	}
	return models.CloneExternalConversationGateway(value), nil
}

func (s *ExternalConversationGatewayService) Get(
	ctx context.Context,
	scope models.Scope,
	id string,
) (*models.ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	value, err := s.store.GetExternalConversationGateway(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, models.ErrExternalConversationGatewayNotFound
	}
	return value, nil
}

func (s *ExternalConversationGatewayService) GetByIngressRoute(
	ctx context.Context,
	route string,
) (*models.ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	value, err := s.store.GetExternalConversationGatewayByIngressRoute(ctx, strings.TrimSpace(route))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, models.ErrExternalConversationGatewayNotFound
	}
	return value, nil
}

func (s *ExternalConversationGatewayService) List(
	ctx context.Context,
	filter models.ExternalConversationGatewayFilter,
) ([]*models.ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	return s.store.ListExternalConversationGateways(ctx, filter)
}

func (s *ExternalConversationGatewayService) Update(
	ctx context.Context,
	scope models.Scope,
	id string,
	request UpdateExternalConversationGatewayRequest,
) (*models.ExternalConversationGatewayRegistration, error) {
	current, err := s.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Status == models.ExternalConversationGatewayRetired {
		return nil, fmt.Errorf("%w: retired gateway is immutable", models.ErrExternalConversationConflict)
	}
	if request.ExpectedRevision != current.Revision {
		return nil, models.ErrExternalConversationConflict
	}
	if err := validateExternalConversationGatewayMutation(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	previousStatus := current.Status
	if request.Name != nil {
		current.Name = strings.TrimSpace(*request.Name)
	}
	if request.Gateway != nil {
		if request.Gateway.Scope != current.Gateway.Scope {
			return nil, fmt.Errorf("%w: gateway scope is immutable", models.ErrExternalConversationConflict)
		}
		current.Gateway = *request.Gateway
	}
	if request.Status != nil {
		current.Status = *request.Status
	}
	now := s.now().UTC()
	if current.Status == models.ExternalConversationGatewayRetired {
		current.RetiredAt = &now
	} else {
		current.RetiredAt = nil
	}
	current.Revision++
	current.UpdatedAt = now
	current.Lifecycle = append(current.Lifecycle, models.ExternalConversationGatewayLifecycleEntry{
		Revision: current.Revision, Action: externalConversationGatewayLifecycleAction(previousStatus, request.Status),
		Actor: request.Actor, Reason: strings.TrimSpace(request.Reason), At: now,
	})
	if err := current.Validate(); err != nil {
		return nil, err
	}
	// A stale adapter must never prevent a safety shutdown. Re-resolve when
	// desired adapter state changes or when the resulting gateway is active;
	// pausing/retiring an unchanged stale gateway remains available.
	if request.Gateway != nil || current.Status == models.ExternalConversationGatewayActive {
		if err := s.resolveGateway(ctx, current.Gateway); err != nil {
			return nil, err
		}
		if err := s.prepareGatewayAuthority(ctx, &current.Gateway); err != nil {
			return nil, err
		}
	}
	if err := s.store.UpdateExternalConversationGateway(ctx, current, request.ExpectedRevision); err != nil {
		return nil, err
	}
	return models.CloneExternalConversationGateway(current), nil
}

func validateExternalConversationGatewayMutation(actor models.ActivityActor, reason string) error {
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" ||
		strings.TrimSpace(reason) == "" || len(strings.TrimSpace(reason)) > 2_000 {
		return fmt.Errorf("%w: gateway lifecycle requires an actor and concise reason", models.ErrInvalidExternalConversation)
	}
	return nil
}

func externalConversationGatewayLifecycleAction(previous models.ExternalConversationGatewayStatus, status *models.ExternalConversationGatewayStatus) models.ExternalConversationGatewayLifecycleAction {
	if status == nil || *status == previous {
		return models.ExternalConversationGatewayUpdated
	}
	switch *status {
	case models.ExternalConversationGatewayActive:
		return models.ExternalConversationGatewayActivated
	case models.ExternalConversationGatewayPaused:
		return models.ExternalConversationGatewayPausedAction
	case models.ExternalConversationGatewayRetired:
		return models.ExternalConversationGatewayRetiredAction
	default:
		return models.ExternalConversationGatewayUpdated
	}
}

// resolveGateway proves that a lifecycle mutation still points at the exact,
// enabled Skill binding reviewed by the operator. Structural validation alone
// must not create a public webhook that will fail only after a provider begins
// sending signed traffic.
func (s *ExternalConversationGatewayService) resolveGateway(
	ctx context.Context,
	gateway models.ExternalConversationIngressGateway,
) error {
	if s.resolver == nil {
		return nil
	}
	ref := gateway.Adapter
	resolved, err := s.resolver.ResolveConversationAdapter(
		ctx,
		skill.ScopeReference{Kind: gateway.Scope.Kind, ID: gateway.Scope.ID},
		gateway.DeploymentID,
		ref.SkillID,
		ref.SkillVersion,
		ref.AdapterID,
		skill.BindingReference{ID: ref.BindingID, Revision: ref.BindingRevision},
	)
	if err != nil || resolved == nil || resolved.Binding == nil ||
		resolved.Binding.SourceIdentity != ref.SourceIdentity ||
		resolved.Adapter.Provider != gateway.Provider ||
		strings.TrimSpace(resolved.Adapter.Transport.IngressEndpoint) == "" {
		return fmt.Errorf("%w: exact gateway Skill adapter is unavailable or stale", models.ErrInvalidExternalConversation)
	}
	return nil
}
