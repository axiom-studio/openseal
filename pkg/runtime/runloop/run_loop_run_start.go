package runloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
)

// The Run-start preparer (SPEC §2.4 Preparers, first in the list). It turns
// kernel state the model must see, but that does not belong in the cached
// system prompt, into `note` entries of the step's context append:
//
//   - rate_limited_source: the Run's newest Lightpanda failure is a typed
//     source rate limit, so live-browser calls will be refused (section 07
//     §8.1). One note per failing call; the tool list never changes.
//   - setup_pending: a foreground Run whose governed history holds a pending
//     setup form (section 07 §8.3). Same id and source key as the note the
//     governed executor appends after the tool entry (pendingSkillSetupNote),
//     so it only fills in when that append did not happen (for example a
//     setup found while resuming open tools).
//
// Every note has a deterministic id (SessionEntryID of its source key) and is
// skipped when the session already holds it, so the preparer may run before
// every step.

// RateLimitedSourceKey dedupes the rate-limited source note: one per failing
// Lightpanda call.
func RateLimitedSourceKey(runID, actionCallID string) string {
	return "rate-limited-source:" + runID + ":" + actionCallID
}

// RunStartPreparer is the StepPreparer the RunLoop runs first. Actions may be
// nil (no governed tools); then it produces no notes.
type RunStartPreparer struct {
	sessions models.AgentSessionStore
	actions  models.ActionStore
}

func NewRunStartPreparer(sessions models.AgentSessionStore, actions models.ActionStore) *RunStartPreparer {
	return &RunStartPreparer{sessions: sessions, actions: actions}
}

var _ agentrun.StepPreparer = (*RunStartPreparer)(nil)

func (p *RunStartPreparer) BeforeStep(ctx context.Context, in agentrun.StepInput, _ agentrun.SteeringPlan) (*agentrun.StepContextAppend, error) {
	run, session, step := in.Run, in.Session, in.Step
	if p == nil || run == nil || session == nil || step == nil {
		return nil, nil
	}
	var notes []*models.SessionEntry
	if p.actions != nil && newGovernedEvidence(in.Path, run.ID) {
		views, err := control.LoadGovernedToolViews(ctx, p.actions, run.Scope, in.Path, run.ID)
		if err != nil {
			return nil, fmt.Errorf("run start: load governed history: %w", err)
		}
		if note := rateLimitedSourceNote(run, session.ID, step, views); note != nil {
			notes = append(notes, note)
		}
		if note := runStartSetupPendingNote(run, session.ID, step, views); note != nil {
			notes = append(notes, note)
		}
	}
	fresh := notes[:0]
	for _, note := range notes {
		present, err := p.holds(ctx, run.Scope, in.Path, note.ID)
		if err != nil {
			return nil, fmt.Errorf("run start: %w", err)
		}
		if !present {
			note.Tokens = modelstep.EstimateEntryTokens(note)
			fresh = append(fresh, note)
		}
	}
	if len(fresh) == 0 {
		return nil, nil
	}
	return &agentrun.StepContextAppend{Entries: fresh}, nil
}

// holds reports whether the session already has the entry: on the loaded
// path, or (compacted away or not loaded) in the store.
func (p *RunStartPreparer) holds(ctx context.Context, scope models.Scope, path *models.AgentSessionPath, entryID string) (bool, error) {
	if path != nil {
		for _, entry := range path.Entries {
			if entry != nil && entry.ID == entryID {
				return true, nil
			}
		}
	}
	if p.sessions == nil {
		return false, errors.New("no agent session store")
	}
	_, err := p.sessions.GetSessionEntry(ctx, scope, entryID)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, models.ErrSessionEntryNotFound):
		return false, nil
	default:
		return false, err
	}
}

func runStartNote(run *models.AgentRun, sessionID string, step *models.AgentTurn, key string, kind models.NoteKind, text string, data interface{}) *models.SessionEntry {
	encoded, _ := json.Marshal(data)
	return &models.SessionEntry{
		ID: models.SessionEntryID(sessionID, key), Scope: run.Scope, SessionID: sessionID,
		RunID: run.ID, StepID: step.ID, Step: step.Sequence, Type: models.SessionEntryNote, SourceKey: key,
		Note: &models.NoteEntry{Kind: kind, Text: text, Data: encoded},
	}
}

// newGovernedEvidence: the path holds a finished governed entry of this Run at
// or after its newest assistant entry, so the governed history may have
// changed since the previous step. It keeps the action-call read off steps
// that ran no governed call.
func newGovernedEvidence(path *models.AgentSessionPath, runID string) bool {
	if path == nil {
		return false
	}
	for index := len(path.Entries) - 1; index >= 0; index-- {
		entry := path.Entries[index]
		if entry == nil || entry.RunID != runID {
			continue
		}
		if entry.Type == models.SessionEntryAssistant {
			return false
		}
		if entry.Type == models.SessionEntryTool && entry.ActionCallID != "" &&
			(entry.State == models.ToolEntryCompleted || entry.State == models.ToolEntryError) {
			return true
		}
	}
	return false
}

// rateLimitedSourceNote follows browserSourceRateLimited: the newest Lightpanda
// call decides; a success clears, a typed source_* failure sets or clears.
func rateLimitedSourceNote(run *models.AgentRun, sessionID string, step *models.AgentTurn, views []control.GovernedToolView) *models.SessionEntry {
	for index := len(views) - 1; index >= 0; index-- {
		view := views[index]
		if view.SkillID != control.LightpandaBrowserSkillID {
			continue
		}
		if view.Status == string(models.ActionCallStatusSucceeded) {
			return nil
		}
		limited, relevant := control.TypedSourceFailure(view)
		if !relevant {
			continue
		}
		if !limited || view.ActionCallID == "" {
			return nil
		}
		return runStartNote(run, sessionID, step, RateLimitedSourceKey(run.ID, view.ActionCallID), models.NoteRateLimitedSource,
			control.LiveBrowserRateLimitedError, map[string]string{"actionCallId": view.ActionCallID, "code": control.LiveBrowserRateLimitedCode})
	}
	return nil
}

// runStartSetupPendingNote follows pendingSkillSetupID and builds the same
// entry as pendingSkillSetupNote (id, source key, text, data), for foreground
// Runs only.
func runStartSetupPendingNote(run *models.AgentRun, sessionID string, step *models.AgentTurn, views []control.GovernedToolView) *models.SessionEntry {
	if control.IsBackgroundTaskRun(run) {
		return nil
	}
	for index := len(views) - 1; index >= 0; index-- {
		view := views[index]
		if view.SkillID != models.SkillManagementSkillID || view.Status != string(models.ActionCallStatusSucceeded) ||
			view.Action != agentrun.SkillActionRequestSetup && view.Action != skillbinding.SkillActionListSetupRequests {
			continue
		}
		id := control.PendingSetupRequestID(run, view, run.PendingInterventions)
		if id == "" || view.ActionCallID == "" {
			return nil
		}
		return runStartNote(run, sessionID, step, control.SkillSetupPendingSourceKey(run.ID, id), models.NoteSetupPending,
			control.SkillSetupPendingNote, map[string]string{"requestId": id, "actionCallId": view.ActionCallID})
	}
	return nil
}
