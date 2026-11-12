package sqlstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/models"
)

const (
	RetiredChangeSetTable    = "workforce_change_sets"
	AgentBuilderCutoverTable = "agent_builder_cutover_drafts"
)

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
	Placement models.AgentBuilderCutoverPlacement `json:"placement"`
}

func decodeAgentBuilderCutoverDraft(scope models.Scope, id, idempotencyKey, payload string) (models.AgentBuilderCutoverDraft, error) {
	draft := models.AgentBuilderCutoverDraft{Scope: scope, ID: id, IdempotencyKey: idempotencyKey}
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

func readChangeSetLineage(ctx context.Context, tx ExecQuerier, query string) ([]changeSetLineageRow, error) {
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

// FoldChangeSetDrafts keeps only the leaf drafts, each with its lineage's
// prompts in cutoverPrompts. The SQL dialect differs only in placeholders
// and the JSON write.
func FoldChangeSetDrafts(ctx context.Context, tx ExecQuerier, table, selectQuery, setPrompts, deleteRow string) error {
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

// ScanAgentBuilderCutoverDrafts decodes (scope_kind, scope_id, id,
// idempotency_key, payload) rows.
func ScanAgentBuilderCutoverDrafts(rows *sql.Rows) ([]models.AgentBuilderCutoverDraft, error) {
	defer rows.Close()
	var drafts []models.AgentBuilderCutoverDraft
	for rows.Next() {
		var scope models.Scope
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
