package sqlstore

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
)

func ScanRunLoopCutoverRefs(rows *sql.Rows) ([]models.RunLoopCutoverRef, error) {
	defer rows.Close()
	refs := make([]models.RunLoopCutoverRef, 0)
	for rows.Next() {
		var ref models.RunLoopCutoverRef
		if err := rows.Scan(&ref.Scope.Kind, &ref.Scope.ID, &ref.ID); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
