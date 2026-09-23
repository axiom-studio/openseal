package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// artifactListQuery applies the common catalog filters and page boundaries in
// SQL so a user-visible page does not load and sort every tenant artifact.
// Evidence and retention queries keep the exact Go filter until those nested
// fields have a portable indexed representation.
func artifactListQuery(filter ArtifactFilter, table string, postgres bool) (string, []any) {
	args := make([]any, 0, 12)
	bind := func(value any) string {
		args = append(args, value)
		if postgres {
			return fmt.Sprintf("$%d", len(args))
		}
		return "?"
	}
	where := []string{"a.scope_kind = " + bind(filter.Scope.Kind), "a.scope_id = " + bind(filter.Scope.ID)}
	if filter.ID != "" {
		where = append(where, "a.id = "+bind(filter.ID))
	}
	if filter.Owner != nil {
		ownerType, ownerID := "json_extract(a.payload, '$.provenance.owner.type')", "json_extract(a.payload, '$.provenance.owner.id')"
		if postgres {
			ownerType, ownerID = "a.payload->'provenance'->'owner'->>'type'", "a.payload->'provenance'->'owner'->>'id'"
		}
		where = append(where, ownerType+" = "+bind(string(filter.Owner.Type)), ownerID+" = "+bind(filter.Owner.ID))
	}
	addStrings := func(column string, values []string) {
		if len(values) == 0 {
			return
		}
		placeholders := make([]string, 0, len(values))
		for _, value := range values {
			placeholders = append(placeholders, bind(strings.TrimSpace(value)))
		}
		where = append(where, column+" IN ("+strings.Join(placeholders, ", ")+")")
	}
	addStrings("a.type", filter.Types)
	addStrings("a.media_type", filter.MediaTypes)
	if len(filter.Classifications) > 0 {
		values := make([]string, len(filter.Classifications))
		for i, value := range filter.Classifications {
			values[i] = string(value)
		}
		addStrings("a.classification", values)
	}
	if filter.ProducerRunID != "" {
		where = append(where, "a.producer_run_id = "+bind(filter.ProducerRunID))
	}
	if filter.ProducerRequestID != "" {
		where = append(where, "a.producer_request_id = "+bind(filter.ProducerRequestID))
	}
	if filter.LatestOnly {
		where = append(where, "NOT EXISTS (SELECT 1 FROM "+table+" newer WHERE newer.scope_kind = a.scope_kind AND newer.scope_id = a.scope_id AND newer.id = a.id AND newer.version > a.version)")
	}
	query := "SELECT a.payload FROM " + table + " a WHERE " + strings.Join(where, " AND ") +
		" ORDER BY a.created_at DESC, a.id ASC, a.version DESC LIMIT " + bind(filter.Limit) + " OFFSET " + bind(filter.Offset)
	return query, args
}

func queryArtifactPage(ctx context.Context, db *sql.DB, query string, args []any) ([]*Artifact, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*Artifact, 0)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		artifact, err := decodeArtifact(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, artifact)
	}
	return result, rows.Err()
}
