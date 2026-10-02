package runtime

import (
	"github.com/axiom-studio/openseal/pkg/skill"
)

// An immutable transport intent follows compatible revisions of its stable
// connection. A changed connection, source, or destination is new authority.
func externalConversationSnapshotMatchesEndpoint(endpoint *ExternalConversationEndpoint, scope Scope, endpointID string,
	revision int64, captured ExternalConversationAdapterReference, origin string,
) bool {
	if endpoint == nil || endpoint.Scope != scope || endpoint.ID != endpointID ||
		endpoint.Status != ExternalConversationEndpointActive || revision < 1 || endpoint.Revision < revision ||
		captured.BindingRevision < 1 || !externalConversationAdapterBelongsToEndpoint(endpoint.Adapter, captured) ||
		endpoint.Adapter.SkillID != captured.SkillID || endpoint.Adapter.SourceIdentity != captured.SourceIdentity {
		return false
	}
	// On an upgrade the saved provider destination remains authoritative.
	// Installation-wide endpoints have no static address to compare.
	return endpoint.Revision == revision || endpoint.Address == "" || endpoint.Address == origin
}

func externalConversationSnapshotMatchesBinding(endpoint *ExternalConversationEndpoint, captured ExternalConversationAdapterReference,
	adapter *skill.BoundConversationAdapter,
) bool {
	return adapter != nil && adapter.Binding != nil && adapter.Definition != nil &&
		adapter.Binding.Scope == (skill.ScopeReference{Kind: endpoint.Scope.Kind, ID: endpoint.Scope.ID}) &&
		adapter.Binding.DeploymentID == endpoint.DeploymentID && adapter.Binding.ID == captured.BindingID &&
		adapter.Binding.Revision >= captured.BindingRevision && adapter.Binding.SkillID == captured.SkillID &&
		adapter.Definition.ID == captured.SkillID && skill.DefinitionSourceIdentity(adapter.Definition) == captured.SourceIdentity &&
		adapter.AdapterID == captured.AdapterID && adapter.Adapter.Provider == endpoint.Provider
}
