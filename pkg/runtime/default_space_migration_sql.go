package runtime

// defaultSpaceRewrites move every personal-<md5> space to default-<md5>. The
// old rows give up is_default first so the new rows fit the one-per-scope
// index; members move before the old rows go, so the cascade drops nothing.
func defaultSpaceRewrites(spaces, members, conversations, artifacts string, postgres bool) []string {
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
