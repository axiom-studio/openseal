package runtime

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
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

func scanMessageReactionRecords(rows *sql.Rows, scope models.Scope, conversationID string) ([]*models.MessageReactionRecord, error) {
	defer rows.Close()
	result := make([]*models.MessageReactionRecord, 0)
	for rows.Next() {
		record := &models.MessageReactionRecord{Scope: scope, ConversationID: conversationID}
		if err := rows.Scan(&record.MessageID, &record.Actor.Type, &record.Actor.ID, &record.Emoji, &record.CreatedAt); err != nil {
			return nil, err
		}
		record.CreatedAt = record.CreatedAt.UTC()
		result = append(result, record)
	}
	return result, rows.Err()
}

func scanConversationReactionEvents(rows *sql.Rows, scope models.Scope, conversationID string) ([]*models.ConversationReactionEvent, error) {
	defer rows.Close()
	result := make([]*models.ConversationReactionEvent, 0)
	for rows.Next() {
		event := &models.ConversationReactionEvent{Scope: scope, ConversationID: conversationID}
		if err := rows.Scan(&event.Sequence, &event.Type, &event.MessageID, &event.Actor.Type, &event.Actor.ID, &event.Emoji, &event.CreatedAt); err != nil {
			return nil, err
		}
		event.CreatedAt = event.CreatedAt.UTC()
		result = append(result, event)
	}
	return result, rows.Err()
}

func reactionEventLimit(limit int) int {
	if limit <= 0 || limit > models.MaximumConversationChangeLimit+1 {
		return models.DefaultConversationChangeLimit
	}
	return limit
}

// SQLite

// PostgreSQL
