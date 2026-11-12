package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// teamMembersMigrationVersion removes Team role slots. What a role carried
// (name, purpose, channel participation, required Skills, approver and lead
// designation) moves onto the roster members that held it.
const teamMembersMigrationVersion int64 = 85

// teamRetiredLeadRoleID was the lead role of a Team that named none.
const teamRetiredLeadRoleID = "lead"

// teamMembersMigration rewrites stored Team payloads in Go so PostgreSQL and
// SQLite share one transformation. Stored definition digests are left as they
// are, like migration 69 left them: nothing recomputes a stored digest, and a
// pending amendment still has to match its base's.
type teamMembersMigration struct {
	ctx   context.Context
	db    teamSkillGrantMigrationDB
	table func(string) string
	// placeholder renders the n-th bind parameter; "?" selects SQLite.
	placeholder func(int) string
	exists      func(table string) (bool, error)
}

type teamMembersRow struct {
	keys     []string
	document map[string]interface{}
}

// rows returns the decoded payloads of table whose text matches any of the
// LIKE patterns, or every row when there are none.
func (m teamMembersMigration) rows(table string, columns []string, patterns ...string) ([]teamMembersRow, error) {
	exists, err := m.exists(table)
	if err != nil || !exists {
		return nil, err
	}
	payload := "payload::text"
	if m.placeholder(1) == "?" {
		payload = "payload"
	}
	query := `SELECT ` + strings.Join(columns, ", ") + `, ` + payload + ` FROM ` + m.table(table)
	for index, pattern := range patterns {
		clause := ` OR `
		if index == 0 {
			clause = ` WHERE `
		}
		query += clause + payload + ` LIKE '%"` + pattern + `"%'`
	}
	rows, err := m.db.QueryContext(m.ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []teamMembersRow
	for rows.Next() {
		row := teamMembersRow{keys: make([]string, len(columns))}
		targets := make([]interface{}, 0, len(columns)+1)
		for index := range row.keys {
			targets = append(targets, &row.keys[index])
		}
		var text string
		if err := rows.Scan(append(targets, &text)...); err != nil {
			return nil, err
		}
		if row.document, err = decodeMigrationDocument(text); err != nil {
			return nil, fmt.Errorf("%s %s: %w", table, strings.Join(row.keys, "/"), err)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (m teamMembersMigration) write(table string, columns []string, row teamMembersRow) error {
	encoded, err := json.Marshal(row.document)
	if err != nil {
		return err
	}
	value := m.placeholder(1)
	if value != "?" {
		value += "::jsonb"
	}
	args := []interface{}{string(encoded)}
	where := make([]string, len(columns))
	for index, column := range columns {
		args = append(args, row.keys[index])
		where[index] = column + ` = ` + m.placeholder(len(args))
	}
	_, err = m.db.ExecContext(m.ctx, `UPDATE `+m.table(table)+` SET payload = `+value+` WHERE `+strings.Join(where, ` AND `), args...)
	return err
}

var (
	teamDefinitionKey = []string{"id", "version"}
	teamScopedKey     = []string{"scope_kind", "scope_id", "id"}
)

func migrationObject(value interface{}) map[string]interface{} {
	object, _ := value.(map[string]interface{})
	return object
}

func migrationObjects(value interface{}) []map[string]interface{} {
	items, _ := value.([]interface{})
	result := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		if object := migrationObject(item); object != nil {
			result = append(result, object)
		}
	}
	return result
}

// renameAllowedAmendmentField renames one entry of a definition's
// amendments.allowedFields.
func renameAllowedAmendmentField(definition map[string]interface{}, from, to string) bool {
	fields, _ := migrationObject(definition["amendments"])["allowedFields"].([]interface{})
	changed := false
	for index, field := range fields {
		if field == from {
			fields[index], changed = to, true
		}
	}
	return changed
}

// stripTeamRoles removes the role slots and the references to them from one
// stored definition document.
func stripTeamRoles(definition map[string]interface{}) bool {
	if definition == nil {
		return false
	}
	changed := renameAllowedAmendmentField(definition, "roles", TeamMemberAmendmentField)
	for _, location := range []struct {
		object map[string]interface{}
		key    string
	}{
		{definition, "roles"},
		{migrationObject(definition["approvals"]), "approverRoleIds"},
		{migrationObject(definition["routing"]), "leadRoleId"},
	} {
		if _, present := location.object[location.key]; present {
			delete(location.object, location.key)
			changed = true
		}
	}
	return changed
}

// up moves every stored Team from role slots to members.
func (m teamMembersMigration) up() error {
	definitions, err := m.rows("team_definitions", teamDefinitionKey, "roles", "approverRoleIds", "leadRoleId")
	if err != nil {
		return err
	}
	byVersion := make(map[string]map[string]interface{}, len(definitions))
	for _, row := range definitions {
		byVersion[row.keys[0]+"\x00"+row.keys[1]] = row.document
	}
	deployments, err := m.rows("team_deployments", teamScopedKey, "roleId")
	if err != nil {
		return err
	}
	// holders maps scope, Team and role id to the first member that held the
	// role: what a stored request for that role now names.
	holders := make(map[string]string)
	for _, row := range deployments {
		definition := byVersion[migrationString(row.document, "definitionId")+"\x00"+migrationString(row.document, "activeVersion")]
		roles := make(map[string]map[string]interface{})
		for _, role := range migrationObjects(definition["roles"]) {
			roles[migrationString(role, "id")] = role
		}
		approvers := make(map[string]bool)
		for _, id := range migrationStrings(migrationObject(definition["approvals"])["approverRoleIds"]) {
			approvers[strings.TrimSpace(id)] = true
		}
		leadRole := migrationString(migrationObject(definition["routing"]), "leadRoleId")
		if leadRole == "" {
			leadRole = teamRetiredLeadRoleID
		}
		lead := false
		for _, member := range migrationObjects(row.document["roster"]) {
			roleID, held := member["roleId"].(string)
			if !held {
				continue
			}
			delete(member, "roleId")
			holder := strings.Join(append(row.keys, roleID), "\x00")
			if _, taken := holders[holder]; !taken {
				holders[holder] = migrationString(member, "id")
			}
			role := roles[roleID]
			if strings.TrimSpace(migrationString(member, "displayName")) == "" && migrationString(role, "displayName") != "" {
				member["displayName"] = role["displayName"]
			}
			for _, key := range []string{"purpose", "channelParticipation"} {
				if migrationString(role, key) != "" {
					member[key] = role[key]
				}
			}
			if skills, _ := role["requiredSkillIds"].([]interface{}); len(skills) > 0 {
				member["requiredSkillIds"] = skills
			}
			if approvers[roleID] {
				member["approver"] = true
			}
			if roleID == leadRole && !lead {
				member["lead"], lead = true, true
			}
		}
		if err := m.write("team_deployments", teamScopedKey, row); err != nil {
			return err
		}
	}
	requests, err := m.rows("agent_requests", teamScopedKey, "roleId", "semanticRole")
	if err != nil {
		return err
	}
	for _, row := range requests {
		changed := false
		for _, candidate := range migrationObjects(row.document["candidates"]) {
			if _, present := candidate["roleId"]; present {
				delete(candidate, "roleId")
				changed = true
			}
		}
		recipient := migrationObject(row.document["recipient"])
		role := migrationString(row.document, "semanticRole")
		if member := holders[strings.Join([]string{row.keys[0], row.keys[1], migrationString(recipient, "id"), role}, "\x00")]; migrationString(recipient, "type") == string(OwnerTypeTeam) && member != "" && member != role {
			row.document["semanticRole"], changed = member, true
		}
		if changed {
			if err := m.write("agent_requests", teamScopedKey, row); err != nil {
				return err
			}
		}
	}
	for _, row := range definitions {
		if stripTeamRoles(row.document) {
			if err := m.write("team_definitions", teamDefinitionKey, row); err != nil {
				return err
			}
		}
	}
	amendments, err := m.rows("team_definition_amendments", teamScopedKey, "roles", "approverRoleIds", "leadRoleId")
	if err != nil {
		return err
	}
	for _, row := range amendments {
		if stripTeamRoles(migrationObject(row.document["candidate"])) {
			if err := m.write("team_definition_amendments", teamScopedKey, row); err != nil {
				return err
			}
		}
	}
	return nil
}

// teamRetiredRoles is the role-slot view of one Team's members, for a kernel
// that still expects it.
type teamRetiredRoles struct {
	roles     []interface{}
	approvers []interface{}
	lead      string
	seen      map[string]bool
}

// restore gives one definition document its role slots back. A document
// that already has roles is left alone.
func (r *teamRetiredRoles) restore(definition map[string]interface{}) bool {
	if definition == nil {
		return false
	}
	changed := renameAllowedAmendmentField(definition, TeamMemberAmendmentField, "roles")
	if _, present := definition["roles"]; present || r == nil {
		return changed
	}
	definition["roles"] = r.roles
	if len(r.approvers) > 0 {
		approvals := migrationObject(definition["approvals"])
		if approvals == nil {
			approvals = map[string]interface{}{}
			definition["approvals"] = approvals
		}
		approvals["approverRoleIds"] = r.approvers
	}
	if r.lead != "" {
		routing := migrationObject(definition["routing"])
		if routing == nil {
			routing = map[string]interface{}{}
			definition["routing"] = routing
		}
		routing["leadRoleId"] = r.lead
	}
	return true
}

// down gives every member a role slot of its own, named after the member,
// in every stored version of its Team's definition. Running it again changes
// nothing: members that have a role and definitions that have roles are
// skipped, and so are tables an earlier rollback dropped.
func (m teamMembersMigration) down() error {
	deployments, err := m.rows("team_deployments", teamScopedKey)
	if err != nil {
		return err
	}
	retired := make(map[string]*teamRetiredRoles)
	for _, row := range deployments {
		changed := false
		definitionID := migrationString(row.document, "definitionId")
		for _, member := range migrationObjects(row.document["roster"]) {
			id := migrationString(member, "id")
			if _, held := member["roleId"]; held || id == "" {
				continue
			}
			changed = true
			member["roleId"] = id
			name := migrationString(member, "displayName")
			if strings.TrimSpace(name) == "" {
				name = id
			}
			role := map[string]interface{}{"id": id, "displayName": name, "purpose": name + " team member", "minimumMembers": 1, "maximumMembers": 1}
			for _, key := range []string{"purpose", "channelParticipation", "requiredSkillIds"} {
				if value, present := member[key]; present {
					role[key] = value
					delete(member, key)
				}
			}
			roles := retired[definitionID]
			if roles == nil {
				roles = &teamRetiredRoles{seen: map[string]bool{}}
				retired[definitionID] = roles
			}
			if member["approver"] == true && !roles.seen[id] {
				roles.approvers = append(roles.approvers, id)
			}
			if member["lead"] == true && roles.lead == "" {
				roles.lead = id
			}
			delete(member, "approver")
			delete(member, "lead")
			// Two Teams that share a definition share its roles by member id.
			if !roles.seen[id] {
				roles.seen[id] = true
				roles.roles = append(roles.roles, role)
			}
		}
		if changed {
			if err := m.write("team_deployments", teamScopedKey, row); err != nil {
				return err
			}
		}
	}
	definitions, err := m.rows("team_definitions", teamDefinitionKey)
	if err != nil {
		return err
	}
	for _, row := range definitions {
		if retired[row.keys[0]].restore(row.document) {
			if err := m.write("team_definitions", teamDefinitionKey, row); err != nil {
				return err
			}
		}
	}
	amendments, err := m.rows("team_definition_amendments", teamScopedKey)
	if err != nil {
		return err
	}
	for _, row := range amendments {
		if retired[migrationString(row.document, "definitionId")].restore(migrationObject(row.document["candidate"])) {
			if err := m.write("team_definition_amendments", teamScopedKey, row); err != nil {
				return err
			}
		}
	}
	requests, err := m.rows("agent_requests", teamScopedKey, "candidates")
	if err != nil {
		return err
	}
	for _, row := range requests {
		changed := false
		for _, candidate := range migrationObjects(row.document["candidates"]) {
			if _, present := candidate["roleId"]; !present {
				candidate["roleId"], changed = candidate["rosterAssignmentId"], true
			}
		}
		if changed {
			if err := m.write("agent_requests", teamScopedKey, row); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *PostgresStore) teamMembersMigration(ctx context.Context, tx *sql.Tx) teamMembersMigration {
	return teamMembersMigration{
		ctx: ctx, db: tx, table: s.table, placeholder: func(n int) string { return fmt.Sprintf("$%d", n) },
		exists: func(table string) (bool, error) {
			var relation sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT to_regclass($1)`, s.table(table)).Scan(&relation)
			return relation.Valid, err
		},
	}
}

// migrateTeamMembers is PostgreSQL migration 85.
func (s *PostgresStore) migrateTeamMembers(ctx context.Context, tx *sql.Tx) error {
	applied, err := s.postgresMigrationApplied(ctx, tx, teamMembersMigrationVersion)
	if err != nil || applied {
		return err
	}
	if err := s.teamMembersMigration(ctx, tx).up(); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO `+s.table("schema_migrations")+`(version, name)
		VALUES ($1, 'Team members replace role slots') ON CONFLICT (version) DO NOTHING`, teamMembersMigrationVersion)
	return err
}

// rollbackTeamMembers restores role slots for a kernel that requires them.
func (s *PostgresStore) rollbackTeamMembers(ctx context.Context, tx *sql.Tx) error {
	return s.teamMembersMigration(ctx, tx).down()
}

func sqliteTeamMembersMigration(tx *sql.Tx) teamMembersMigration {
	return teamMembersMigration{
		ctx: context.Background(), db: tx, table: func(name string) string { return name }, placeholder: func(int) string { return "?" },
		exists: func(table string) (bool, error) {
			var count int
			err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&count)
			return count > 0, err
		},
	}
}

// migrateTeamMembersSQLite mirrors PostgreSQL migration 85. It only touches
// payloads that still carry role slots, so a later open does nothing.
func migrateTeamMembersSQLite(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := sqliteTeamMembersMigration(tx).up(); err != nil {
		return err
	}
	return tx.Commit()
}
