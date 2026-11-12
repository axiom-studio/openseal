package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
)

func decodeSurfaceInteraction(payload string) (*models.SurfaceInteraction, error) {
	var value models.SurfaceInteraction
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, fmt.Errorf("decode surface interaction: %w", err)
	}
	return &value, nil
}

// latestSurfaceInteractionQuery keeps the last write per item; table is the
// (qualified) surface_interactions table and placeholders are the four
// scope/conversation/surface parameters in order.
func latestSurfaceInteractionQuery(table string, acrossActors bool, placeholders [4]string) string {
	partition := "component_id, item_key"
	if !acrossActors {
		partition += ", actor_type, actor_id"
	}
	return `SELECT payload FROM (
		SELECT payload, sequence, ROW_NUMBER() OVER (PARTITION BY ` + partition + ` ORDER BY sequence DESC) AS position
		FROM ` + table + ` WHERE scope_kind = ` + placeholders[0] + ` AND scope_id = ` + placeholders[1] + `
			AND conversation_id = ` + placeholders[2] + ` AND surface_id = ` + placeholders[3] + `
	) latest WHERE position = 1 ORDER BY sequence ASC`
}

func scanSurfaceInteractions(rows *sql.Rows) ([]*models.SurfaceInteraction, error) {
	defer rows.Close()
	result := make([]*models.SurfaceInteraction, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		interaction, err := decodeSurfaceInteraction(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, interaction)
	}
	return result, rows.Err()
}
