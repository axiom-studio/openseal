package runtime

import (
	"context"
	"database/sql"
)

// workingArtifactIDs reads a scope's working files for the list paths that
// filter in Go.
func workingArtifactIDs(ctx context.Context, db *sql.DB, query string, scope Scope) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, query, scope.Kind, scope.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	return ids, rows.Err()
}
