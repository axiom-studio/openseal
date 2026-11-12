package externalconversation

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const MaximumExternalConversationIngressBytes = 1 << 20

// IngressRequest is the bounded provider request passed to
// the exact Skill-owned ingress entrypoint. The kernel never interprets
// provider signatures or event payloads.
type IngressRequest struct {
	Scope      models.Scope        `json:"scope"`
	EndpointID string              `json:"endpointId"`
	Method     string              `json:"method"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       []byte              `json:"body"`
}

// PublicIngressRequest is the provider-facing form. Its
// opaque route is globally unique but is not an authentication secret; the
// exact installed Skill still verifies the provider signature before any
// event can enter the durable inbox.
type PublicIngressRequest struct {
	Route   string              `json:"route"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body"`
}

func (r *PublicIngressRequest) Validate() error {
	if r == nil || !models.IsValidOpaqueIdentifier(strings.TrimSpace(r.Route), 128) {
		return models.ErrInvalidExternalConversation
	}
	return (&IngressRequest{
		Scope:      models.Scope{Kind: "tenant", ID: "route-validation"},
		EndpointID: "route-validation", Method: r.Method, Headers: r.Headers, Body: r.Body,
	}).Validate()
}

func (r *IngressRequest) Validate() error {
	if r == nil || r.Scope.Validate() != nil || !models.IsValidOpaqueIdentifier(r.EndpointID, 256) ||
		r.Method != http.MethodPost || len(r.Body) == 0 || len(r.Body) > MaximumExternalConversationIngressBytes ||
		len(r.Headers) > 64 {
		return models.ErrInvalidExternalConversation
	}
	for name, values := range r.Headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name == "" || name == "Authorization" || name == "Cookie" || len(name) > 128 ||
			len(values) == 0 || len(values) > 16 {
			return models.ErrInvalidExternalConversation
		}
		for _, value := range values {
			if len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
				return models.ErrInvalidExternalConversation
			}
		}
	}
	return nil
}

// IngressHostResult contains verified canonical events
// plus the bounded response the provider expects (for example a Slack URL
// verification challenge). Non-2xx results cannot carry accepted events.
type IngressHostResult struct {
	StatusCode  int                                          `json:"statusCode"`
	ContentType string                                       `json:"contentType,omitempty"`
	Body        []byte                                       `json:"body,omitempty"`
	Events      []models.NormalizedExternalConversationEvent `json:"events,omitempty"`
}

func (r *IngressHostResult) Validate() error {
	if r == nil || r.StatusCode < 200 || r.StatusCode > 599 ||
		len(r.ContentType) > 256 || strings.ContainsAny(r.ContentType, "\r\n") ||
		len(r.Body) > MaximumExternalConversationIngressBytes || len(r.Events) > 100 {
		return models.ErrInvalidExternalConversation
	}
	if r.StatusCode >= 300 && len(r.Events) > 0 {
		return models.ErrInvalidExternalConversation
	}
	for index := range r.Events {
		if err := r.Events[index].Validate(); err != nil {
			return fmt.Errorf("%w: normalized ingress event %d: %v", models.ErrInvalidExternalConversation, index, err)
		}
	}
	return nil
}

type IngressHostRequest struct {
	Endpoint *models.ExternalConversationEndpoint `json:"endpoint"`
	Adapter  *skill.BoundConversationAdapter      `json:"adapter"`
	Request  *IngressRequest                      `json:"request"`
}

type GatewayEvent struct {
	InstallationID string                                     `json:"installationId"`
	ApplicationID  string                                     `json:"applicationId,omitempty"`
	Address        string                                     `json:"address"`
	Event          models.NormalizedExternalConversationEvent `json:"event"`
}

type GatewayHostRequest struct {
	Gateway models.ExternalConversationIngressGateway `json:"gateway"`
	Adapter *skill.BoundConversationAdapter           `json:"adapter"`
	Request *PublicIngressRequest                     `json:"request"`
	// VerificationOnly is internal authority to answer a provider setup
	// challenge without accepting events. It cannot be supplied over JSON.
	VerificationOnly bool `json:"-"`
}

type GatewayHostResult struct {
	StatusCode  int            `json:"statusCode"`
	ContentType string         `json:"contentType,omitempty"`
	Body        []byte         `json:"body,omitempty"`
	Events      []GatewayEvent `json:"events,omitempty"`
}

func (r *GatewayHostResult) Validate() error {
	if r == nil || r.StatusCode < 200 || r.StatusCode > 599 ||
		len(r.ContentType) > 256 || strings.ContainsAny(r.ContentType, "\r\n") ||
		len(r.Body) > MaximumExternalConversationIngressBytes || len(r.Events) > 100 {
		return models.ErrInvalidExternalConversation
	}
	if r.StatusCode >= 300 && len(r.Events) > 0 {
		return models.ErrInvalidExternalConversation
	}
	for index := range r.Events {
		item := r.Events[index]
		if !models.IsValidOpaqueIdentifier(strings.TrimSpace(item.InstallationID), 1024) ||
			(item.ApplicationID != "" && !models.IsValidOpaqueIdentifier(strings.TrimSpace(item.ApplicationID), 1024)) ||
			!models.IsValidOpaqueIdentifier(strings.TrimSpace(item.Address), 1024) {
			return fmt.Errorf("%w: verified ingress route %d is invalid", models.ErrInvalidExternalConversation, index)
		}
		if err := item.Event.Validate(); err != nil {
			return fmt.Errorf("%w: normalized gateway event %d: %v", models.ErrInvalidExternalConversation, index, err)
		}
	}
	return nil
}

type GatewayAdapterHost interface {
	NormalizeExternalConversationGateway(
		context.Context,
		GatewayHostRequest,
	) (*GatewayHostResult, error)
}

// GatewayVerificationAdapterHost explicitly opts in to
// verification while a saved gateway is paused. Implementations must admit
// only supported setup challenges and must not process ordinary events or
// perform provider mutations. The kernel rejects any emitted events before
// route lookup, observation publication, or durable intake.
type GatewayVerificationAdapterHost interface {
	VerifyExternalConversationGateway(
		context.Context,
		GatewayHostRequest,
	) (*GatewayHostResult, error)
}

// IngressAdapterHost executes provider verification and
// normalization from the installed Skill. It is intentionally separate from
// delivery so send-only and receive-only adapters remain composable.
type IngressAdapterHost interface {
	NormalizeExternalConversation(
		context.Context,
		IngressHostRequest,
	) (*IngressHostResult, error)
}

type IngressResult struct {
	Response *IngressHostResult                        `json:"response"`
	Received []*ReceiveExternalConversationEventResult `json:"received,omitempty"`
}

type GatewayIngressResult struct {
	Response *GatewayHostResult                        `json:"response"`
	Received []*ReceiveExternalConversationEventResult `json:"received,omitempty"`
}

// NormalizeExternalConversationIngress resolves the endpoint's immutable
// Skill adapter, delegates provider verification, then persists only verified
// canonical events through the ordinary durable inbox service.
func (s *TransportService) NormalizeExternalConversationIngress(
	ctx context.Context,
	request IngressRequest,
	host IngressAdapterHost,
) (*IngressResult, error) {
	if s == nil || s.store == nil || s.resolver == nil || host == nil {
		return nil, errors.New("external conversation ingress is not configured")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	endpoint, adapter, err := s.resolveActiveEndpoint(ctx, request.Scope, request.EndpointID)
	if err != nil {
		return nil, err
	}
	effectiveEndpoint := models.CloneExternalConversationEndpoint(endpoint)
	effectiveEndpoint.Adapter = externalConversationResolvedAdapterReference(adapter)
	result, err := host.NormalizeExternalConversation(ctx, IngressHostRequest{
		Endpoint: effectiveEndpoint, Adapter: adapter, Request: &request,
	})
	if err != nil {
		return nil, err
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	for _, event := range result.Events {
		if event.Type != capability.ConversationEventApprovalDecided {
			if !containsConversationEventType(adapter.Adapter.InboundEventTypes, event.Type) ||
				(endpoint.Mode == capability.ConversationEndpointDirect && !event.Direct) {
				return nil, fmt.Errorf("%w: adapter emitted an event outside the endpoint contract", models.ErrInvalidExternalConversation)
			}
		}
	}
	received := make([]*ReceiveExternalConversationEventResult, 0, len(result.Events))
	for index := range result.Events {
		if err := offerConversationRoutineEvent(ctx, s.routines, endpoint.Scope, endpoint.DeploymentID,
			effectiveEndpoint.Adapter, endpoint.Provider, endpoint.InstallationID, &result.Events[index]); err != nil {
			return nil, err
		}
		persisted, receiveErr := s.Receive(ctx, ReceiveExternalConversationEventRequest{
			Scope: request.Scope, EndpointID: endpoint.ID, Event: result.Events[index],
		})
		if receiveErr != nil {
			return nil, receiveErr
		}
		received = append(received, persisted)
	}
	return &IngressResult{Response: result, Received: received}, nil
}

// NormalizeExternalConversationPublicIngress resolves the opaque host route
// to its authoritative scope and endpoint before invoking Skill-owned
// verification. No provider-controlled tenant or endpoint input is trusted.
func (s *TransportService) NormalizeExternalConversationPublicIngress(
	ctx context.Context,
	request PublicIngressRequest,
	host IngressAdapterHost,
) (*IngressResult, error) {
	if s == nil || s.store == nil || host == nil {
		return nil, errors.New("external conversation ingress is not configured")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	endpoint, err := s.store.GetExternalConversationEndpointByIngressRoute(ctx, strings.TrimSpace(request.Route))
	if err != nil {
		return nil, err
	}
	if endpoint == nil {
		return nil, models.ErrExternalConversationEndpointNotFound
	}
	return s.NormalizeExternalConversationIngress(ctx, IngressRequest{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Method: request.Method,
		Headers: request.Headers, Body: request.Body,
	}, host)
}

// NormalizeExternalConversationGatewayIngress authenticates and normalizes a
// shared provider request through one exact Skill verifier before performing
// any cross-scope endpoint lookup. Verified installation/application/address
// claims are then fanned out through the ordinary per-endpoint durable inbox.
func (s *TransportService) NormalizeExternalConversationGatewayIngress(
	ctx context.Context,
	gateway models.ExternalConversationIngressGateway,
	request PublicIngressRequest,
	host GatewayAdapterHost,
) (*GatewayIngressResult, error) {
	return s.normalizeExternalConversationGatewayIngress(ctx, gateway, request, host, false)
}

func (s *TransportService) normalizeExternalConversationGatewayIngress(
	ctx context.Context,
	gateway models.ExternalConversationIngressGateway,
	request PublicIngressRequest,
	host GatewayAdapterHost,
	verificationOnly bool,
) (*GatewayIngressResult, error) {
	if s == nil || s.store == nil || s.resolver == nil || host == nil {
		return nil, errors.New("external conversation gateway ingress is not configured")
	}
	normalize := host.NormalizeExternalConversationGateway
	if verificationOnly {
		verifier, ok := host.(GatewayVerificationAdapterHost)
		if !ok {
			return nil, fmt.Errorf("%w: gateway verification while paused is unavailable", models.ErrExternalConversationConflict)
		}
		normalize = verifier.VerifyExternalConversationGateway
	}
	if err := gateway.Validate(); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	ref := gateway.Adapter
	adapter, err := s.resolver.ResolveConversationAdapterBinding(
		ctx, skill.ScopeReference{Kind: gateway.Scope.Kind, ID: gateway.Scope.ID}, gateway.DeploymentID,
		ref.BindingID, ref.AdapterID,
	)
	// A reviewed configuration save advances the same binding. Resolve its
	// current revision just as endpoint ingress, delivery and callbacks do;
	// keeping the creation revision here permanently disconnects the inbox.
	if err != nil || adapter == nil || adapter.Binding == nil ||
		adapter.Binding.SkillID != ref.SkillID || adapter.Binding.SourceIdentity != ref.SourceIdentity ||
		adapter.Binding.Revision < ref.BindingRevision || adapter.Adapter.Provider != gateway.Provider {
		return nil, fmt.Errorf("%w: exact gateway Skill adapter is unavailable or stale", models.ErrExternalConversationConflict)
	}
	effectiveGateway := gateway
	effectiveGateway.Adapter = externalConversationResolvedAdapterReference(adapter)
	result, err := normalize(ctx, GatewayHostRequest{
		Gateway: effectiveGateway, Adapter: adapter, Request: &request, VerificationOnly: verificationOnly,
	})
	if err != nil {
		return nil, err
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	if verificationOnly {
		if len(result.Events) != 0 {
			return nil, fmt.Errorf("%w: paused gateway verification cannot accept events", models.ErrExternalConversationConflict)
		}
		return &GatewayIngressResult{Response: result}, nil
	}
	type verifiedEventRoute struct {
		event     GatewayEvent
		endpoints []*models.ExternalConversationEndpoint
	}
	routes := make([]verifiedEventRoute, 0, len(result.Events))
	for index := range result.Events {
		routed := result.Events[index]
		if (gateway.InstallationID != "" && gateway.InstallationID != routed.InstallationID) ||
			(gateway.ApplicationID != "" && gateway.ApplicationID != routed.ApplicationID) {
			continue
		}
		route := models.ExternalConversationVerifiedRoute{
			Provider: gateway.Provider, SkillID: adapter.Binding.SkillID, SkillVersion: adapter.Binding.SkillVersion,
			SourceIdentity: adapter.Binding.SourceIdentity, AdapterID: ref.AdapterID,
			InstallationID: routed.InstallationID, ApplicationID: routed.ApplicationID, Address: routed.Address,
		}
		if err := route.Validate(); err != nil {
			return nil, err
		}
		endpoints, err := s.store.ListExternalConversationEndpointsByVerifiedRoute(ctx, route)
		if err != nil {
			return nil, err
		}
		// An optional application pin may be absent when provider discovery only
		// returns the installation identity. Such endpoints may receive events
		// only through their own exact, authenticated tenant gateway binding.
		// Platform gateways and other bindings still require an explicit app pin.
		bound := endpoints[:0]
		for _, endpoint := range endpoints {
			if endpoint == nil {
				continue
			}
			if endpoint.Scope == gateway.Scope && endpoint.DeploymentID == gateway.DeploymentID &&
				endpoint.Adapter.BindingID == ref.BindingID && endpoint.Adapter.BindingRevision > adapter.Binding.Revision {
				continue
			}
			if endpoint.ApplicationID == "" && (gateway.Scope.Kind == "platform" ||
				endpoint.Scope != gateway.Scope || endpoint.Owner != gateway.Owner || endpoint.DeploymentID != gateway.DeploymentID ||
				endpoint.Adapter.BindingID != ref.BindingID) {
				continue
			}
			bound = append(bound, endpoint)
		}
		endpoints = bound
		// Tenant- and local-owned gateways may route only within their own
		// ownership boundary. A host may deliberately provision a platform
		// gateway for one provider application shared across tenants, but a
		// tenant-controlled signing credential must never be able to select a
		// different tenant by signing invented provider routing claims.
		if gateway.Scope.Kind != "platform" {
			scoped := endpoints[:0]
			for _, endpoint := range endpoints {
				if endpoint != nil && endpoint.Scope == gateway.Scope {
					scoped = append(scoped, endpoint)
				}
			}
			endpoints = scoped
		} else {
			// A platform verifier may serve many installations across tenants, but
			// one verified provider route must have a single ownership boundary.
			// Fan-out remains valid inside that tenant (for example several
			// specialized Agents observing one channel); a duplicated installation
			// mapping across tenants is configuration ambiguity and accepts nothing.
			var routedScope *models.Scope
			for _, endpoint := range endpoints {
				if endpoint == nil {
					continue
				}
				if routedScope == nil {
					scope := endpoint.Scope
					routedScope = &scope
					continue
				}
				if endpoint.Scope != *routedScope {
					return nil, fmt.Errorf("%w: verified ingress route is ambiguous across ownership scopes", models.ErrExternalConversationConflict)
				}
			}
		}
		if len(endpoints) > 100 {
			return nil, fmt.Errorf("%w: verified ingress route fanout exceeds limit", models.ErrInvalidExternalConversation)
		}
		routes = append(routes, verifiedEventRoute{event: routed, endpoints: endpoints})
	}
	for _, route := range routes {
		if route.event.Event.Type != capability.ConversationEventApprovalDecided {
			if !containsConversationEventType(adapter.Adapter.InboundEventTypes, route.event.Event.Type) {
				return nil, fmt.Errorf("%w: adapter emitted undeclared event type %q", models.ErrInvalidExternalConversation, route.event.Event.Type)
			}
		}
	}
	// A gateway pinned to one tenant installation is itself the connection,
	// so routines hear its messages before any endpoint lookup or message
	// selection. Unpinned and platform gateways take their authority from
	// each mapped endpoint's live binding instead.
	pinned := gateway.Scope.Kind != "platform" && gateway.InstallationID != ""
	received := make([]*ReceiveExternalConversationEventResult, 0, len(result.Events))
	for index := range routes {
		route := &routes[index]
		if pinned {
			if err := offerConversationRoutineEvent(ctx, s.routines, gateway.Scope, gateway.DeploymentID,
				effectiveGateway.Adapter, gateway.Provider, gateway.InstallationID, &route.event.Event); err != nil {
				return nil, err
			}
		}
		for _, endpoint := range route.endpoints {
			persisted, receiveErr := s.Receive(ctx, ReceiveExternalConversationEventRequest{
				Scope: endpoint.Scope, EndpointID: endpoint.ID, Event: route.event.Event,
			})
			if receiveErr != nil {
				return nil, receiveErr
			}
			if !pinned && persisted.Item != nil {
				if err := offerConversationRoutineEvent(ctx, s.routines, endpoint.Scope, endpoint.DeploymentID,
					persisted.Item.Adapter, endpoint.Provider, endpoint.InstallationID, &route.event.Event); err != nil {
					return nil, err
				}
			}
			received = append(received, persisted)
		}
	}
	return &GatewayIngressResult{Response: result, Received: received}, nil
}

// NormalizeExternalConversationRegisteredGatewayIngress resolves a durable
// gateway registration by its public route before invoking the exact pinned
// Skill verifier. Hosts therefore do not need a parallel environment-backed
// provider routing table.
func (s *TransportService) NormalizeExternalConversationRegisteredGatewayIngress(
	ctx context.Context,
	request PublicIngressRequest,
	host GatewayAdapterHost,
) (*GatewayIngressResult, error) {
	if s == nil || s.store == nil || host == nil {
		return nil, errors.New("external conversation gateway ingress is not configured")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	registration, err := s.store.GetExternalConversationGatewayByIngressRoute(
		ctx, strings.TrimSpace(request.Route),
	)
	if err != nil {
		return nil, err
	}
	if registration == nil {
		return nil, models.ErrExternalConversationGatewayNotFound
	}
	if err := registration.Validate(); err != nil {
		return nil, err
	}
	if registration.Status != models.ExternalConversationGatewayActive && registration.Status != models.ExternalConversationGatewayPaused {
		return nil, fmt.Errorf("%w: gateway is not active", models.ErrExternalConversationConflict)
	}
	return s.normalizeExternalConversationGatewayIngress(ctx, registration.Gateway, request, host, registration.Status == models.ExternalConversationGatewayPaused)
}
