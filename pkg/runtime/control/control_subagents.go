package control

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentbuilder"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Subagents of a Run (start_subagents and its follow-up tools). Each
// subagent has a short id, stable within the Run that started it (s1, s2,
// ...): the model never sees a Run id. The ids live in the kernel meta of
// the tool entries that created their Runs (EntryMeta.Subagents); a
// follow-up to a finished subagent is a new Run under the same id, and the
// newest Run of an id is the subagent. Their ends reach the parent either
// as a tool result (EntryMeta.Reported) or, for background subagents, as a
// subagent note at the parent's next step.

const (
	// maximumRunningSubagents bounds the subagents of one Run that work at
	// the same time.
	maximumRunningSubagents = 8
	subagentIDPrefix        = "s"
)

// SubagentRef maps a subagent id to the Run that works under it.
type SubagentRef struct {
	ID         string `json:"id"`
	RunID      string `json:"runId"`
	Background bool   `json:"background,omitempty"`
}

// subagentEntryTools are the tools whose entries carry subagent meta.
var subagentEntryTools = []string{
	string(models.ControlToolStartSubagents), string(models.ControlToolWaitSubagents),
	string(models.ControlToolMessageSubagent), string(models.ControlToolStopSubagents),
}

// subagentFollowUpTools are surfaced by the first start_subagents call that
// returns, so a step of a Run without subagents keeps its tool list.
var subagentFollowUpTools = []models.ControlToolName{
	models.ControlToolWaitSubagents, models.ControlToolReadSubagent,
	models.ControlToolMessageSubagent, models.ControlToolStopSubagents,
}

// Subagents is what a Run knows about its subagents.
type Subagents struct {
	order    []string               // ids, first creation first
	current  map[string]SubagentRef // id -> its newest Run
	runs     map[string]*models.AgentRun
	reported map[string]bool // Run ids whose end is already in the parent's context
	numbered int             // highest id number in use
}

// LoadSubagents reads run's subagents. skipEntryID leaves out the entry being
// settled, whose ids are not committed yet.
func LoadSubagents(ctx context.Context, sessions models.AgentSessionStore, runs models.PortfolioStore, run *models.AgentRun, skipEntryID string) (*Subagents, error) {
	s := &Subagents{current: map[string]SubagentRef{}, runs: map[string]*models.AgentRun{}, reported: map[string]bool{}}
	entries, err := sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, ToolNames: subagentEntryTools, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
	for _, entry := range entries {
		if entry.ID == skipEntryID {
			continue
		}
		meta, err := DecodeControlEntryMeta(entry)
		if err != nil {
			return nil, err
		}
		if meta == nil {
			continue
		}
		for _, ref := range meta.Subagents {
			if _, known := s.current[ref.ID]; !known {
				s.order = append(s.order, ref.ID)
			}
			s.current[ref.ID] = ref
			if n, ok := subagentNumber(ref.ID); ok && n > s.numbered {
				s.numbered = n
			}
		}
		if entry.State.Terminal() {
			for _, runID := range meta.Reported {
				s.reported[runID] = true
			}
		}
	}
	if len(s.order) == 0 {
		return s, nil
	}
	notes, err := sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, NoteKinds: []models.NoteKind{models.NoteSubagent}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	for _, note := range notes {
		var data subagentNoteData
		if note.Note == nil {
			continue
		}
		if err := json.Unmarshal(note.Note.Data, &data); err != nil {
			return nil, fmt.Errorf("decode subagent note %s: %w", note.ID, err)
		}
		s.reported[data.RunID] = true
	}
	for _, id := range s.order {
		ref := s.current[id]
		child, err := runs.GetAgentRun(ctx, run.Scope, ref.RunID)
		if err != nil {
			return nil, err
		}
		if child == nil || child.ParentRunID != run.ID {
			return nil, fmt.Errorf("subagent %s: run %s is not a child of %s", id, ref.RunID, run.ID)
		}
		s.runs[ref.RunID] = child
	}
	return s, nil
}

func subagentNumber(id string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(id, subagentIDPrefix))
	return n, err == nil && strings.HasPrefix(id, subagentIDPrefix) && n > 0
}

// Empty reports whether the Run has no subagent.
func (s *Subagents) Empty() bool { return s == nil || len(s.order) == 0 }

// nextIDs are the ids of n new subagents.
func (s *Subagents) nextIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = subagentIDPrefix + strconv.Itoa(s.numbered+i+1)
	}
	return ids
}

// lookup is the subagent id's newest Run.
func (s *Subagents) lookup(id string) (SubagentRef, *models.AgentRun, bool) {
	ref, ok := s.current[strings.TrimSpace(id)]
	if !ok {
		return SubagentRef{}, nil, false
	}
	return ref, s.runs[ref.RunID], true
}

// running is the ids still working, in creation order; background only
// keeps those whose end would come back as a note.
func (s *Subagents) running(background bool) []string {
	var ids []string
	for _, id := range s.order {
		ref, child, _ := s.lookup(id)
		if child != nil && !models.IsTerminalAgentRunStatus(child.Status) && (!background || ref.Background) {
			ids = append(ids, id)
		}
	}
	return ids
}

// unreported is the ids that ended without their end reaching the parent.
func (s *Subagents) unreported() []string {
	var ids []string
	for _, id := range s.order {
		ref, child, _ := s.lookup(id)
		if child != nil && models.IsTerminalAgentRunStatus(child.Status) && !s.reported[ref.RunID] {
			ids = append(ids, id)
		}
	}
	return ids
}

// unknown is the error result for an id the Run does not have.
func (s *Subagents) unknown(id string) *models.ToolResult {
	known := "none"
	if !s.Empty() {
		known = strings.Join(s.order, ", ")
	}
	return ErrorResult(models.ToolResultRefused, "subagent_unknown",
		fmt.Sprintf("There is no subagent %q; your subagents are: %s.", id, known), nil)
}

// subagentValue is one subagent in a tool result: its answer or failure,
// or "already reported" when its end is already in the parent's context.
func subagentValue(id string, child *models.AgentRun, source *models.AgentRun, reported bool) map[string]interface{} {
	value := map[string]interface{}{"id": id, "goal": truncateControlText(child.Goal, id), "status": subagentStatus(child)}
	if child.AssignedAgentID != source.AssignedAgentID {
		value["agentId"] = child.AssignedAgentID
	}
	if !models.IsTerminalAgentRunStatus(child.Status) {
		return value
	}
	if reported {
		value["result"] = "already reported"
		return value
	}
	if answer := agentbuilder.ChildRunAnswer(child); answer != "" {
		value["answer"] = truncateControlText(answer, id)
	}
	if message := strings.TrimSpace(child.Error); message != "" {
		value["error"] = truncateControlText(message, id)
	}
	return value
}

// subagentStatus is the model-facing status: running, completed, failed or
// stopped.
func subagentStatus(child *models.AgentRun) string {
	switch child.Status {
	case models.AgentRunStatusCompleted:
		return "completed"
	case models.AgentRunStatusFailed:
		return "failed"
	case models.AgentRunStatusCanceled:
		return "stopped"
	}
	return "running"
}

// subagentFollowUpSurface is the frozen definitions of the follow-up tools,
// surfaced by a start_subagents entry.
func (x *Executor) subagentFollowUpSurface(a agentrun.ControlAuthority) []models.SurfacedTool {
	registry := x.registry()
	var tools []models.SurfacedTool
	for _, name := range subagentFollowUpTools {
		h, ok := registry[name]
		if !ok {
			continue
		}
		spec := h.Spec(a)
		spec.Name = name
		tools = append(tools, modelstep.NewSurfacedTool("", spec.ModelStepTool()))
	}
	return tools
}

// ---------------------------------------------------------------------------
// Notes and the final-answer guard

// subagentNoteData is the kernel data of a subagent note.
type subagentNoteData struct {
	Subagent string `json:"subagent"`
	RunID    string `json:"runId"`
	Status   string `json:"status"`
}

// SubagentNoteKey dedupes the note of one ended subagent Run.
func SubagentNoteKey(runID string) string { return "subagent:" + runID }

// mayHaveSubagents is false when path shows that run started none: no
// subagent entry of the Run and no compaction that could hide one. It keeps
// the store reads off the steps of Runs without subagents.
func mayHaveSubagents(path *models.AgentSessionPath, runID string) bool {
	if path == nil || path.Compaction != nil {
		return true
	}
	for _, entry := range path.Entries {
		if entry != nil && entry.RunID == runID && entry.Type == models.SessionEntryTool && entry.Tool != nil &&
			(entry.Tool.Name == string(models.ControlToolStartSubagents) || entry.Tool.Name == string(models.ControlToolMessageSubagent)) {
			return true
		}
	}
	return false
}

// SubagentNotes are the notes the next step of run appends: one per
// background subagent that ended without its end reaching run yet. Each has
// a deterministic id (SessionEntryID of SubagentNoteKey).
func SubagentNotes(ctx context.Context, sessions models.AgentSessionStore, runs models.PortfolioStore, run *models.AgentRun, sessionID string, step *models.AgentTurn, path *models.AgentSessionPath) ([]*models.SessionEntry, error) {
	if run == nil || step == nil || sessionID == "" || !mayHaveSubagents(path, run.ID) {
		return nil, nil
	}
	subagents, err := LoadSubagents(ctx, sessions, runs, run, "")
	if err != nil {
		return nil, fmt.Errorf("subagent notes: %w", err)
	}
	var notes []*models.SessionEntry
	for _, id := range subagents.unreported() {
		ref, child, _ := subagents.lookup(id)
		if !ref.Background {
			continue
		}
		data, err := json.Marshal(subagentNoteData{Subagent: id, RunID: child.ID, Status: subagentStatus(child)})
		if err != nil {
			return nil, err
		}
		key := SubagentNoteKey(child.ID)
		notes = append(notes, &models.SessionEntry{
			ID: models.SessionEntryID(sessionID, key), Scope: run.Scope, SessionID: sessionID, RunID: run.ID,
			StepID: step.ID, Step: step.Sequence, Type: models.SessionEntryNote, SourceKey: key,
			Note: &models.NoteEntry{Kind: models.NoteSubagent, Text: subagentNoteText(id, child), Data: data},
		})
	}
	return notes, nil
}

func subagentNoteText(id string, child *models.AgentRun) string {
	switch child.Status {
	case models.AgentRunStatusCompleted:
		if answer := agentbuilder.ChildRunAnswer(child); answer != "" {
			return "Subagent " + id + " finished: " + truncateControlText(answer, id)
		}
		return "Subagent " + id + " finished without an answer."
	case models.AgentRunStatusFailed:
		reason := strings.TrimSpace(child.Error)
		if reason == "" {
			reason = "no reason was given"
		}
		return "Subagent " + id + " failed: " + truncateControlText(reason, id)
	}
	return "Subagent " + id + " was stopped."
}

const subagentsRunningCode = "subagents_running"

// subagentsRunningRejection refuses a final answer while background
// subagents still work: their results may change it, and they would be
// orphaned.
func (x *Executor) subagentsRunningRejection(ctx context.Context, cc agentrun.ControlContext) (*finalAnswerRejection, error) {
	if !mayHaveSubagents(cc.Path, cc.Run.ID) {
		return nil, nil
	}
	subagents, err := LoadSubagents(ctx, x.Sessions, x.Portfolio, cc.Run, "")
	if err != nil {
		return nil, err
	}
	running := subagents.running(true)
	if len(running) == 0 {
		return nil, nil
	}
	verb, pronoun := "is", "it"
	if len(running) > 1 {
		verb, pronoun = "are", "them"
	}
	return &finalAnswerRejection{Code: subagentsRunningCode, Message: fmt.Sprintf("%s %s still running: wait for %s or stop %s.",
		strings.Join(running, ", "), verb, pronoun, pronoun)}, nil
}
