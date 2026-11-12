package agentrun

// RunContextSessionKey is reserved for a kernel-created durable session
// reference. Ordinary request handlers must reject caller-authored context
// keys with the _openseal prefix.
const RunContextSessionKey = "_opensealSession"

const (
	// AgentRequestInboxContextKey is kernel-authored input for a bounded
	// recipient decision Run. It is model-visible but never model-writable.
	AgentRequestInboxContextKey = "agentRequestInbox"
	// AgentRequestCompletionReviewContextKey is kernel-authored input for the
	// requester-side independent review of completed delegated work.
	AgentRequestCompletionReviewContextKey = "agentRequestCompletionReview"
)
