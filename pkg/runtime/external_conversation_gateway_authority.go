package runtime

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// gatewayAuthorityFromEndpoints derives account routing authority only from
// reviewed endpoint records belonging to the exact tenant/deployment/binding.
// Unrelated endpoints and unverified provider bodies are never candidates.
func gatewayAuthorityFromEndpoints(gateway ExternalConversationIngressGateway, endpoints []*ExternalConversationEndpoint) (ExternalConversationIngressGateway, bool, error) {
	if gateway.Scope.Kind == "platform" || gateway.InstallationID != "" {
		return gateway, false, nil
	}
	installationID, applicationID := "", ""
	for _, endpoint := range endpoints {
		if endpoint == nil || endpoint.Scope != gateway.Scope || endpoint.Status != ExternalConversationEndpointActive ||
			endpoint.DeploymentID != gateway.DeploymentID || endpoint.Provider != gateway.Provider ||
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
