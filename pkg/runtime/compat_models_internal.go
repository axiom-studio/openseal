// Temporary compatibility for the pkg/runtime split. New code imports
// pkg/models. Remove once in-flight branches have landed.
//
// Unexported names that runtime code still uses for declarations
// moved to pkg/models: constants, types and forwarding functions.

package runtime

import (
	"context"
	"crypto/sha256"
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
	return models.CheckActionCredentialLeaseCore(lease, call, run, transport)
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

func runAttemptBudgetExceeded(run *AgentRun) bool {
	return models.RunAttemptBudgetExceeded(run)
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
	return models.CheckReplyRunGuard(run, guard, now)
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
	return models.CheckConversationTaskSteeringSettled(record, current)
}

func verifyConversationTaskToolEntry(record ConversationTaskCreateRecord, current *AgentRun, entry, assistant *SessionEntry) error {
	return models.VerifyConversationTaskToolEntry(record, current, entry, assistant)
}

func conversationTaskToolReplayMatches(record ConversationTaskCreateRecord, entry *SessionEntry) error {
	return models.CheckConversationTaskToolReplay(record, entry)
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
	return models.IsTerminalAgentTurnStatus(status)
}

// matchActionCredentialLeaseUmbrella forwards to models.CheckActionCredentialLeaseUmbrella.
func matchActionCredentialLeaseUmbrella(lease models.ActionCredentialLease, run *models.AgentRun, binding *skill.Binding) error {
	return models.CheckActionCredentialLeaseUmbrella(lease, run, binding)
}

// digestActionCredentialFieldSelection forwards to models.ActionCredentialFieldSelectionDigest.
func digestActionCredentialFieldSelection(fields map[string][]string) (string, error) {
	return models.ActionCredentialFieldSelectionDigest(fields)
}

// actionCredentialLeaseNonceDigest forwards to models.ActionCredentialLeaseNonceDigest.
func actionCredentialLeaseNonceDigest(lease models.ActionCredentialLease) [sha256.Size]byte {
	return models.ActionCredentialLeaseNonceDigest(lease)
}

// matchCurrentActionCredentialBinding forwards to models.CheckCurrentActionCredentialBinding.
func matchCurrentActionCredentialBinding(lease models.ActionCredentialLease, call *models.ActionCall, binding *skill.Binding) error {
	return models.CheckCurrentActionCredentialBinding(lease, call, binding)
}

// externalOperationClaimDigest forwards to models.ExternalOperationClaimDigest.
func externalOperationClaimDigest(call *models.ActionCall) string {
	return models.ExternalOperationClaimDigest(call)
}

// validateObjectiveActivity forwards to models.ValidateObjectiveActivity.
func validateObjectiveActivity(objective *models.Objective, event *models.ActivityEvent) error {
	return models.ValidateObjectiveActivity(objective, event)
}

// reconcileAgentRunbookActivations forwards to models.ReconcileAgentRunbookActivations.
func reconcileAgentRunbookActivations(current []*models.RunbookActivation, definition *kernelagent.AgentDefinition, deployment *kernelagent.AgentDeployment, updatedAt time.Time) ([]*models.RunbookActivation, error) {
	return models.ReconcileAgentRunbookActivations(current, definition, deployment, updatedAt)
}

// normalizeStoredCallbackReceipt forwards to models.NormalizeStoredCallbackReceipt.
func normalizeStoredCallbackReceipt(value *models.CallbackEventReceipt) *models.CallbackEventReceipt {
	return models.NormalizeStoredCallbackReceipt(value)
}

// normalizeExternalConversationPage forwards to models.NormalizeExternalConversationPage.
func normalizeExternalConversationPage(limit, offset int) (int, int) {
	return models.NormalizeExternalConversationPage(limit, offset)
}

// validExternalConversationInboxSave forwards to models.IsValidExternalConversationInboxSave.
func validExternalConversationInboxSave(current, next *models.ExternalConversationInboxItem, expectedRevision int64, leaseOwner string) bool {
	return models.IsValidExternalConversationInboxSave(current, next, expectedRevision, leaseOwner)
}

// validExternalConversationDeliverySave forwards to models.IsValidExternalConversationDeliverySave.
func validExternalConversationDeliverySave(current, next *models.ExternalConversationDelivery, expectedRevision int64, leaseOwner string) bool {
	return models.IsValidExternalConversationDeliverySave(current, next, expectedRevision, leaseOwner)
}

// summarizeAgentRunOwners forwards to models.SummarizeAgentRunOwners.
func summarizeAgentRunOwners(runs []*models.AgentRun, owners []models.ObjectiveOwner) []models.AgentRunOwnerSummary {
	return models.SummarizeAgentRunOwners(runs, owners)
}

// pageObjectives forwards to models.PageObjectives.
func pageObjectives(values []*models.Objective, offset, limit int) []*models.Objective {
	return models.PageObjectives(values, offset, limit)
}

// normalizeRunSkillDependencyError forwards to models.NormalizeRunSkillDependencyError.
func normalizeRunSkillDependencyError(err error) error {
	return models.NormalizeRunSkillDependencyError(err)
}

// cloneBudgetUsage forwards to models.CloneBudgetUsage.
func cloneBudgetUsage(usage *models.BudgetUsage) *models.BudgetUsage {
	return models.CloneBudgetUsage(usage)
}

// canTransitionAgentRun forwards to models.CanTransitionAgentRun.
func canTransitionAgentRun(from, to models.AgentRunStatus) bool {
	return models.CanTransitionAgentRun(from, to)
}

// isWaitingRunStatus forwards to models.IsWaitingRunStatus.
func isWaitingRunStatus(status models.AgentRunStatus) bool {
	return models.IsWaitingRunStatus(status)
}

// terminalCreditError forwards to models.TerminalCreditError.
func terminalCreditError(status models.AgentRunStatus, failure *models.RunFailureExplanation) *models.RunCreditError {
	return models.TerminalCreditError(status, failure)
}

// projectActivityEvent forwards to models.ProjectActivityEvent.
func projectActivityEvent(event *models.ActivityEvent, includeDetails bool) ActivityProjection {
	return models.ProjectActivityEvent(event, includeDetails)
}

// encodeActivityFeedCursor forwards to models.EncodeActivityFeedCursor.
func encodeActivityFeedCursor(event *models.ActivityEvent) (string, error) {
	return models.EncodeActivityFeedCursor(event)
}

// decodeActivityFeedCursor forwards to models.DecodeActivityFeedCursor.
func decodeActivityFeedCursor(value string) (*models.ActivityFeedCursor, error) {
	return models.DecodeActivityFeedCursor(value)
}

const agentProfileResourceType = models.AgentProfileResourceType

const agentBehaviorResourceType = models.AgentBehaviorResourceType

// normalizedActionStrings forwards to models.TrimmedUniqueStrings.
func normalizedActionStrings(values []string) []string {
	return models.TrimmedUniqueStrings(values)
}

// isAgentManagementAction forwards to models.IsAgentManagementAction.
func isAgentManagementAction(bound *skill.BoundAction) bool {
	return models.IsAgentManagementAction(bound)
}

// canonicalActionReview forwards to models.CanonicalActionReview.
func canonicalActionReview(definition *skill.Definition, action skill.Action) skill.Action {
	return models.CanonicalActionReview(definition, action)
}

const maxAgentDisplayNameRunes = models.MaxAgentDisplayNameRunes

const maxAgentBuilderSkillGrants = models.MaxAgentBuilderSkillGrants

const maxAgentBuilderConnections = models.MaxAgentBuilderConnections

const maxAgentTrialMessageRunes = models.MaxAgentTrialMessageRunes

type agentProfileArguments = models.AgentProfileArguments

type agentCreateArguments = models.AgentCreateArguments

type agentUpdateArguments = models.AgentUpdateArguments

type agentTryArguments = models.AgentTryArguments

type agentListArguments = models.AgentListArguments

// normalizedAgentName forwards to models.NormalizedAgentName.
func normalizedAgentName(name string) string {
	return models.NormalizedAgentName(name)
}

// validateAgentProfileText forwards to models.ValidateAgentProfileText.
func validateAgentProfileText(profile agentProfileArguments) error {
	return models.ValidateAgentProfileText(profile)
}

// validateAgentProfileFields forwards to models.ValidateAgentProfileFields.
func validateAgentProfileFields(profile agentProfileArguments, fields map[string]bool) error {
	return models.ValidateAgentProfileFields(profile, fields)
}

// validateAgentDisplayName forwards to models.ValidateAgentDisplayName.
func validateAgentDisplayName(name string) error {
	return models.ValidateAgentDisplayName(name)
}

// agentBuilderScope forwards to models.ScopeReference.
func agentBuilderScope(scope models.Scope) capability.ScopeReference {
	return models.ScopeReference(scope)
}

// toolOutputPreviewText forwards to models.ToolOutputPreviewText.
func toolOutputPreviewText(content []models.ContentPart) string {
	return models.ToolOutputPreviewText(content)
}

// compactToolResultContent forwards to models.CompactToolResultContent.
func compactToolResultContent(content []models.ContentPart, total int, reason string) []models.ContentPart {
	return models.CompactToolResultContent(content, total, reason)
}

// normalizeArtifact forwards to models.NormalizeArtifact.
func normalizeArtifact(artifact *models.Artifact) {
	models.NormalizeArtifact(artifact)
}

// runContextConversationID forwards to models.RunContextConversationID.
func runContextConversationID(run *models.AgentRun) string {
	return models.RunContextConversationID(run)
}

// markRunFiles forwards to models.MarkRunFiles.
func markRunFiles(ctx context.Context, store models.ArtifactStore, scope models.Scope, saved []savedArtifact, handedOver []models.ConversationReference) error {
	return models.MarkRunFiles(ctx, store, scope, saved, handedOver)
}

const maximumAskUserQuestionBytes = models.MaximumAskUserQuestionBytes

type askUserArgs = models.AskUserArgs

// parseAskUserArgs forwards to models.ParseAskUserArgs.
func parseAskUserArgs(raw json.RawMessage) (*askUserArgs, error) {
	return models.ParseAskUserArgs(raw)
}

// askUserEntryArgs forwards to models.AskUserEntryArgs.
func askUserEntryArgs(entry *models.SessionEntry) *askUserArgs {
	return models.AskUserEntryArgs(entry)
}

// askUserQuestion forwards to models.AskUserEntryQuestion.
func askUserQuestion(entry *models.SessionEntry) string {
	return models.AskUserEntryQuestion(entry)
}

// validateConversationTaskStartAuthority forwards to models.ValidateConversationTaskStartAuthority.
func validateConversationTaskStartAuthority(run *models.AgentRun, taskContext *HostedConversationTaskContext) error {
	return models.ValidateConversationTaskStartAuthority(run, taskContext)
}

const maximumStartTaskGoalBytes = models.MaximumStartTaskGoalBytes

const maximumStartTaskAcknowledgmentBytes = models.MaximumStartTaskAcknowledgmentBytes

type startTaskArgs = models.StartTaskArgs

// parseStartTaskArgs forwards to models.ParseStartTaskArgs.
func parseStartTaskArgs(raw json.RawMessage) (*startTaskArgs, error) {
	return models.ParseStartTaskArgs(raw)
}

// startTaskArguments forwards to models.StartTaskEntryArgs.
func startTaskArguments(entry *models.SessionEntry) *startTaskArgs {
	return models.StartTaskEntryArgs(entry)
}

// truncateRunes forwards to models.TruncateRunes.
func truncateRunes(value string, limit int) string {
	return models.TruncateRunes(value, limit)
}

// teamRosterReader forwards to models.TeamRosterReaderOf.
func teamRosterReader(store any) models.TeamRosterReader {
	return models.TeamRosterReaderOf(store)
}

// startTaskKey forwards to models.StartTaskKey.
func startTaskKey(entryID string) string {
	return models.StartTaskKey(entryID)
}

// decodeControlArguments forwards to models.DecodeControlArguments.
func decodeControlArguments(raw json.RawMessage, target interface{}) error {
	return models.DecodeControlArguments(raw, target)
}

type agentRunReader = models.AgentRunReader

// inheritRoutineEventBatch forwards to models.InheritRoutineEventBatch.
func inheritRoutineEventBatch(source *models.AgentRun, context map[string]interface{}) map[string]interface{} {
	return models.InheritRoutineEventBatch(source, context)
}

// routineEventBatchOf forwards to models.RoutineEventBatchID.
func routineEventBatchOf(run *models.AgentRun) string {
	return models.RoutineEventBatchID(run)
}

type savedArtifact = models.SavedArtifact

// teamRoster forwards to models.TeamRoster.
func teamRoster(ctx context.Context, teams models.TeamRosterReader, scope models.Scope, teamID string) ([]string, bool, error) {
	return models.TeamRoster(ctx, teams, scope, teamID)
}

// canEnterDependencyWait forwards to models.CanEnterDependencyWait.
func canEnterDependencyWait(run *models.AgentRun) bool {
	return models.CanEnterDependencyWait(run)
}

// sameDependencyGroupRequest forwards to models.SameDependencyGroupRequest.
func sameDependencyGroupRequest(group *models.RunDependencyGroup, edges []*models.RunDependency, req CreateRunDependencyGroupRequest) bool {
	return models.SameDependencyGroupRequest(group, edges, req)
}

// sourceLabel forwards to models.ExternalSourceLabel.
func sourceLabel(value string, max int) string {
	return models.ExternalSourceLabel(value, max)
}

// externalOperationReceiptComplete forwards to models.ExternalOperationReceiptComplete.
func externalOperationReceiptComplete(status models.ActionCallStatus) bool {
	return models.ExternalOperationReceiptComplete(status)
}

const maxModelStepMediaBytes = models.MaxModelStepMediaBytes

// validModelStepToolName forwards to models.ValidModelStepToolName.
func validModelStepToolName(name string) bool {
	return models.ValidModelStepToolName(name)
}

// objectiveActivityEvent forwards to models.ObjectiveActivityEvent.
func objectiveActivityEvent(objective *models.Objective, actor models.ActivityActor, visibility models.ActivityVisibility, eventType, summary string, occurredAt time.Time, payload map[string]interface{}) *models.ActivityEvent {
	return models.ObjectiveActivityEvent(objective, actor, visibility, eventType, summary, occurredAt, payload)
}

// canTransitionObjective forwards to models.CanTransitionObjective.
func canTransitionObjective(from, to models.ObjectiveStatus) bool {
	return models.CanTransitionObjective(from, to)
}

// routineUngrantableSkill forwards to models.RoutineUngrantableSkill.
func routineUngrantableSkill(skillID string) bool {
	return models.RoutineUngrantableSkill(skillID)
}

// routineGrantIdentity forwards to models.RoutineGrantIdentity.
func routineGrantIdentity(skillID, action string) string {
	return models.RoutineGrantIdentity(skillID, action)
}

// routineRecipientKey forwards to models.IsRoutineRecipientKey.
func routineRecipientKey(name string) bool {
	return models.IsRoutineRecipientKey(name)
}

// routineActionDestinationKeys forwards to models.RoutineActionDestinationKeys.
func routineActionDestinationKeys(schema map[string]interface{}) []string {
	return models.RoutineActionDestinationKeys(schema)
}

// routineActionPostsToDestination forwards to models.RoutineActionPostsToDestination.
func routineActionPostsToDestination(action skill.Action) bool {
	return models.RoutineActionPostsToDestination(action)
}

type routineGrantStore = models.RoutineGrantStore

// routineOwner forwards to models.RoutineOwner.
func routineOwner(activation *models.RunbookActivation) (models.ConversationParticipant, error) {
	return models.RoutineOwner(activation)
}

// resolveRoutineGrant forwards to models.ResolveRoutineGrant.
func resolveRoutineGrant(ctx context.Context, runs agentRunReader, store routineGrantStore, owners RoutineOwnerAddressResolver, run *models.AgentRun, bound *skill.BoundAction, arguments map[string]interface{}) (*models.RoutineGrant, error) {
	return models.ResolveRoutineGrant(ctx, runs, store, owners, run, bound, arguments)
}

const scheduledTaskContextKey = models.ScheduledTaskContextKey

// agentsInSpace forwards to models.CheckAgentsInSpace.
func agentsInSpace(space *models.Space, members []*models.SpaceMember, agentIDs []string) error {
	return models.CheckAgentsInSpace(space, members, agentIDs)
}

// boundStepNarration forwards to models.BoundStepNarration.
func boundStepNarration(text string) string {
	return models.BoundStepNarration(text)
}

// normalizeModelIdentity forwards to models.NormalizeModelIdentity.
func normalizeModelIdentity(provider, model string) (string, string, error) {
	return models.NormalizeModelIdentity(provider, model)
}

// projectTurnAnswerPreview forwards to models.ProjectTurnAnswerPreview.
func projectTurnAnswerPreview(event *models.ActivityEvent) *TurnAnswerPreview {
	return models.ProjectTurnAnswerPreview(event)
}
