package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
)

// agentBuilderMigrationVersion retires workforce_change_sets (spec §3.6): the
// change-set work family, its outbox triggers and its Runs go, and the leaf
// ready/review drafts are kept in agent_builder_cutover_drafts until
// Engine.AgentBuilderCutover turns each into a builder conversation.
const agentBuilderMigrationVersion int64 = 76

const (
	retiredChangeSetTable    = "workforce_change_sets"
	agentBuilderCutoverTable = "agent_builder_cutover_drafts"
	// agentBuilderCutoverRequestMarker precedes the typed request in a
	// change-set draft prompt.
	agentBuilderCutoverRequestMarker = "Request: "
)

// retiredChangeSetIndexes are the change-set indexes; a rename keeps them,
// so the migration drops them by name. The SQLite names carry the idx_
// prefix.
var retiredChangeSetIndexes = []string{
	"workforce_change_sets_pending_work_idx", "workforce_change_sets_parent_idx",
	"workforce_change_sets_status_idx", "workforce_change_sets_global_recovery_idx",
}

var retiredChangeSetIndexesSQLite = []string{
	"idx_workforce_change_sets_parent", "idx_workforce_change_sets_status", "idx_workforce_change_sets_global_recovery",
}

// AgentBuilderCutoverDraft is one leaf change-set draft left by migration 76.
type AgentBuilderCutoverDraft struct {
	Scope          Scope
	ID             string
	IdempotencyKey string
	ActorType      string
	ActorID        string
	Mode           string
	// Prompt is the draft's own prompt; CutoverPrompts are its ancestors'
	// prompts, oldest first.
	Prompt         string
	CutoverPrompts []string
	// Agent is the drafted profile (result.candidate.agents[0]), if any.
	Agent     *kernelagent.AgentDefinition
	Placement AgentBuilderCutoverPlacement
	// Readable is false when the payload could not be decoded.
	Readable bool
}

// AgentBuilderCutoverPlacement is the part of a draft's placement the
// cutover reads.
type AgentBuilderCutoverPlacement struct {
	AgentDeploymentIDs map[string]string `json:"agentDeploymentIds,omitempty"`
}

// AgentBuilderCutoverStore holds the drafts migration 76 kept. The
// PostgreSQL and SQLite stores read agent_builder_cutover_drafts.
type AgentBuilderCutoverStore interface {
	// ListAgentBuilderCutoverDrafts lists every kept draft across scopes,
	// oldest first. It returns none when the table is absent.
	ListAgentBuilderCutoverDrafts(ctx context.Context) ([]AgentBuilderCutoverDraft, error)
	DeleteAgentBuilderCutoverDraft(ctx context.Context, scope Scope, id string) error
	// DropAgentBuilderCutoverTable drops the table when it exists and is
	// empty. It reports whether this call dropped it, so exactly one caller
	// sees true across processes.
	DropAgentBuilderCutoverTable(ctx context.Context) (bool, error)
}

// agentBuilderCutoverPayload is the part of a change-set payload the cutover
// reads; pkg/authoring is gone, so it is decoded locally.
type agentBuilderCutoverPayload struct {
	Actor struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"actor"`
	Mode           string   `json:"mode"`
	Prompt         string   `json:"prompt"`
	CutoverPrompts []string `json:"cutoverPrompts"`
	Result         struct {
		Candidate struct {
			Agents []*struct {
				DisplayName         string   `json:"displayName"`
				Purpose             string   `json:"purpose"`
				SystemPrompt        string   `json:"systemPrompt"`
				Personality         string   `json:"personality"`
				OperatingPrinciples []string `json:"operatingPrinciples"`
			} `json:"agents"`
		} `json:"candidate"`
	} `json:"result"`
	Placement AgentBuilderCutoverPlacement `json:"placement"`
}

func decodeAgentBuilderCutoverDraft(scope Scope, id, idempotencyKey, payload string) (AgentBuilderCutoverDraft, error) {
	draft := AgentBuilderCutoverDraft{Scope: scope, ID: id, IdempotencyKey: idempotencyKey}
	var value agentBuilderCutoverPayload
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return draft, fmt.Errorf("decode agent builder cutover draft %s: %w", id, err)
	}
	draft.Readable = true
	draft.ActorType, draft.ActorID = value.Actor.Type, value.Actor.ID
	draft.Mode, draft.Prompt, draft.CutoverPrompts = value.Mode, value.Prompt, value.CutoverPrompts
	draft.Placement = value.Placement
	if agents := value.Result.Candidate.Agents; len(agents) > 0 && agents[0] != nil {
		agent := agents[0]
		draft.Agent = &kernelagent.AgentDefinition{
			DisplayName: agent.DisplayName, Purpose: agent.Purpose, SystemPrompt: agent.SystemPrompt,
			Personality: agent.Personality, OperatingPrinciples: agent.OperatingPrinciples,
		}
	}
	return draft, nil
}

// StartRequest is the builder conversation the draft becomes (D7, D12): the
// content is only what the person typed, oldest request first and each
// later revision on its own line; name, model and the drafted profile travel
// in Parameters, fitted to the parameter bounds. A field that cannot be
// fitted (sensitive text) is left out rather than failing the conversion.
func (d AgentBuilderCutoverDraft) StartRequest(model, spaceID string) StartAgentBuilderRequest {
	prompts := append(append([]string{}, d.CutoverPrompts...), d.Prompt)
	lines := make([]string, 0, len(prompts))
	for _, prompt := range prompts {
		line := agentBuilderCutoverTypedRequest(prompt)
		if line == "" || (len(lines) > 0 && lines[len(lines)-1] == line) {
			continue
		}
		lines = append(lines, line)
	}
	parameters := AgentBuilderParameters{Kind: AgentBuilderKindAgent}
	parameters.Model = fitAgentBuilderParameter("model", strings.TrimSpace(model), maximumAgentBuilderModelBytes, true)
	if agent := d.Agent; agent != nil {
		parameters.Name = fitAgentBuilderParameter("name", agent.DisplayName, maximumAgentBuilderNameBytes, true)
		draft := &AgentBuilderDraftProfile{
			Purpose:      fitAgentBuilderParameter("draft purpose", agent.Purpose, maximumAgentBuilderPurposeBytes, true),
			SystemPrompt: fitAgentBuilderParameter("draft behaviour", agent.SystemPrompt, maximumAgentBuilderSystemPromptBytes, false),
			Personality:  fitAgentBuilderParameter("draft personality", agent.Personality, maximumAgentBuilderPersonalityBytes, false),
		}
		seen := map[string]struct{}{}
		for _, principle := range agent.OperatingPrinciples {
			if len(draft.OperatingPrinciples) == maximumAgentBuilderOperatingPrinciples {
				break
			}
			fitted := fitAgentBuilderParameter("draft operating principle", principle, maximumAgentBuilderOperatingPrincipleBytes, false)
			if _, duplicate := seen[fitted]; fitted == "" || duplicate {
				continue
			}
			seen[fitted] = struct{}{}
			draft.OperatingPrinciples = append(draft.OperatingPrinciples, fitted)
		}
		if !draft.IsEmpty() {
			parameters.Draft = draft
		}
	}
	return StartAgentBuilderRequest{
		Scope:          d.Scope,
		Requester:      ConversationParticipant{Type: ConversationParticipantUser, ID: strings.TrimSpace(d.ActorID)},
		Prompt:         strings.Join(lines, "\n"),
		Parameters:     parameters,
		SpaceID:        spaceID,
		IdempotencyKey: "cutover-" + d.ID,
	}
}

// agentBuilderCutoverTypedRequest is the text after the last "Request: " of
// a draft prompt, else the whole prompt.
func agentBuilderCutoverTypedRequest(prompt string) string {
	if index := strings.LastIndex(prompt, agentBuilderCutoverRequestMarker); index >= 0 {
		prompt = prompt[index+len(agentBuilderCutoverRequestMarker):]
	}
	return strings.TrimSpace(prompt)
}

// fitAgentBuilderParameter trims value into a builder parameter: one line
// when oneLine, at most limit bytes at a rune boundary. It returns "" when
// the result is still refused.
func fitAgentBuilderParameter(field, value string, limit int, oneLine bool) string {
	value = strings.TrimSpace(value)
	if oneLine {
		value = strings.Join(strings.Fields(value), " ")
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if len(value) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(value[cut]) {
			cut--
		}
		value = strings.TrimSpace(value[:cut])
	}
	if value == "" {
		return ""
	}
	if err := agentBuilderText(field, value, limit); err != nil {
		return ""
	}
	return value
}

// changeSetLineageRow is one change-set row the leaf fold reads.
type changeSetLineageRow struct {
	scopeKind, scopeID, id, status, parentID, prompt string
}

func (r changeSetLineageRow) key() string { return r.scopeKind + "\x00" + r.scopeID + "\x00" + r.id }

func (r changeSetLineageRow) draft() bool { return r.status == "ready" || r.status == "review" }

// foldChangeSetLineages returns, for each leaf draft (a ready/review row no
// other ready/review row names as its parent), its ancestors' prompts oldest
// first, and the rows that are not leaves.
func foldChangeSetLineages(rows []changeSetLineageRow) (map[string][]string, []changeSetLineageRow) {
	byKey := make(map[string]changeSetLineageRow, len(rows))
	parentOfDraft := map[string]bool{}
	for _, row := range rows {
		byKey[row.key()] = row
	}
	for _, row := range rows {
		if row.draft() && row.parentID != "" {
			parentOfDraft[changeSetLineageRow{scopeKind: row.scopeKind, scopeID: row.scopeID, id: row.parentID}.key()] = true
		}
	}
	leaves := map[string][]string{}
	var removed []changeSetLineageRow
	for _, row := range rows {
		if !row.draft() || parentOfDraft[row.key()] {
			removed = append(removed, row)
			continue
		}
		ancestors := []string{}
		visited := map[string]bool{row.key(): true}
		for parentID := row.parentID; parentID != ""; {
			parent, ok := byKey[changeSetLineageRow{scopeKind: row.scopeKind, scopeID: row.scopeID, id: parentID}.key()]
			if !ok || visited[parent.key()] {
				break
			}
			visited[parent.key()] = true
			ancestors = append(ancestors, parent.prompt)
			parentID = parent.parentID
		}
		for left, right := 0, len(ancestors)-1; left < right; left, right = left+1, right-1 {
			ancestors[left], ancestors[right] = ancestors[right], ancestors[left]
		}
		leaves[row.key()] = ancestors
	}
	return leaves, removed
}

type agentBuilderMigrationExecer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func readChangeSetLineage(ctx context.Context, tx agentBuilderMigrationExecer, query string) ([]changeSetLineageRow, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lineage []changeSetLineageRow
	for rows.Next() {
		var row changeSetLineageRow
		var parentID, prompt sql.NullString
		if err := rows.Scan(&row.scopeKind, &row.scopeID, &row.id, &row.status, &parentID, &prompt); err != nil {
			return nil, err
		}
		row.parentID, row.prompt = parentID.String, prompt.String
		lineage = append(lineage, row)
	}
	return lineage, rows.Err()
}

// foldChangeSetDrafts keeps only the leaf drafts, each with its lineage's
// prompts in cutoverPrompts. The SQL dialect differs only in placeholders
// and the JSON write.
func foldChangeSetDrafts(ctx context.Context, tx agentBuilderMigrationExecer, table, selectQuery, setPrompts, deleteRow string) error {
	lineage, err := readChangeSetLineage(ctx, tx, selectQuery)
	if err != nil {
		return err
	}
	leaves, removed := foldChangeSetLineages(lineage)
	for _, row := range lineage {
		prompts, leaf := leaves[row.key()]
		if !leaf || len(prompts) == 0 {
			continue
		}
		encoded, err := json.Marshal(prompts)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, setPrompts, string(encoded), row.scopeKind, row.scopeID, row.id); err != nil {
			return fmt.Errorf("fold %s lineage: %w", table, err)
		}
	}
	for _, row := range removed {
		if _, err := tx.ExecContext(ctx, deleteRow, row.scopeKind, row.scopeID, row.id); err != nil {
			return fmt.Errorf("delete retired %s row: %w", table, err)
		}
	}
	return nil
}

// migrateAgentBuilder is PostgreSQL migration 76 (spec §3.6).
func (s *PostgresStore) migrateAgentBuilder(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, agentBuilderMigrationVersion)
	if err != nil || applied {
		return err
	}
	var relation sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)::text`, s.schema+"."+retiredChangeSetTable).Scan(&relation); err != nil {
		return err
	}
	changeSets := relation.Valid
	statements := []string{}
	for _, name := range []string{"work_outbox_insert", "work_outbox_update"} {
		trigger := workOutboxTrigger{table: retiredChangeSetTable, name: name}
		if changeSets {
			statements = append(statements, `DROP TRIGGER IF EXISTS `+name+` ON `+s.table(retiredChangeSetTable))
		}
		statements = append(statements, `DROP FUNCTION IF EXISTS `+s.table(trigger.function())+`()`)
	}
	// The agent_runs outbox functions still route workforce_authoring Runs
	// to the authoring family; replace them with the current bodies.
	for _, trigger := range workOutboxTriggers() {
		if trigger.table == "agent_runs" {
			statements = append(statements, s.workOutboxTriggerStatements(trigger)...)
		}
	}
	statements = append(statements,
		`DROP INDEX IF EXISTS `+s.table("agent_runs_global_recovery_idx"),
		`UPDATE `+s.table("agent_runs")+` SET status = 'canceled', revision = revision + 1, lease_owner = '', lease_expires_at = NULL,
			payload = (payload - 'leaseOwner' - 'leaseExpiresAt') || jsonb_build_object('status', 'canceled', 'revision', revision + 1,
				'updatedAt', to_jsonb(now()), 'completedAt', to_jsonb(now()))
			WHERE payload->>'kind' = 'workforce_authoring' AND status NOT IN ('completed', 'failed', 'canceled')`,
		`UPDATE `+s.table("agent_deployments")+` SET payload = payload - 'activation' WHERE payload ? 'activation'`,
		`UPDATE `+s.table("team_deployments")+` SET payload = payload - 'activation' WHERE payload ? 'activation'`,
	)
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if changeSets {
		table := s.table(retiredChangeSetTable)
		if err := foldChangeSetDrafts(ctx, tx, retiredChangeSetTable,
			`SELECT scope_kind, scope_id, id, status, payload->>'parentId', payload->>'prompt' FROM `+table+` ORDER BY created_at, id`,
			`UPDATE `+table+` SET payload = jsonb_set(payload, '{cutoverPrompts}', $1::jsonb, true) WHERE scope_kind = $2 AND scope_id = $3 AND id = $4`,
			`DELETE FROM `+table+` WHERE scope_kind = $1 AND scope_id = $2 AND id = $3`,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `ALTER TABLE `+table+` RENAME TO `+agentBuilderCutoverTable); err != nil {
			return err
		}
		for _, index := range retiredChangeSetIndexes {
			if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+s.table(index)); err != nil {
				return err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'agent builder: retire workforce change sets') ON CONFLICT (version) DO NOTHING`, agentBuilderMigrationVersion)
	return err
}

// migrateAgentBuilderSQLite mirrors PostgreSQL migration 76. SQLite has no
// ledger: the retirement runs only while workforce_change_sets exists, so a
// later open does nothing.
func migrateAgentBuilderSQLite(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS idx_agent_runs_global_recovery`); err != nil {
		return err
	}
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, retiredChangeSetTable).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return tx.Commit()
	}
	for _, statement := range []string{
		`UPDATE agent_runs SET status = 'canceled', revision = revision + 1, lease_owner = '', lease_expires_at = NULL,
			payload = json_set(json_remove(payload, '$.leaseOwner', '$.leaseExpiresAt'), '$.status', 'canceled', '$.revision', revision + 1,
				'$.updatedAt', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), '$.completedAt', strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			WHERE json_extract(payload, '$.kind') = 'workforce_authoring' AND status NOT IN ('completed', 'failed', 'canceled')`,
		`UPDATE agent_deployments SET payload = json_remove(payload, '$.activation') WHERE json_type(payload, '$.activation') IS NOT NULL`,
		`UPDATE team_deployments SET payload = json_remove(payload, '$.activation') WHERE json_type(payload, '$.activation') IS NOT NULL`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if err := foldChangeSetDrafts(ctx, tx, retiredChangeSetTable,
		`SELECT scope_kind, scope_id, id, status, json_extract(payload, '$.parentId'), json_extract(payload, '$.prompt') FROM workforce_change_sets ORDER BY created_at, id`,
		`UPDATE workforce_change_sets SET payload = json_set(payload, '$.cutoverPrompts', json(?)) WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
		`DELETE FROM workforce_change_sets WHERE scope_kind = ? AND scope_id = ? AND id = ?`,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE workforce_change_sets RENAME TO `+agentBuilderCutoverTable); err != nil {
		return err
	}
	for _, index := range retiredChangeSetIndexesSQLite {
		if _, err := tx.ExecContext(ctx, `DROP INDEX IF EXISTS `+index); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// scanAgentBuilderCutoverDrafts decodes (scope_kind, scope_id, id,
// idempotency_key, payload) rows.
func scanAgentBuilderCutoverDrafts(rows *sql.Rows) ([]AgentBuilderCutoverDraft, error) {
	defer rows.Close()
	var drafts []AgentBuilderCutoverDraft
	for rows.Next() {
		var scope Scope
		var id, key, payload string
		if err := rows.Scan(&scope.Kind, &scope.ID, &id, &key, &payload); err != nil {
			return nil, err
		}
		// An unreadable payload keeps only its identity: the cutover cannot
		// convert it, so it stays in the table and is reported.
		draft, _ := decodeAgentBuilderCutoverDraft(scope, id, key, payload)
		drafts = append(drafts, draft)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return drafts, nil
}
