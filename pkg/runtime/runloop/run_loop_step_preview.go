package runloop

import (
	"context"
	"strings"
	"sync"

	"github.com/axiom-studio/openseal/pkg/models"
)

// observeStepProgress records host and tool progress as turn.progress /
// turn.commentary activity of the step, deduplicating repeats.
func (d *drive) observeStepProgress(ctx context.Context, st *stepState) context.Context {
	var mu sync.Mutex
	lastSummary, lastKind, lastOrigin := "", "", ""
	run := models.CloneAgentRun(d.run)
	stepID := st.step.ID
	return models.WithTurnProgress(ctx, func(progressCtx context.Context, summary string) error {
		kind := "turn.progress"
		if models.IsTurnCommentary(progressCtx) {
			kind = "turn.commentary"
		}
		origin := models.TurnProgressOrigin(progressCtx)
		mu.Lock()
		defer mu.Unlock()
		if summary == lastSummary && kind == lastKind && origin == lastOrigin {
			return nil
		}
		if err := d.stepFenced(progressCtx, run.Scope, run.ID, stepID); err != nil {
			return err
		}
		event := &models.ActivityEvent{
			Scope: run.Scope, RunID: run.ID, TurnID: stepID, AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: models.TeamIDForRun(run),
			EventType: kind, Summary: summary, Actor: d.actor(), Visibility: models.ActivityVisibilityScope,
		}
		if origin != "" {
			event.Payload = map[string]interface{}{"originRef": origin}
		}
		if _, err := d.l.activity.AppendActivity(progressCtx, event); err != nil {
			return err
		}
		lastSummary, lastKind, lastOrigin = summary, kind, origin
		return nil
	})
}

// previewTracker remembers the last answer-preview frame of the step, so a
// step that ends in tool calls can reset it (D11).
type previewTracker struct {
	mu        sync.Mutex
	attemptID string
	sequence  uint64
}

func (p *previewTracker) seen(preview models.TurnAnswerPreview) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if preview.AttemptID != p.attemptID || preview.Sequence > p.sequence {
		p.attemptID, p.sequence = preview.AttemptID, preview.Sequence
	}
}

func (p *previewTracker) last() (string, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attemptID, p.sequence
}

func (d *drive) observeStepPreview(ctx context.Context, st *stepState) context.Context {
	run := models.CloneAgentRun(d.run)
	stepID := st.step.ID
	tracker := &st.preview
	return models.WithTurnAnswerPreview(ctx, func(previewCtx context.Context, preview models.TurnAnswerPreview) error {
		if err := d.stepFenced(previewCtx, run.Scope, run.ID, stepID); err != nil {
			return err
		}
		if err := d.appendPreview(previewCtx, run, stepID, preview); err != nil {
			return err
		}
		tracker.seen(preview)
		return nil
	})
}

func (d *drive) appendPreview(ctx context.Context, run *models.AgentRun, stepID string, preview models.TurnAnswerPreview) error {
	_, err := d.l.activity.AppendActivity(ctx, &models.ActivityEvent{
		Scope: run.Scope, RunID: run.ID, TurnID: stepID, AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: models.TeamIDForRun(run),
		EventType: "turn.answer_preview", Summary: "Reply preview updated", Actor: d.actor(), Visibility: models.ActivityVisibilityScope,
		Payload: map[string]interface{}{"attemptId": preview.AttemptID, "sequence": preview.Sequence, "text": preview.Text, "reset": preview.Reset},
	})
	return err
}

// resetPreview clears the answer preview of a step that ended in tool calls
// (same attempt id, next sequence). A lost reset is not retried: the next
// preview or the reply replaces it.
func (d *drive) resetPreview(ctx context.Context, st *stepState) {
	attemptID, sequence := st.preview.last()
	if attemptID == "" {
		return
	}
	reset := models.TurnAnswerPreview{AttemptID: attemptID, Sequence: sequence + 1, Reset: true}
	if err := d.appendPreview(ctx, d.run, st.step.ID, reset); err != nil {
		d.l.logger.Warnw("reset answer preview", "run", d.run.ID, "step", st.step.ID, "error", err)
		return
	}
	st.preview.seen(reset)
}
