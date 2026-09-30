package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// WorkspaceSearchHit is a tenant-scoped candidate. Hosts must apply their
// resource authorization policy before returning it to a caller.
type WorkspaceSearchHit struct {
	Kind         string          `json:"kind"`
	Conversation *Conversation   `json:"conversation,omitempty"`
	Message      *ChannelMessage `json:"message,omitempty"`
	Artifact     *Artifact       `json:"artifact,omitempty"`
	Project      *Project        `json:"project,omitempty"`
	Run          *AgentRun       `json:"run,omitempty"`
	Score        float64         `json:"score"`
	UpdatedAt    time.Time       `json:"updatedAt"`
}

// SearchWorkspacePage searches existing active conversations and their full
// message history. Expression indexes keep older records
// searchable without a separate backfill or asynchronous indexing queue.
func (s *PostgresStore) SearchWorkspacePage(ctx context.Context, scope Scope, query string, limit, offset int) ([]WorkspaceSearchHit, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" || len(query) > 256 || limit < 1 || limit > 200 || offset < 0 {
		return nil, fmt.Errorf("invalid workspace search query or page")
	}
	search := `plainto_tsquery('simple', $3)`
	statement := `
		SELECT kind, payload, conversation_payload, score, updated_at FROM (
			SELECT 'chat' AS kind, c.payload, NULL::jsonb AS conversation_payload,
				ts_rank_cd(to_tsvector('simple', coalesce(c.payload->>'title', '')), ` + search + `)::float8 AS score, c.updated_at
			FROM ` + s.table("conversations") + ` c
			WHERE c.scope_kind=$1 AND c.scope_id=$2 AND c.status='active' AND to_tsvector('simple', coalesce(c.payload->>'title', '')) @@ ` + search + `
			UNION ALL
			SELECT 'message', m.payload, c.payload,
				ts_rank_cd(to_tsvector('simple', coalesce(m.payload->>'content', '')), ` + search + `)::float8, m.created_at
			FROM ` + s.table("channel_messages") + ` m JOIN ` + s.table("conversations") + ` c
			ON c.scope_kind=m.scope_kind AND c.scope_id=m.scope_id AND c.id=m.conversation_id
			WHERE m.scope_kind=$1 AND m.scope_id=$2 AND c.status='active' AND to_tsvector('simple', coalesce(m.payload->>'content', '')) @@ ` + search + `
			UNION ALL
			SELECT 'artifact', a.payload, NULL::jsonb,
				ts_rank_cd(to_tsvector('simple', coalesce(a.payload->>'name', '') || ' ' || coalesce(a.payload->>'metadata', '')), ` + search + `)::float8, a.created_at
			FROM ` + s.table("artifacts") + ` a
			WHERE a.scope_kind=$1 AND a.scope_id=$2 AND a.version=(SELECT max(version) FROM ` + s.table("artifacts") + ` latest WHERE latest.scope_kind=a.scope_kind AND latest.scope_id=a.scope_id AND latest.id=a.id)
			AND to_tsvector('simple', coalesce(a.payload->>'name', '') || ' ' || coalesce(a.payload->>'metadata', '')) @@ ` + search + `
			UNION ALL
			SELECT 'project', p.payload, NULL::jsonb,
				ts_rank_cd(to_tsvector('simple', coalesce(p.payload->>'title','') || ' ' || coalesce(p.payload->>'purpose','') || ' ' || coalesce(p.payload->>'milestones','') || ' ' || coalesce(p.payload->>'deliverables','')), ` + search + `)::float8, p.updated_at
			FROM ` + s.table("projects") + ` p
			WHERE p.scope_kind=$1 AND p.scope_id=$2 AND to_tsvector('simple', coalesce(p.payload->>'title','') || ' ' || coalesce(p.payload->>'purpose','') || ' ' || coalesce(p.payload->>'milestones','') || ' ' || coalesce(p.payload->>'deliverables','')) @@ ` + search + `
			UNION ALL
			SELECT 'run', r.payload, NULL::jsonb,
				ts_rank_cd(to_tsvector('simple', coalesce(r.payload->>'goal','') || ' ' || coalesce(r.payload->>'output','')), ` + search + `)::float8, (r.payload->>'updatedAt')::timestamptz
			FROM ` + s.table("agent_runs") + ` r
			WHERE r.scope_kind=$1 AND r.scope_id=$2 AND to_tsvector('simple', coalesce(r.payload->>'goal','') || ' ' || coalesce(r.payload->>'output','')) @@ ` + search + `
		) hits ORDER BY score DESC, updated_at DESC, kind, payload->>'id' LIMIT $4 OFFSET $5`
	rows, err := s.db.QueryContext(ctx, statement, scope.Kind, scope.ID, query, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	hits := make([]WorkspaceSearchHit, 0, limit)
	for rows.Next() {
		var hit WorkspaceSearchHit
		var payload []byte
		var conversationPayload []byte
		if err := rows.Scan(&hit.Kind, &payload, &conversationPayload, &hit.Score, &hit.UpdatedAt); err != nil {
			return nil, err
		}
		switch hit.Kind {
		case "chat":
			var value Conversation
			if err := json.Unmarshal(payload, &value); err != nil {
				return nil, err
			}
			hit.Conversation = &value
		case "message":
			var message ChannelMessage
			var conversation Conversation
			if err := json.Unmarshal(payload, &message); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(conversationPayload, &conversation); err != nil {
				return nil, err
			}
			hit.Message, hit.Conversation = &message, &conversation
		case "artifact":
			var value Artifact
			if err := json.Unmarshal(payload, &value); err != nil {
				return nil, err
			}
			hit.Artifact = &value
		case "project":
			var value Project
			if err := json.Unmarshal(payload, &value); err != nil {
				return nil, err
			}
			hit.Project = &value
		case "run":
			var value AgentRun
			if err := json.Unmarshal(payload, &value); err != nil {
				return nil, err
			}
			hit.Run = &value
		}
		hits = append(hits, hit)
	}
	return hits, rows.Err()
}
