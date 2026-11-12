// Temporary compatibility for the pkg/runtime split. Store code moving
// to store/postgres and store/sqlite imports internal/sqlstore directly.
// Remove once those moves have landed.
//
// Unexported names that runtime store code still uses for declarations
// moved to pkg/runtime/internal/sqlstore: constants, types and forwarding
// functions.

package runtime

import (
	"context"
	"database/sql"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/internal/sqlstore"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"github.com/axiom-studio/openseal/pkg/workforce"
)

const actionCredentialLeaseCleanupBatch = sqlstore.ActionCredentialLeaseCleanupBatch

// decodeActionCall forwards to sqlstore.DecodeActionCall.
func decodeActionCall(payload string) (*models.ActionCall, error) {
	return sqlstore.DecodeActionCall(payload)
}

// decodeApproval forwards to sqlstore.DecodeApproval.
func decodeApproval(payload string) (*models.ApprovalCheckpoint, error) {
	return sqlstore.DecodeApproval(payload)
}

const retiredChangeSetTable = sqlstore.RetiredChangeSetTable

const agentBuilderCutoverTable = sqlstore.AgentBuilderCutoverTable

// foldChangeSetDrafts forwards to sqlstore.FoldChangeSetDrafts.
func foldChangeSetDrafts(ctx context.Context, tx sqlstore.ExecQuerier, table, selectQuery, setPrompts, deleteRow string) error {
	return sqlstore.FoldChangeSetDrafts(ctx, tx, table, selectQuery, setPrompts, deleteRow)
}

// scanAgentBuilderCutoverDrafts forwards to sqlstore.ScanAgentBuilderCutoverDrafts.
func scanAgentBuilderCutoverDrafts(rows *sql.Rows) ([]models.AgentBuilderCutoverDraft, error) {
	return sqlstore.ScanAgentBuilderCutoverDrafts(rows)
}

type endpointUpdateExecutor = sqlstore.Execer

// registryPayloads forwards to sqlstore.AgentRegistryPayloads.
func registryPayloads(deployment *kernelagent.AgentDeployment, activation kernelagent.DefinitionActivation) (string, string, error) {
	return sqlstore.AgentRegistryPayloads(deployment, activation)
}

// artifactListQuery forwards to sqlstore.ArtifactListQuery.
func artifactListQuery(filter models.ArtifactFilter, table, workingTable string, postgres bool) (string, []any) {
	return sqlstore.ArtifactListQuery(filter, table, workingTable, postgres)
}

// queryArtifactPage forwards to sqlstore.QueryArtifactPage.
func queryArtifactPage(ctx context.Context, db *sql.DB, query string, args []any) ([]*models.Artifact, error) {
	return sqlstore.QueryArtifactPage(ctx, db, query, args)
}

// decodeArtifact forwards to sqlstore.DecodeArtifact.
func decodeArtifact(payload string) (*models.Artifact, error) {
	return sqlstore.DecodeArtifact(payload)
}

// workingArtifactIDs forwards to sqlstore.WorkingArtifactIDs.
func workingArtifactIDs(ctx context.Context, db *sql.DB, query string, scope models.Scope) (map[string]bool, error) {
	return sqlstore.WorkingArtifactIDs(ctx, db, query, scope)
}

// decodeAgentRequest forwards to sqlstore.DecodeAgentRequest.
func decodeAgentRequest(payload string) (*models.AgentRequest, error) {
	return sqlstore.DecodeAgentRequest(payload)
}

type connectOnceMigration = sqlstore.ConnectOnceMigration

// scanConversationActiveRunsSQL forwards to sqlstore.ScanConversationActiveRuns.
func scanConversationActiveRunsSQL(rows *sql.Rows) ([]*models.AgentRun, error) {
	return sqlstore.ScanConversationActiveRuns(rows)
}

const conversationActiveRunProjectionColumns = sqlstore.ConversationActiveRunProjectionColumns

// conversationForegroundSQLPageSize forwards to sqlstore.ConversationForegroundPageSize.
func conversationForegroundSQLPageSize(limit, offset int) int {
	return sqlstore.ConversationForegroundPageSize(limit, offset)
}

// scanConversationForegroundRunsSQL forwards to sqlstore.ScanConversationForegroundRuns.
func scanConversationForegroundRunsSQL(rows *sql.Rows) ([]*models.AgentRun, error) {
	return sqlstore.ScanConversationForegroundRuns(rows)
}

// getChannelMessageBatchSQL forwards to sqlstore.GetChannelMessageBatch.
func getChannelMessageBatchSQL(ctx context.Context, reader sqlstore.Querier, table string, scope models.Scope, conversationID string, ids []string, postgres bool) ([]*models.ChannelMessage, error) {
	return sqlstore.GetChannelMessageBatch(ctx, reader, table, scope, conversationID, ids, postgres)
}

// reactionMessagePlaceholders forwards to sqlstore.ReactionMessagePlaceholders.
func reactionMessagePlaceholders(start, count int, positional bool) string {
	return sqlstore.ReactionMessagePlaceholders(start, count, positional)
}

// scanMessageReactionRecords forwards to sqlstore.ScanMessageReactionRecords.
func scanMessageReactionRecords(rows *sql.Rows, scope models.Scope, conversationID string) ([]*models.MessageReactionRecord, error) {
	return sqlstore.ScanMessageReactionRecords(rows, scope, conversationID)
}

// scanConversationReactionEvents forwards to sqlstore.ScanConversationReactionEvents.
func scanConversationReactionEvents(rows *sql.Rows, scope models.Scope, conversationID string) ([]*models.ConversationReactionEvent, error) {
	return sqlstore.ScanConversationReactionEvents(rows, scope, conversationID)
}

// reactionEventLimit forwards to sqlstore.ReactionEventLimit.
func reactionEventLimit(limit int) int {
	return sqlstore.ReactionEventLimit(limit)
}

// decodeConversation forwards to sqlstore.DecodeConversation.
func decodeConversation(payload string) (*models.Conversation, error) {
	return sqlstore.DecodeConversation(payload)
}

// decodeChannelMessage forwards to sqlstore.DecodeChannelMessage.
func decodeChannelMessage(payload string) (*models.ChannelMessage, error) {
	return sqlstore.DecodeChannelMessage(payload)
}

// decodeParticipationRound forwards to sqlstore.DecodeParticipationRound.
func decodeParticipationRound(payload string) (*models.ParticipationRound, error) {
	return sqlstore.DecodeParticipationRound(payload)
}

// decodeConversationCursor forwards to sqlstore.DecodeConversationCursor.
func decodeConversationCursor(payload string) (*models.ConversationCursor, error) {
	return sqlstore.DecodeConversationCursor(payload)
}

// decodeConversationPresence forwards to sqlstore.DecodeConversationPresence.
func decodeConversationPresence(payload string) (*models.ConversationPresence, error) {
	return sqlstore.DecodeConversationPresence(payload)
}

// decodeConversationTaskSQL forwards to sqlstore.DecodeConversationTask.
func decodeConversationTaskSQL(payload string) (*models.ConversationTask, error) {
	return sqlstore.DecodeConversationTask(payload)
}

// scanConversationTaskResults forwards to sqlstore.ScanConversationTaskResults.
func scanConversationTaskResults(rows *sql.Rows) ([]*models.ConversationTaskResult, error) {
	return sqlstore.ScanConversationTaskResults(rows)
}

// getCredentialRequest forwards to sqlstore.GetCredentialRequest.
func getCredentialRequest(ctx context.Context, db *sql.DB, table string, scope models.Scope, id string) (*models.CredentialRequest, error) {
	return sqlstore.GetCredentialRequest(ctx, db, table, scope, id)
}

// listCredentialRequests forwards to sqlstore.ListCredentialRequests.
func listCredentialRequests(ctx context.Context, db *sql.DB, table string, scope models.Scope, deploymentID, conversationID string) ([]*models.CredentialRequest, error) {
	return sqlstore.ListCredentialRequests(ctx, db, table, scope, deploymentID, conversationID)
}

// saveCredentialRequest forwards to sqlstore.SaveCredentialRequest.
func saveCredentialRequest(ctx context.Context, db *sql.DB, table string, r *models.CredentialRequest, expected int64) error {
	return sqlstore.SaveCredentialRequest(ctx, db, table, r, expected)
}

// defaultSpaceRewrites forwards to sqlstore.DefaultSpaceRewrites.
func defaultSpaceRewrites(spaces, members, conversations, artifacts string, postgres bool) []string {
	return sqlstore.DefaultSpaceRewrites(spaces, members, conversations, artifacts, postgres)
}

// decodeRunDependencyGroup forwards to sqlstore.DecodeRunDependencyGroup.
func decodeRunDependencyGroup(payload string) (*models.RunDependencyGroup, error) {
	return sqlstore.DecodeRunDependencyGroup(payload)
}

// decodeRunDependency forwards to sqlstore.DecodeRunDependency.
func decodeRunDependency(payload string) (*models.RunDependency, error) {
	return sqlstore.DecodeRunDependency(payload)
}

// deploymentIdentityCollisions forwards to sqlstore.DeploymentIdentityCollisions.
func deploymentIdentityCollisions(rows *sql.Rows) ([]string, error) {
	return sqlstore.DeploymentIdentityCollisions(rows)
}

type sqlExecQueryer = sqlstore.ExecRowQuerier

type sqliteExternalConversationScanner = sqlstore.RowScanner

// scanSQLiteExternalConversationEndpoint forwards to sqlstore.ScanExternalConversationEndpoint.
func scanSQLiteExternalConversationEndpoint(scanner sqliteExternalConversationScanner) (*models.ExternalConversationEndpoint, error) {
	return sqlstore.ScanExternalConversationEndpoint(scanner)
}

// scanSQLiteExternalConversationInbox forwards to sqlstore.ScanExternalConversationInbox.
func scanSQLiteExternalConversationInbox(scanner sqliteExternalConversationScanner) (*models.ExternalConversationInboxItem, error) {
	return sqlstore.ScanExternalConversationInbox(scanner)
}

// scanSQLiteExternalConversationMapping forwards to sqlstore.ScanExternalConversationMapping.
func scanSQLiteExternalConversationMapping(scanner sqliteExternalConversationScanner) (*models.ExternalConversationMapping, error) {
	return sqlstore.ScanExternalConversationMapping(scanner)
}

// scanSQLiteExternalParticipantMapping forwards to sqlstore.ScanExternalParticipantMapping.
func scanSQLiteExternalParticipantMapping(scanner sqliteExternalConversationScanner) (*models.ExternalParticipantMapping, error) {
	return sqlstore.ScanExternalParticipantMapping(scanner)
}

// scanSQLiteExternalMessageMapping forwards to sqlstore.ScanExternalMessageMapping.
func scanSQLiteExternalMessageMapping(scanner sqliteExternalConversationScanner) (*models.ExternalMessageMapping, error) {
	return sqlstore.ScanExternalMessageMapping(scanner)
}

// scanSQLiteExternalConversationDelivery forwards to sqlstore.ScanExternalConversationDelivery.
func scanSQLiteExternalConversationDelivery(scanner sqliteExternalConversationScanner) (*models.ExternalConversationDelivery, error) {
	return sqlstore.ScanExternalConversationDelivery(scanner)
}

// stampInteractionUmbrellas forwards to sqlstore.StampInteractionUmbrellas.
func stampInteractionUmbrellas(ctx context.Context, db sqlstore.ExecQuerier, table func(string) string, placeholder func(int) string) error {
	return sqlstore.StampInteractionUmbrellas(ctx, db, table, placeholder)
}

// markOwnerDefaultSpaces forwards to sqlstore.MarkOwnerDefaultSpaces.
func markOwnerDefaultSpaces(spaces, isTrue string) string {
	return sqlstore.MarkOwnerDefaultSpaces(spaces, isTrue)
}

// decodeObjective forwards to sqlstore.DecodeObjective.
func decodeObjective(payload string) (*models.Objective, error) {
	return sqlstore.DecodeObjective(payload)
}

// decodeAgentRun forwards to sqlstore.DecodeAgentRun.
func decodeAgentRun(payload string) (*models.AgentRun, error) {
	return sqlstore.DecodeAgentRun(payload)
}

// nullableRoutineEventIndex forwards to sqlstore.NullableRoutineEventIndex.
func nullableRoutineEventIndex(activation *models.RunbookActivation) (interface{}, interface{}) {
	return sqlstore.NullableRoutineEventIndex(activation)
}

// routineEventRecordPayload forwards to sqlstore.RoutineEventRecordPayload.
func routineEventRecordPayload(record *models.RoutineEventRecord) ([]byte, error) {
	return sqlstore.RoutineEventRecordPayload(record)
}

// decodeRoutineEventRecordEvent forwards to sqlstore.DecodeRoutineEventRecordEvent.
func decodeRoutineEventRecordEvent(record *models.RoutineEventRecord, payload []byte, eventID string, test bool, occurredAt time.Time) error {
	return sqlstore.DecodeRoutineEventRecordEvent(record, payload, eventID, test, occurredAt)
}

// scanRunLoopCutoverRefs forwards to sqlstore.ScanRunLoopCutoverRefs.
func scanRunLoopCutoverRefs(rows *sql.Rows) ([]models.RunLoopCutoverRef, error) {
	return sqlstore.ScanRunLoopCutoverRefs(rows)
}

const legacyRunSkillDependencyBatch = sqlstore.LegacyRunSkillDependencyBatch

// backfillLegacyRunSkillDependencies forwards to sqlstore.BackfillLegacyRunSkillDependencies.
func backfillLegacyRunSkillDependencies(ctx context.Context, tx *sql.Tx, runs, deployments, definitions, dependencies string, postgres bool) error {
	return sqlstore.BackfillLegacyRunSkillDependencies(ctx, tx, runs, deployments, definitions, dependencies, postgres)
}

const runTerminalReportingWhitespace = sqlstore.RunTerminalReportingWhitespace

// runTerminalReportLeaseResult forwards to sqlstore.RunTerminalReportLeaseResult.
func runTerminalReportLeaseResult(result sql.Result, err error) error {
	return sqlstore.RunTerminalReportLeaseResult(result, err)
}

// decodeRunbookActivation forwards to sqlstore.DecodeRunbookActivation.
func decodeRunbookActivation(payload string) (*models.RunbookActivation, error) {
	return sqlstore.DecodeRunbookActivation(payload)
}

// logicalSkillRuntimeUsage forwards to sqlstore.LogicalSkillRuntimeUsage.
func logicalSkillRuntimeUsage(ctx context.Context, q skillRuntimeUsageQuerier, scope models.Scope, skillID, actions, runs string, postgres bool) (bool, error) {
	return sqlstore.LogicalSkillRuntimeUsage(ctx, q, scope, skillID, actions, runs, postgres)
}

// maintenanceBindingsMatch forwards to sqlstore.MaintenanceBindingsMatch.
func maintenanceBindingsMatch(ctx context.Context, q skillRuntimeUsageQuerier, g *models.SkillRuntimeMaintenance, version, bindings string, postgres bool) (bool, error) {
	return sqlstore.MaintenanceBindingsMatch(ctx, q, g, version, bindings, postgres)
}

// readSkillRuntimeMaintenance forwards to sqlstore.ReadSkillRuntimeMaintenance.
func readSkillRuntimeMaintenance(ctx context.Context, q skillRuntimeUsageQuerier, table string, scope models.Scope, skillID string, postgres, lock bool) (*models.SkillRuntimeMaintenance, error) {
	return sqlstore.ReadSkillRuntimeMaintenance(ctx, q, table, scope, skillID, postgres, lock)
}

// writeSkillRuntimeMaintenance forwards to sqlstore.WriteSkillRuntimeMaintenance.
func writeSkillRuntimeMaintenance(ctx context.Context, q sqlstore.MaintenanceDB, table string, g *models.SkillRuntimeMaintenance, postgres bool) error {
	return sqlstore.WriteSkillRuntimeMaintenance(ctx, q, table, g, postgres)
}

// validateMaintenanceBindingSnapshot forwards to sqlstore.ValidateMaintenanceBindingSnapshot.
func validateMaintenanceBindingSnapshot(ctx context.Context, q sqlstore.MaintenanceDB, table string, r models.SkillRuntimeMaintenanceRequest, postgres bool) error {
	return sqlstore.ValidateMaintenanceBindingSnapshot(ctx, q, table, r, postgres)
}

// listActiveSkillRuntimeMaintenances forwards to sqlstore.ListActiveSkillRuntimeMaintenances.
func listActiveSkillRuntimeMaintenances(ctx context.Context, q sqlstore.MaintenanceDB, table string, scope models.Scope, postgres bool) ([]*models.SkillRuntimeMaintenance, error) {
	return sqlstore.ListActiveSkillRuntimeMaintenances(ctx, q, table, scope, postgres)
}

// listMaintenanceBindings forwards to sqlstore.ListMaintenanceBindings.
func listMaintenanceBindings(ctx context.Context, q sqlstore.MaintenanceDB, table string, scope models.Scope, skillID, afterDeploymentID, afterBindingID string, limit int, postgres bool) ([]*skill.Binding, error) {
	return sqlstore.ListMaintenanceBindings(ctx, q, table, scope, skillID, afterDeploymentID, afterBindingID, limit, postgres)
}

// listCompletedMaintenanceWaiters forwards to sqlstore.ListCompletedMaintenanceWaiters.
func listCompletedMaintenanceWaiters(ctx context.Context, q sqlstore.MaintenanceDB, runs, gates string, scope models.Scope, limit int, postgres bool) ([]*models.AgentRun, error) {
	return sqlstore.ListCompletedMaintenanceWaiters(ctx, q, runs, gates, scope, limit, postgres)
}

type skillRuntimeUsageQuerier = sqlstore.RowQuerier

const unfinishedSkillRunSQL = sqlstore.UnfinishedSkillRunSQL

// skillRuntimeUsageQuery forwards to sqlstore.SkillRuntimeUsageQuery.
func skillRuntimeUsageQuery(filter models.SkillRuntimeUsageFilter, actions, runs string, postgres bool) (string, []interface{}) {
	return sqlstore.SkillRuntimeUsageQuery(filter, actions, runs, postgres)
}

// hasSkillRuntimeUsage forwards to sqlstore.HasSkillRuntimeUsage.
func hasSkillRuntimeUsage(ctx context.Context, query skillRuntimeUsageQuerier, filter models.SkillRuntimeUsageFilter, actions, runs string, postgres bool) (bool, error) {
	return sqlstore.HasSkillRuntimeUsage(ctx, query, filter, actions, runs, postgres)
}

// listReferencedSkillRuntimeVersionsQuery forwards to sqlstore.ListReferencedSkillRuntimeVersionsQuery.
func listReferencedSkillRuntimeVersionsQuery(bindings, actions, runs string, postgres bool) string {
	return sqlstore.ListReferencedSkillRuntimeVersionsQuery(bindings, actions, runs, postgres)
}

// scanReferencedSkillRuntimeVersions forwards to sqlstore.ScanReferencedSkillRuntimeVersions.
func scanReferencedSkillRuntimeVersions(rows *sql.Rows) ([]models.SkillRuntimeReference, error) {
	return sqlstore.ScanReferencedSkillRuntimeVersions(rows)
}

// listPendingSkillSetupRequests forwards to sqlstore.ListPendingSkillSetupRequests.
func listPendingSkillSetupRequests(ctx context.Context, db *sql.DB, table string, scope models.Scope, limit, offset int) ([]*models.SkillSetupRequest, error) {
	return sqlstore.ListPendingSkillSetupRequests(ctx, db, table, scope, limit, offset)
}

// skillUpgradeDrainIndexStatements forwards to sqlstore.SkillUpgradeDrainIndexStatements.
func skillUpgradeDrainIndexStatements(actions, runs string, postgres bool) []string {
	return sqlstore.SkillUpgradeDrainIndexStatements(actions, runs, postgres)
}

const postgresSpaceColumns = sqlstore.SpaceColumns

const postgresSpaceMemberColumns = sqlstore.SpaceMemberColumns

const postgresSpacePersonColumns = sqlstore.SpacePersonColumns

// scanSpacePerson forwards to sqlstore.ScanSpacePerson.
func scanSpacePerson(row interface{ Scan(...interface{}) error }) (*models.SpacePerson, error) {
	return sqlstore.ScanSpacePerson(row)
}

// decodeSurfaceInteraction forwards to sqlstore.DecodeSurfaceInteraction.
func decodeSurfaceInteraction(payload string) (*models.SurfaceInteraction, error) {
	return sqlstore.DecodeSurfaceInteraction(payload)
}

// latestSurfaceInteractionQuery forwards to sqlstore.LatestSurfaceInteractionQuery.
func latestSurfaceInteractionQuery(table string, acrossActors bool, placeholders [4]string) string {
	return sqlstore.LatestSurfaceInteractionQuery(table, acrossActors, placeholders)
}

// scanSurfaceInteractions forwards to sqlstore.ScanSurfaceInteractions.
func scanSurfaceInteractions(rows *sql.Rows) ([]*models.SurfaceInteraction, error) {
	return sqlstore.ScanSurfaceInteractions(rows)
}

type teamMembersMigration = sqlstore.TeamMembersMigration

// teamRegistryPayloads forwards to sqlstore.TeamRegistryPayloads.
func teamRegistryPayloads(deployment *kernelteam.Deployment, activation workforce.DefinitionActivation) (string, string, error) {
	return sqlstore.TeamRegistryPayloads(deployment, activation)
}

// stripTeamSkillGrants forwards to sqlstore.StripTeamSkillGrants.
func stripTeamSkillGrants(ctx context.Context, db sqlstore.ExecQuerier, table func(string) string, placeholder func(int) string) error {
	return sqlstore.StripTeamSkillGrants(ctx, db, table, placeholder)
}

// decodeAgentTurnRecord forwards to sqlstore.DecodeAgentTurnRecord.
func decodeAgentTurnRecord(payload string) (*models.AgentTurn, error) {
	return sqlstore.DecodeAgentTurnRecord(payload)
}
