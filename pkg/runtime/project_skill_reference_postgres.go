package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// Read the expected Project without a tuple lock, acquire its logical Skill
// fences in stable order, then let the write's revision CAS fence changes that
// occurred before those locks. This matches canonical upgrade lock ordering.
func (s *PostgresStore) prepareProjectSkillReferenceWrite(ctx context.Context, tx *sql.Tx, next *Project, expected *int64) error {
	var previous *Project
	if expected != nil {
		var payload []byte
		err := tx.QueryRowContext(ctx, `SELECT payload FROM `+s.table("projects")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3 AND revision=$4`,
			next.Scope.Kind, next.Scope.ID, next.ID, *expected).Scan(&payload)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrProjectConflict
		}
		if err != nil {
			return err
		}
		previous = &Project{}
		if err := json.Unmarshal(payload, previous); err != nil {
			return err
		}
	}
	if err := lockProjectSkillReferencesPostgresTx(ctx, tx, next.Scope, previous, next); err != nil {
		return err
	}
	return validateProjectSkillReferenceAdmissionSQL(ctx, tx, previous, next, s.table("skill_bindings"), true)
}
