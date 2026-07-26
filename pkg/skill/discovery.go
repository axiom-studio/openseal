package skill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	DefaultDiscoveryLimit = 8
	MaximumDiscoveryLimit = 20
)

// DiscoveryReadiness describes whether an authorized Skill can be bound to a
// deployment now. Discovery never installs or binds a Skill implicitly.
type DiscoveryReadiness string

const (
	DiscoveryReadinessBindable          DiscoveryReadiness = "bindable"
	DiscoveryReadinessNeedsInstallation DiscoveryReadiness = "needs_installation"
	DiscoveryReadinessUnavailable       DiscoveryReadiness = "unavailable"
)

// DiscoveryRequest is the product-neutral query sent to a host catalog. Scope
// and deployment identity are derived from the durable Run, never from model
// arguments. Cursor values are opaque to OpenSeal and the model.
type DiscoveryRequest struct {
	Scope           ScopeReference `json:"scope"`
	DeploymentID    string         `json:"deploymentId"`
	Query           string         `json:"query"`
	RequiredActions []string       `json:"requiredActions,omitempty"`
	MaximumRisk     RiskLevel      `json:"maximumRisk,omitempty"`
	Cursor          string         `json:"cursor,omitempty"`
	Limit           int            `json:"limit,omitempty"`
}

// DiscoveryAction is the credential-free action projection safe to place in
// model context. Its exact Skill identity remains source-qualified on the
// enclosing candidate.
type DiscoveryAction struct {
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Risk        RiskLevel `json:"risk"`
}

// DiscoveryCredential reports an access requirement and whether the host has
// at least one authorized choice. It deliberately excludes credential names,
// opaque references, metadata, and values. Selection remains a host/user step.
type DiscoveryCredential struct {
	Name       string             `json:"name"`
	Kind       string             `json:"kind"`
	Optional   bool               `json:"optional,omitempty"`
	Configured bool               `json:"configured"`
	OAuth2     *OAuth2Requirement `json:"oauth2,omitempty"`
}

// DiscoveryConversationAdapter is the credential-free catalog projection of
// a Skill-owned provider adapter. Executable entrypoints and opaque connection
// references are intentionally absent.
type DiscoveryConversationAdapter struct {
	ID                           string                           `json:"id"`
	ProtocolVersion              string                           `json:"protocolVersion"`
	Provider                     string                           `json:"provider"`
	EndpointModes                []ConversationEndpointMode       `json:"endpointModes"`
	InboundEventTypes            []string                         `json:"inboundEventTypes"`
	Features                     []ConversationAdapterFeature     `json:"features,omitempty"`
	DiscoverableDestinationModes []ConversationEndpointMode       `json:"discoverableDestinationModes,omitempty"`
	Delivery                     ConversationDeliveryCapabilities `json:"delivery"`
	Credentials                  []DiscoveryCredential            `json:"credentials,omitempty"`
}

// DiscoveryCompatibility is host-supplied evidence. Consumers may rank this
// fact but must not infer compatibility that the provider did not assert.
type DiscoveryCompatibility struct {
	Requirement string `json:"requirement"`
	Compatible  bool   `json:"compatible"`
	Evidence    string `json:"evidence"`
	Reference   string `json:"reference,omitempty"`
}

// DiscoveryCandidate is an authorized, credential-free catalog projection.
// SourceIdentity is mandatory for sourced variants so a later binding cannot
// silently select a different publisher with the same declared ID/version.
type DiscoveryCandidate struct {
	ID                   string                         `json:"id"`
	Version              string                         `json:"version"`
	SourceIdentity       string                         `json:"sourceIdentity,omitempty"`
	Name                 string                         `json:"name"`
	Description          string                         `json:"description,omitempty"`
	Actions              []DiscoveryAction              `json:"actions,omitempty"`
	Credentials          []DiscoveryCredential          `json:"credentials,omitempty"`
	ConversationAdapters []DiscoveryConversationAdapter `json:"conversationAdapters,omitempty"`
	// BindingConfigSchema contains non-secret constraints for host-owned
	// configuration that must be reviewed before binding. It never contains
	// configured values.
	BindingConfigSchema map[string]interface{}   `json:"bindingConfigSchema,omitempty"`
	PromptAvailable     bool                     `json:"promptAvailable,omitempty"`
	MaximumRisk         RiskLevel                `json:"maximumRisk,omitempty"`
	Readiness           DiscoveryReadiness       `json:"readiness"`
	Compatibility       []DiscoveryCompatibility `json:"compatibility,omitempty"`
}

type DiscoveryPage struct {
	Items      []DiscoveryCandidate `json:"items"`
	NextCursor string               `json:"nextCursor,omitempty"`
}

// DiscoveryProvider is the host authorization boundary for runtime Skill
// discovery. Enterprise hosts project tenant policy, marketplace state and
// credential availability; standalone hosts can project their local catalog.
type DiscoveryProvider interface {
	DiscoverSkills(context.Context, DiscoveryRequest) (*DiscoveryPage, error)
}

type DiscoveryProviderFunc func(context.Context, DiscoveryRequest) (*DiscoveryPage, error)

func (f DiscoveryProviderFunc) DiscoverSkills(ctx context.Context, request DiscoveryRequest) (*DiscoveryPage, error) {
	return f(ctx, request)
}

func NormalizeDiscoveryRequest(request DiscoveryRequest) (DiscoveryRequest, error) {
	request.DeploymentID = strings.TrimSpace(request.DeploymentID)
	request.Query = strings.TrimSpace(request.Query)
	request.Cursor = strings.TrimSpace(request.Cursor)
	if request.Scope.Kind == "" || strings.TrimSpace(request.Scope.ID) == "" || request.DeploymentID == "" {
		return DiscoveryRequest{}, errors.New("discovery scope and deployment are required")
	}
	if request.Query == "" {
		return DiscoveryRequest{}, errors.New("discovery query is required")
	}
	if len(request.Query) > 512 || len(request.Cursor) > 1024 {
		return DiscoveryRequest{}, errors.New("discovery query or cursor is too long")
	}
	var err error
	request.RequiredActions, err = normalizeDiscoveryStrings(request.RequiredActions, 32, 128)
	if err != nil {
		return DiscoveryRequest{}, fmt.Errorf("discovery required actions: %w", err)
	}
	if request.Limit == 0 {
		request.Limit = DefaultDiscoveryLimit
	}
	if request.Limit < 1 || request.Limit > MaximumDiscoveryLimit {
		return DiscoveryRequest{}, errors.New("discovery limit is outside the supported range")
	}
	if request.MaximumRisk != "" && !validRisk(request.MaximumRisk) {
		return DiscoveryRequest{}, errors.New("discovery maximum risk is invalid")
	}
	return request, nil
}

// NormalizeDiscoveryPage rejects a provider response that could confuse the
// model about exact identity, authorization state, pagination, or credentials.
// It also returns stable ordering for replay and restart behavior.
func NormalizeDiscoveryPage(request DiscoveryRequest, page *DiscoveryPage) (*DiscoveryPage, error) {
	if page == nil {
		return nil, errors.New("discovery provider returned no page")
	}
	if len(page.Items) > request.Limit {
		return nil, errors.New("discovery provider exceeded the requested limit")
	}
	if len(page.NextCursor) > 1024 {
		return nil, errors.New("discovery provider cursor is too long")
	}
	result := &DiscoveryPage{Items: make([]DiscoveryCandidate, 0, len(page.Items)), NextCursor: strings.TrimSpace(page.NextCursor)}
	seen := make(map[string]struct{}, len(page.Items))
	for index, value := range page.Items {
		value.ID, value.Version = strings.TrimSpace(value.ID), strings.TrimSpace(value.Version)
		value.SourceIdentity, value.Name = strings.TrimSpace(value.SourceIdentity), strings.TrimSpace(value.Name)
		value.Description = strings.TrimSpace(value.Description)
		if value.ID == "" || value.Version == "" || value.Name == "" || len(value.ID) > 256 || len(value.Version) > 256 || len(value.SourceIdentity) > 1024 || len(value.Name) > 256 || len(value.Description) > 2048 {
			return nil, fmt.Errorf("discovery candidate %d has invalid identity or display metadata", index)
		}
		identity := value.ID + "\x00" + value.Version + "\x00" + value.SourceIdentity
		if _, ok := seen[identity]; ok {
			return nil, fmt.Errorf("discovery candidate %d duplicates an exact Skill identity", index)
		}
		seen[identity] = struct{}{}
		switch value.Readiness {
		case DiscoveryReadinessBindable, DiscoveryReadinessNeedsInstallation, DiscoveryReadinessUnavailable:
		default:
			return nil, fmt.Errorf("discovery candidate %d has invalid readiness", index)
		}
		if value.MaximumRisk != "" && !validRisk(value.MaximumRisk) {
			return nil, fmt.Errorf("discovery candidate %d has invalid maximum risk", index)
		}
		actionNames := make(map[string]struct{}, len(value.Actions))
		for actionIndex := range value.Actions {
			action := &value.Actions[actionIndex]
			action.Name, action.Description = strings.TrimSpace(action.Name), strings.TrimSpace(action.Description)
			if action.Name == "" || len(action.Name) > 128 || len(action.Description) > 1024 || !validRisk(action.Risk) {
				return nil, fmt.Errorf("discovery candidate %d action %d is invalid", index, actionIndex)
			}
			if _, ok := actionNames[action.Name]; ok {
				return nil, fmt.Errorf("discovery candidate %d repeats action %q", index, action.Name)
			}
			actionNames[action.Name] = struct{}{}
		}
		sort.Slice(value.Actions, func(i, j int) bool { return value.Actions[i].Name < value.Actions[j].Name })
		credentialNames := make(map[string]struct{}, len(value.Credentials))
		for credentialIndex := range value.Credentials {
			credential := &value.Credentials[credentialIndex]
			credential.Name, credential.Kind = strings.TrimSpace(credential.Name), strings.TrimSpace(credential.Kind)
			if credential.Name == "" || credential.Kind == "" || len(credential.Name) > 128 || len(credential.Kind) > 128 {
				return nil, fmt.Errorf("discovery candidate %d credential %d is invalid", index, credentialIndex)
			}
			normalizedOAuth2, err := capability.NormalizeOAuth2Requirement(credential.OAuth2)
			if err != nil {
				return nil, fmt.Errorf("discovery candidate %d credential %d OAuth 2 requirement is invalid: %w", index, credentialIndex, err)
			}
			credential.OAuth2 = normalizedOAuth2
			if _, ok := credentialNames[credential.Name]; ok {
				return nil, fmt.Errorf("discovery candidate %d repeats credential %q", index, credential.Name)
			}
			credentialNames[credential.Name] = struct{}{}
		}
		sort.Slice(value.Credentials, func(i, j int) bool { return value.Credentials[i].Name < value.Credentials[j].Name })
		adapterIDs := make(map[string]struct{}, len(value.ConversationAdapters))
		for adapterIndex := range value.ConversationAdapters {
			adapter := &value.ConversationAdapters[adapterIndex]
			adapter.ID, adapter.Provider = strings.TrimSpace(adapter.ID), strings.TrimSpace(adapter.Provider)
			if adapter.ID == "" || len(adapter.ID) > 128 {
				return nil, fmt.Errorf("discovery candidate %d conversation adapter %d is invalid", index, adapterIndex)
			}
			if _, exists := adapterIDs[adapter.ID]; exists {
				return nil, fmt.Errorf("discovery candidate %d repeats conversation adapter %q", index, adapter.ID)
			}
			adapterIDs[adapter.ID] = struct{}{}
			normalized, err := capability.NormalizeConversationAdapter(capability.ConversationAdapter{
				ProtocolVersion: adapter.ProtocolVersion,
				Name:            adapter.ID, Description: adapter.ID, Provider: adapter.Provider,
				EndpointModes: adapter.EndpointModes, InboundEventTypes: adapter.InboundEventTypes,
				Features: adapter.Features, Delivery: adapter.Delivery,
				Transport: capability.ConversationAdapterTransport{
					Kind: "discovery", IngressEndpoint: "discovery", DeliveryEndpoint: "discovery",
				},
			})
			if err != nil {
				return nil, fmt.Errorf("discovery candidate %d conversation adapter %d is invalid: %w", index, adapterIndex, err)
			}
			adapter.ProtocolVersion, adapter.Provider, adapter.EndpointModes = normalized.ProtocolVersion, normalized.Provider, normalized.EndpointModes
			adapter.InboundEventTypes, adapter.Features = normalized.InboundEventTypes, normalized.Features
			adapter.Delivery = normalized.Delivery
			adapter.DiscoverableDestinationModes, err = normalizeDiscoveryDestinationModes(adapter.DiscoverableDestinationModes, adapter.EndpointModes)
			if err != nil {
				return nil, fmt.Errorf("discovery candidate %d conversation adapter %d destination discovery is invalid: %w", index, adapterIndex, err)
			}
			credentialNames := make(map[string]struct{}, len(adapter.Credentials))
			for credentialIndex := range adapter.Credentials {
				credential := &adapter.Credentials[credentialIndex]
				credential.Name, credential.Kind = strings.TrimSpace(credential.Name), strings.TrimSpace(credential.Kind)
				if credential.Name == "" || credential.Kind == "" || len(credential.Name) > 128 || len(credential.Kind) > 128 {
					return nil, fmt.Errorf("discovery candidate %d conversation adapter %d credential %d is invalid", index, adapterIndex, credentialIndex)
				}
				if _, exists := credentialNames[credential.Name]; exists {
					return nil, fmt.Errorf("discovery candidate %d conversation adapter %d repeats credential %q", index, adapterIndex, credential.Name)
				}
				credentialNames[credential.Name] = struct{}{}
				credential.OAuth2, err = capability.NormalizeOAuth2Requirement(credential.OAuth2)
				if err != nil {
					return nil, fmt.Errorf("discovery candidate %d conversation adapter %d credential %d OAuth 2 requirement is invalid: %w", index, adapterIndex, credentialIndex, err)
				}
			}
			sort.Slice(adapter.Credentials, func(i, j int) bool { return adapter.Credentials[i].Name < adapter.Credentials[j].Name })
		}
		sort.Slice(value.ConversationAdapters, func(i, j int) bool { return value.ConversationAdapters[i].ID < value.ConversationAdapters[j].ID })
		if value.BindingConfigSchema != nil {
			encoded, err := json.Marshal(value.BindingConfigSchema)
			if err != nil || len(encoded) > 64<<10 {
				return nil, fmt.Errorf("discovery candidate %d binding config schema is invalid", index)
			}
			if _, err := compileSchema(value.ID+"-"+value.Version+"-discovery-binding-config.json", value.BindingConfigSchema); err != nil {
				return nil, fmt.Errorf("discovery candidate %d binding config schema is invalid: %w", index, err)
			}
			value.BindingConfigSchema = cloneMap(value.BindingConfigSchema)
		}
		for compatibilityIndex := range value.Compatibility {
			compatibility := &value.Compatibility[compatibilityIndex]
			compatibility.Requirement = strings.TrimSpace(compatibility.Requirement)
			compatibility.Evidence = strings.TrimSpace(compatibility.Evidence)
			compatibility.Reference = strings.TrimSpace(compatibility.Reference)
			if compatibility.Requirement == "" || compatibility.Evidence == "" || len(compatibility.Requirement) > 256 || len(compatibility.Evidence) > 1024 || len(compatibility.Reference) > 1024 {
				return nil, fmt.Errorf("discovery candidate %d compatibility evidence %d is invalid", index, compatibilityIndex)
			}
		}
		result.Items = append(result.Items, value)
	}
	sort.Slice(result.Items, func(i, j int) bool {
		left, right := result.Items[i], result.Items[j]
		return left.ID+"\x00"+left.Version+"\x00"+left.SourceIdentity < right.ID+"\x00"+right.Version+"\x00"+right.SourceIdentity
	})
	return result, nil
}

func normalizeDiscoveryDestinationModes(values, endpointModes []ConversationEndpointMode) ([]ConversationEndpointMode, error) {
	if len(values) == 0 {
		return nil, nil
	}
	allowed := make(map[ConversationEndpointMode]bool, len(endpointModes))
	for _, mode := range endpointModes {
		allowed[mode] = true
	}
	seen := make(map[ConversationEndpointMode]bool, len(values))
	result := make([]ConversationEndpointMode, 0, len(values))
	for _, mode := range values {
		if !allowed[mode] || seen[mode] {
			return nil, errors.New("discoverable destination mode is not an adapter endpoint mode")
		}
		seen[mode] = true
		result = append(result, mode)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func normalizeDiscoveryStrings(values []string, maximum, maximumLength int) ([]string, error) {
	if len(values) > maximum {
		return nil, errors.New("too many values")
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > maximumLength {
			return nil, errors.New("value is empty or too long")
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}
