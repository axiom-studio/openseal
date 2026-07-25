package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/axiom-studio/openseal/pkg/skill"
)

const MaximumExternalConversationIngressBytes = 1 << 20

// ExternalConversationIngressRequest is the bounded provider request passed to
// the exact Skill-owned ingress entrypoint. The kernel never interprets
// provider signatures or event payloads.
type ExternalConversationIngressRequest struct {
	Scope      Scope               `json:"scope"`
	EndpointID string              `json:"endpointId"`
	Method     string              `json:"method"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       []byte              `json:"body"`
}

// ExternalConversationPublicIngressRequest is the provider-facing form. Its
// opaque route is globally unique but is not an authentication secret; the
// exact installed Skill still verifies the provider signature before any
// event can enter the durable inbox.
type ExternalConversationPublicIngressRequest struct {
	Route   string              `json:"route"`
	Method  string              `json:"method"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body"`
}

func (r *ExternalConversationPublicIngressRequest) Validate() error {
	if r == nil || !validOpaqueIdentifier(strings.TrimSpace(r.Route), 128) {
		return ErrInvalidExternalConversation
	}
	return (&ExternalConversationIngressRequest{
		Scope:      Scope{Kind: "tenant", ID: "route-validation"},
		EndpointID: "route-validation", Method: r.Method, Headers: r.Headers, Body: r.Body,
	}).Validate()
}

func (r *ExternalConversationIngressRequest) Validate() error {
	if r == nil || r.Scope.Validate() != nil || !validOpaqueIdentifier(r.EndpointID, 256) ||
		r.Method != http.MethodPost || len(r.Body) == 0 || len(r.Body) > MaximumExternalConversationIngressBytes ||
		len(r.Headers) > 64 {
		return ErrInvalidExternalConversation
	}
	for name, values := range r.Headers {
		name = http.CanonicalHeaderKey(strings.TrimSpace(name))
		if name == "" || name == "Authorization" || name == "Cookie" || len(name) > 128 ||
			len(values) == 0 || len(values) > 16 {
			return ErrInvalidExternalConversation
		}
		for _, value := range values {
			if len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
				return ErrInvalidExternalConversation
			}
		}
	}
	return nil
}

// ExternalConversationIngressHostResult contains verified canonical events
// plus the bounded response the provider expects (for example a Slack URL
// verification challenge). Non-2xx results cannot carry accepted events.
type ExternalConversationIngressHostResult struct {
	StatusCode  int                                   `json:"statusCode"`
	ContentType string                                `json:"contentType,omitempty"`
	Body        []byte                                `json:"body,omitempty"`
	Events      []NormalizedExternalConversationEvent `json:"events,omitempty"`
}

func (r *ExternalConversationIngressHostResult) Validate() error {
	if r == nil || r.StatusCode < 200 || r.StatusCode > 599 ||
		len(r.ContentType) > 256 || strings.ContainsAny(r.ContentType, "\r\n") ||
		len(r.Body) > MaximumExternalConversationIngressBytes || len(r.Events) > 100 {
		return ErrInvalidExternalConversation
	}
	if r.StatusCode >= 300 && len(r.Events) > 0 {
		return ErrInvalidExternalConversation
	}
	for index := range r.Events {
		if err := r.Events[index].Validate(); err != nil {
			return fmt.Errorf("%w: normalized ingress event %d: %v", ErrInvalidExternalConversation, index, err)
		}
	}
	return nil
}

type ExternalConversationIngressHostRequest struct {
	Endpoint *ExternalConversationEndpoint       `json:"endpoint"`
	Adapter  *skill.BoundConversationAdapter     `json:"adapter"`
	Request  *ExternalConversationIngressRequest `json:"request"`
}

// ExternalConversationIngressGateway pins one shared provider webhook to an
// immutable Skill-owned verifier. It may live in a host/platform scope while
// the endpoints selected from its verified routing claims remain tenant
// scoped. Provider application secrets are supplied only by the Skill host.
type ExternalConversationIngressGateway struct {
	Scope        Scope                                `json:"scope"`
	DeploymentID string                               `json:"deploymentId"`
	Adapter      ExternalConversationAdapterReference `json:"adapter"`
	Provider     string                               `json:"provider"`
}

func (g ExternalConversationIngressGateway) Validate() error {
	if g.Scope.Validate() != nil || !validAgentReference(strings.TrimSpace(g.DeploymentID), 256) ||
		!validOpaqueIdentifier(strings.TrimSpace(g.Provider), 128) {
		return ErrInvalidExternalConversation
	}
	return g.Adapter.Validate()
}

// ExternalConversationVerifiedRoute contains only routing facts asserted
// after a Skill has authenticated the provider request. It is never populated
// from an unverified HTTP body by the kernel or generic host.
type ExternalConversationVerifiedRoute struct {
	Provider       string `json:"provider"`
	SkillID        string `json:"skillId"`
	SkillVersion   string `json:"skillVersion"`
	SourceIdentity string `json:"sourceIdentity,omitempty"`
	AdapterID      string `json:"adapterId"`
	InstallationID string `json:"installationId"`
	ApplicationID  string `json:"applicationId,omitempty"`
	Address        string `json:"address"`
}

func (r ExternalConversationVerifiedRoute) Validate() error {
	for _, value := range []string{r.Provider, r.SkillID, r.SkillVersion, r.AdapterID, r.InstallationID, r.Address} {
		if !validOpaqueIdentifier(strings.TrimSpace(value), 1024) {
			return ErrInvalidExternalConversation
		}
	}
	if r.ApplicationID != "" && !validOpaqueIdentifier(strings.TrimSpace(r.ApplicationID), 1024) {
		return ErrInvalidExternalConversation
	}
	if len(r.SourceIdentity) > 1024 || strings.ContainsAny(r.SourceIdentity, "\r\n") {
		return ErrInvalidExternalConversation
	}
	return nil
}

type ExternalConversationGatewayEvent struct {
	InstallationID string                              `json:"installationId"`
	ApplicationID  string                              `json:"applicationId,omitempty"`
	Address        string                              `json:"address"`
	Event          NormalizedExternalConversationEvent `json:"event"`
}

type ExternalConversationGatewayHostRequest struct {
	Gateway ExternalConversationIngressGateway        `json:"gateway"`
	Adapter *skill.BoundConversationAdapter           `json:"adapter"`
	Request *ExternalConversationPublicIngressRequest `json:"request"`
}

type ExternalConversationGatewayHostResult struct {
	StatusCode  int                                `json:"statusCode"`
	ContentType string                             `json:"contentType,omitempty"`
	Body        []byte                             `json:"body,omitempty"`
	Events      []ExternalConversationGatewayEvent `json:"events,omitempty"`
}

func (r *ExternalConversationGatewayHostResult) Validate() error {
	if r == nil || r.StatusCode < 200 || r.StatusCode > 599 ||
		len(r.ContentType) > 256 || strings.ContainsAny(r.ContentType, "\r\n") ||
		len(r.Body) > MaximumExternalConversationIngressBytes || len(r.Events) > 100 {
		return ErrInvalidExternalConversation
	}
	if r.StatusCode >= 300 && len(r.Events) > 0 {
		return ErrInvalidExternalConversation
	}
	for index := range r.Events {
		item := r.Events[index]
		if !validOpaqueIdentifier(strings.TrimSpace(item.InstallationID), 1024) ||
			(item.ApplicationID != "" && !validOpaqueIdentifier(strings.TrimSpace(item.ApplicationID), 1024)) ||
			!validOpaqueIdentifier(strings.TrimSpace(item.Address), 1024) {
			return fmt.Errorf("%w: verified ingress route %d is invalid", ErrInvalidExternalConversation, index)
		}
		if err := item.Event.Validate(); err != nil {
			return fmt.Errorf("%w: normalized gateway event %d: %v", ErrInvalidExternalConversation, index, err)
		}
	}
	return nil
}

type ExternalConversationGatewayAdapterHost interface {
	NormalizeExternalConversationGateway(
		context.Context,
		ExternalConversationGatewayHostRequest,
	) (*ExternalConversationGatewayHostResult, error)
}

// ExternalConversationIngressAdapterHost executes provider verification and
// normalization from the installed Skill. It is intentionally separate from
// delivery so send-only and receive-only adapters remain composable.
type ExternalConversationIngressAdapterHost interface {
	NormalizeExternalConversation(
		context.Context,
		ExternalConversationIngressHostRequest,
	) (*ExternalConversationIngressHostResult, error)
}

type ExternalConversationIngressResult struct {
	Response *ExternalConversationIngressHostResult    `json:"response"`
	Received []*ReceiveExternalConversationEventResult `json:"received,omitempty"`
}

type ExternalConversationGatewayIngressResult struct {
	Response *ExternalConversationGatewayHostResult    `json:"response"`
	Received []*ReceiveExternalConversationEventResult `json:"received,omitempty"`
}

// NormalizeExternalConversationIngress resolves the endpoint's immutable
// Skill adapter, delegates provider verification, then persists only verified
// canonical events through the ordinary durable inbox service.
func (s *ExternalConversationTransportService) NormalizeExternalConversationIngress(
	ctx context.Context,
	request ExternalConversationIngressRequest,
	host ExternalConversationIngressAdapterHost,
) (*ExternalConversationIngressResult, error) {
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
	result, err := host.NormalizeExternalConversation(ctx, ExternalConversationIngressHostRequest{
		Endpoint: endpoint, Adapter: adapter, Request: &request,
	})
	if err != nil {
		return nil, err
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	received := make([]*ReceiveExternalConversationEventResult, 0, len(result.Events))
	for index := range result.Events {
		persisted, receiveErr := s.Receive(ctx, ReceiveExternalConversationEventRequest{
			Scope: request.Scope, EndpointID: endpoint.ID, Event: result.Events[index],
		})
		if receiveErr != nil {
			return nil, receiveErr
		}
		received = append(received, persisted)
	}
	return &ExternalConversationIngressResult{Response: result, Received: received}, nil
}

// NormalizeExternalConversationPublicIngress resolves the opaque host route
// to its authoritative scope and endpoint before invoking Skill-owned
// verification. No provider-controlled tenant or endpoint input is trusted.
func (s *ExternalConversationTransportService) NormalizeExternalConversationPublicIngress(
	ctx context.Context,
	request ExternalConversationPublicIngressRequest,
	host ExternalConversationIngressAdapterHost,
) (*ExternalConversationIngressResult, error) {
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
		return nil, ErrExternalConversationEndpointNotFound
	}
	return s.NormalizeExternalConversationIngress(ctx, ExternalConversationIngressRequest{
		Scope: endpoint.Scope, EndpointID: endpoint.ID, Method: request.Method,
		Headers: request.Headers, Body: request.Body,
	}, host)
}

// NormalizeExternalConversationGatewayIngress authenticates and normalizes a
// shared provider request through one exact Skill verifier before performing
// any cross-scope endpoint lookup. Verified installation/application/address
// claims are then fanned out through the ordinary per-endpoint durable inbox.
func (s *ExternalConversationTransportService) NormalizeExternalConversationGatewayIngress(
	ctx context.Context,
	gateway ExternalConversationIngressGateway,
	request ExternalConversationPublicIngressRequest,
	host ExternalConversationGatewayAdapterHost,
) (*ExternalConversationGatewayIngressResult, error) {
	if s == nil || s.store == nil || s.resolver == nil || host == nil {
		return nil, errors.New("external conversation gateway ingress is not configured")
	}
	if err := gateway.Validate(); err != nil {
		return nil, err
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	ref := gateway.Adapter
	adapter, err := s.resolver.ResolveConversationAdapter(
		ctx, skill.ScopeReference{Kind: gateway.Scope.Kind, ID: gateway.Scope.ID}, gateway.DeploymentID,
		ref.SkillID, ref.SkillVersion, ref.AdapterID,
		skill.BindingReference{ID: ref.BindingID, Revision: ref.BindingRevision},
	)
	if err != nil || adapter == nil || adapter.Binding.SourceIdentity != ref.SourceIdentity ||
		adapter.Adapter.Provider != gateway.Provider {
		return nil, fmt.Errorf("%w: exact gateway Skill adapter is unavailable or stale", ErrExternalConversationConflict)
	}
	result, err := host.NormalizeExternalConversationGateway(ctx, ExternalConversationGatewayHostRequest{
		Gateway: gateway, Adapter: adapter, Request: &request,
	})
	if err != nil {
		return nil, err
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	received := make([]*ReceiveExternalConversationEventResult, 0, len(result.Events))
	for index := range result.Events {
		routed := result.Events[index]
		route := ExternalConversationVerifiedRoute{
			Provider: gateway.Provider, SkillID: ref.SkillID, SkillVersion: ref.SkillVersion,
			SourceIdentity: ref.SourceIdentity, AdapterID: ref.AdapterID,
			InstallationID: routed.InstallationID, ApplicationID: routed.ApplicationID, Address: routed.Address,
		}
		if err := route.Validate(); err != nil {
			return nil, err
		}
		endpoints, err := s.store.ListExternalConversationEndpointsByVerifiedRoute(ctx, route)
		if err != nil {
			return nil, err
		}
		if len(endpoints) > 100 {
			return nil, fmt.Errorf("%w: verified ingress route fanout exceeds limit", ErrInvalidExternalConversation)
		}
		for _, endpoint := range endpoints {
			persisted, receiveErr := s.Receive(ctx, ReceiveExternalConversationEventRequest{
				Scope: endpoint.Scope, EndpointID: endpoint.ID, Event: routed.Event,
			})
			if receiveErr != nil {
				return nil, receiveErr
			}
			received = append(received, persisted)
		}
	}
	return &ExternalConversationGatewayIngressResult{Response: result, Received: received}, nil
}
