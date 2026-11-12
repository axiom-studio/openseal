package models

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"time"
)

type ActivityProjection struct {
	AnswerPreview    *TurnAnswerPreview     `json:"answerPreview,omitempty"`
	ID               string                 `json:"id"`
	Sequence         int64                  `json:"sequence"`
	EventType        string                 `json:"eventType"`
	Category         string                 `json:"category"`
	Severity         ActivitySeverity       `json:"severity"`
	Visibility       ActivityVisibility     `json:"visibility"`
	AgentID          string                 `json:"agentId,omitempty"`
	ObjectiveID      string                 `json:"objectiveId,omitempty"`
	RunID            string                 `json:"runId,omitempty"`
	TurnID           string                 `json:"turnId,omitempty"`
	ParentRunID      string                 `json:"parentRunId,omitempty"`
	TeamID           string                 `json:"teamId,omitempty"`
	Actor            ActivityActor          `json:"actor"`
	Summary          string                 `json:"summary"`
	UsageDelta       *BudgetUsage           `json:"usageDelta,omitempty"`
	CreatedAt        time.Time              `json:"createdAt"`
	DetailAvailable  bool                   `json:"detailAvailable"`
	Payload          map[string]interface{} `json:"payload,omitempty"`
	ConversationRefs []string               `json:"conversationRefs,omitempty"`
	CorrelationID    string                 `json:"correlationId,omitempty"`
	CausationID      string                 `json:"causationId,omitempty"`
}

type ActivityFeedCursor struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

func ProjectActivityEvent(event *ActivityEvent, includeDetails bool) ActivityProjection {
	projection := ActivityProjection{
		ID: event.ID, Sequence: event.Sequence, EventType: event.EventType, Category: activityCategory(event.EventType),
		Severity: event.Severity, Visibility: event.Visibility, AgentID: event.AgentID, ObjectiveID: event.ObjectiveID,
		RunID: event.RunID, TurnID: event.TurnID, ParentRunID: event.ParentRunID, TeamID: event.TeamID,
		Actor: event.Actor, Summary: event.Summary, UsageDelta: CloneBudgetUsage(event.UsageDelta), CreatedAt: event.CreatedAt,
		DetailAvailable: len(event.Payload) > 0 || len(event.ConversationRefs) > 0 || event.CorrelationID != "" || event.CausationID != "",
	}
	if includeDetails {
		projection.Payload = maps.Clone(event.Payload)
		projection.ConversationRefs = append([]string(nil), event.ConversationRefs...)
		projection.CorrelationID = event.CorrelationID
		projection.CausationID = event.CausationID
	}
	projection.AnswerPreview = ProjectTurnAnswerPreview(event)
	return projection
}

func activityCategory(eventType string) string {
	prefix, _, _ := strings.Cut(strings.TrimSpace(eventType), ".")
	switch prefix {
	case "run", "turn", "action", "approval", "skill", "artifact", "handoff", "objective", "message", "system":
		return prefix
	default:
		return "activity"
	}
}

func EncodeActivityFeedCursor(event *ActivityEvent) (string, error) {
	encoded, err := json.Marshal(ActivityFeedCursor{CreatedAt: event.CreatedAt.UTC(), ID: event.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func DecodeActivityFeedCursor(value string) (*ActivityFeedCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, errors.New("invalid activity feed cursor")
	}
	var cursor ActivityFeedCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.CreatedAt.IsZero() || strings.TrimSpace(cursor.ID) == "" {
		return nil, errors.New("invalid activity feed cursor")
	}
	return &cursor, nil
}
