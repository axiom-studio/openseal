package runtime

const postgresSpaceColumns = `id, scope_kind, scope_id, name, look, position, is_default, access, owner_id, created_at, updated_at`

const postgresSpaceMemberColumns = `space_id, member_type, member_id, position, added_at`

const postgresSpacePersonColumns = `space_id, user_id, added_by, added_at`

func scanSpacePerson(row interface{ Scan(...interface{}) error }) (*SpacePerson, error) {
	var person SpacePerson
	if err := row.Scan(&person.SpaceID, &person.UserID, &person.AddedBy, &person.AddedAt); err != nil {
		return nil, err
	}
	person.AddedAt = person.AddedAt.UTC()
	return &person, nil
}
