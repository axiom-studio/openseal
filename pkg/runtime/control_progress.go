package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// turn_report_progress (SPEC §5.3): a deliberate public update while the Run
// keeps working. Inline phase; not counted by D9. Commentary is published as
// turn.commentary (chat bubble, voice, Slack/Telegram, thread status) and an
// activity note as turn.progress. Runs that are the work of a conversation
// task may only write activity notes. The "one opening update, then only
// concrete findings" guidance is the progress_rules system section.

const (
	reportProgressDestinationCommentary = "commentary"
	reportProgressDestinationActivity   = "activity"

	// maximumReportProgressRunes bounds a progress summary (schema maxLength).
	maximumReportProgressRunes = 600

	reportProgressRepeatCode = "already_reported"
)

type reportProgressArgs struct {
	Summary     string `json:"summary"`
	Destination string `json:"destination,omitempty"`
}

type reportProgressHandler struct{}

func (reportProgressHandler) spec(a ControlAuthority) ControlToolSpec {
	if a.TaskWork {
		return ControlToolSpec{
			Description: "Record a concise internal activity-trail note and continue the existing task. Do not send a new public acknowledgment or routine progress commentary.",
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []string{"summary"},
				"properties": map[string]interface{}{
					"summary":     map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumReportProgressRunes},
					"destination": map[string]interface{}{"type": "string", "enum": []string{reportProgressDestinationActivity}},
				},
			},
		}
	}
	return ControlToolSpec{
		Description: "Send one concise public update to the user and continue working. This does not finish the task or execute an action. Use only observed facts or your immediate intended next step.",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false, "required": []string{"summary"},
			"properties": map[string]interface{}{
				"summary": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumReportProgressRunes},
				"destination": map[string]interface{}{
					"type": "string", "enum": []string{reportProgressDestinationCommentary, reportProgressDestinationActivity},
					"description": "commentary (default) is shown to the user; activity is a trail-only note.",
				},
			},
		},
	}
}

func (reportProgressHandler) decode(raw json.RawMessage) (interface{}, error) {
	return decodeControlInto[reportProgressArgs](raw)
}

func (reportProgressHandler) run(ctx context.Context, x *ControlExecutor, cc ControlContext) (ControlResult, error) {
	args, _ := cc.Arguments.(*reportProgressArgs)
	if args == nil {
		return ControlResult{}, errors.New("turn_report_progress: arguments were not decoded")
	}
	summary := strings.TrimSpace(args.Summary)
	switch {
	case summary == "":
		return x.refuse(ctx, cc, controlArgumentsInvalid("summary is required."))
	case !utf8.ValidString(summary) || utf8.RuneCountInString(summary) > maximumReportProgressRunes:
		return x.refuse(ctx, cc, controlArgumentsInvalid("summary must be at most 600 characters."))
	}
	destination := args.Destination
	switch destination {
	case "":
		destination = reportProgressDestinationCommentary
	case reportProgressDestinationCommentary, reportProgressDestinationActivity:
	default:
		return x.refuse(ctx, cc, controlArgumentsInvalid(`destination must be "commentary" or "activity".`))
	}
	if cc.Authority.TaskWork {
		destination = reportProgressDestinationActivity
	}
	repeated, err := x.progressAlreadyReported(ctx, cc, summary)
	if err != nil {
		return ControlResult{}, err
	}
	if repeated {
		return x.refuse(ctx, cc, controlErrorResult(ToolResultRefused, reportProgressRepeatCode,
			"Already reported. Add a new finding or keep working.", nil))
	}
	if x.Activity == nil {
		return ControlResult{}, errors.New("turn_report_progress: control executor has no activity service")
	}
	eventType := "turn.commentary"
	if destination == reportProgressDestinationActivity {
		eventType = "turn.progress"
	}
	event := &ActivityEvent{
		Scope: cc.Run.Scope, RunID: cc.Run.ID,
		AgentID: strings.TrimSpace(cc.Run.AssignedAgentID), TeamID: teamIDForRun(cc.Run),
		EventType: eventType, Severity: ActivitySeverityInfo, Summary: summary,
		Actor:      ActivityActor{Type: "worker", ID: cc.WorkerID},
		Payload:    map[string]interface{}{"entryId": cc.Entry.ID},
		Visibility: ActivityVisibilityScope,
		CreatedAt:  x.now(),
	}
	if cc.Step != nil {
		event.TurnID = cc.Step.ID
	}
	if _, err := x.Activity.AppendActivity(ctx, event); err != nil {
		return ControlResult{}, err
	}
	return x.complete(ctx, cc, controlJSONResult(ToolResultSucceeded, map[string]interface{}{"status": "posted"}), nil)
}

// progressAlreadyReported reports whether an earlier completed
// turn_report_progress entry of the Run carried the same summary.
func (x *ControlExecutor) progressAlreadyReported(ctx context.Context, cc ControlContext, summary string) (bool, error) {
	entries, err := x.Sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID,
		ToolNames: []string{string(ControlToolReportProgress)}, States: []ToolEntryState{ToolEntryCompleted},
		Limit: maxSessionEntryListLimit,
	})
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry == nil || entry.ID == cc.Entry.ID || entry.Tool == nil {
			continue
		}
		var earlier reportProgressArgs
		if decodeControlArguments(entry.Tool.Arguments, &earlier) != nil {
			continue
		}
		if strings.TrimSpace(earlier.Summary) == summary {
			return true, nil
		}
	}
	return false, nil
}
