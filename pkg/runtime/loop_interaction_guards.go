package runtime

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skillerror"
)

// Loop interaction guards (section 07 §8). They replace the Atlas browser and
// setup heuristics and derive their state only from the governed tool entries
// on the session path, read through each entry's action call (section 03 §7:
// the call row carries the skill, action, status, typed error and uncapped
// output; the tool entry payload carries no extra fields).

const (
	// The Lightpanda browser Skill. Its typed failures are the only evidence of
	// a source rate limit; unstructured error text never becomes HTTP status.
	lightpandaBrowserSkillID = "skill-browser"
	liveBrowserSkillID       = "skill-live-browser"
	liveBrowserActionPrefix  = "live-browser-"

	liveBrowserRateLimitedCode  = "source_rate_limited"
	liveBrowserRateLimitedError = "The source rate-limited requests (HTTP 429). The live browser is not available to work around it in this run; use another source, or tell the person and suggest trying later."

	skillSetupWaitSummary = "Waiting for you to finish connection setup."
	skillSetupPendingNote = "A setup form is waiting for the person. Tell them to complete it and finish your answer; do not poll list_setup_requests."
)

// governedToolView is what the guards know about one governed call of the Run.
type governedToolView struct {
	SkillID, Action, Status, ErrorCode, FailureKind string
	ErrorDetails                                    map[string]interface{}
	Result                                          map[string]interface{}
	ActionCallID                                    string
	RunID                                           string
}

// governedToolViews joins the session path with the Run's terminal loop calls,
// newest last. Calls a compaction summarized come first (in the compaction's
// order), so a rate limit or pending form outlives compaction. Entries of other
// Runs in the session, open entries and host tools (no call row) are skipped.
func governedToolViews(path *AgentSessionPath, runID string, calls map[string]*ActionCall) []governedToolView {
	if path == nil || runID == "" || len(calls) == 0 {
		return nil
	}
	views := []governedToolView{}
	seen := map[string]bool{}
	add := func(id string) {
		call := calls[id]
		if id == "" || seen[id] || call == nil || call.ID != id || call.RunID != runID || call.Origin != ActionOriginLoop {
			return
		}
		seen[id] = true
		views = append(views, governedToolViewOf(call))
	}
	if path.Compaction != nil && path.Compaction.Compaction != nil {
		for _, id := range path.Compaction.Compaction.ActionCallIDs {
			add(id)
		}
	}
	for _, entry := range path.Entries {
		if entry == nil || entry.Type != SessionEntryTool || entry.RunID != runID ||
			entry.State != ToolEntryCompleted && entry.State != ToolEntryError {
			continue
		}
		add(entry.ActionCallID)
	}
	return views
}

// governedToolViewOf classifies only typed execution evidence, never text from
// a tool or model: FailureKind and ErrorDetails come from re-validating the
// persisted error through skillerror, as the old action history did.
func governedToolViewOf(call *ActionCall) governedToolView {
	view := governedToolView{
		SkillID: call.SkillID, Action: call.Action, Status: string(call.Status),
		ErrorCode: call.ErrorCode, ActionCallID: call.ID, RunID: call.RunID,
		Result: call.Output,
	}
	if call.Status == ActionCallStatusFailed {
		if failure := skillerror.NewActionError(call.ErrorCode, "", call.ErrorDetails); failure != nil {
			view.ErrorCode, view.FailureKind = failure.Code(), failure.Code()
			view.ErrorDetails = map[string]interface{}{}
			for key, value := range failure.Details() {
				view.ErrorDetails[key] = value
			}
		}
	}
	return view
}

// loadGovernedToolViews reads the Run's terminal calls once and joins them with
// the path (governedToolViews).
func loadGovernedToolViews(ctx context.Context, store ActionStore, scope Scope, path *AgentSessionPath, runID string) ([]governedToolView, error) {
	if store == nil || path == nil || runID == "" {
		return nil, nil
	}
	listed, err := agentrun.ListAllRunActionHistory(ctx, store, scope, runID)
	if err != nil {
		return nil, err
	}
	calls := make(map[string]*ActionCall, len(listed))
	for _, call := range listed {
		if call != nil {
			calls[call.ID] = call
		}
	}
	return governedToolViews(path, runID, calls), nil
}

// browserSourceRateLimited: the newest Lightpanda failure with a typed source_*
// failure decides; a newer Lightpanda success clears it. Other Lightpanda
// failures (untyped, or not source_*) neither set nor clear it.
func browserSourceRateLimited(views []governedToolView) bool {
	for index := len(views) - 1; index >= 0; index-- {
		view := views[index]
		if view.SkillID != lightpandaBrowserSkillID {
			continue
		}
		if view.Status == string(ActionCallStatusSucceeded) {
			return false
		}
		if limited, relevant := typedSourceFailure(view); relevant {
			return limited
		}
	}
	return false
}

func typedSourceFailure(view governedToolView) (limited, relevant bool) {
	code := view.ErrorCode
	if view.Status != string(ActionCallStatusFailed) || !strings.HasPrefix(code, "source_") ||
		view.FailureKind != code || view.ErrorDetails["failureKind"] != code {
		return false, false
	}
	details := map[string]string{}
	for key, value := range view.ErrorDetails {
		if text, ok := value.(string); ok {
			details[key] = text
		}
	}
	failure := skillerror.NewActionError(code, "", details)
	if failure == nil {
		return false, false
	}
	return failure.HasRateLimitedSource(), true
}

// refuseLiveBrowserWhileRateLimited runs before ActionCoordinator.Propose for a
// governed call whose action name starts with "live-browser-". The tool list
// is never changed (prefix cache); the call is refused with an is_error result
// instead, and the governed executor moves the entry to error and records the
// refusal activity (D11).
func refuseLiveBrowserWhileRateLimited(views []governedToolView, actionName string) (*ToolResult, bool) {
	if !strings.HasPrefix(actionName, liveBrowserActionPrefix) || !browserSourceRateLimited(views) {
		return nil, false
	}
	value, _ := json.Marshal(map[string]interface{}{
		"status": string(ToolResultRefused), "code": liveBrowserRateLimitedCode, "error": liveBrowserRateLimitedError,
	})
	return &ToolResult{
		Status: ToolResultRefused, IsError: true,
		Content: []ContentPart{{Type: ContentPartJSON, Value: value}},
	}, true
}

// browserAwaitsUserReply: the newest successful live-browser call's result
// status or paymentChallenge is "otp_required" or "approve_in_app" (a site or
// bank one-time code, or an approval in the bank's app).
func browserAwaitsUserReply(views []governedToolView) bool {
	for index := len(views) - 1; index >= 0; index-- {
		view := views[index]
		if view.SkillID != liveBrowserSkillID || view.Status != string(ActionCallStatusSucceeded) {
			continue
		}
		for _, field := range []string{"status", "paymentChallenge"} {
			switch view.Result[field] {
			case "otp_required", "approve_in_app":
				return true
			}
		}
		return false
	}
	return false
}

// finalAnswerParkPlan is finalAnswerPark's verdict. It carries exactly the
// SPEC §2.4 FinalPark content (Deliver plus the LoopPark status, wake and
// summary); the final-answer producer (CTL-14) sets
// FinalAnswer.Park = &FinalPark{Deliver: p.Deliver, Park: LoopPark{Status: p.Status, Wake: p.Wake, Summary: p.Summary}}.
// FinalPark and LoopPark are declared by loop-1 (run_loop_hooks.go), which
// lands in the same wave, so this file does not declare them.
type finalAnswerParkPlan struct {
	Deliver bool // post the answer first (OTP question), then park
	Status  AgentRunStatus
	Wake    *WakeCondition
	Summary string
}

// finalAnswerPark decides, at the moment the loop would post a final answer and
// complete the Run, whether the Run must park instead. It returns nil to
// complete.
//
//   - Background task Runs with a pending setup form park on skill_setup
//     without posting (this wins over a browser wait, as before).
//   - A Run that can ask its conversation, whose live browser waits for a code
//     or an in-app approval, posts the answer (the question) and parks on the
//     person's reply; the browser stays open because the Run is not terminal.
//     An empty answer completes normally.
func finalAnswerPark(run *AgentRun, views []governedToolView, canAskConversation bool, answer string, interventions []AgentRunIntervention) *finalAnswerParkPlan {
	if run == nil {
		return nil
	}
	if backgroundTaskRun(run) {
		if id := pendingSkillSetupID(run, views, interventions); id != "" {
			return &finalAnswerParkPlan{
				Status: AgentRunStatusWaitingForEvent, Summary: skillSetupWaitSummary,
				Wake: &WakeCondition{Type: ConversationTaskSkillSetupWakeType, Reference: id},
			}
		}
	}
	answer = strings.TrimSpace(answer)
	if canAskConversation && answer != "" && browserAwaitsUserReply(views) {
		return &finalAnswerParkPlan{
			Deliver: true, Status: AgentRunStatusWaitingForEvent, Summary: answer,
			Wake: &WakeCondition{Type: "user_message"},
		}
	}
	return nil
}

// backgroundTaskRun is an independent conversation task's work Run.
func backgroundTaskRun(run *AgentRun) bool {
	return run != nil && run.Kind == RunKindAgentWork && run.Context[ConversationTaskContextKey] != nil
}

// pendingSkillSetupID: the newest successful openseal.skills request_setup
// (result.setupRequest) or list_setup_requests (result.requests[]) call whose
// item is pending for this Run and is not superseded by a kernel-authored
// SkillSetupResolution intervention with matching scope, runId, requestId and
// actionCallId and status resolved|dismissed. Model text never clears it.
func pendingSkillSetupID(run *AgentRun, views []governedToolView, interventions []AgentRunIntervention) string {
	if run == nil {
		return ""
	}
	for index := len(views) - 1; index >= 0; index-- {
		view := views[index]
		if view.SkillID != SkillManagementSkillID || view.Status != string(ActionCallStatusSucceeded) ||
			view.Action != agentrun.SkillActionRequestSetup && view.Action != skillbinding.SkillActionListSetupRequests {
			continue
		}
		return pendingSetupIn(run, view, interventions)
	}
	return ""
}

func pendingSetupIn(run *AgentRun, view governedToolView, interventions []AgentRunIntervention) string {
	pending := func(value interface{}) string {
		item, ok := value.(map[string]interface{})
		if !ok {
			return ""
		}
		id, _ := item["id"].(string)
		if id == "" || item["status"] != "pending" || item["runId"] != run.ID {
			return ""
		}
		callID, _ := item["actionCallId"].(string)
		for _, intervention := range interventions {
			receipt := intervention.SkillSetupResolution
			if receipt != nil && receipt.Scope == run.Scope && receipt.RunID == run.ID &&
				receipt.RequestID == id && receipt.ActionCallID != "" && receipt.ActionCallID == callID &&
				(receipt.Status == "resolved" || receipt.Status == "dismissed") {
				return ""
			}
		}
		return id
	}
	switch view.Action {
	case agentrun.SkillActionRequestSetup:
		return pending(view.Result["setupRequest"])
	case skillbinding.SkillActionListSetupRequests:
		values, _ := view.Result["requests"].([]interface{})
		for _, value := range values {
			if id := pending(value); id != "" {
				return id
			}
		}
	}
	return ""
}

// SkillSetupPendingSourceKey dedupes the foreground setup note: one per form.
func SkillSetupPendingSourceKey(runID, requestID string) string {
	return "setup-pending:" + runID + ":" + requestID
}

// pendingSkillSetupNote is the note the loop appends right after the governed
// tool entry that created or listed a pending setup form, on foreground Runs
// (background task Runs park at the final answer instead). The tool list never
// changes; the final answer then completes the Run normally. It returns nil
// when no note is due. The note is idempotent by SourceKey.
func pendingSkillSetupNote(run *AgentRun, sessionID string, entry *SessionEntry, call *ActionCall, interventions []AgentRunIntervention) *SessionEntry {
	if run == nil || backgroundTaskRun(run) || sessionID == "" || entry == nil || call == nil ||
		entry.Type != SessionEntryTool || entry.ActionCallID == "" || entry.ActionCallID != call.ID ||
		call.RunID != run.ID || call.SkillID != SkillManagementSkillID || call.Status != ActionCallStatusSucceeded {
		return nil
	}
	id := pendingSetupIn(run, governedToolViewOf(call), interventions)
	if id == "" {
		return nil
	}
	key := SkillSetupPendingSourceKey(run.ID, id)
	data, _ := json.Marshal(map[string]string{"requestId": id, "actionCallId": call.ID})
	return &SessionEntry{
		ID: SessionEntryID(sessionID, key), Scope: run.Scope, SessionID: sessionID,
		RunID: run.ID, StepID: entry.StepID, Step: entry.Step, Type: SessionEntryNote, SourceKey: key,
		Note: &NoteEntry{Kind: NoteSetupPending, Text: skillSetupPendingNote, Data: data},
	}
}
