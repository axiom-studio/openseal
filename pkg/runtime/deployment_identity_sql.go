package runtime

import (
	"context"
	"database/sql"
)

func deploymentIdentityCollisions(rows *sql.Rows) ([]string, error) {
	defer rows.Close()
	var collisions []string
	for rows.Next() {
		var kind, scopeID, id string
		if err := rows.Scan(&kind, &scopeID, &id); err != nil {
			return nil, err
		}
		collisions = append(collisions, kind+"/"+scopeID+"/"+id)
	}
	return collisions, rows.Err()
}

type sqlExecQueryer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}
