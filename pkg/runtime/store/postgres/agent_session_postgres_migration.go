package postgres

import (
	"context"
	"database/sql"
)

const agentSessionLogMigrationVersion int64 = 75

// migrateAgentSessionLog creates the session log: one row per Agent context
// (agent_sessions) and its append-only entries (agent_session_entries). It
// creates tables only; nothing existing is altered or backfilled.
func (s *Store) migrateAgentSessionLog(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, agentSessionLogMigrationVersion)
	if err != nil || applied {
		return err
	}
	sessions := s.table("agent_sessions")
	entries := s.table("agent_session_entries")
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS ` + sessions + ` (
			scope_kind        TEXT        NOT NULL,
			scope_id          TEXT        NOT NULL,
			id                TEXT        NOT NULL,
			kind              TEXT        NOT NULL CHECK (kind IN ('conversation','run')),
			lane_key          TEXT        NOT NULL DEFAULT '',
			conversation_id   TEXT        NOT NULL DEFAULT '',
			agent_id          TEXT        NOT NULL,
			owner_run_id      TEXT        NOT NULL DEFAULT '',
			parent_session_id TEXT        NOT NULL DEFAULT '',
			fork_sequence     BIGINT      NOT NULL DEFAULT 0 CHECK (fork_sequence >= 0),
			root_session_id   TEXT        NOT NULL,
			depth             INTEGER     NOT NULL DEFAULT 0 CHECK (depth BETWEEN 0 AND 8),
			next_sequence     BIGINT      NOT NULL DEFAULT 1 CHECK (next_sequence > 0),
			channel_watermark BIGINT      NOT NULL DEFAULT 0 CHECK (channel_watermark >= 0),
			revision          BIGINT      NOT NULL CHECK (revision > 0),
			created_at        TIMESTAMPTZ NOT NULL,
			updated_at        TIMESTAMPTZ NOT NULL,
			payload           JSONB       NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, id),
			CHECK ((kind = 'conversation') = (lane_key <> '')),
			CHECK ((kind = 'run') = (owner_run_id <> '')),
			CHECK ((parent_session_id = '') = (depth = 0))
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_sessions_lane_idx
			ON ` + sessions + ` (scope_kind, scope_id, lane_key) WHERE kind = 'conversation'`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_sessions_owner_run_idx
			ON ` + sessions + ` (scope_kind, scope_id, owner_run_id) WHERE kind = 'run'`,
		`CREATE INDEX IF NOT EXISTS agent_sessions_parent_idx
			ON ` + sessions + ` (scope_kind, scope_id, parent_session_id) WHERE parent_session_id <> ''`,
		`CREATE TABLE IF NOT EXISTS ` + entries + ` (
			scope_kind     TEXT        NOT NULL,
			scope_id       TEXT        NOT NULL,
			session_id     TEXT        NOT NULL,
			sequence       BIGINT      NOT NULL CHECK (sequence > 0),
			id             TEXT        NOT NULL,
			run_id         TEXT        NOT NULL,
			step_id        TEXT        NOT NULL DEFAULT '',
			step           BIGINT      NOT NULL DEFAULT 0 CHECK (step >= 0),
			type           TEXT        NOT NULL CHECK (type IN ('user','assistant','tool','compaction','note','model_change')),
			state          TEXT        NOT NULL DEFAULT '' CHECK (state IN ('','pending','awaiting_approval','awaiting_child','awaiting_event','running','completed','error','interrupted')),
			tool_call_id   TEXT        NOT NULL DEFAULT '',
			action_call_id TEXT        NOT NULL DEFAULT '',
			source_key     TEXT        NOT NULL DEFAULT '',
			tokens         BIGINT      NOT NULL DEFAULT 0 CHECK (tokens >= 0),
			revision       BIGINT      NOT NULL CHECK (revision > 0),
			created_at     TIMESTAMPTZ NOT NULL,
			updated_at     TIMESTAMPTZ NOT NULL,
			payload        JSONB       NOT NULL,
			PRIMARY KEY (scope_kind, scope_id, session_id, sequence),
			UNIQUE (scope_kind, scope_id, id),
			CHECK ((type = 'tool') = (state <> '')),
			CHECK ((type = 'tool') = (tool_call_id <> '')),
			FOREIGN KEY (scope_kind, scope_id, session_id)
				REFERENCES ` + sessions + ` (scope_kind, scope_id, id) ON DELETE CASCADE
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS agent_session_entries_source_idx
			ON ` + entries + ` (scope_kind, scope_id, session_id, source_key) WHERE source_key <> ''`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_compaction_idx
			ON ` + entries + ` (scope_kind, scope_id, session_id, sequence DESC) WHERE type = 'compaction'`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_run_idx
			ON ` + entries + ` (scope_kind, scope_id, run_id, created_at, session_id, sequence)`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_action_idx
			ON ` + entries + ` (scope_kind, scope_id, action_call_id) WHERE action_call_id <> ''`,
		`CREATE INDEX IF NOT EXISTS agent_session_entries_open_tool_idx
			ON ` + entries + ` (scope_kind, scope_id, run_id)
			WHERE type = 'tool' AND state IN ('pending','awaiting_approval','awaiting_child','awaiting_event','running')`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version,name) VALUES($1,'agent session log') ON CONFLICT(version) DO NOTHING`, agentSessionLogMigrationVersion)
	return err
}
