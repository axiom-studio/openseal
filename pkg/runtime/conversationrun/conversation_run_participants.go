package conversationrun

// AgentBuilderDeploymentID is the builder Agent's deployment id. The cortex
// default assistant deployment id must equal it.
const AgentBuilderDeploymentID = "openseal-assistant"

// ChannelReviewParticipantID sends review-request projections. Seal Chat shows
// its own approval and setup cards and hides these messages; they never wake
// the owning Agent.
const ChannelReviewParticipantID = "openseal.channel-review"
