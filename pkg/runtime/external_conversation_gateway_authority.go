package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func (s *ExternalConversationGatewayService) prepareGatewayAuthority(ctx context.Context, gateway *ExternalConversationIngressGateway) error {
	if gateway == nil || gateway.Scope.Kind == "platform" || gateway.InstallationID != "" {
		return nil
	}
	store, ok := s.store.(ExternalConversationEndpointStore)
	if !ok {
		return nil
	}
	endpoints := make([]*ExternalConversationEndpoint, 0)
	for offset := 0; ; offset += 500 {
		page, err := store.ListExternalConversationEndpoints(ctx, ExternalConversationEndpointFilter{
			Scope: gateway.Scope, Provider: gateway.Provider, Statuses: []ExternalConversationEndpointStatus{ExternalConversationEndpointActive},
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
			return fmt.Errorf("%w: current gateway binding authority is unavailable", ErrExternalConversationConflict)
		}
		effective.Adapter.BindingRevision = bound.Binding.Revision
	}
	resolved, _, err := gatewayAuthorityFromEndpoints(effective, endpoints)
	if err == nil {
		gateway.InstallationID, gateway.ApplicationID = resolved.InstallationID, resolved.ApplicationID
	}
	return err
}

func backfilledGatewayAuthority(value *ExternalConversationGatewayRegistration, endpoints []*ExternalConversationEndpoint, now time.Time, currentBindingRevision ...int64) (*ExternalConversationGatewayRegistration, bool, error) {
	if value.Status == ExternalConversationGatewayRetired {
		return value, false, nil
	}
	effective := value.Gateway
	if len(currentBindingRevision) > 0 && currentBindingRevision[0] > effective.Adapter.BindingRevision {
		effective.Adapter.BindingRevision = currentBindingRevision[0]
	}
	gateway, changed, err := gatewayAuthorityFromEndpoints(effective, endpoints)
	if err != nil || !changed {
		return value, false, err
	}
	updated := cloneExternalConversationGateway(value)
	updated.Gateway.InstallationID, updated.Gateway.ApplicationID = gateway.InstallationID, gateway.ApplicationID
	updated.Revision++
	if now.Before(updated.UpdatedAt) {
		now = updated.UpdatedAt
	}
	updated.UpdatedAt = now.UTC()
	updated.Lifecycle = append(updated.Lifecycle, ExternalConversationGatewayLifecycleEntry{
		Revision: updated.Revision, Action: ExternalConversationGatewayUpdated,
		Actor:  ActivityActor{Type: "system", ID: "gateway-authority-migration"},
		Reason: "Pin provider installation from unique reviewed endpoint authority", At: updated.UpdatedAt,
	})
	if err := updated.Validate(); err != nil {
		return nil, false, err
	}
	return updated, true, nil
}
