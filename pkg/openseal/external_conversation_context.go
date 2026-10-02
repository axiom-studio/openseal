package openseal

import (
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

type ExternalConversationContextHost = runtime.ExternalConversationContextHost
type ExternalConversationContextHostRequest = runtime.ExternalConversationContextHostRequest
type ExternalConversationContextResult = runtime.ExternalConversationContextResult
type ExternalConversationContextMessage = runtime.ExternalConversationContextMessage
type ExternalMessageSource = runtime.ExternalMessageSource

const ConversationFeatureContextHistory = capability.ConversationFeatureContextHistory
