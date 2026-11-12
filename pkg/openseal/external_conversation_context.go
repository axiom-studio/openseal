package openseal

import (
	"context"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/externalconversation"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type ExternalConversationContextHost = externalconversation.ContextHost
type ExternalConversationContextHostRequest = externalconversation.ContextHostRequest
type ExternalConversationContextResult = externalconversation.ContextResult
type ExternalConversationContextMessage = externalconversation.ContextMessage
type ExternalMessageSource = models.ExternalMessageSource

const ConversationFeatureContextHistory = capability.ConversationFeatureContextHistory

// ResolveConversationAdapterBinding follows the current authorized binding
// when a host reconciles an existing connector after a Skill update.
func (e *Engine) ResolveConversationAdapterBinding(ctx context.Context, scope skill.ScopeReference, deploymentID, bindingID, adapterID string) (*skill.BoundConversationAdapter, error) {
	return e.skills.ResolveConversationAdapterBinding(ctx, scope, deploymentID, bindingID, adapterID)
}
