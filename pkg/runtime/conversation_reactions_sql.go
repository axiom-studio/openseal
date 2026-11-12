package runtime

import (
	"database/sql"
	"fmt"
	"strings"
)

func reactionMessagePlaceholders(start, count int, positional bool) string {
	parts := make([]string, count)
	for index := range parts {
		if positional {
			parts[index] = fmt.Sprintf("$%d", start+index)
		} else {
			parts[index] = "?"
		}
	}
	return strings.Join(parts, ",")
}

func scanMessageReactionRecords(rows *sql.Rows, scope Scope, conversationID string) ([]*MessageReactionRecord, error) {
	defer rows.Close()
	result := make([]*MessageReactionRecord, 0)
	for rows.Next() {
		record := &MessageReactionRecord{Scope: scope, ConversationID: conversationID}
		if err := rows.Scan(&record.MessageID, &record.Actor.Type, &record.Actor.ID, &record.Emoji, &record.CreatedAt); err != nil {
			return nil, err
		}
		record.CreatedAt = record.CreatedAt.UTC()
		result = append(result, record)
	}
	return result, rows.Err()
}

func scanConversationReactionEvents(rows *sql.Rows, scope Scope, conversationID string) ([]*ConversationReactionEvent, error) {
	defer rows.Close()
	result := make([]*ConversationReactionEvent, 0)
	for rows.Next() {
		event := &ConversationReactionEvent{Scope: scope, ConversationID: conversationID}
		if err := rows.Scan(&event.Sequence, &event.Type, &event.MessageID, &event.Actor.Type, &event.Actor.ID, &event.Emoji, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		result = append(result, event)
	}
	return result, rows.Err()
}

func reactionEventLimit(limit int) int {
	if limit <= 0 || limit > maximumConversationChangeLimit+1 {
		return defaultConversationChangeLimit
	}
	return limit
}

// SQLite

// PostgreSQL
