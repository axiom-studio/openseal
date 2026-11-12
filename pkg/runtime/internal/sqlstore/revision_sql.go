package sqlstore

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
)

// ExpectRevisionRow turns a revision-guarded write that matched no row into
// models.ErrRevisionConflict.
func ExpectRevisionRow(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return models.ErrRevisionConflict
	}
	return nil
}
