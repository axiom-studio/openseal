package sqlstore

import "github.com/axiom-studio/openseal/pkg/models"

const SpaceColumns = `id, scope_kind, scope_id, name, look, position, is_default, access, owner_id, created_at, updated_at`

const SpaceMemberColumns = `space_id, member_type, member_id, position, added_at`

const SpacePersonColumns = `space_id, user_id, added_by, added_at`

func ScanSpacePerson(row RowScanner) (*models.SpacePerson, error) {
	var person models.SpacePerson
	if err := row.Scan(&person.SpaceID, &person.UserID, &person.AddedBy, &person.AddedAt); err != nil {
		return nil, err
	}
	person.AddedAt = person.AddedAt.UTC()
	return &person, nil
}

func ScanSpace(row RowScanner) (*models.Space, error) {
	var space models.Space
	if err := row.Scan(&space.ID, &space.Scope.Kind, &space.Scope.ID, &space.Name, &space.Look, &space.Position,
		&space.IsDefault, &space.Access, &space.OwnerID, &space.CreatedAt, &space.UpdatedAt); err != nil {
		return nil, err
	}
	space.CreatedAt, space.UpdatedAt = space.CreatedAt.UTC(), space.UpdatedAt.UTC()
	return &space, nil
}

func ScanSpaceMember(row RowScanner) (*models.SpaceMember, error) {
	var member models.SpaceMember
	if err := row.Scan(&member.SpaceID, &member.Type, &member.ID, &member.Position, &member.AddedAt); err != nil {
		return nil, err
	}
	member.AddedAt = member.AddedAt.UTC()
	return &member, nil
}
