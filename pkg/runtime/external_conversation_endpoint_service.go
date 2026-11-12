package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

type CreateExternalConversationEndpointRequest struct {
	ID               string
	Scope            models.Scope
	Owner            models.ObjectiveOwner
	DeploymentID     string
	Name             string
	Adapter          models.ExternalConversationAdapterReference
	Mode             capability.ConversationEndpointMode
	InstallationID   string
	ApplicationID    string
	Address          string
	InstallationWide bool
	Handler          models.ExternalConversationHandler
	Policy           models.ExternalConversationPolicy
	Configuration    map[string]interface{}
	Status           models.ExternalConversationEndpointStatus
	// CreatedBy is the person creating the endpoint.
	CreatedBy string
}

type UpdateExternalConversationEndpointRequest struct {
	ExpectedRevision int64
	Adapter          *models.ExternalConversationAdapterReference
	Name             *string
	Address          *string
	InstallationWide *bool
	Handler          *models.ExternalConversationHandler
	Policy           *models.ExternalConversationPolicy
	Configuration    map[string]interface{}
	ReplaceConfig    bool
	Status           *models.ExternalConversationEndpointStatus
}

type ExternalConversationAdapterResolver interface {
	ResolveConversationAdapter(context.Context, skill.ScopeReference, string, string, string, string, ...skill.BindingReference) (*skill.BoundConversationAdapter, error)
	ResolveConversationAdapterBinding(context.Context, skill.ScopeReference, string, string, string) (*skill.BoundConversationAdapter, error)
}

type ExternalConversationEndpointService struct {
	store    models.ExternalConversationEndpointStore
	resolver ExternalConversationAdapterResolver
	now      func() time.Time
	newID    func() string
}

func NewExternalConversationEndpointService(store models.ExternalConversationEndpointStore, resolver ExternalConversationAdapterResolver, opts ...models.ServiceOption) *ExternalConversationEndpointService {
	return &ExternalConversationEndpointService{store: store, resolver: resolver, now: models.ClockFrom(opts), newID: uuid.NewString}
}

func (s *ExternalConversationEndpointService) Create(ctx context.Context, req CreateExternalConversationEndpointRequest) (*models.ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil || s.resolver == nil {
		return nil, errors.New("external conversation endpoint service is not configured")
	}
	status := req.Status
	if status == "" {
		status = models.ExternalConversationEndpointPaused
	}
	now := s.now().UTC()
	endpoint := &models.ExternalConversationEndpoint{
		ID: strings.TrimSpace(req.ID), Scope: req.Scope, Owner: req.Owner, DeploymentID: strings.TrimSpace(req.DeploymentID),
		Name: strings.TrimSpace(req.Name), Adapter: normalizeExternalConversationAdapterReference(req.Adapter),
		Mode: req.Mode, InstallationID: strings.TrimSpace(req.InstallationID), ApplicationID: strings.TrimSpace(req.ApplicationID),
		Address: strings.TrimSpace(req.Address), InstallationWide: req.InstallationWide, Handler: normalizeExternalConversationHandler(req.Handler),
		Policy: req.Policy, Configuration: maps.Clone(req.Configuration), Status: status,
		CreatedBy: strings.TrimSpace(req.CreatedBy), Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if endpoint.ID == "" {
		endpoint.ID = "conversation-endpoint:" + s.newID()
	}
	endpoint.IngressRoute = s.newID()
	resolved, err := s.resolveAdapter(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	endpoint.Provider = resolved.Adapter.Provider
	if err := endpoint.Policy.Validate(endpoint.Mode, resolved.Adapter.Features); err != nil {
		return nil, err
	}
	if err := endpoint.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.CreateExternalConversationEndpoint(ctx, endpoint); err != nil {
		return nil, err
	}
	return models.CloneExternalConversationEndpoint(endpoint), nil
}

func (s *ExternalConversationEndpointService) Get(ctx context.Context, scope models.Scope, id string) (*models.ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("external conversation endpoint service is not configured")
	}
	endpoint, err := s.store.GetExternalConversationEndpoint(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if endpoint == nil {
		return nil, models.ErrExternalConversationEndpointNotFound
	}
	return endpoint, nil
}

func (s *ExternalConversationEndpointService) List(ctx context.Context, filter models.ExternalConversationEndpointFilter) ([]*models.ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil || filter.Scope.Validate() != nil || filter.Offset < 0 {
		return nil, models.ErrInvalidExternalConversation
	}
	if filter.Owner != nil && filter.Owner.Validate() != nil {
		return nil, models.ErrInvalidExternalConversation
	}
	for _, status := range filter.Statuses {
		if !validExternalConversationEndpointStatus(status) {
			return nil, models.ErrInvalidExternalConversation
		}
	}
	filter.Provider = strings.TrimSpace(filter.Provider)
	if filter.Provider != "" && !models.IsValidOpaqueIdentifier(filter.Provider, 128) {
		return nil, models.ErrInvalidExternalConversation
	}
	if filter.Limit <= 0 || filter.Limit > 500 {
		filter.Limit = 50
	}
	return s.store.ListExternalConversationEndpoints(ctx, filter)
}

func (s *ExternalConversationEndpointService) Update(ctx context.Context, scope models.Scope, id string, req UpdateExternalConversationEndpointRequest) (*models.ExternalConversationEndpoint, error) {
	next, err := s.PrepareUpdate(ctx, scope, id, req)
	if err != nil {
		return nil, err
	}
	if err := s.store.UpdateExternalConversationEndpoint(ctx, next, req.ExpectedRevision); err != nil {
		return nil, err
	}
	return models.CloneExternalConversationEndpoint(next), nil
}

// PrepareUpdate applies the same lifecycle, adapter and policy validation as
// Update without persisting anything. Callers must still compare the reviewed
// revision when committing the returned state.
func (s *ExternalConversationEndpointService) PrepareUpdate(ctx context.Context, scope models.Scope, id string, req UpdateExternalConversationEndpointRequest) (*models.ExternalConversationEndpoint, error) {
	if s == nil || s.store == nil || s.resolver == nil || req.ExpectedRevision < 1 {
		return nil, models.ErrInvalidExternalConversation
	}
	current, err := s.Get(ctx, scope, id)
	if err != nil {
		return nil, err
	}
	if current.Revision != req.ExpectedRevision {
		return nil, models.ErrExternalConversationConflict
	}
	next := models.CloneExternalConversationEndpoint(current)
	if req.Adapter != nil {
		next.Adapter = normalizeExternalConversationAdapterReference(*req.Adapter)
	}
	if req.Name != nil {
		next.Name = strings.TrimSpace(*req.Name)
	}
	if req.Address != nil {
		next.Address = strings.TrimSpace(*req.Address)
	}
	if req.InstallationWide != nil {
		next.InstallationWide = *req.InstallationWide
	}
	if req.Handler != nil {
		next.Handler = normalizeExternalConversationHandler(*req.Handler)
	}
	if req.Policy != nil {
		next.Policy = *req.Policy
	}
	if req.ReplaceConfig {
		next.Configuration = maps.Clone(req.Configuration)
	}
	if req.Status != nil {
		if current.Status == models.ExternalConversationEndpointRetired && *req.Status != models.ExternalConversationEndpointRetired {
			return nil, fmt.Errorf("%w: retired endpoints cannot be reactivated; configure an active or paused endpoint", models.ErrInvalidExternalConversation)
		}
		next.Status = *req.Status
	}
	now := s.now().UTC()
	if next.Status == models.ExternalConversationEndpointRetired && current.Status != models.ExternalConversationEndpointRetired {
		next.RetiredAt = &now
	}
	next.Revision++
	next.UpdatedAt = now
	// A stale or disabled adapter must never prevent an operator from shutting
	// down an endpoint. Configuration changes and activation still require the
	// exact reviewed binding, while a lifecycle-only pause/retirement preserves
	// the already validated endpoint state and fails safely closed.
	if !externalConversationEndpointSafetyShutdown(current, next, req) {
		resolved, resolveErr := s.resolveAdapter(ctx, next)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if err := next.Policy.Validate(next.Mode, resolved.Adapter.Features); err != nil {
			return nil, err
		}
	}
	if err := next.Validate(); err != nil {
		return nil, err
	}
	return models.CloneExternalConversationEndpoint(next), nil
}

func externalConversationEndpointSafetyShutdown(
	current, next *models.ExternalConversationEndpoint,
	req UpdateExternalConversationEndpointRequest,
) bool {
	if current == nil || next == nil || req.Status == nil ||
		req.Adapter != nil || req.Name != nil || req.Address != nil || req.InstallationWide != nil || req.Handler != nil || req.Policy != nil || req.ReplaceConfig {
		return false
	}
	return next.Status == models.ExternalConversationEndpointPaused || next.Status == models.ExternalConversationEndpointRetired
}

func (s *ExternalConversationEndpointService) resolveAdapter(ctx context.Context, endpoint *models.ExternalConversationEndpoint) (*skill.BoundConversationAdapter, error) {
	ref := endpoint.Adapter
	resolved, err := s.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}, endpoint.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve exact Skill adapter: %v", models.ErrInvalidExternalConversation, err)
	}
	if (endpoint.Provider != "" && resolved.Adapter.Provider != endpoint.Provider) ||
		!slices.Contains(resolved.Adapter.EndpointModes, endpoint.Mode) {
		return nil, fmt.Errorf("%w: exact Skill adapter does not match endpoint", models.ErrInvalidExternalConversation)
	}
	return resolved, nil
}

func normalizeExternalConversationAdapterReference(value models.ExternalConversationAdapterReference) models.ExternalConversationAdapterReference {
	value.SkillID = strings.TrimSpace(value.SkillID)
	value.SkillVersion = strings.TrimSpace(value.SkillVersion)
	value.SourceIdentity = strings.TrimSpace(value.SourceIdentity)
	value.BindingID = strings.TrimSpace(value.BindingID)
	value.AdapterID = strings.TrimSpace(value.AdapterID)
	return value
}

func normalizeExternalConversationHandler(value models.ExternalConversationHandler) models.ExternalConversationHandler {
	value.ID = strings.TrimSpace(value.ID)
	return value
}

func validExternalConversationEndpointStatus(value models.ExternalConversationEndpointStatus) bool {
	return value == models.ExternalConversationEndpointActive || value == models.ExternalConversationEndpointPaused || value == models.ExternalConversationEndpointRetired
}

func cloneExternalConversationDeliveryCorrelation(value *models.ExternalConversationDeliveryCorrelation) *models.ExternalConversationDeliveryCorrelation {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func sortedExternalConversationEndpointStatuses(values []models.ExternalConversationEndpointStatus) []models.ExternalConversationEndpointStatus {
	result := append([]models.ExternalConversationEndpointStatus(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
