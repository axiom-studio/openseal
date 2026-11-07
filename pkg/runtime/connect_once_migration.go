package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

const connectOnceMigrationVersion int64 = 74

// connectOnceMigration repairs records that migrations 71 to 73 left
// inconsistent and records the creator of every routine:
//  1. binding event waits are woken again by the binding they were made for;
//  2. setup-request copies in action outputs carry the request's umbrella;
//  3. in-flight Team amendments and change sets carry current digests;
//  4. conversation gateways name their deployment's kind;
//  5. routines record the person who created them.
//
// Every step only rewrites a row that differs from its repaired value, so
// the migration is safe to replay; SQLite replays it on every open.
type connectOnceMigration struct {
	db     teamSkillGrantMigrationDB
	table  func(string) string
	sqlite bool
}

func (m connectOnceMigration) run(ctx context.Context) error {
	for _, step := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"binding event waits", m.repairBindingEventWaits},
		{"setup request copies", m.repairSetupRequestCopies},
		{"Team amendments", m.repairTeamAmendments},
		{"gateway owners", m.repairGatewayOwnerKinds},
		{"routine creators", m.stampRoutineCreators},
	} {
		if err := step.run(ctx); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

func (m connectOnceMigration) placeholder(n int) string {
	if m.sqlite {
		return "?"
	}
	return fmt.Sprintf("$%d", n)
}

func (m connectOnceMigration) payloadText(column string) string {
	if m.sqlite {
		return column
	}
	return column + "::text"
}

func (m connectOnceMigration) payloadValue(n int) string {
	if m.sqlite {
		return m.placeholder(n)
	}
	return m.placeholder(n) + "::jsonb"
}

// setJSON returns column with each path set to the JSON value bound at
// placeholders first, first+1, ... Every parent of a path must exist.
func (m connectOnceMigration) setJSON(column string, first int, paths ...[]string) string {
	if m.sqlite {
		parts := []string{column}
		for index, path := range paths {
			parts = append(parts, `'$.`+strings.Join(path, ".")+`'`, `json(`+m.placeholder(first+index)+`)`)
		}
		return `json_set(` + strings.Join(parts, ", ") + `)`
	}
	expression := column
	for index, path := range paths {
		expression = `jsonb_set(` + expression + `, '{` + strings.Join(path, ",") + `}', ` + m.placeholder(first+index) + `::jsonb)`
	}
	return expression
}

// rows runs query and passes every row to scan, closing the result before
// returning so callers may write afterwards inside the same transaction.
func (m connectOnceMigration) rows(ctx context.Context, query string, args []interface{}, scan func(*sql.Rows) error) error {
	rows, err := m.db.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		if err := scan(rows); err != nil {
			rows.Close()
			return err
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	return rows.Close()
}

func jsonValue(value interface{}) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// repairBindingEventWaits restores the pre-73 match of a parked binding wait:
// migration 73 moved a Team run's wait to the Team, but its source and
// selectors still name the binding of the deployment that created it.
func (m connectOnceMigration) repairBindingEventWaits(ctx context.Context) error {
	type repair struct{ scopeKind, scopeID, runID, key, umbrella string }
	var repairs []repair
	err := m.rows(ctx, `SELECT scope_kind, scope_id, run_id, wait_key, `+m.payloadText("payload")+` FROM `+m.table("run_event_waits")+`
		WHERE status IN ('pending', 'paused') AND source LIKE 'binding:%'`, nil, func(rows *sql.Rows) error {
		var item repair
		var payload string
		if err := rows.Scan(&item.scopeKind, &item.scopeID, &item.runID, &item.key, &payload); err != nil {
			return err
		}
		var wait RunEventWait
		if err := json.Unmarshal([]byte(payload), &wait); err != nil {
			return fmt.Errorf("run %s wait %s: %w", item.runID, item.key, err)
		}
		deployment, _ := wait.Spec.Attributes["deploymentId"].(string)
		binding, _ := wait.Spec.Attributes["bindingId"].(string)
		adapter, _ := wait.Spec.Attributes["adapterId"].(string)
		if deployment == "" || binding == "" || adapter == "" || RunEventBindingSource(deployment, binding, adapter) != wait.Spec.Source {
			slog.Warn("connect-once migration: binding event wait does not name its binding; left unchanged",
				"scope", item.scopeKind+"/"+item.scopeID, "run", item.runID, "wait", item.key)
			return nil
		}
		if deployment != wait.UmbrellaID {
			item.umbrella = deployment
			repairs = append(repairs, item)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, item := range repairs {
		if _, err := m.db.ExecContext(ctx, `UPDATE `+m.table("run_event_waits")+` SET payload = `+m.setJSON("payload", 1, []string{"umbrellaId"})+`
			WHERE scope_kind = `+m.placeholder(2)+` AND scope_id = `+m.placeholder(3)+` AND run_id = `+m.placeholder(4)+` AND wait_key = `+m.placeholder(5),
			jsonValue(item.umbrella), item.scopeKind, item.scopeID, item.runID, item.key); err != nil {
			return err
		}
	}
	return nil
}

// repairSetupRequestCopies overwrites the owner, deployment and asking member
// of every setup request copied into an action output with the stored
// request's values, which migration 72 moved to the conversation umbrella.
func (m connectOnceMigration) repairSetupRequestCopies(ctx context.Context) error {
	type repair struct {
		scopeKind, scopeID, id string
		owner, deployment, agent string
	}
	requestID, copyOwner, copyDeployment, copyAgent := `a.payload#>>'{output,setupRequest,id}'`, `a.payload#>'{output,setupRequest,owner}'`, `a.payload#>'{output,setupRequest,deploymentId}'`, `a.payload#>'{output,setupRequest,agentId}'`
	owner, deployment, agent := `r.payload->'owner'`, `r.payload->'deploymentId'`, `r.payload->'agentId'`
	differs := copyOwner + ` IS DISTINCT FROM ` + owner + ` OR ` + copyDeployment + ` IS DISTINCT FROM ` + deployment + ` OR ` + copyAgent + ` IS DISTINCT FROM ` + agent
	present := `r.payload ? 'owner' AND r.payload ? 'deploymentId' AND r.payload ? 'agentId'`
	if m.sqlite {
		requestID, copyOwner, copyDeployment, copyAgent = `json_extract(a.payload, '$.output.setupRequest.id')`, `json_extract(a.payload, '$.output.setupRequest.owner')`, `json_extract(a.payload, '$.output.setupRequest.deploymentId')`, `json_extract(a.payload, '$.output.setupRequest.agentId')`
		owner, deployment, agent = `json_extract(r.payload, '$.owner')`, `json_extract(r.payload, '$.deploymentId')`, `json_extract(r.payload, '$.agentId')`
		differs = copyOwner + ` IS NOT ` + owner + ` OR ` + copyDeployment + ` IS NOT ` + deployment + ` OR ` + copyAgent + ` IS NOT ` + agent
		present = `json_type(r.payload, '$.owner') = 'object' AND json_type(r.payload, '$.deploymentId') = 'text' AND json_type(r.payload, '$.agentId') = 'text'`
	}
	var repairs []repair
	err := m.rows(ctx, `SELECT a.scope_kind, a.scope_id, a.id, `+m.payloadText("r.payload")+` FROM `+m.table("action_calls")+` a
		JOIN `+m.table("skill_setup_requests")+` r ON r.scope_kind = a.scope_kind AND r.scope_id = a.scope_id AND r.id = `+requestID+`
		WHERE `+present+` AND (`+differs+`)`, nil, func(rows *sql.Rows) error {
		var item repair
		var payload string
		if err := rows.Scan(&item.scopeKind, &item.scopeID, &item.id, &payload); err != nil {
			return err
		}
		var request struct {
			Owner        json.RawMessage `json:"owner"`
			DeploymentID json.RawMessage `json:"deploymentId"`
			AgentID      json.RawMessage `json:"agentId"`
		}
		if err := json.Unmarshal([]byte(payload), &request); err != nil {
			return fmt.Errorf("action call %s setup request: %w", item.id, err)
		}
		item.owner, item.deployment, item.agent = string(request.Owner), string(request.DeploymentID), string(request.AgentID)
		repairs = append(repairs, item)
		return nil
	})
	if err != nil {
		return err
	}
	for _, item := range repairs {
		if _, err := m.db.ExecContext(ctx, `UPDATE `+m.table("action_calls")+` SET payload = `+m.setJSON("payload", 1,
			[]string{"output", "setupRequest", "owner"}, []string{"output", "setupRequest", "deploymentId"}, []string{"output", "setupRequest", "agentId"})+`
			WHERE scope_kind = `+m.placeholder(4)+` AND scope_id = `+m.placeholder(5)+` AND id = `+m.placeholder(6),
			item.owner, item.deployment, item.agent, item.scopeKind, item.scopeID, item.id); err != nil {
			return err
		}
	}
	return nil
}

// decodeMigrationDocument decodes a JSON object keeping numbers exact.
func decodeMigrationDocument(payload string) (map[string]interface{}, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var document map[string]interface{}
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, fmt.Errorf("payload is not a JSON object")
	}
	return document, nil
}

// teamDefinitionDigestOf returns the digest of a stored Team definition
// document.
func teamDefinitionDigestOf(document map[string]interface{}) (string, error) {
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	var definition kernelteam.Definition
	if err := json.Unmarshal(encoded, &definition); err != nil {
		return "", err
	}
	return kernelteam.DefinitionDigest(&definition), nil
}

func migrationString(document map[string]interface{}, key string) string {
	value, _ := document[key].(string)
	return value
}

func migrationStrings(value interface{}) []string {
	items, _ := value.([]interface{})
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

// repairTeamAmendments recomputes the digests of every Team amendment still
// in flight: migration 71 rewrote its base definition's digest and stripped
// its candidate, so the stored base digest, provenance, candidate digest and
// request fingerprint no longer agree with what the registry recomputes.
func (m connectOnceMigration) repairTeamAmendments(ctx context.Context) error {
	type row struct{ scopeKind, scopeID, id, payload string }
	var pending []row
	err := m.rows(ctx, `SELECT scope_kind, scope_id, id, `+m.payloadText("payload")+` FROM `+m.table("team_definition_amendments")+`
		WHERE status NOT IN ('rejected', 'activated', 'evaluation_failed')`, nil, func(rows *sql.Rows) error {
		var item row
		if err := rows.Scan(&item.scopeKind, &item.scopeID, &item.id, &item.payload); err != nil {
			return err
		}
		pending = append(pending, item)
		return nil
	})
	if err != nil {
		return err
	}
	for _, item := range pending {
		document, err := decodeMigrationDocument(item.payload)
		if err != nil {
			return fmt.Errorf("Team amendment %s: %w", item.id, err)
		}
		candidate, _ := document["candidate"].(map[string]interface{})
		if candidate == nil {
			return fmt.Errorf("Team amendment %s has no candidate", item.id)
		}
		var baseDigest string
		found := false
		if err := m.rows(ctx, `SELECT digest FROM `+m.table("team_definitions")+` WHERE id = `+m.placeholder(1)+` AND version = `+m.placeholder(2),
			[]interface{}{migrationString(document, "definitionId"), migrationString(document, "baseVersion")}, func(rows *sql.Rows) error {
				found = true
				return rows.Scan(&baseDigest)
			}); err != nil {
			return err
		}
		if found {
			document["baseDigest"] = baseDigest
			provenance, _ := candidate["provenance"].(map[string]interface{})
			if provenance == nil {
				provenance = map[string]interface{}{}
				candidate["provenance"] = provenance
			}
			provenance["derivedFrom"] = baseDigest
		}
		candidateDigest, err := teamDefinitionDigestOf(candidate)
		if err != nil {
			return fmt.Errorf("Team amendment %s candidate: %w", item.id, err)
		}
		candidate["digest"] = candidateDigest
		document["requestDigest"] = kernelteam.AmendmentRequestDigest(migrationString(document, "deploymentId"), migrationString(document, "baseDigest"), candidateDigest,
			migrationString(document, "proposerType"), migrationString(document, "proposerId"), migrationString(document, "rationale"), migrationStrings(document["evidenceRefs"]))
		if err := m.rewriteDocument(ctx, "team_definition_amendments", item.scopeKind, item.scopeID, item.id, item.payload, document); err != nil {
			return err
		}
	}
	return nil
}

// rewriteDocument stores document as the payload of a scoped row when it
// differs from the stored payload.
func (m connectOnceMigration) rewriteDocument(ctx context.Context, table, scopeKind, scopeID, id, stored string, document map[string]interface{}) error {
	before, err := decodeMigrationDocument(stored)
	if err != nil {
		return err
	}
	beforeEncoded, _ := json.Marshal(before)
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	if bytes.Equal(beforeEncoded, encoded) {
		return nil
	}
	_, err = m.db.ExecContext(ctx, `UPDATE `+m.table(table)+` SET payload = `+m.payloadValue(1)+`
		WHERE scope_kind = `+m.placeholder(2)+` AND scope_id = `+m.placeholder(3)+` AND id = `+m.placeholder(4), string(encoded), scopeKind, scopeID, id)
	return err
}

// repairGatewayOwnerKinds sets each gateway owner's type from the deployment
// identity of its ID; migration 73 marked every owner an Agent.
func (m connectOnceMigration) repairGatewayOwnerKinds(ctx context.Context) error {
	ownerID, ownerType := `g.payload#>>'{gateway,owner,id}'`, `g.payload#>>'{gateway,owner,type}'`
	if m.sqlite {
		ownerID, ownerType = `json_extract(g.payload, '$.gateway.owner.id')`, `json_extract(g.payload, '$.gateway.owner.type')`
	}
	type repair struct{ scopeKind, scopeID, id, kind string }
	var repairs []repair
	err := m.rows(ctx, `SELECT g.scope_kind, g.scope_id, g.id, d.kind FROM `+m.table("external_conversation_gateways")+` g
		LEFT JOIN `+m.table("deployment_identities")+` d ON d.scope_kind = g.scope_kind AND d.scope_id = g.scope_id AND d.id = `+ownerID+`
		WHERE d.kind IS NULL OR `+ownerType+` IS NULL OR `+ownerType+` <> d.kind`, nil, func(rows *sql.Rows) error {
		var item repair
		var kind sql.NullString
		if err := rows.Scan(&item.scopeKind, &item.scopeID, &item.id, &kind); err != nil {
			return err
		}
		if !kind.Valid {
			slog.Warn("connect-once migration: gateway owner has no deployment identity; left unchanged",
				"scope", item.scopeKind+"/"+item.scopeID, "gateway", item.id)
			return nil
		}
		item.kind = kind.String
		repairs = append(repairs, item)
		return nil
	})
	if err != nil {
		return err
	}
	for _, item := range repairs {
		if _, err := m.db.ExecContext(ctx, `UPDATE `+m.table("external_conversation_gateways")+` SET payload = `+m.setJSON("payload", 1, []string{"gateway", "owner", "type"})+`
			WHERE scope_kind = `+m.placeholder(2)+` AND scope_id = `+m.placeholder(3)+` AND id = `+m.placeholder(4),
			jsonValue(item.kind), item.scopeKind, item.scopeID, item.id); err != nil {
			return err
		}
	}
	return nil
}

// stampRoutineCreators records, on every routine without a creator, the
// person whose message created it. A Team hand-off may have stored the
// agent request's ID as the source message; then the creator is the person
// whose message started the requesting run.
func (m connectOnceMigration) stampRoutineCreators(ctx context.Context) error {
	unstamped := `jsonb_typeof(payload->'task') = 'object' AND COALESCE(payload#>>'{task,createdBy}', '') = ''`
	if m.sqlite {
		unstamped = `json_type(payload, '$.task') = 'object' AND IFNULL(json_extract(payload, '$.task.createdBy'), '') = ''`
	}
	type row struct {
		scope Scope
		id    string
		task  ScheduledAgentTask
	}
	var routines []row
	err := m.rows(ctx, `SELECT scope_kind, scope_id, id, `+m.payloadText("payload")+` FROM `+m.table("runbook_activations")+` WHERE `+unstamped, nil, func(rows *sql.Rows) error {
		var item row
		var payload string
		if err := rows.Scan(&item.scope.Kind, &item.scope.ID, &item.id, &payload); err != nil {
			return err
		}
		var activation struct {
			Task *ScheduledAgentTask `json:"task"`
		}
		if err := json.Unmarshal([]byte(payload), &activation); err != nil {
			return fmt.Errorf("runbook activation %s: %w", item.id, err)
		}
		if activation.Task != nil {
			item.task = *activation.Task
			routines = append(routines, item)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, routine := range routines {
		creator, err := m.routineCreator(ctx, routine.scope, routine.task)
		if err != nil {
			return fmt.Errorf("runbook activation %s: %w", routine.id, err)
		}
		if creator == "" {
			continue
		}
		if _, err := m.db.ExecContext(ctx, `UPDATE `+m.table("runbook_activations")+` SET payload = `+m.setJSON("payload", 1, []string{"task", "createdBy"})+`
			WHERE scope_kind = `+m.placeholder(2)+` AND scope_id = `+m.placeholder(3)+` AND id = `+m.placeholder(4),
			jsonValue(creator), routine.scope.Kind, routine.scope.ID, routine.id); err != nil {
			return err
		}
	}
	return nil
}

func (m connectOnceMigration) routineCreator(ctx context.Context, scope Scope, task ScheduledAgentTask) (string, error) {
	conversationID, messageID := task.ConversationID, task.SourceMessageID
	message, err := m.channelMessage(ctx, scope, conversationID, messageID)
	if err != nil {
		return "", err
	}
	if message == nil {
		var requestPayload string
		found := false
		if err := m.rows(ctx, `SELECT `+m.payloadText("payload")+` FROM `+m.table("agent_requests")+` WHERE scope_kind = `+m.placeholder(1)+` AND scope_id = `+m.placeholder(2)+` AND id = `+m.placeholder(3),
			[]interface{}{scope.Kind, scope.ID, messageID}, func(rows *sql.Rows) error {
				found = true
				return rows.Scan(&requestPayload)
			}); err != nil || !found {
			return "", err
		}
		var request AgentRequest
		if err := json.Unmarshal([]byte(requestPayload), &request); err != nil {
			return "", fmt.Errorf("agent request %s: %w", messageID, err)
		}
		var runPayload string
		found = false
		if err := m.rows(ctx, `SELECT `+m.payloadText("payload")+` FROM `+m.table("agent_runs")+` WHERE scope_kind = `+m.placeholder(1)+` AND scope_id = `+m.placeholder(2)+` AND id = `+m.placeholder(3),
			[]interface{}{scope.Kind, scope.ID, request.SourceRunID}, func(rows *sql.Rows) error {
				found = true
				return rows.Scan(&runPayload)
			}); err != nil || !found {
			return "", err
		}
		var source AgentRun
		if err := json.Unmarshal([]byte(runPayload), &source); err != nil {
			return "", fmt.Errorf("run %s: %w", request.SourceRunID, err)
		}
		conversationID, _ = source.Context[conversationRunContextConversationID].(string)
		messageID, _ = source.Context[conversationRunContextTriggerID].(string)
		if conversationID == "" || messageID == "" {
			return "", nil
		}
		if message, err = m.channelMessage(ctx, scope, conversationID, messageID); err != nil || message == nil {
			return "", err
		}
	}
	var conversationPayload string
	found := false
	if err := m.rows(ctx, `SELECT `+m.payloadText("payload")+` FROM `+m.table("conversations")+` WHERE scope_kind = `+m.placeholder(1)+` AND scope_id = `+m.placeholder(2)+` AND id = `+m.placeholder(3),
		[]interface{}{scope.Kind, scope.ID, conversationID}, func(rows *sql.Rows) error {
			found = true
			return rows.Scan(&conversationPayload)
		}); err != nil || !found {
		return "", err
	}
	var conversation Conversation
	if err := json.Unmarshal([]byte(conversationPayload), &conversation); err != nil {
		return "", fmt.Errorf("conversation %s: %w", conversationID, err)
	}
	return MessageActingUser(&conversation, message), nil
}

func (m connectOnceMigration) channelMessage(ctx context.Context, scope Scope, conversationID, messageID string) (*ChannelMessage, error) {
	var payload string
	found := false
	if err := m.rows(ctx, `SELECT `+m.payloadText("payload")+` FROM `+m.table("channel_messages")+` WHERE scope_kind = `+m.placeholder(1)+` AND scope_id = `+m.placeholder(2)+` AND conversation_id = `+m.placeholder(3)+` AND id = `+m.placeholder(4),
		[]interface{}{scope.Kind, scope.ID, conversationID, messageID}, func(rows *sql.Rows) error {
			found = true
			return rows.Scan(&payload)
		}); err != nil || !found {
		return nil, err
	}
	message, err := decodeChannelMessage(payload)
	if err != nil {
		return nil, fmt.Errorf("message %s: %w", messageID, err)
	}
	return message, nil
}

func (s *PostgresStore) migrateConnectOnce(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, connectOnceMigrationVersion)
	if err != nil || applied {
		return err
	}
	if err := (connectOnceMigration{db: tx, table: s.table}).run(ctx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'connect once: event waits, setup copies, amendments, gateway owners, routine creators') ON CONFLICT(version) DO NOTHING`, connectOnceMigrationVersion)
	return err
}

// migrateConnectOnceSQLite mirrors PostgreSQL migration 74 and is safe to
// replay on every open.
func migrateConnectOnceSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := (connectOnceMigration{db: tx, table: func(name string) string { return name }, sqlite: true}).run(context.Background()); err != nil {
		return err
	}
	return tx.Commit()
}
