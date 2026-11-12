package sqlstore

import (
	"database/sql"
	"encoding/json"

	"github.com/axiom-studio/openseal/pkg/models"
)

// ScanActivityEvents decodes the payload column of every row and closes rows.
func ScanActivityEvents(rows *sql.Rows, capacity int) ([]*models.ActivityEvent, error) {
	defer rows.Close()
	result := make([]*models.ActivityEvent, 0, capacity)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var event models.ActivityEvent
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			return nil, err
		}
		result = append(result, &event)
	}
	return result, rows.Err()
}
