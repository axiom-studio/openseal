package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runbook"
)

// ConversationRunbookStatus is a routine's state as a routine card shows it.
type ConversationRunbookStatus string

const (
	ConversationRunbookActive  ConversationRunbookStatus = "active"
	ConversationRunbookPaused  ConversationRunbookStatus = "paused"
	ConversationRunbookStopped ConversationRunbookStatus = "stopped"
)

// conversationRunbookRunLookback bounds the Objective's newest Runs read to
// find a routine's last Run.
const conversationRunbookRunLookback = 20

// ConversationRunbook is the live state of one routine (a runbook
// activation) shown in a conversation: the fields a routine card overlays.
type ConversationRunbook struct {
	ID          string `json:"id"`
	ObjectiveID string `json:"objectiveId"`
	// Title is the routine's Objective title.
	Title string `json:"title"`
	// Status is stopped once the activation or its Objective is retired or
	// finished, paused when either is paused, and active otherwise.
	Status ConversationRunbookStatus `json:"status"`
	// Schedule is the cron schedule of a scheduled routine.
	Schedule *runbook.Schedule `json:"schedule,omitempty"`
	// Trigger says what starts the routine.
	Trigger *ConversationRunbookTrigger `json:"trigger,omitempty"`
	// NextRunAt is the next scheduled run of a scheduled routine, or for an
	// event routine at its hourly limit, when its waiting events run.
	NextRunAt *time.Time              `json:"nextRunAt,omitempty"`
	LastRun   *ConversationRunbookRun `json:"lastRun,omitempty"`
	// LastFiredAt, FiredLast24h, FiredLastHour and Pending count an event
	// routine's started runs and waiting events (test events not counted
	// as runs).
	LastFiredAt   *time.Time `json:"lastFiredAt,omitempty"`
	FiredLast24h  int        `json:"firedLast24h,omitempty"`
	FiredLastHour int        `json:"firedLastHour,omitempty"`
	Pending       int        `json:"pending,omitempty"`
	// PausedReason says why the system paused the routine.
	PausedReason models.RoutinePausedReason `json:"pausedReason,omitempty"`
	// Revision is the activation's revision.
	Revision int64 `json:"revision"`
	// Script is the chat script a script routine runs.
	Script *ConversationRunbookScript `json:"script,omitempty"`
}

// ConversationRunbookTrigger is what starts a routine. When is set only for
// event routines; the client words schedules itself.
type ConversationRunbookTrigger struct {
	Kind        runbook.TriggerKind     `json:"kind"`
	EventKind   models.RoutineEventKind `json:"eventKind,omitempty"`
	When        string                  `json:"when,omitempty"`
	SourceLabel string                  `json:"sourceLabel,omitempty"`
	MaxPerHour  int                     `json:"maxPerHour,omitempty"`
}

// ConversationRunbookScript names the chat file a script routine runs.
type ConversationRunbookScript struct {
	Path string `json:"path"`
}

// ConversationRunbookRun is the newest Run a routine started.
type ConversationRunbookRun struct {
	ID          string                `json:"id"`
	Status      models.AgentRunStatus `json:"status"`
	CreatedAt   time.Time             `json:"createdAt"`
	CompletedAt *time.Time            `json:"completedAt,omitempty"`
}

// listConversationRunbooks projects the routines linked to the conversation
// (Conversation.RunbookActivationIDs, recorded when a routine visual is
// stored) and the scheduled and event tasks created from it
// (Task.ConversationID) that are still active or paused. Linked routines that no longer exist are left
// out. Every viewer of the conversation's change stream sees them; the
// routine endpoints authorize any command.
func (s *ConversationChangeService) listConversationRunbooks(ctx context.Context, conversation *models.Conversation) ([]*ConversationRunbook, error) {
	if s.runbooks == nil || conversation == nil {
		return []*ConversationRunbook{}, nil
	}
	activations := make([]*models.RunbookActivation, 0, len(conversation.RunbookActivationIDs))
	seen := map[string]bool{}
	for _, id := range conversation.RunbookActivationIDs {
		activation, err := s.runbooks.GetRunbookActivation(ctx, conversation.Scope, id)
		if errors.Is(err, models.ErrRunbookActivationNotFound) || err == nil && activation == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		seen[activation.ID] = true
		activations = append(activations, activation)
	}
	owner := conversation.Owner
	tasks, err := s.runbooks.ListRunbookActivations(ctx, models.RunbookActivationFilter{
		Scope: conversation.Scope, Owner: &owner, TriggerKinds: []runbook.TriggerKind{runbook.TriggerSchedule, runbook.TriggerEvent},
		Statuses: []models.RunbookActivationStatus{models.RunbookActivationActive, models.RunbookActivationPaused},
	})
	if err != nil {
		return nil, err
	}
	for _, activation := range tasks {
		if activation != nil && activation.Task != nil && activation.Task.ConversationID == conversation.ID && !seen[activation.ID] {
			seen[activation.ID] = true
			activations = append(activations, activation)
		}
	}
	sort.SliceStable(activations, func(i, j int) bool { return activations[i].ID < activations[j].ID })
	activity, err := s.conversationRoutineEventActivity(ctx, conversation.Scope, activations)
	if err != nil {
		return nil, err
	}
	result := make([]*ConversationRunbook, 0, len(activations))
	for _, activation := range activations {
		var stats *models.RoutineEventActivity
		if value, ok := activity[activation.ID]; ok {
			stats = &value
		}
		projected, err := s.projectConversationRunbook(ctx, activation, stats)
		if err != nil {
			return nil, err
		}
		result = append(result, projected)
	}
	return result, nil
}

// conversationRoutineEventActivity reads the run counts of the event
// routines among activations in one store call; none when there are no event
// routines or the store keeps no routine events.
func (s *ConversationChangeService) conversationRoutineEventActivity(ctx context.Context, scope models.Scope, activations []*models.RunbookActivation) (map[string]models.RoutineEventActivity, error) {
	events, ok := s.portfolio.(models.RoutineEventStore)
	if !ok {
		return nil, nil
	}
	ids := make([]string, 0, len(activations))
	for _, activation := range activations {
		if activation.Task != nil && activation.Task.Event != nil {
			ids = append(ids, activation.ID)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	return events.RoutineEventActivity(ctx, scope, ids, s.now())
}

func (s *ConversationChangeService) projectConversationRunbook(ctx context.Context, activation *models.RunbookActivation, activity *models.RoutineEventActivity) (*ConversationRunbook, error) {
	projected := &ConversationRunbook{
		ID: activation.ID, ObjectiveID: activation.ObjectiveID, Revision: activation.Revision, Status: ConversationRunbookActive,
		Trigger: &ConversationRunbookTrigger{Kind: activation.Trigger.Kind}, PausedReason: activation.PausedReason,
	}
	if activation.Trigger.Schedule != nil {
		schedule := *activation.Trigger.Schedule
		projected.Schedule = &schedule
	}
	var event *models.RoutineEventTrigger
	if activation.Task != nil {
		event = activation.Task.Event
	}
	if event != nil {
		projected.Trigger.EventKind = event.Kind
		projected.Trigger.When = models.DescribeRoutineEventTrigger(event)
		projected.Trigger.SourceLabel = strings.TrimSpace(event.SourceLabel)
		projected.Trigger.MaxPerHour = event.MaxPerHour
	}
	if activity != nil {
		if activity.LastFiredAt != nil {
			fired := *activity.LastFiredAt
			projected.LastFiredAt = &fired
		}
		projected.FiredLast24h, projected.FiredLastHour, projected.Pending = activity.FiredLast24h, activity.FiredLastHour, activity.Pending
	}
	objective, err := s.portfolio.GetObjective(ctx, activation.Scope, activation.ObjectiveID)
	if err != nil && !errors.Is(err, models.ErrObjectiveNotFound) {
		return nil, err
	}
	switch {
	case activation.Status == models.RunbookActivationRetired:
		projected.Status = ConversationRunbookStopped
	case objective != nil && objective.Status != models.ObjectiveStatusActive && objective.Status != models.ObjectiveStatusDraft && objective.Status != models.ObjectiveStatusPaused:
		projected.Status = ConversationRunbookStopped
	case activation.Status == models.RunbookActivationPaused || objective != nil && objective.Status == models.ObjectiveStatusPaused:
		projected.Status = ConversationRunbookPaused
	}
	if objective != nil {
		projected.Title = strings.TrimSpace(objective.Title)
	}
	if activation.Task != nil && activation.Task.Script != nil {
		projected.Script = &ConversationRunbookScript{Path: activation.Task.Script.Path}
	}
	if projected.Title == "" && activation.Task != nil {
		projected.Title = strings.TrimSpace(activation.Task.Goal)
		if runes := []rune(projected.Title); len(runes) > 80 {
			projected.Title = strings.TrimSpace(string(runes[:80])) + "…"
		}
	}
	next := activation.NextRunAt
	if event != nil && activity != nil {
		next = activity.NextRunAt
	}
	if projected.Status == ConversationRunbookActive && next != nil {
		value := *next
		projected.NextRunAt = &value
	}
	runs, err := s.portfolio.ListAgentRuns(ctx, models.AgentRunFilter{
		Scope: activation.Scope, ObjectiveID: activation.ObjectiveID, Order: models.AgentRunOrderCreatedDesc, Limit: conversationRunbookRunLookback,
	})
	if err != nil {
		return nil, err
	}
	// Scheduled, on-demand and event (Source event) Runs all carry the
	// activation ID.
	for _, run := range runs {
		if run == nil {
			continue
		}
		if id, _ := run.Context["runbookActivationId"].(string); strings.TrimSpace(id) == activation.ID {
			last := &ConversationRunbookRun{ID: run.ID, Status: run.Status, CreatedAt: run.CreatedAt}
			if run.CompletedAt != nil {
				completed := *run.CompletedAt
				last.CompletedAt = &completed
			}
			projected.LastRun = last
			break
		}
	}
	return projected, nil
}

func conversationRunbookProjectionDigest(runbooks []*ConversationRunbook) (string, error) {
	encoded, err := json.Marshal(runbooks)
	if err != nil {
		return "", err
	}
	return models.SHA256Digest(encoded), nil
}
