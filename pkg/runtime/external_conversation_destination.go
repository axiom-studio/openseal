package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	DefaultConversationDestinationLimit = 50
	MaximumConversationDestinationLimit = 200
)

// ExternalConversationDestination is a secret-free, operator-facing provider
// destination. ID is persisted only after selection; display metadata remains
// contextual and can be refreshed from the Skill.
type ExternalConversationDestination struct {
	ID          string `json:"id"`
	DisplayName string `json:"displayName"`
	Description string `json:"description,omitempty"`
}

type ExternalConversationDestinationPage struct {
	Items      []ExternalConversationDestination      `json:"items"`
	NextCursor string                                 `json:"nextCursor,omitempty"`
	Connection *capability.CredentialExternalIdentity `json:"connection,omitempty"`
}

// ExternalConversationDestinationDiscoveryRequest identifies one exact bound
// adapter and carries only search/pagination input. Credential resolution and
// action invocation remain host responsibilities.
type ExternalConversationDestinationDiscoveryRequest struct {
	Scope                 Scope                                `json:"scope"`
	DeploymentID          string                               `json:"deploymentId"`
	ExecutionDeploymentID string                               `json:"executionDeploymentId"`
	Adapter               *capability.BoundConversationAdapter `json:"-"`
	Mode                  capability.ConversationEndpointMode  `json:"mode"`
	Cursor                string                               `json:"cursor,omitempty"`
	Query                 string                               `json:"query,omitempty"`
	Limit                 int                                  `json:"limit,omitempty"`
}

func (r *ExternalConversationDestinationDiscoveryRequest) Validate() error {
	if r == nil || r.Scope.Validate() != nil || !validAgentReference(strings.TrimSpace(r.DeploymentID), 256) ||
		!validAgentReference(strings.TrimSpace(r.ExecutionDeploymentID), 256) ||
		r.Adapter == nil || r.Adapter.Definition == nil || r.Adapter.Binding == nil ||
		r.Adapter.Binding.Scope != capability.ScopeReference(r.Scope) || r.Adapter.Binding.DeploymentID != r.DeploymentID ||
		len(r.Cursor) > 4096 || strings.ContainsAny(r.Cursor, "\r\n") || len(r.Query) > 256 || strings.ContainsAny(r.Query, "\r\n") ||
		r.Limit < 0 || r.Limit > MaximumConversationDestinationLimit {
		return ErrInvalidExternalConversation
	}
	if _, _, err := conversationDestinationDiscovery(r.Adapter, r.Mode); err != nil {
		return err
	}
	return nil
}

// ExternalConversationDestinationDiscoveryHost invokes the exact read-only
// Skill action declared by the adapter and returns its already-projected page.
type ExternalConversationDestinationDiscoveryHost interface {
	DiscoverExternalConversationDestinations(context.Context, ExternalConversationDestinationDiscoveryRequest) (*ExternalConversationDestinationPage, error)
}

// ConversationDestinationDiscoveryArguments deterministically creates action
// inputs from the adapter mapping. The operator cannot inject arbitrary action
// arguments through destination search.
func ConversationDestinationDiscoveryArguments(request ExternalConversationDestinationDiscoveryRequest) (capability.ConversationDestinationDiscovery, map[string]interface{}, error) {
	if err := request.Validate(); err != nil {
		return capability.ConversationDestinationDiscovery{}, nil, err
	}
	discovery, _, _ := conversationDestinationDiscovery(request.Adapter, request.Mode)
	arguments := map[string]interface{}{}
	if request.Cursor != "" && discovery.CursorArgument != "" {
		arguments[discovery.CursorArgument] = request.Cursor
	}
	if request.Query != "" && discovery.QueryArgument != "" {
		arguments[discovery.QueryArgument] = request.Query
	}
	limit := request.Limit
	if limit == 0 {
		limit = DefaultConversationDestinationLimit
	}
	if discovery.LimitArgument != "" {
		arguments[discovery.LimitArgument] = limit
	}
	return discovery, arguments, nil
}

// ProjectConversationDestinationPage copies only fields selected by the
// validated manifest mapping. All other provider output—including accidental
// credentials or internal metadata—is discarded.
func ProjectConversationDestinationPage(discovery capability.ConversationDestinationDiscovery, output map[string]interface{}) (*ExternalConversationDestinationPage, error) {
	rawItems, ok := valueAtConversationPath(output, discovery.ItemsPath)
	if !ok {
		return nil, errors.New("conversation destination output has no items")
	}
	items, ok := rawItems.([]interface{})
	if !ok || len(items) > MaximumConversationDestinationLimit {
		return nil, errors.New("conversation destination output items are invalid")
	}
	page := &ExternalConversationDestinationPage{Items: make([]ExternalConversationDestination, 0, len(items))}
	seen := make(map[string]bool, len(items))
	for index, raw := range items {
		item, ok := raw.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("conversation destination item %d is invalid", index)
		}
		id := conversationPathString(item, discovery.IDPath)
		displayName := conversationPathString(item, discovery.DisplayNamePath)
		description := conversationPathString(item, discovery.DescriptionPath)
		if id == "" || displayName == "" || len(id) > 1024 || len(displayName) > 160 || len(description) > 1024 ||
			strings.ContainsAny(id+displayName+description, "\r\n") || seen[id] {
			return nil, fmt.Errorf("conversation destination item %d has invalid identity or display metadata", index)
		}
		seen[id] = true
		page.Items = append(page.Items, ExternalConversationDestination{ID: id, DisplayName: displayName, Description: description})
	}
	if discovery.NextCursorPath != "" {
		page.NextCursor = conversationPathString(output, discovery.NextCursorPath)
		if len(page.NextCursor) > 4096 || strings.ContainsAny(page.NextCursor, "\r\n") {
			return nil, errors.New("conversation destination next cursor is invalid")
		}
	}
	if discovery.InstallationIDPath != "" {
		connection := &capability.CredentialExternalIdentity{
			InstallationID: conversationPathString(output, discovery.InstallationIDPath),
			ApplicationID:  conversationPathString(output, discovery.ApplicationIDPath),
			DisplayName:    conversationPathString(output, discovery.ConnectionDisplayNamePath),
		}
		if connection.InstallationID == "" || len(connection.InstallationID) > 1024 ||
			len(connection.ApplicationID) > 1024 || len(connection.DisplayName) > 160 ||
			strings.ContainsAny(connection.InstallationID+connection.ApplicationID+connection.DisplayName, "\r\n") {
			return nil, errors.New("conversation destination connection identity is invalid")
		}
		page.Connection = connection
	}
	return page, nil
}

func conversationDestinationDiscovery(adapter *capability.BoundConversationAdapter, mode capability.ConversationEndpointMode) (capability.ConversationDestinationDiscovery, capability.Action, error) {
	if adapter == nil || adapter.Definition == nil || adapter.Binding == nil {
		return capability.ConversationDestinationDiscovery{}, capability.Action{}, ErrInvalidExternalConversation
	}
	for _, discovery := range adapter.Adapter.DestinationDiscovery {
		if discovery.Mode == mode {
			action, ok := adapter.Definition.Actions[discovery.Action]
			if !ok {
				return capability.ConversationDestinationDiscovery{}, capability.Action{}, ErrInvalidExternalConversation
			}
			return discovery, action, nil
		}
	}
	return capability.ConversationDestinationDiscovery{}, capability.Action{}, ErrInvalidExternalConversation
}

func valueAtConversationPath(value map[string]interface{}, path string) (interface{}, bool) {
	if path == "" {
		return nil, false
	}
	var current interface{} = value
	for _, component := range strings.Split(path, ".") {
		object, ok := current.(map[string]interface{})
		if !ok {
			return nil, false
		}
		current, ok = object[component]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func conversationPathString(value map[string]interface{}, path string) string {
	if path == "" {
		return ""
	}
	resolved, ok := valueAtConversationPath(value, path)
	if !ok {
		return ""
	}
	text, _ := resolved.(string)
	return strings.TrimSpace(text)
}
