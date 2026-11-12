// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/models. Remove once in-flight branches have landed.
//
// Unexported names that runtime code still uses for declarations
// moved to pkg/models: constants, types and forwarding functions.

package runtime

import (
	"context"
	"encoding/json"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	maxSessionEntryListLimit             = models.MaxSessionEntryListLimit
	capabilityInvocationContextKey       = models.CapabilityInvocationContextKey
	capabilityActionPrefix               = models.CapabilityActionPrefix
	maximumAppAddress                    = models.MaximumAppAddress
	minimumAskUserOptions                = models.MinimumAskUserOptions
	maximumAskUserOptions                = models.MaximumAskUserOptions
	maximumAskUserOptionLabelBytes       = models.MaximumAskUserOptionLabelBytes
	maximumAskUserOptionDescriptionBytes = models.MaximumAskUserOptionDescriptionBytes
	defaultConversationChangeLimit       = models.DefaultConversationChangeLimit
	maximumConversationChangeLimit       = models.MaximumConversationChangeLimit
	conversationRunContextConversationID = models.ConversationRunConversationIDContextKey
	conversationRunContextTriggerID      = models.ConversationRunTriggerIDContextKey
	conversationRunSchedulerParticipant  = models.ConversationRunSchedulerParticipant
	maximumChildBriefBytes               = models.MaximumChildBriefBytes
	routingReasonContextKey              = models.RoutingReasonContextKey
	routineCallbackSourcePrefix          = models.RoutineCallbackSourcePrefix
	routineGoogleSourcePrefix            = models.RoutineGoogleSourcePrefix
	routineEventSubscriptionPageLimit    = models.RoutineEventSubscriptionPageLimit
	maximumRoutineGrants                 = models.MaximumRoutineGrants
	maximumRoutineGrantLabel             = models.MaximumRoutineGrantLabel
	maximumRoutineGrantToolArguments     = models.MaximumRoutineGrantToolArguments
	maximumRoutineGrantDestinations      = models.MaximumRoutineGrantDestinations
	maximumRoutineGrantDestinationBytes  = models.MaximumRoutineGrantDestinationBytes
	maximumRoutineScriptCommand          = models.MaximumRoutineScriptCommand
	runReportingContextMilestones        = models.RunReportingMilestonesContextKey
	runReportingContextRootRunID         = models.RunReportingRootRunIDContextKey
	skillRuntimeMaintenanceWakeType      = models.SkillRuntimeMaintenanceWakeType
	skillSetupModePerPerson              = models.SkillSetupModePerPerson
	skillSetupModeShared                 = models.SkillSetupModeShared
	spokenResponseStyleInstruction       = models.SpokenResponseStyleInstruction
	spokenResponseAuthorityInstruction   = models.SpokenResponseAuthorityInstruction
	voiceCallGreetingBehaviorInstruction = models.VoiceCallGreetingBehaviorInstruction
	maximumSurfaceInteractionLimit       = models.MaximumSurfaceInteractionLimit
)

type (
	contentPartLimits               = models.ContentPartLimits
	conversationTaskAdmissionLookup = models.ConversationTaskAdmissionLookup
	nestedActionTurnState           = models.NestedActionTurnState
	workItemKey                     = models.WorkItemKey
)

func matchActionCredentialLeaseCore(lease ActionCredentialLease, call *ActionCall, run *AgentRun, transport string) error {
	return models.MatchActionCredentialLeaseCore(lease, call, run, transport)
}

func actionCredentialFields(references map[string]skill.CredentialReference, fields map[string][]string) ([]ActionCredentialFieldReference, error) {
	return models.ActionCredentialFieldReferences(references, fields)
}

func computeActionLeaseIdentity(scope Scope, actionCallID, runID string, lease ActionLeaseIdentity) string {
	return models.ComputeActionLeaseIdentity(scope, actionCallID, runID, lease)
}

func canonicalLeaseTime(value time.Time) time.Time {
	return models.CanonicalLeaseTime(value)
}

func runHasPausedActionDependency(run *AgentRun, call *ActionCall) bool {
	return models.RunHasPausedActionDependency(run, call)
}

func teamIDForRun(run *AgentRun) string {
	return models.TeamIDForRun(run)
}

func activityStreamID(event *ActivityEvent) string {
	return models.ActivityStreamID(event)
}

func isTerminalAgentRunStatus(status AgentRunStatus) bool {
	return models.IsTerminalAgentRunStatus(status)
}

func agentBuilderDraftStarter(conversation *Conversation, message *ChannelMessage) string {
	return models.AgentBuilderDraftStarterID(conversation, message)
}

func agentRunOwnerSchedulingKey(owner ObjectiveOwner) string {
	return models.AgentRunOwnerSchedulingKey(owner)
}

func evaluateAgentRunAdmission(runs []*AgentRun, objectives map[string]*Objective, claim AgentRunClaim) (*AgentRun, *AgentRunAdmissionDecision) {
	return models.EvaluateAgentRunAdmission(runs, objectives, claim)
}

func applyAgentRunClaim(run *AgentRun, claim AgentRunClaim) error {
	return models.ApplyAgentRunClaim(run, claim)
}

func runAttemptBudgetAtLimit(run *AgentRun) bool {
	return models.RunAttemptBudgetAtLimit(run)
}

func invalidEntry(format string, args ...interface{}) error {
	return models.InvalidSessionEntryf(format, args...)
}

func jsonPartBytes(raw json.RawMessage) int {
	return models.JSONPartBytes(raw)
}

func compactJSONValue(raw json.RawMessage) json.RawMessage {
	return models.CompactJSONValue(raw)
}

func stampAppendedSessionEntry(in *SessionEntry, req AppendAgentSessionEntriesRequest, sequence int64) *SessionEntry {
	return models.StampAppendedSessionEntry(in, req, sequence)
}

func applyToolEntryTransition(current *SessionEntry, req ToolEntryTransitionRequest) (*SessionEntry, error) {
	return models.ApplyToolEntryTransition(current, req)
}

func normalizeSessionEntryFilter(filter SessionEntryFilter) (SessionEntryFilter, error) {
	return models.NormalizeSessionEntryFilter(filter)
}

func sessionEntryMatchesFilter(e *SessionEntry, filter SessionEntryFilter) bool {
	return models.SessionEntryMatchesFilter(e, filter)
}

func buildAgentSessionPath(chain []*AgentSession, segments [][]*SessionEntry, compaction *SessionEntry) (*AgentSessionPath, error) {
	return models.BuildAgentSessionPath(chain, segments, compaction)
}

func initialEntriesAppend(req CreateRunAgentSessionTxRequest, sessionID string) AppendAgentSessionEntriesRequest {
	return models.InitialEntriesAppendRequest(req, sessionID)
}

func cutUTF8(text string, limit int) string {
	return models.TruncateUTF8(text, limit)
}

func matchesArtifactFilter(artifact *Artifact, filter ArtifactFilter) bool {
	return models.MatchesArtifactFilter(artifact, filter)
}

func containsString(filter []string, value string) bool {
	return models.MatchesStringFilter(filter, value)
}

func budgetReservationAdmission(policy BudgetPolicy, used, reservation BudgetUsage) BudgetAdmission {
	return models.BudgetReservationAdmission(policy, used, reservation)
}

func completeChildBudgetAllocation(parent *AgentRun, allocation *BudgetPolicy) (*BudgetPolicy, error) {
	return models.CompleteChildBudgetAllocation(parent, allocation)
}

func addRunBudgetAllocation(run *AgentRun, allocationID string, allocation *BudgetPolicy) error {
	return models.AddRunBudgetAllocation(run, allocationID, allocation)
}

func reserveRunBudget(run *AgentRun, reservation BudgetReservation) error {
	return models.ReserveRunBudget(run, reservation)
}

func settleRunBudgetReservation(run *AgentRun, reservationID string, usage BudgetUsage) error {
	return models.SettleRunBudgetReservation(run, reservationID, usage)
}

func releaseRunBudgetReservation(run *AgentRun, reservationID string) error {
	return models.ReleaseRunBudgetReservation(run, reservationID)
}

func allocateObjectiveRunBudget(objective *Objective, run *AgentRun, now time.Time) (*Objective, error) {
	return models.AllocateObjectiveRunBudget(objective, run, now)
}

func sumBudgetPolicies(policies map[string]BudgetPolicy) BudgetPolicy {
	return models.SumBudgetPolicies(policies)
}

func completionReviewCounts(decisions []AgentRequestCompletionReviewDecision) (approvals, rejections int) {
	return models.CompletionReviewCounts(decisions)
}

func requesterControlsRun(requester CollaborationParty, run *AgentRun) bool {
	return models.RequesterControlsRun(requester, run)
}

func collaborationChildOwner(source *AgentRun, request *AgentRequest) ObjectiveOwner {
	return models.CollaborationChildOwner(source, request)
}

func normalizeAgentRequestAcceptancePolicy(policy AgentRequestAcceptancePolicy) AgentRequestAcceptancePolicy {
	return models.NormalizeAgentRequestAcceptancePolicy(policy)
}

func controlJSONResult(status ToolResultStatus, value map[string]interface{}) *ToolResult {
	return models.ControlJSONResult(status, value)
}

func normalizeAgentBuilderParameters(in *AgentBuilderParameters) *AgentBuilderParameters {
	return models.NormalizeAgentBuilderParameters(in)
}

func newChannelMessageQuote(message *ChannelMessage) *ChannelMessageQuote {
	return models.NewChannelMessageQuote(message)
}

func applyConversationDefaults(conversation *Conversation) {
	models.ApplyConversationDefaults(conversation)
}

func conversationFollowUpInterventionID(scope Scope, messageID string) string {
	return models.ConversationFollowUpInterventionID(scope, messageID)
}

func conversationRunFollowUps(run *AgentRun) []ConversationMessageDelivery {
	return models.ConversationRunFollowUps(run)
}

func newConversationReactionEvent(eventType string, record MessageReactionRecord, sequence int64) *ConversationReactionEvent {
	return models.NewConversationReactionEvent(eventType, record, sequence)
}

func persistedChannelMessage(message *ChannelMessage) *ChannelMessage {
	return models.PersistedChannelMessage(message)
}

func sameChannelMessage(left, right *ChannelMessage) bool {
	return models.SameChannelMessage(left, right)
}

func sameParticipationRound(left, right *ParticipationRound) bool {
	return models.SameParticipationRound(left, right)
}

func externalChannelContext(conversation *Conversation) bool {
	return models.IsExternalChannelConversation(conversation)
}

func externalConversationThreadRoot(conversation *Conversation, trigger *ChannelMessage) string {
	return models.ExternalConversationThreadRoot(conversation, trigger)
}

func replyRunGuardHolds(run *AgentRun, guard *ReplyRunGuard, now time.Time) error {
	return models.ReplyRunGuardHolds(run, guard, now)
}

func sameRequestedSpace(existing *Conversation, spaceID string) bool {
	return models.SameRequestedSpace(existing, spaceID)
}

func stableConversationID(scope Scope, key, suffix string) string {
	return models.StableConversationID(scope, key, suffix)
}

func conversationSessionID(scope Scope, lane string) string {
	return models.ConversationSessionID(scope, lane)
}

func conversationRunSession(ctx context.Context, sessions AgentSessionStore, run *AgentRun) (*AgentSession, error) {
	return models.ConversationRunSession(ctx, sessions, run)
}

func newChildSession(req ChildSessionRequest) (CreateRunAgentSessionTxRequest, error) {
	return models.NewChildSession(req)
}

func conversationTaskBrief(goal string) string {
	return models.ConversationTaskBrief(goal)
}

func childSessionSeed(scope Scope, runID, kind, brief string) (*SessionEntry, error) {
	return models.ChildSessionSeed(scope, runID, kind, brief)
}

func conversationTaskDigest(task *ConversationTask) (string, error) {
	return models.ConversationTaskDigest(task)
}

func sameConversationTask(a, b *ConversationTask) bool {
	return models.SameConversationTask(a, b)
}

func conversationTaskID(scope Scope, sourceRunID, key string) string {
	return models.ConversationTaskID(scope, sourceRunID, key)
}

func prepareConversationTaskAdmission(ctx context.Context, claim *AgentRunClaim, runs []*AgentRun, lookup conversationTaskAdmissionLookup) error {
	return models.PrepareConversationTaskAdmission(ctx, claim, runs, lookup)
}

func conversationTaskAdmissionLineage(ctx context.Context, scope Scope, task *ConversationTask, candidate *AgentRun, getRun func(string) (*AgentRun, error), getRequest func(context.Context, Scope, string) (*AgentRequest, error)) (bool, error) {
	return models.ConversationTaskAdmissionLineage(ctx, scope, task, candidate, getRun, getRequest)
}

func conversationTaskContinuationNeedsAssignmentProof(task *ConversationTask, run *AgentRun) bool {
	return models.ConversationTaskContinuationNeedsAssignmentProof(task, run)
}

func conversationTaskLedgerParticipationRoundKey(task *ConversationTask) string {
	return models.ConversationTaskLedgerParticipationRoundKey(task)
}

func buildConversationTaskAcknowledgment(task *ConversationTask, conversation *Conversation, source *ChannelMessage) (ChannelMessageCommitRecord, error) {
	return models.BuildConversationTaskAcknowledgment(task, conversation, source)
}

func conversationTaskContinuationCandidate(run *AgentRun) bool {
	return models.ConversationTaskContinuationCandidate(run)
}

func conversationTaskFinalResponseKey(run *AgentRun) string {
	return models.ConversationTaskFinalResponseKey(run)
}

func conversationRunResponseKey(run *AgentRun, triggerID string) string {
	return models.ConversationRunResponseKey(run, triggerID)
}

func conversationTaskFinalResponseKeys(run *AgentRun) []string {
	return models.ConversationTaskFinalResponseKeys(run)
}

func conversationTaskFinishedBeforePromotion(step *AgentTurn) bool {
	return models.ConversationTaskFinishedBeforePromotion(step)
}

func buildConversationTaskContinuation(req ConversationTaskPromotionRequest, run *AgentRun, conversation *Conversation, message *ChannelMessage, chat *AgentSession) (*ConversationTask, *AgentRun, *ActivityEvent, *models.ConversationTaskPromotionFork, error) {
	return models.BuildConversationTaskContinuation(req, run, conversation, message, chat)
}

func replayConversationTaskContinuation(task *ConversationTask, run *AgentRun) (*ConversationTaskResult, error) {
	return models.ReplayConversationTaskContinuation(task, run)
}

func conversationTaskSteeringSettled(record ConversationTaskCreateRecord, current *AgentRun) error {
	return models.ConversationTaskSteeringSettled(record, current)
}

func verifyConversationTaskToolEntry(record ConversationTaskCreateRecord, current *AgentRun, entry, assistant *SessionEntry) error {
	return models.VerifyConversationTaskToolEntry(record, current, entry, assistant)
}

func conversationTaskToolReplayMatches(record ConversationTaskCreateRecord, entry *SessionEntry) error {
	return models.ConversationTaskToolReplayMatches(record, entry)
}

func sameToolResult(a, b *ToolResult) bool {
	return models.SameToolResult(a, b)
}

func optionalSessionEntry(entry *SessionEntry, err error) (*SessionEntry, error) {
	return models.OptionalSessionEntry(entry, err)
}

func isTeamMemberRun(run *AgentRun) bool {
	return models.IsTeamMemberRun(run)
}

func applyRunDependencyResolution(group *RunDependencyGroup, edges []*RunDependency, source *AgentRun, record RunDependencyResolutionRecord) (*RunDependencyResult, error) {
	return models.ApplyRunDependencyResolution(group, edges, source, record)
}

func isTerminalDependencyGroup(group *RunDependencyGroup) bool {
	return models.IsTerminalDependencyGroup(group)
}

func sourceWaitsForDependencyGroup(source *AgentRun, groupID string) (waiting, paused bool) {
	return models.SourceWaitsForDependencyGroup(source, groupID)
}

func applyDependencyGroupReconciliation(group *RunDependencyGroup, edges []*RunDependency, source *AgentRun, targets map[string]*AgentRun, record RunDependencyGroupReconciliationRecord) (*RunDependencyResult, error) {
	return models.ApplyDependencyGroupReconciliation(group, edges, source, targets, record)
}

func dependencyGroupToolTransition(record RunDependencyGroupCreateRecord) ToolEntryTransitionRequest {
	return models.DependencyGroupToolTransition(record)
}

func dependencyGroupChildSession(record RunDependencyGroupCreateRecord, i int) CreateRunAgentSessionTxRequest {
	return models.DependencyGroupChildSession(record, i)
}

func checkDependencyGroupCreateSource(current *AgentRun, record RunDependencyGroupCreateRecord) error {
	return models.CheckDependencyGroupCreateSource(current, record)
}

func sameDependencyGroupRecord(group *RunDependencyGroup, edges []*RunDependency, record RunDependencyGroupCreateRecord) bool {
	return models.SameDependencyGroupRecord(group, edges, record)
}

func normalizeEmbedOrigin(value string) (string, error) {
	return models.NormalizeEmbedOrigin(value)
}

func applyExternalConversationReplyProgress(current *ExternalConversationInboxItem, expectedRevision int64, progress ExternalConversationReplyProgress) (*ExternalConversationInboxItem, error) {
	return models.ApplyExternalConversationReplyProgress(current, expectedRevision, progress)
}

func hasConversationFeature(values []capability.ConversationAdapterFeature, wanted capability.ConversationAdapterFeature) bool {
	return models.HasConversationFeature(values, wanted)
}

func gatewayAuthorityFromEndpoints(gateway ExternalConversationIngressGateway, endpoints []*ExternalConversationEndpoint) (ExternalConversationIngressGateway, bool, error) {
	return models.GatewayAuthorityFromEndpoints(gateway, endpoints)
}

func externalConversationEndpointMatchesVerifiedRoute(endpoint *ExternalConversationEndpoint, route ExternalConversationVerifiedRoute) bool {
	return models.ExternalConversationEndpointMatchesVerifiedRoute(endpoint, route)
}

func sameExternalConversationInboxIntent(existing, candidate *ExternalConversationInboxItem) bool {
	return models.SameExternalConversationInboxIntent(existing, candidate)
}

func sameExternalConversationDeliveryIntent(existing, candidate *ExternalConversationDelivery) bool {
	return models.SameExternalConversationDeliveryIntent(existing, candidate)
}

func sameExternalConversationDeliveryEffect(existing, candidate *ExternalConversationDelivery) bool {
	return models.SameExternalConversationDeliveryEffect(existing, candidate)
}

func rebindExternalConversationDelivery(existing, candidate *ExternalConversationDelivery) (*ExternalConversationDelivery, bool) {
	return models.RebindExternalConversationDelivery(existing, candidate)
}

func externalConversationAdapterBelongsToEndpoint(endpoint, captured ExternalConversationAdapterReference) bool {
	return models.ExternalConversationAdapterBelongsToEndpoint(endpoint, captured)
}

func externalOperationProtects(status ActionCallStatus) bool {
	return models.ExternalOperationProtects(status)
}

func sortAgentRuns(runs []*AgentRun, order AgentRunOrder) {
	models.SortAgentRuns(runs, order)
}

func normalizeRunKind(kind RunKind) RunKind {
	return models.NormalizeRunKind(kind)
}

func matchesObjectiveFilter(objective *Objective, filter ObjectiveFilter) bool {
	return models.MatchesObjectiveFilter(objective, filter)
}

func matchesRunFilter(run *AgentRun, filter AgentRunFilter) bool {
	return models.MatchesRunFilter(run, filter)
}

func routineEventPlaceField(kind RoutineEventKind) string {
	return models.RoutineEventPlaceField(kind)
}

func routineEventIndexOf(activation *RunbookActivation) (RoutineEventKind, string) {
	return models.RoutineEventIndexOf(activation)
}

func routineEventSubscriptionOf(activation *RunbookActivation) (RoutineEventSubscription, bool) {
	return models.RoutineEventSubscriptionOf(activation)
}

func prepareRoutineEventRecord(record *RoutineEventRecord) error {
	return models.PrepareRoutineEventRecord(record)
}

func newRoutineEventBatchID() string {
	return models.NewRoutineEventBatchID()
}

func normalizeRoutineEventBatchSize(maxEvents int) int {
	return models.NormalizeRoutineEventBatchSize(maxEvents)
}

func normalizeRoutineEventLimit(limit, ceiling int) int {
	return models.NormalizeRoutineEventLimit(limit, ceiling)
}

func encodeRoutineEventSubscriptionCursor(subscription RoutineEventSubscription) string {
	return models.EncodeRoutineEventSubscriptionCursor(subscription)
}

func decodeRoutineEventSubscriptionCursor(cursor string) (scopeKind, scopeID, activationID string, err error) {
	return models.DecodeRoutineEventSubscriptionCursor(cursor)
}

func routineEventKindStrings(kinds []RoutineEventKind) ([]string, error) {
	return models.RoutineEventKindStrings(kinds)
}

func uniqueRoutineActivationIDs(ids []string) []string {
	return models.UniqueRoutineActivationIDs(ids)
}

func finishRoutineEventActivity(activity *RoutineEventActivity, maxPerHour int, oldestInHour *time.Time) {
	models.FinishRoutineEventActivity(activity, maxPerHour, oldestInHour)
}

func prepareHook(hook *Hook) (*Hook, error) {
	return models.PrepareHook(hook)
}

func sameHookOwner(a, b *Hook) bool {
	return models.SameHookOwner(a, b)
}

func routineBriefText(value string, limit int) (string, error) {
	return models.RoutineBriefText(value, limit)
}

func normalizeRoutineGrants(values []RoutineGrant, allowEmptyLabels bool) ([]RoutineGrant, error) {
	return models.NormalizeRoutineGrants(values, allowEmptyLabels)
}

func normalizeRoutineGrantDestination(value string) string {
	return models.NormalizeRoutineGrantDestination(value)
}

func normalizeRoutineScriptCommand(command, path string) (string, error) {
	return models.NormalizeRoutineScriptCommand(command, path)
}

func hashString(value string) string {
	return models.SHA256DigestString(value)
}

func hashBytes(value []byte) string {
	return models.SHA256Digest(value)
}

func runReportsMilestone(run *AgentRun, milestone runbook.ReportingMilestone) bool {
	return models.RunReportsMilestone(run, milestone)
}

func runSkillDependencyDeployment(run *AgentRun) string {
	return models.RunSkillDependencyDeployment(run)
}

func legacyAcceptedRunExecution(run *AgentRun, deployment *kernelagent.AgentDeployment, definition *kernelagent.AgentDefinition) *AcceptedRunExecution {
	return models.LegacyAcceptedRunExecution(run, deployment, definition)
}

func runNeedsTerminalReporting(run *AgentRun) bool {
	return models.RunNeedsTerminalReporting(run)
}

func matchesRunbookActivationFilter(value *RunbookActivation, filter RunbookActivationFilter) bool {
	return models.MatchesRunbookActivationFilter(value, filter)
}

func sortAndLimitRunbookActivations(values []*RunbookActivation, limit, offset int) []*RunbookActivation {
	return models.SortAndLimitRunbookActivations(values, limit, offset)
}

func runbookBudgetPolicyValue(budget *runbook.BudgetAllocation) *BudgetPolicy {
	return models.BudgetPolicyFromRunbook(budget)
}

func nextSkillRuntimeMaintenance(current *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceRequest) (*SkillRuntimeMaintenance, bool, error) {
	return models.NextSkillRuntimeMaintenance(current, r)
}

func beginMaintenanceRollbackRecord(g *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceCompletion) *SkillRuntimeMaintenance {
	return models.BeginMaintenanceRollbackRecord(g, r)
}

func completeMaintenanceRecord(g *SkillRuntimeMaintenance, r SkillRuntimeMaintenanceCompletion) *SkillRuntimeMaintenance {
	return models.CompleteMaintenanceRecord(g, r)
}

func executeSkillRuntimeMaintenanceMutation(ctx context.Context, proof, current *SkillRuntimeMaintenance, mutation func(context.Context) error) error {
	return models.ExecuteSkillRuntimeMaintenanceMutation(ctx, proof, current, mutation)
}

func skillBindingUpgradeUsageFilter(application *SkillReferenceUpgradeMutation, bindingID string) SkillRuntimeUsageFilter {
	return models.SkillBindingUpgradeUsageFilter(application, bindingID)
}

func invalidSkillSetupField(field, problem string) error {
	return models.NewSkillSetupValidationError(field, problem)
}

func interactionChatOwner(owner ObjectiveOwner, agentID string) ObjectiveOwner {
	return models.InteractionChatOwner(owner, agentID)
}

func moveSpace(ordered []*Space, spaceID string, position int) map[string]int {
	return models.SpacePositionsAfterMove(ordered, spaceID, position)
}

func applySpacePatch(space *Space, patch SpacePatch, now time.Time) {
	models.ApplySpacePatch(space, patch, now)
}

func voiceCallStartedMessage(message *ChannelMessage) bool {
	return models.VoiceCallStartedMessage(message)
}

func sameSurfaceInteractionTarget(left, right *SurfaceInteraction) bool {
	return models.SameSurfaceInteractionTarget(left, right)
}

func latestSurfaceInteractions(interactions []*SurfaceInteraction, acrossActors bool) []*SurfaceInteraction {
	return models.LatestSurfaceInteractions(interactions, acrossActors)
}

func failedConversationReportCandidate(run *AgentRun) bool {
	return models.FailedConversationReportCandidate(run)
}

func terminalAgentTurnStatus(status AgentTurnStatus) bool {
	return models.TerminalAgentTurnStatus(status)
}
