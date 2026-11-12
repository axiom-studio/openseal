package externalconversation

import (
	"context"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *GatewayService) prepareGatewayAuthority(ctx context.Context, gateway *models.ExternalConversationIngressGateway) error {
	if gateway == nil || gateway.Scope.Kind == "platform" || gateway.InstallationID != "" {
		return nil
	}
	store, ok := s.store.(models.ExternalConversationEndpointStore)
	if !ok {
		return nil
	}
	endpoints := make([]*models.ExternalConversationEndpoint, 0)
	for offset := 0; ; offset += 500 {
		page, err := store.ListExternalConversationEndpoints(ctx, models.ExternalConversationEndpointFilter{
			Scope: gateway.Scope, Provider: gateway.Provider, Statuses: []models.ExternalConversationEndpointStatus{models.ExternalConversationEndpointActive},
			Limit: 500, Offset: offset,
		})
		if err != nil {
			return err
		}
		endpoints = append(endpoints, page...)
		if len(page) < 500 {
			break
		}
	}
	effective := *gateway
	if s.resolver != nil {
		bound, err := s.resolver.ResolveConversationAdapterBinding(ctx,
			skill.ScopeReference{Kind: gateway.Scope.Kind, ID: gateway.Scope.ID}, gateway.DeploymentID, gateway.Adapter.BindingID, gateway.Adapter.AdapterID)
		if err != nil || bound == nil || bound.Binding == nil {
			return fmt.Errorf("%w: current gateway binding authority is unavailable", models.ErrExternalConversationConflict)
		}
		effective.Adapter.BindingRevision = bound.Binding.Revision
	}
	resolved, _, err := models.GatewayAuthorityFromEndpoints(effective, endpoints)
	if err == nil {
		gateway.InstallationID, gateway.ApplicationID = resolved.InstallationID, resolved.ApplicationID
	}
	return err
}
