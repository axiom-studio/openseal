package storetest

import "github.com/axiom-studio/openseal/pkg/models"

// IngressGateway is a Slack ingress gateway of deployment skill-deployment-1
// in scope.
func IngressGateway(scope models.Scope) models.ExternalConversationIngressGateway {
	return models.ExternalConversationIngressGateway{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "skill-deployment-1"}, DeploymentID: "skill-deployment-1", Provider: "slack",
		Adapter: models.ExternalConversationAdapterReference{
			SkillID: "skill-slack", SkillVersion: "2.0.0",
			SourceIdentity: "oci://example/slack@sha256:abc", BindingID: "binding-1",
			BindingRevision: 1, AdapterID: "conversations",
		},
	}
}
