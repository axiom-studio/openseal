package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type channelMessageBatchSQLReader interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

func getChannelMessageBatchSQL(ctx context.Context, reader channelMessageBatchSQLReader, table string, scope Scope, conversationID string, ids []string, postgres bool) ([]*ChannelMessage, error) {
	if err := validateChannelMessageBatch(scope, ids); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []*ChannelMessage{}, nil
	}
	args := []interface{}{scope.Kind, scope.ID, conversationID}
	placeholders := make([]string, len(ids))
	for index, id := range ids {
		args = append(args, id)
		placeholders[index] = "?"
		if postgres {
			placeholders[index] = fmt.Sprintf("$%d", index+4)
		}
	}
	predicate := "scope_kind = ? AND scope_id = ? AND conversation_id = ?"
	if postgres {
		predicate = "scope_kind = $1 AND scope_id = $2 AND conversation_id = $3"
	}
	rows, err := reader.QueryContext(ctx, "SELECT payload FROM "+table+" WHERE "+predicate+" AND id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]*ChannelMessage, 0, len(ids))
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		message, err := decodeChannelMessage(payload)
		if err != nil {
			return nil, err
		}
		result = append(result, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
