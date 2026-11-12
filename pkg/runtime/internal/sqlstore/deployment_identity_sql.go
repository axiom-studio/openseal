package sqlstore

import (
	"database/sql"
)

func DeploymentIdentityCollisions(rows *sql.Rows) ([]string, error) {
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
