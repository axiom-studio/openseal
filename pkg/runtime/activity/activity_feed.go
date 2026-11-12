package activity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
)

const (
	defaultActivityFeedLimit = 25
	maximumActivityFeedLimit = 100
)

// FeedRequest selects one portable activity projection. At least one
// resource selector is required so callers cannot accidentally request an
// unbounded scope-wide audit dump.
type FeedRequest struct {
	Scope          models.Scope
	RunID          string
	AgentID        string
	ObjectiveID    string
	TeamID         string
	EventTypes     []string
	Severities     []models.ActivitySeverity
	Visibilities   []models.ActivityVisibility
	Cursor         string
	Limit          int
	IncludeDetails bool
}

type FeedPage struct {
	Items      []models.ActivityProjection `json:"items"`
	NextCursor string                      `json:"nextCursor,omitempty"`
	HasMore    bool                        `json:"hasMore"`
}

// ListActivityFeed returns a stable newest-first projection. Payload and
// correlation detail are omitted unless explicitly requested.
func (s *RunActivityService) ListActivityFeed(ctx context.Context, request FeedRequest) (*FeedPage, error) {
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
		cursor, err := models.DecodeActivityFeedCursor(request.Cursor)
		if err != nil {
			return nil, err
		}
		before = &cursor.CreatedAt
		beforeID = cursor.ID
	}
	events, err := s.activity.ListActivity(ctx, models.ActivityFilter{
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
	items := make([]models.ActivityProjection, 0, len(events))
	for _, event := range events {
		items = append(items, models.ProjectActivityEvent(event, request.IncludeDetails))
	}
	page := &FeedPage{Items: items, HasMore: hasMore}
	if hasMore && len(events) > 0 {
		page.NextCursor, err = models.EncodeActivityFeedCursor(events[len(events)-1])
		if err != nil {
			return nil, err
		}
	}
	return page, nil
}
