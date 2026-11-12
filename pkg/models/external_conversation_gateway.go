package models

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrExternalConversationGatewayNotFound = errors.New("external conversation gateway not found")
)

type ExternalConversationGatewayStatus string

type ExternalConversationGatewayLifecycleAction string

const (
	ExternalConversationGatewayActive  ExternalConversationGatewayStatus = "active"
	ExternalConversationGatewayPaused  ExternalConversationGatewayStatus = "paused"
	ExternalConversationGatewayRetired ExternalConversationGatewayStatus = "retired"

	ExternalConversationGatewayCreated       ExternalConversationGatewayLifecycleAction = "created"
	ExternalConversationGatewayActivated     ExternalConversationGatewayLifecycleAction = "activated"
	ExternalConversationGatewayPausedAction  ExternalConversationGatewayLifecycleAction = "paused"
	ExternalConversationGatewayRetiredAction ExternalConversationGatewayLifecycleAction = "retired"
	ExternalConversationGatewayUpdated       ExternalConversationGatewayLifecycleAction = "updated"
)

type ExternalConversationGatewayLifecycleEntry struct {
	Revision int64                                      `json:"revision"`
	Action   ExternalConversationGatewayLifecycleAction `json:"action"`
	Actor    ActivityActor                              `json:"actor"`
	Reason   string                                     `json:"reason"`
	At       time.Time                                  `json:"at"`
}

// ExternalConversationGatewayRegistration is the durable, host-owned binding
// for one shared provider webhook. IngressRoute is public but unguessable;
// provider authentication remains the responsibility of the exact Skill
// adapter pinned by Gateway.
type ExternalConversationGatewayRegistration struct {
	ID           string                                      `json:"id"`
	IngressRoute string                                      `json:"ingressRoute"`
	Name         string                                      `json:"name"`
	Gateway      ExternalConversationIngressGateway          `json:"gateway"`
	Status       ExternalConversationGatewayStatus           `json:"status"`
	Revision     int64                                       `json:"revision"`
	CreatedAt    time.Time                                   `json:"createdAt"`
	UpdatedAt    time.Time                                   `json:"updatedAt"`
	RetiredAt    *time.Time                                  `json:"retiredAt,omitempty"`
	Lifecycle    []ExternalConversationGatewayLifecycleEntry `json:"lifecycle"`
}

func (g *ExternalConversationGatewayRegistration) Validate() error {
	if g == nil || !IsValidOpaqueIdentifier(strings.TrimSpace(g.ID), 256) ||
		!IsValidOpaqueIdentifier(strings.TrimSpace(g.IngressRoute), 128) ||
		strings.TrimSpace(g.Name) == "" || len(g.Name) > 160 ||
		g.Revision < 1 || g.CreatedAt.IsZero() || g.UpdatedAt.IsZero() || g.UpdatedAt.Before(g.CreatedAt) ||
		len(g.Lifecycle) == 0 {
		return ErrInvalidExternalConversation
	}
	var previousRevision int64
	for index, entry := range g.Lifecycle {
		if entry.Revision < 1 || (index > 0 && entry.Revision <= previousRevision) || strings.TrimSpace(entry.Actor.Type) == "" ||
			strings.TrimSpace(entry.Actor.ID) == "" || strings.TrimSpace(entry.Reason) == "" ||
			len(entry.Reason) > 2_000 || entry.At.IsZero() {
			return fmt.Errorf("%w: gateway lifecycle is invalid", ErrInvalidExternalConversation)
		}
		previousRevision = entry.Revision
	}
	if g.Lifecycle[len(g.Lifecycle)-1].Revision != g.Revision {
		return fmt.Errorf("%w: gateway lifecycle revision does not match", ErrInvalidExternalConversation)
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
	Scope        Scope
	Provider     string
	Owner        *ObjectiveOwner
	DeploymentID string
	Statuses     []ExternalConversationGatewayStatus
	Limit        int
	Offset       int
}

type ExternalConversationGatewayStore interface {
	CreateExternalConversationGateway(context.Context, *ExternalConversationGatewayRegistration) error
	GetExternalConversationGateway(context.Context, Scope, string) (*ExternalConversationGatewayRegistration, error)
	GetExternalConversationGatewayByIngressRoute(context.Context, string) (*ExternalConversationGatewayRegistration, error)
	ListExternalConversationGateways(context.Context, ExternalConversationGatewayFilter) ([]*ExternalConversationGatewayRegistration, error)
	UpdateExternalConversationGateway(context.Context, *ExternalConversationGatewayRegistration, int64) error
}

func CloneExternalConversationGateway(value *ExternalConversationGatewayRegistration) *ExternalConversationGatewayRegistration {
	if value == nil {
		return nil
	}
	copy := *value
	if value.RetiredAt != nil {
		retired := *value.RetiredAt
		copy.RetiredAt = &retired
	}
	copy.Lifecycle = append([]ExternalConversationGatewayLifecycleEntry(nil), value.Lifecycle...)
	return &copy
}

// GatewayAuthorityFromEndpoints derives account routing authority only from
// reviewed endpoint records belonging to the exact tenant/deployment/binding.
// Unrelated endpoints and unverified provider bodies are never candidates.
func GatewayAuthorityFromEndpoints(gateway ExternalConversationIngressGateway, endpoints []*ExternalConversationEndpoint) (ExternalConversationIngressGateway, bool, error) {
	if gateway.Scope.Kind == "platform" || gateway.InstallationID != "" {
		return gateway, false, nil
	}
	installationID, applicationID := "", ""
	for _, endpoint := range endpoints {
		if endpoint == nil || endpoint.Scope != gateway.Scope || endpoint.Status != ExternalConversationEndpointActive ||
			endpoint.Owner != gateway.Owner || endpoint.DeploymentID != gateway.DeploymentID || endpoint.Provider != gateway.Provider ||
			endpoint.Adapter.BindingID != gateway.Adapter.BindingID || endpoint.Adapter.AdapterID != gateway.Adapter.AdapterID ||
			endpoint.Adapter.SkillID != gateway.Adapter.SkillID || endpoint.Adapter.SourceIdentity != gateway.Adapter.SourceIdentity ||
			endpoint.Adapter.BindingRevision > gateway.Adapter.BindingRevision ||
			strings.TrimSpace(endpoint.InstallationID) == "" {
			continue
		}
		if installationID != "" && (installationID != endpoint.InstallationID || applicationID != endpoint.ApplicationID) {
			return gateway, false, fmt.Errorf("%w: gateway binding has ambiguous provider installation authority", ErrExternalConversationConflict)
		}
		installationID, applicationID = endpoint.InstallationID, endpoint.ApplicationID
	}
	if installationID == "" {
		return gateway, false, nil
	}
	gateway.InstallationID, gateway.ApplicationID = installationID, applicationID
	if err := gateway.Validate(); err != nil {
		return gateway, false, err
	}
	return gateway, true, nil
}
