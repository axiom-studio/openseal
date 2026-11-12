package sqlstore

// defaultSpaceRewrites move every personal-<md5> space to default-<md5>. The
// old rows give up is_default first so the new rows fit the one-per-scope
// index; members move before the old rows go, so the cascade drops nothing.
func DefaultSpaceRewrites(spaces, members, conversations, artifacts string, postgres bool) []string {
	newID := func(column string) string { return `'default-' || substr(` + column + `, 10)` }
	isTrue, isFalse := "1", "0"
	conversationPayload := `json_set(payload, '$.spaceId', ` + newID("space_id") + `)`
	artifactSpaceID := `json_extract(payload, '$.provenance.spaceId')`
	artifactPayload := `json_set(payload, '$.provenance.spaceId', ` + newID(artifactSpaceID) + `)`
	if postgres {
		isTrue, isFalse = "TRUE", "FALSE"
		conversationPayload = `jsonb_set(payload, '{spaceId}', to_jsonb(` + newID("space_id") + `), true)`
		artifactSpaceID = `(payload->'provenance'->>'spaceId')`
		artifactPayload = `jsonb_set(payload, '{provenance,spaceId}', to_jsonb(` + newID(artifactSpaceID) + `), true)`
	}
	return []string{
		`UPDATE ` + spaces + ` SET is_default = ` + isFalse + ` WHERE id LIKE 'personal-%'`,
		`INSERT INTO ` + spaces + ` (id, scope_kind, scope_id, name, look, position, is_default, created_by, created_at, updated_at)
			SELECT ` + newID("id") + `, scope_kind, scope_id, CASE WHEN name = 'Personal' THEN 'Default' ELSE name END,
				look, position, ` + isTrue + `, created_by, created_at, updated_at
			FROM ` + spaces + ` WHERE id LIKE 'personal-%'`,
		`UPDATE ` + members + ` SET space_id = ` + newID("space_id") + ` WHERE space_id LIKE 'personal-%'`,
		`UPDATE ` + conversations + ` SET space_id = ` + newID("space_id") + `, payload = ` + conversationPayload + `
			WHERE space_id LIKE 'personal-%'`,
		`UPDATE ` + artifacts + ` SET payload = ` + artifactPayload + ` WHERE ` + artifactSpaceID + ` LIKE 'personal-%'`,
		`DELETE FROM ` + spaces + ` WHERE id LIKE 'personal-%'`,
	}
}

// retiredKernelProjectTables are dropped by migration 77, dependents first.
var RetiredKernelProjectTables = []string{
	"project_skill_references", "outreach_threads", "source_monitor_checkpoints", "source_observations", "projects",
	"source_policy_lifecycle_events", "source_policy_lifecycles", "source_policy_versions",
}

// legacyEventSourceTables are dropped by migration 79, dependents first.
var LegacyEventSourceTables = []string{"event_source_health", "event_source_subscriptions", "event_source_checkpoints"}

// legacyEventWaitTables are dropped by migration 81, dependents first.
// run_event_retention_scopes only ever existed in SQLite.
var LegacyEventWaitTables = []string{
	"run_event_notifications", "run_event_consumptions", "run_event_wait_scopes", "run_event_waits",
	"run_event_retention_cursor", "run_event_retention_scopes", "run_event_inbox",
}

// markOwnerDefaultSpaces marks, for every (scope, owner) that owns spaces and
// has no default space, their oldest space by (created_at, id). An owner who
// has a default space keeps it, so running it again changes nothing.
func MarkOwnerDefaultSpaces(spaces, isTrue string) string {
	sameOwner := func(alias string) string {
		return alias + `.scope_kind = s.scope_kind AND ` + alias + `.scope_id = s.scope_id AND ` + alias + `.owner_id = s.owner_id`
	}
	return `UPDATE ` + spaces + ` AS s SET is_default = ` + isTrue + `
		WHERE NOT EXISTS (SELECT 1 FROM ` + spaces + ` d WHERE ` + sameOwner("d") + ` AND d.is_default = ` + isTrue + `)
		AND NOT EXISTS (SELECT 1 FROM ` + spaces + ` o WHERE ` + sameOwner("o") + `
			AND (o.created_at < s.created_at OR (o.created_at = s.created_at AND o.id < s.id)))`
}

func SkillUpgradeDrainIndexStatements(actions, runs string, postgres bool) []string {
	prefix := "idx_"
	if postgres {
		prefix = ""
	}
	return []string{
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_runtime_active ON " + actions + " (scope_kind,scope_id,skill_id,skill_version,status) WHERE status IN (" + unfinishedSkillActionSQL + ")",
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_binding_active ON " + actions + " (scope_kind,scope_id,deployment_id,binding_id,skill_id,skill_version,status) WHERE status IN (" + unfinishedSkillActionSQL + ")",
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_run_receipt ON " + actions + " (scope_kind,scope_id,run_id,skill_id,skill_version,deployment_id,binding_id) WHERE status='succeeded'",
		"CREATE INDEX IF NOT EXISTS " + prefix + "agent_runs_skill_runtime_active ON " + runs + " (scope_kind,scope_id,status,id) WHERE status IN (" + UnfinishedSkillRunSQL + ")",
	}
}

// teamCoordinationPaths are the JSON locations of the retired coordination
// policy (speaker caps and participation flags) in every durable artifact that
// can carry a Team definition. Team channels now route each message to the
// members it addresses, or to one member a router picks.
var TeamCoordinationPaths = []struct{ Table, Postgres, SQLite string }{
	{"team_definitions", "{coordination}", "$.coordination"},
	{"team_definition_amendments", "{candidate,coordination}", "$.candidate.coordination"},
}
