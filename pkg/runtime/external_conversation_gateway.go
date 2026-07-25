package runtime

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrExternalConversationGatewayNotFound = errors.New("external conversation gateway not found")
)

type ExternalConversationGatewayStatus string

const (
	ExternalConversationGatewayActive  ExternalConversationGatewayStatus = "active"
	ExternalConversationGatewayPaused  ExternalConversationGatewayStatus = "paused"
	ExternalConversationGatewayRetired ExternalConversationGatewayStatus = "retired"
)

// ExternalConversationGatewayRegistration is the durable, host-owned binding
// for one shared provider webhook. IngressRoute is public but unguessable;
// provider authentication remains the responsibility of the exact Skill
// adapter pinned by Gateway.
type ExternalConversationGatewayRegistration struct {
	ID           string                             `json:"id"`
	IngressRoute string                             `json:"ingressRoute"`
	Name         string                             `json:"name"`
	Gateway      ExternalConversationIngressGateway `json:"gateway"`
	Status       ExternalConversationGatewayStatus  `json:"status"`
	Revision     int64                              `json:"revision"`
	CreatedAt    time.Time                          `json:"createdAt"`
	UpdatedAt    time.Time                          `json:"updatedAt"`
	RetiredAt    *time.Time                         `json:"retiredAt,omitempty"`
}

func (g *ExternalConversationGatewayRegistration) Validate() error {
	if g == nil || !validOpaqueIdentifier(strings.TrimSpace(g.ID), 256) ||
		!validOpaqueIdentifier(strings.TrimSpace(g.IngressRoute), 128) ||
		strings.TrimSpace(g.Name) == "" || len(g.Name) > 160 ||
		g.Revision < 1 || g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() || g.UpdatedAt.Before(g.CreatedAt) {
		return ErrInvalidExternalConversation
	}
	if err := g.Gateway.Validate(); err != nil {
		return err
	}
	switch g.Status {
	case ExternalConversationGatewayActive, ExternalConversationGatewayPaused:
		if g.RetiredAt != nil {
			return fmt.Errorf("%w: non-retired gateway has retiredAt", ErrInvalidExternalConversation)
		}
	case ExternalConversationGatewayRetired:
		if g.RetiredAt == nil || g.RetiredAt.Before(g.CreatedAt) {
			return fmt.Errorf("%w: retired gateway requires a valid retiredAt", ErrInvalidExternalConversation)
		}
	default:
		return fmt.Errorf("%w: gateway status is invalid", ErrInvalidExternalConversation)
	}
	return nil
}

type ExternalConversationGatewayFilter struct {
	Scope    Scope
	Provider string
	Statuses []ExternalConversationGatewayStatus
	Limit    int
	Offset   int
}

type CreateExternalConversationGatewayRequest struct {
	ID      string
	Name    string
	Gateway ExternalConversationIngressGateway
	Status  ExternalConversationGatewayStatus
}

type UpdateExternalConversationGatewayRequest struct {
	ExpectedRevision int64
	Name             *string
	Gateway          *ExternalConversationIngressGateway
	Status           *ExternalConversationGatewayStatus
}

type ExternalConversationGatewayStore interface {
	CreateExternalConversationGateway(context.Context, *ExternalConversationGatewayRegistration) error
	GetExternalConversationGateway(context.Context, Scope, string) (*ExternalConversationGatewayRegistration, error)
	GetExternalConversationGatewayByIngressRoute(context.Context, string) (*ExternalConversationGatewayRegistration, error)
	ListExternalConversationGateways(context.Context, ExternalConversationGatewayFilter) ([]*ExternalConversationGatewayRegistration, error)
	UpdateExternalConversationGateway(context.Context, *ExternalConversationGatewayRegistration, int64) error
}

type ExternalConversationGatewayService struct {
	store ExternalConversationGatewayStore
	now   func() time.Time
	newID func() string
}

func NewExternalConversationGatewayService(store ExternalConversationGatewayStore) *ExternalConversationGatewayService {
	return &ExternalConversationGatewayService{store: store, now: time.Now, newID: uuid.NewString}
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
	now := s.now().UTC()
	value := &ExternalConversationGatewayRegistration{
		ID: strings.TrimSpace(request.ID), IngressRoute: s.newID(),
		Name: strings.TrimSpace(request.Name), Gateway: request.Gateway,
		Status: status, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if value.ID == "" {
		value.ID = s.newID()
	}
	if err := value.Validate(); err != nil {
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
	if request.ExpectedRevision != current.Revision {
		return nil, ErrExternalConversationConflict
	}
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
	if err := current.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.UpdateExternalConversationGateway(ctx, current, request.ExpectedRevision); err != nil {
		return nil, err
	}
	return cloneExternalConversationGateway(current), nil
}

func cloneExternalConversationGateway(value *ExternalConversationGatewayRegistration) *ExternalConversationGatewayRegistration {
	if value == nil {
		return nil
	}
	copy := *value
	if value.RetiredAt != nil {
		retired := *value.RetiredAt
		copy.RetiredAt = &retired
	}
	return &copy
}

func externalConversationGatewayStatusMatches(
	status ExternalConversationGatewayStatus,
	statuses []ExternalConversationGatewayStatus,
) bool {
	if len(statuses) == 0 {
		return true
	}
	for _, candidate := range statuses {
		if status == candidate {
			return true
		}
	}
	return false
}

func paginateExternalConversationGateways(
	values []*ExternalConversationGatewayRegistration,
	limit, offset int,
) []*ExternalConversationGatewayRegistration {
	limit, offset = normalizeExternalConversationPage(limit, offset)
	if offset >= len(values) {
		return []*ExternalConversationGatewayRegistration{}
	}
	end := offset + limit
	if end > len(values) {
		end = len(values)
	}
	result := make([]*ExternalConversationGatewayRegistration, 0, end-offset)
	for _, value := range values[offset:end] {
		result = append(result, cloneExternalConversationGateway(value))
	}
	return result
}

func sortExternalConversationGateways(values []*ExternalConversationGatewayRegistration) {
	sort.Slice(values, func(i, j int) bool {
		if !values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].UpdatedAt.After(values[j].UpdatedAt)
		}
		return values[i].ID < values[j].ID
	})
}
