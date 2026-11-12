package runtime

import (
	"context"
	"errors"
	"fmt"
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

type ActivityFeedPage struct {
	Items      []ActivityProjection `json:"items"`
	NextCursor string               `json:"nextCursor,omitempty"`
	HasMore    bool                 `json:"hasMore"`
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
