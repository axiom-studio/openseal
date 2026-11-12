package runtime

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
	Gateway ExternalConversationIngressGateway
	Status  ExternalConversationGatewayStatus
	Actor   ActivityActor
	Reason  string
}

type UpdateExternalConversationGatewayRequest struct {
	ExpectedRevision int64
	Name             *string
	Gateway          *ExternalConversationIngressGateway
	Status           *ExternalConversationGatewayStatus
	Actor            ActivityActor
	Reason           string
}

type ExternalConversationGatewayService struct {
	store    ExternalConversationGatewayStore
	resolver ExternalConversationAdapterResolver
	now      func() time.Time
	newID    func() string
}

func NewExternalConversationGatewayService(
	store ExternalConversationGatewayStore,
	resolver ExternalConversationAdapterResolver,
	opts ...models.ServiceOption,
) *ExternalConversationGatewayService {
	return &ExternalConversationGatewayService{store: store, resolver: resolver, now: models.ClockFrom(opts), newID: uuid.NewString}
}

func (s *ExternalConversationGatewayService) Create(
	ctx context.Context,
	request CreateExternalConversationGatewayRequest,
) (*ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	status := request.Status
	if status == "" {
		status = ExternalConversationGatewayPaused
	}
	if status != ExternalConversationGatewayPaused {
		return nil, fmt.Errorf("%w: gateway creation must begin paused", ErrInvalidExternalConversation)
	}
	now := s.now().UTC()
	if err := validateExternalConversationGatewayMutation(request.Actor, request.Reason); err != nil {
		return nil, err
	}
	value := &ExternalConversationGatewayRegistration{
		ID: strings.TrimSpace(request.ID), IngressRoute: s.newID(),
		Name: strings.TrimSpace(request.Name), Gateway: request.Gateway,
		Status: status, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Lifecycle: []ExternalConversationGatewayLifecycleEntry{{
			Revision: 1, Action: ExternalConversationGatewayCreated, Actor: request.Actor,
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
	return cloneExternalConversationGateway(value), nil
}

func (s *ExternalConversationGatewayService) Get(
	ctx context.Context,
	scope Scope,
	id string,
) (*ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	value, err := s.store.GetExternalConversationGateway(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrExternalConversationGatewayNotFound
	}
	return value, nil
}

func (s *ExternalConversationGatewayService) GetByIngressRoute(
	ctx context.Context,
	route string,
) (*ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	value, err := s.store.GetExternalConversationGatewayByIngressRoute(ctx, strings.TrimSpace(route))
	if err != nil {
		return nil, err
	}
	if value == nil {
		return nil, ErrExternalConversationGatewayNotFound
	}
	return value, nil
}

func (s *ExternalConversationGatewayService) List(
	ctx context.Context,
	filter ExternalConversationGatewayFilter,
) ([]*ExternalConversationGatewayRegistration, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation gateway service is not configured")
	}
	return s.store.ListExternalConversationGateways(ctx, filter)
}

func (s *ExternalConversationGatewayService) Update(
	ctx context.Context,
	scope Scope,
	id string,
	request UpdateExternalConversationGatewayRequest,
) (*ExternalConversationGatewayRegistration, error) {
	current, err := s.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Status == ExternalConversationGatewayRetired {
		return nil, fmt.Errorf("%w: retired gateway is immutable", ErrExternalConversationConflict)
	}
	if request.ExpectedRevision != current.Revision {
		return nil, ErrExternalConversationConflict
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
			return nil, fmt.Errorf("%w: gateway scope is immutable", ErrExternalConversationConflict)
		}
		current.Gateway = *request.Gateway
	}
	if request.Status != nil {
		current.Status = *request.Status
	}
	now := s.now().UTC()
	if current.Status == ExternalConversationGatewayRetired {
		current.RetiredAt = &now
	} else {
		current.RetiredAt = nil
	}
	current.Revision++
	current.UpdatedAt = now
	current.Lifecycle = append(current.Lifecycle, ExternalConversationGatewayLifecycleEntry{
		Revision: current.Revision, Action: externalConversationGatewayLifecycleAction(previousStatus, request.Status),
		Actor: request.Actor, Reason: strings.TrimSpace(request.Reason), At: now,
	})
	if err := current.Validate(); err != nil {
		return nil, err
	}
	// A stale adapter must never prevent a safety shutdown. Re-resolve when
	// desired adapter state changes or when the resulting gateway is active;
	// pausing/retiring an unchanged stale gateway remains available.
	if request.Gateway != nil || current.Status == ExternalConversationGatewayActive {
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
	return cloneExternalConversationGateway(current), nil
}

func validateExternalConversationGatewayMutation(actor ActivityActor, reason string) error {
	if strings.TrimSpace(actor.Type) == "" || strings.TrimSpace(actor.ID) == "" ||
		strings.TrimSpace(reason) == "" || len(strings.TrimSpace(reason)) > 2_000 {
		return fmt.Errorf("%w: gateway lifecycle requires an actor and concise reason", ErrInvalidExternalConversation)
	}
	return nil
}

func externalConversationGatewayLifecycleAction(previous ExternalConversationGatewayStatus, status *ExternalConversationGatewayStatus) ExternalConversationGatewayLifecycleAction {
	if status == nil || *status == previous {
		return ExternalConversationGatewayUpdated
	}
	switch *status {
	case ExternalConversationGatewayActive:
		return ExternalConversationGatewayActivated
	case ExternalConversationGatewayPaused:
		return ExternalConversationGatewayPausedAction
	case ExternalConversationGatewayRetired:
		return ExternalConversationGatewayRetiredAction
	default:
		return ExternalConversationGatewayUpdated
	}
}

// resolveGateway proves that a lifecycle mutation still points at the exact,
// enabled Skill binding reviewed by the operator. Structural validation alone
// must not create a public webhook that will fail only after a provider begins
// sending signed traffic.
func (s *ExternalConversationGatewayService) resolveGateway(
	ctx context.Context,
	gateway ExternalConversationIngressGateway,
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
		return fmt.Errorf("%w: exact gateway Skill adapter is unavailable or stale", ErrInvalidExternalConversation)
	}
	return nil
}
