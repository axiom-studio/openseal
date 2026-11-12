package runtime

// markOwnerDefaultSpaces marks, for every (scope, owner) that owns spaces and
// has no default space, their oldest space by (created_at, id). An owner who
// has a default space keeps it, so running it again changes nothing.
func markOwnerDefaultSpaces(spaces, isTrue string) string {
	sameOwner := func(alias string) string {
		return alias + `.scope_kind = s.scope_kind AND ` + alias + `.scope_id = s.scope_id AND ` + alias + `.owner_id = s.owner_id`
	}
	return `UPDATE ` + spaces + ` AS s SET is_default = ` + isTrue + `
		WHERE NOT EXISTS (SELECT 1 FROM ` + spaces + ` d WHERE ` + sameOwner("d") + ` AND d.is_default = ` + isTrue + `)
		AND NOT EXISTS (SELECT 1 FROM ` + spaces + ` o WHERE ` + sameOwner("o") + `
			AND (o.created_at < s.created_at OR (o.created_at = s.created_at AND o.id < s.id)))`
}
