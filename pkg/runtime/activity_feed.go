package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

const (
	defaultActivityFeedLimit = 25
	maximumActivityFeedLimit = 100
)

// ActivityFeedRequest selects one portable activity projection. At least one
// resource selector is required so callers cannot accidentally request an
// unbounded scope-wide audit dump.
type ActivityFeedRequest struct {
	Scope          Scope
	RunID          string
	AgentID        string
	ObjectiveID    string
	TeamID         string
	EventTypes     []string
	Severities     []ActivitySeverity
	Visibilities   []ActivityVisibility
	Cursor         string
	Limit          int
	IncludeDetails bool
}

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

type ActivityFeedPage struct {
	Items      []ActivityProjection `json:"items"`
	NextCursor string               `json:"nextCursor,omitempty"`
	HasMore    bool                 `json:"hasMore"`
}

type activityFeedCursor struct {
	CreatedAt time.Time `json:"createdAt"`
	ID        string    `json:"id"`
}

// ListActivityFeed returns a stable newest-first projection. Payload and
// correlation detail are omitted unless explicitly requested.
func (s *RunActivityService) ListActivityFeed(ctx context.Context, request ActivityFeedRequest) (*ActivityFeedPage, error) {
	if s == nil || s.activity == nil {
		return nil, errors.New("run activity service is not configured")
	}
	if err := request.Scope.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(request.RunID) == "" && strings.TrimSpace(request.AgentID) == "" &&
		strings.TrimSpace(request.ObjectiveID) == "" && strings.TrimSpace(request.TeamID) == "" {
		return nil, errors.New("an activity run, agent, objective, or team selector is required")
	}
	limit := request.Limit
	if limit <= 0 {
		limit = defaultActivityFeedLimit
	}
	if limit > maximumActivityFeedLimit {
		return nil, fmt.Errorf("activity feed limit cannot exceed %d", maximumActivityFeedLimit)
	}
	var before *time.Time
	beforeID := ""
	if strings.TrimSpace(request.Cursor) != "" {
		cursor, err := decodeActivityFeedCursor(request.Cursor)
		if err != nil {
			return nil, err
		}
		before = &cursor.CreatedAt
		beforeID = cursor.ID
	}
	events, err := s.activity.ListActivity(ctx, ActivityFilter{
		Scope: request.Scope, RunID: request.RunID, AgentID: request.AgentID, ObjectiveID: request.ObjectiveID,
		TeamID: request.TeamID, EventTypes: request.EventTypes, Severities: request.Severities,
		Visibilities: request.Visibilities, BeforeCreatedAt: before, BeforeID: beforeID,
		Descending: true, Limit: limit + 1,
	})
	if err != nil {
		return nil, err
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	items := make([]ActivityProjection, 0, len(events))
	for _, event := range events {
		items = append(items, projectActivityEvent(event, request.IncludeDetails))
	}
	page := &ActivityFeedPage{Items: items, HasMore: hasMore}
	if hasMore && len(events) > 0 {
		page.NextCursor, err = encodeActivityFeedCursor(events[len(events)-1])
		if err != nil {
			return nil, err
		}
	}
	return page, nil
}

func projectActivityEvent(event *ActivityEvent, includeDetails bool) ActivityProjection {
	projection := ActivityProjection{
		ID: event.ID, Sequence: event.Sequence, EventType: event.EventType, Category: activityCategory(event.EventType),
		Severity: event.Severity, Visibility: event.Visibility, AgentID: event.AgentID, ObjectiveID: event.ObjectiveID,
		RunID: event.RunID, TurnID: event.TurnID, ParentRunID: event.ParentRunID, TeamID: event.TeamID,
		Actor: event.Actor, Summary: event.Summary, UsageDelta: cloneBudgetUsage(event.UsageDelta), CreatedAt: event.CreatedAt,
		DetailAvailable: len(event.Payload) > 0 || len(event.ConversationRefs) > 0 || event.CorrelationID != "" || event.CausationID != "",
	}
	if includeDetails {
		projection.Payload = maps.Clone(event.Payload)
		projection.ConversationRefs = append([]string(nil), event.ConversationRefs...)
		projection.CorrelationID = event.CorrelationID
		projection.CausationID = event.CausationID
	}
	projection.AnswerPreview = projectTurnAnswerPreview(event)
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

func encodeActivityFeedCursor(event *ActivityEvent) (string, error) {
	encoded, err := json.Marshal(activityFeedCursor{CreatedAt: event.CreatedAt.UTC(), ID: event.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(encoded), nil
}

func decodeActivityFeedCursor(value string) (*activityFeedCursor, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil {
		return nil, errors.New("invalid activity feed cursor")
	}
	var cursor activityFeedCursor
	if err := json.Unmarshal(decoded, &cursor); err != nil || cursor.CreatedAt.IsZero() || strings.TrimSpace(cursor.ID) == "" {
		return nil, errors.New("invalid activity feed cursor")
	}
	return &cursor, nil
}
