// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/models. Remove once in-flight branches have landed.
//
// Unexported validators that runtime code still uses, forwarding to
// pkg/models.

package runtime

import (
	"context"
	"encoding/json"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func validActingUser(value string) bool {
	return models.IsValidActingUser(value)
}

func validLeaseIdentifier(value string, maximum int) bool {
	return models.IsValidLeaseIdentifier(value, maximum)
}

func validateActionProposalRecord(proposal ActionProposalRecord) error {
	return models.ValidateActionProposalRecord(proposal)
}

func validateActionSubmissionBinding(call *ActionCall, binding *skill.Binding) error {
	return models.ValidateActionSubmissionBinding(call, binding)
}

func validateApprovalResolutionRecord(resolution ApprovalResolutionRecord) error {
	return models.ValidateApprovalResolutionRecord(resolution)
}

func validateActionExecutionRecord(execution ActionExecutionRecord) error {
	return models.ValidateActionExecutionRecord(execution)
}

func validateContentParts(parts []ContentPart, limits contentPartLimits) (images int, err error) {
	return models.ValidateContentParts(parts, limits)
}

func validateToolArguments(field string, args json.RawMessage) error {
	return models.ValidateToolArguments(field, args)
}

func validateAppendAgentSessionEntries(req AppendAgentSessionEntriesRequest) error {
	return models.ValidateAppendAgentSessionEntries(req)
}

func validateToolEntryTransitionRequest(req ToolEntryTransitionRequest) error {
	return models.ValidateToolEntryTransitionRequest(req)
}

func validCompactionCut(e *SessionEntry) bool {
	return models.IsValidCompactionCut(e)
}

func validCompactionCutAt(entries []*SessionEntry, cut int) bool {
	return models.IsValidCompactionCutAt(entries, cut)
}

func validateCompactionAgainstPath(path *AgentSessionPath, entry *SessionEntry) error {
	return models.ValidateCompactionAgainstPath(path, entry)
}

func validateSessionEvents(scope Scope, runID string, events []*ActivityEvent, kind error) error {
	return models.ValidateSessionEvents(scope, runID, events, kind)
}

func validateCreateRunAgentSessionTxRequest(req CreateRunAgentSessionTxRequest) error {
	return models.ValidateCreateRunAgentSessionTxRequest(req)
}

func validateSHA256Digest(value string) error {
	return models.ValidateSHA256Digest(value)
}

func validOpaqueIdentifier(value string, maxLength int) bool {
	return models.IsValidOpaqueIdentifier(value, maxLength)
}

func validatePublicEvidenceURL(value string) error {
	return models.ValidatePublicEvidenceURL(value)
}

func validateChildBudgetAllocation(parent *AgentRun, allocation *BudgetPolicy) error {
	return models.ValidateChildBudgetAllocation(parent, allocation)
}

func validateGroupedBudgetAllocations(parent *AgentRun, allocations []*BudgetPolicy) error {
	return models.ValidateGroupedBudgetAllocations(parent, allocations)
}

func validateObjectiveRunBudget(objective *Objective, allocation *BudgetPolicy) error {
	return models.ValidateObjectiveRunBudget(objective, allocation)
}

func validateArtifactRequirements(requirements []ArtifactRequirement) error {
	return models.ValidateArtifactRequirements(requirements)
}

func validateArtifactReferences(requirements []ArtifactRequirement, artifacts []ArtifactReference) error {
	return models.ValidateArtifactReferences(requirements, artifacts)
}

func validAgentRequestKind(kind AgentRequestKind) bool {
	return models.IsValidAgentRequestKind(kind)
}

func validateAgentRequestCreateRecord(record AgentRequestCreateRecord) error {
	return models.ValidateAgentRequestCreateRecord(record)
}

func validateAgentRequestCoordinationRecord(record AgentRequestCoordinationRecord) error {
	return models.ValidateAgentRequestCoordinationRecord(record)
}

func validateAgentRequestResponseRecord(record AgentRequestResponseRecord) error {
	return models.ValidateAgentRequestResponseRecord(record)
}

func validateAgentRequestCompletionRecord(record AgentRequestCompletionRecord) error {
	return models.ValidateAgentRequestCompletionRecord(record)
}

func validAppSlug(value string) bool {
	return models.IsValidAppSlug(value)
}

func validLogoDigest(value string) bool {
	return models.IsValidLogoDigest(value)
}

func validChannelQuestionOptionText(label, description string) error {
	return models.ValidateChannelQuestionOptionText(label, description)
}

func validateConversationActiveRunsQuery(scope Scope, owner ObjectiveOwner, conversationID string, limit int) error {
	return models.ValidateConversationActiveRunsQuery(scope, owner, conversationID, limit)
}

func validateConversationEvent(conversation *Conversation, event *ActivityEvent) error {
	return models.ValidateConversationEvent(conversation, event)
}

func validateConversationForegroundRunsQuery(scope Scope, owner ObjectiveOwner, conversationID string, limit, offset int) error {
	return models.ValidateConversationForegroundRunsQuery(scope, owner, conversationID, limit, offset)
}

func validateChannelMessageBatch(scope Scope, ids []string) error {
	return models.ValidateChannelMessageBatch(scope, ids)
}

func validateChannelMessageCommitRecord(record ChannelMessageCommitRecord) error {
	return models.ValidateChannelMessageCommitRecord(record)
}

func validateParticipationRoundCommitRecord(record ParticipationRoundCommitRecord) error {
	return models.ValidateParticipationRoundCommitRecord(record)
}

func validateConversationRun(run *AgentRun) error {
	return models.ValidateConversationRun(run)
}

func validateChannelMessageViewFilter(filter ChannelMessageFilter) error {
	return models.ValidateChannelMessageViewFilter(filter)
}

func validateConversationTaskSource(current *AgentRun, task *ConversationTask, revision int64, worker string) error {
	return models.ValidateConversationTaskSource(current, task, revision, worker)
}

func validateConversationTaskCreateRecord(record ConversationTaskCreateRecord) error {
	return models.ValidateConversationTaskCreateRecord(record)
}

func validateConversationTaskSourceUpdate(current *AgentRun, record ConversationTaskCreateRecord) error {
	return models.ValidateConversationTaskSourceUpdate(current, record)
}

func validateConversationTaskForegroundReserve(reserve, maximum int) error {
	return models.ValidateConversationTaskForegroundReserve(reserve, maximum)
}

func validConversationTaskAdmissionRoot(root *AgentRun, task *ConversationTask) bool {
	return models.IsValidConversationTaskAdmissionRoot(root, task)
}

func validConversationTaskAdmissionSourceWithRound(source *AgentRun, conversation *Conversation, message *ChannelMessage, task *ConversationTask, round *ParticipationRound) bool {
	return models.IsValidConversationTaskAdmissionSourceWithRound(source, conversation, message, task, round)
}

func validateWaitingDependencyGroupPage(scope Scope, afterID string, limit int) error {
	return models.ValidateWaitingDependencyGroupPage(scope, afterID, limit)
}

func validateDependencyGroupReconciliationRecord(record RunDependencyGroupReconciliationRecord) error {
	return models.ValidateDependencyGroupReconciliationRecord(record)
}

func validateRunDependencyGroupCreateRecord(record RunDependencyGroupCreateRecord) error {
	return models.ValidateRunDependencyGroupCreateRecord(record)
}

func validateRunDependencyResolutionRecord(record RunDependencyResolutionRecord) error {
	return models.ValidateRunDependencyResolutionRecord(record)
}

func validAgentReference(value string, maxLength int) bool {
	return models.IsValidAgentReference(value, maxLength)
}

func validateExternalConversationConfiguration(value map[string]interface{}) error {
	return models.ValidateExternalConversationConfiguration(value)
}

func validExternalConversationReference(value string, maximum int) bool {
	return models.IsValidExternalConversationReference(value, maximum)
}

func validateExternalAttachments(files []ExternalConversationAttachment) error {
	return models.ValidateExternalAttachments(files)
}

func validRunKind(kind RunKind) bool {
	return models.IsValidRunKind(kind)
}

func validateRoutineSourceRef(kind RoutineEventKind, sourceRef string) error {
	return models.ValidateRoutineSourceRef(kind, sourceRef)
}

func validRoutineEventDropReason(reason string) bool {
	return models.IsValidRoutineEventDropReason(reason)
}

func validateRoutineEventScopeActivation(scope Scope, activationID string) error {
	return models.ValidateRoutineEventScopeActivation(scope, activationID)
}

func validateRoutineScriptPath(value string) error {
	return models.ValidateRoutineScriptPath(value)
}

func validRoutinePausedReason(reason RoutinePausedReason) bool {
	return models.IsValidRoutinePausedReason(reason)
}

func validateSkillReferenceUpgradeMutation(mutation *SkillReferenceUpgradeMutation) error {
	return models.ValidateSkillReferenceUpgradeMutation(mutation)
}

func validateSkillUpgradeMaintenance(ctx context.Context, gate *SkillRuntimeMaintenance, application *SkillReferenceUpgradeMutation, now time.Time) error {
	return models.ValidateSkillUpgradeMaintenance(ctx, gate, application, now)
}

func validateMaintenanceIdentity(scope Scope, skillID string) error {
	return models.ValidateMaintenanceIdentity(scope, skillID)
}

func validateMaintenanceCompletion(g *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceCompletion) error {
	return models.ValidateMaintenanceCompletion(g, r)
}

func validateMaintenanceBindingPage(scope Scope, skillID, afterDeploymentID, afterBindingID string, limit int) error {
	return models.ValidateMaintenanceBindingPage(scope, skillID, afterDeploymentID, afterBindingID, limit)
}

func validateMaintenanceMutationArguments(proof *SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	return models.ValidateMaintenanceMutationArguments(proof, mutation)
}

func validateSpaceName(name string) error {
	return models.ValidateSpaceName(name)
}

func validateStepModelWrite(current, next *AgentTurn, expectedRevision int64) error {
	return models.ValidateStepModelWrite(current, next, expectedRevision)
}

func validateStepModelRecordInput(turn *AgentTurn, expectedRevision int64, events []*ActivityEvent) error {
	return models.ValidateStepModelRecordInput(turn, expectedRevision, events)
}

func validStepProducerKind(kind StepProducerKind) bool {
	return models.IsValidStepProducerKind(kind)
}

func validStepStopReason(reason string) bool {
	return models.IsValidStepStopReason(reason)
}
