package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

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
	Schedule  *runbook.Schedule       `json:"schedule,omitempty"`
	NextRunAt *time.Time              `json:"nextRunAt,omitempty"`
	LastRun   *ConversationRunbookRun `json:"lastRun,omitempty"`
	// Revision is the activation's revision.
	Revision int64 `json:"revision"`
}

// ConversationRunbookRun is the newest Run a routine started.
type ConversationRunbookRun struct {
	ID          string         `json:"id"`
	Status      AgentRunStatus `json:"status"`
	CreatedAt   time.Time      `json:"createdAt"`
	CompletedAt *time.Time     `json:"completedAt,omitempty"`
}

// listConversationRunbooks projects the routines linked to the conversation
// (Conversation.RunbookActivationIDs, recorded when a routine visual is
// stored) and the scheduled tasks created from it (Task.ConversationID) that
// are still active or paused. Linked routines that no longer exist are left
// out. Every viewer of the conversation's change stream sees them; the
// routine endpoints authorize any command.
func (s *ConversationChangeService) listConversationRunbooks(ctx context.Context, conversation *Conversation) ([]*ConversationRunbook, error) {
	if s.runbooks == nil || conversation == nil {
		return []*ConversationRunbook{}, nil
	}
	activations := make([]*RunbookActivation, 0, len(conversation.RunbookActivationIDs))
	seen := map[string]bool{}
	for _, id := range conversation.RunbookActivationIDs {
		activation, err := s.runbooks.GetRunbookActivation(ctx, conversation.Scope, id)
		if errors.Is(err, ErrRunbookActivationNotFound) || err == nil && activation == nil {
			continue
		}
		if err != nil {
			return nil, err
		}
		seen[activation.ID] = true
		activations = append(activations, activation)
	}
	owner := conversation.Owner
	scheduled, err := s.runbooks.ListRunbookActivations(ctx, RunbookActivationFilter{
		Scope: conversation.Scope, Owner: &owner, TriggerKinds: []runbook.TriggerKind{runbook.TriggerSchedule},
		Statuses: []RunbookActivationStatus{RunbookActivationActive, RunbookActivationPaused},
	})
	if err != nil {
		return nil, err
	}
	for _, activation := range scheduled {
		if activation != nil && activation.Task != nil && activation.Task.ConversationID == conversation.ID && !seen[activation.ID] {
			seen[activation.ID] = true
			activations = append(activations, activation)
		}
	}
	sort.SliceStable(activations, func(i, j int) bool { return activations[i].ID < activations[j].ID })
	result := make([]*ConversationRunbook, 0, len(activations))
	for _, activation := range activations {
		projected, err := s.projectConversationRunbook(ctx, activation)
		if err != nil {
			return nil, err
		}
		result = append(result, projected)
	}
	return result, nil
}

func (s *ConversationChangeService) projectConversationRunbook(ctx context.Context, activation *RunbookActivation) (*ConversationRunbook, error) {
	projected := &ConversationRunbook{ID: activation.ID, ObjectiveID: activation.ObjectiveID, Revision: activation.Revision, Status: ConversationRunbookActive}
	if activation.Trigger.Schedule != nil {
		schedule := *activation.Trigger.Schedule
		projected.Schedule = &schedule
	}
	objective, err := s.portfolio.GetObjective(ctx, activation.Scope, activation.ObjectiveID)
	if err != nil && !errors.Is(err, ErrObjectiveNotFound) {
		return nil, err
	}
	switch {
	case activation.Status == RunbookActivationRetired:
		projected.Status = ConversationRunbookStopped
	case objective != nil && objective.Status != ObjectiveStatusActive && objective.Status != ObjectiveStatusDraft && objective.Status != ObjectiveStatusPaused:
		projected.Status = ConversationRunbookStopped
	case activation.Status == RunbookActivationPaused || objective != nil && objective.Status == ObjectiveStatusPaused:
		projected.Status = ConversationRunbookPaused
	}
	if objective != nil {
		projected.Title = strings.TrimSpace(objective.Title)
	}
	if projected.Title == "" && activation.Task != nil {
		projected.Title = strings.TrimSpace(activation.Task.Goal)
		if runes := []rune(projected.Title); len(runes) > 80 {
			projected.Title = strings.TrimSpace(string(runes[:80])) + "…"
		}
	}
	if projected.Status == ConversationRunbookActive && activation.NextRunAt != nil {
		next := *activation.NextRunAt
		projected.NextRunAt = &next
	}
	runs, err := s.portfolio.ListAgentRuns(ctx, AgentRunFilter{
		Scope: activation.Scope, ObjectiveID: activation.ObjectiveID, Order: AgentRunOrderCreatedDesc, Limit: conversationRunbookRunLookback,
	})
	if err != nil {
		return nil, err
	}
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
	return hashBytes(encoded), nil
}
