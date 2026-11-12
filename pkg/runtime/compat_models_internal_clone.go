// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/models. Remove once in-flight branches have landed.
//
// Unexported clone helpers that runtime code still uses, forwarding
// to pkg/models.

package runtime

import (
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func clonePreparedRuntime(value *skill.PreparedRuntime) *skill.PreparedRuntime {
	return models.ClonePreparedRuntime(value)
}

func cloneActionCredentialFieldReferences(values []ActionCredentialFieldReference) []ActionCredentialFieldReference {
	return models.CloneActionCredentialFieldReferences(values)
}

func cloneActionCredentialFields(values map[string][]string) map[string][]string {
	return models.CloneActionCredentialFields(values)
}

func cloneActionCall(value *ActionCall) *ActionCall {
	return models.CloneActionCall(value)
}

func cloneApprovalCheckpoint(value *ApprovalCheckpoint) *ApprovalCheckpoint {
	return models.CloneApprovalCheckpoint(value)
}

func cloneActivityEvent(in *ActivityEvent) *ActivityEvent {
	return models.CloneActivityEvent(in)
}

func cloneRaw(in json.RawMessage) json.RawMessage {
	return models.CloneRawMessage(in)
}

func cloneContentParts(in []ContentPart) []ContentPart {
	return models.CloneContentParts(in)
}

func cloneToolResult(in *ToolResult) *ToolResult {
	return models.CloneToolResult(in)
}

func cloneAgentSession(in *AgentSession) *AgentSession {
	return models.CloneAgentSession(in)
}

func cloneSessionEntry(in *SessionEntry) *SessionEntry {
	return models.CloneSessionEntry(in)
}

func cloneArtifact(in *Artifact) *Artifact {
	return models.CloneArtifact(in)
}

func cloneCallbackEventReceipt(value *CallbackEventReceipt) *CallbackEventReceipt {
	return models.CloneCallbackEventReceipt(value)
}

func cloneCallbackRegistration(value *CallbackRegistration) *CallbackRegistration {
	return models.CloneCallbackRegistration(value)
}

func cloneSurfacedTools(in []SurfacedTool) []SurfacedTool {
	return models.CloneSurfacedTools(in)
}

func cloneJSONObject(in map[string]interface{}) map[string]interface{} {
	return models.CloneJSONObject(in)
}

func cloneBudgetPolicy(policy *BudgetPolicy) *BudgetPolicy {
	return models.CloneBudgetPolicy(policy)
}

func cloneAgentRequest(in *AgentRequest) *AgentRequest {
	return models.CloneAgentRequest(in)
}

func cloneAgentRequestDelegationPolicy(policy *AgentRequestDelegationPolicy) *AgentRequestDelegationPolicy {
	return models.CloneAgentRequestDelegationPolicy(policy)
}

func cloneArtifactRequirements(in []ArtifactRequirement) []ArtifactRequirement {
	return models.CloneArtifactRequirements(in)
}

func cloneArtifactReferences(in []ArtifactReference) []ArtifactReference {
	return models.CloneArtifactReferences(in)
}

func cloneChannelQuestion(in *ChannelQuestion) *ChannelQuestion {
	return models.CloneChannelQuestion(in)
}

func cloneChannelMessageQuote(in *ChannelMessageQuote) *ChannelMessageQuote {
	return models.CloneChannelMessageQuote(in)
}

func cloneConversationReactionEvent(in *ConversationReactionEvent) *ConversationReactionEvent {
	return models.CloneConversationReactionEvent(in)
}

func cloneMessageReactions(in []MessageReaction) []MessageReaction {
	return models.CloneMessageReactions(in)
}

func cloneParticipationRoundResult(in *ParticipationRoundResult, replayed bool) *ParticipationRoundResult {
	return models.CloneParticipationRoundResult(in, replayed)
}

func cloneParticipationRound(in *ParticipationRound) *ParticipationRound {
	return models.CloneParticipationRound(in)
}

func cloneConversation(in *Conversation) *Conversation {
	return models.CloneConversation(in)
}

func cloneChannelMessage(in *ChannelMessage) *ChannelMessage {
	return models.CloneChannelMessage(in)
}

func cloneConversationParticipant(in *ConversationParticipant) *ConversationParticipant {
	return models.CloneConversationParticipant(in)
}

func cloneParticipants(in []ConversationParticipant) []ConversationParticipant {
	return models.CloneConversationParticipants(in)
}

func cloneConversationReferences(in []ConversationReference) []ConversationReference {
	return models.CloneConversationReferences(in)
}

func cloneConversationCursor(in *ConversationCursor) *ConversationCursor {
	return models.CloneConversationCursor(in)
}

func cloneConversationPresence(in *ConversationPresence) *ConversationPresence {
	return models.CloneConversationPresence(in)
}

func cloneChannelMessages(in []*ChannelMessage) []*ChannelMessage {
	return models.CloneChannelMessages(in)
}

func cloneConversationTask(task *ConversationTask) *ConversationTask {
	return models.CloneConversationTask(task)
}

func cloneCredentialRequest(r *CredentialRequest) *CredentialRequest {
	return models.CloneCredentialRequest(r)
}

func cloneRunDependencyGroup(in *RunDependencyGroup) *RunDependencyGroup {
	return models.CloneRunDependencyGroup(in)
}

func cloneRunDependency(in *RunDependency) *RunDependency {
	return models.CloneRunDependency(in)
}

func cloneDependencySlice(values []*RunDependency) []*RunDependency {
	return models.CloneRunDependencies(values)
}

func cloneEmbedInstallation(value *EmbedInstallation) *EmbedInstallation {
	return models.CloneEmbedInstallation(value)
}

func cloneEmbedSession(value *EmbedSession) *EmbedSession {
	return models.CloneEmbedSession(value)
}

func cloneExternalConversationEndpoint(value *ExternalConversationEndpoint) *ExternalConversationEndpoint {
	return models.CloneExternalConversationEndpoint(value)
}

func cloneExternalConversationInboxItem(value *ExternalConversationInboxItem) *ExternalConversationInboxItem {
	return models.CloneExternalConversationInboxItem(value)
}

func cloneExternalConversationMapping(value *ExternalConversationMapping) *ExternalConversationMapping {
	return models.CloneExternalConversationMapping(value)
}

func cloneExternalParticipantMapping(value *ExternalParticipantMapping) *ExternalParticipantMapping {
	return models.CloneExternalParticipantMapping(value)
}

func cloneExternalConversationDelivery(value *ExternalConversationDelivery) *ExternalConversationDelivery {
	return models.CloneExternalConversationDelivery(value)
}

func cloneExternalConversationGateway(value *ExternalConversationGatewayRegistration) *ExternalConversationGatewayRegistration {
	return models.CloneExternalConversationGateway(value)
}

func cloneExternalMessageSource(in *ExternalMessageSource) *ExternalMessageSource {
	return models.CloneExternalMessageSource(in)
}

func cloneObjective(in *Objective) *Objective {
	return models.CloneObjective(in)
}

func cloneAgentRun(in *AgentRun) *AgentRun {
	return models.CloneAgentRun(in)
}

func cloneRoutineGrants(values []RoutineGrant) []RoutineGrant {
	return models.CloneRoutineGrants(values)
}

func cloneRoutineScript(s *RoutineScript) *RoutineScript {
	return models.CloneRoutineScript(s)
}

func cloneRunbookActivation(value *RunbookActivation) *RunbookActivation {
	return models.CloneRunbookActivation(value)
}

func cloneScheduledAgentTask(t *ScheduledAgentTask) *ScheduledAgentTask {
	return models.CloneScheduledAgentTask(t)
}

func cloneSkillRuntimeMaintenance(g *SkillRuntimeMaintenance) *SkillRuntimeMaintenance {
	return models.CloneSkillRuntimeMaintenance(g)
}

func cloneSkillSetupRequest(r *SkillSetupRequest) *SkillSetupRequest {
	return models.CloneSkillSetupRequest(r)
}

func cloneSpace(space *Space) *Space {
	return models.CloneSpace(space)
}

func cloneSpaceMember(member *SpaceMember) *SpaceMember {
	return models.CloneSpaceMember(member)
}

func cloneSurfaceInteraction(in *SurfaceInteraction) *SurfaceInteraction {
	return models.CloneSurfaceInteraction(in)
}

func cloneAgentTurn(in *AgentTurn) *AgentTurn {
	return models.CloneAgentTurn(in)
}
