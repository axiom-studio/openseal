package runtime

import (
	"context"
	"encoding/json"
)

// Summary streams expose only the typed public preview, never arbitrary payload
// fields. This also reconstructs snapshots after a database JSON round-trip.
func projectTurnAnswerPreview(event *ActivityEvent) *TurnAnswerPreview {
	if event.EventType != "turn.answer_preview" || event.Visibility == ActivityVisibilityPrivate {
		return nil
	}
	data, err := json.Marshal(map[string]interface{}{
		"attemptId": event.Payload["attemptId"], "sequence": event.Payload["sequence"],
		"text": event.Payload["text"], "reset": event.Payload["reset"],
	})
	if err != nil {
		return nil
	}
	var preview TurnAnswerPreview
	if json.Unmarshal(data, &preview) != nil || preview.Validate() != nil {
		return nil
	}
	return &preview
}

// Preview rows use the same tenant-scoped activity repository as turn progress.
// The model never supplies scope, run, turn, owner or worker identity.
func (c *TurnCoordinator) observeTurnAnswer(ctx context.Context, scope Scope, run *AgentRun, turn *AgentTurn, workerID string) context.Context {
	return WithTurnAnswerPreview(ctx, func(previewCtx context.Context, preview TurnAnswerPreview) error {
		current, err := c.turns.turns.GetAgentTurn(previewCtx, scope, turn.ID)
		if err != nil {
			return err
		}
		if current == nil || current.Status != AgentTurnStatusRunning || current.LeaseOwner != workerID || current.LeaseExpiresAt == nil || !current.LeaseExpiresAt.After(c.activity.now()) {
			return ErrTurnLeaseHeld
		}
		active, err := c.portfolio.GetAgentRun(previewCtx, scope, run.ID)
		if err != nil {
			return err
		}
		if active == nil || active.Status != AgentRunStatusRunning || active.LeaseOwner != "" && active.LeaseOwner != workerID {
			return ErrLeaseLost
		}
		_, err = c.activity.AppendActivity(previewCtx, &ActivityEvent{
			Scope: scope, RunID: run.ID, TurnID: turn.ID, AgentID: run.Owner.ID,
			EventType: "turn.answer_preview", Summary: "Reply preview updated",
			Actor: ActivityActor{Type: "worker", ID: workerID}, Visibility: ActivityVisibilityScope,
			Payload: map[string]interface{}{"attemptId": preview.AttemptID, "sequence": preview.Sequence, "text": preview.Text, "reset": preview.Reset},
		})
		return err
	})
}
