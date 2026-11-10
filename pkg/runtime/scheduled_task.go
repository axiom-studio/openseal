package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/validation"
)

const (
	RunbookActionCreateTask = "create_task"
	RunbookActionList       = "list"
	RunbookActionSetStatus  = "set_status"
	RunbookActionSetScript  = "set_script"
	scheduledTaskContextKey = "scheduledAgentTask"
)

// ScheduledAgentTask is the cognitive execution method of an Objective-owned
// routine. It pins work and its reporting destination, never credentials or
// model-generated execution authority. Every occurrence resolves the current
// deployed Agent and its currently authorized tools through the normal worker.
type ScheduledAgentTask struct {
	Goal            string `json:"goal"`
	ConversationID  string `json:"conversationId"`
	SourceMessageID string `json:"sourceMessageId"`
	// AppID is the app this routine keeps up to date. It belongs to the same
	// conversation and owner; each occurrence is told to save the app's data.
	AppID string `json:"appId,omitempty"`
	// Grants are the actions the user approved in the routine's brief; its
	// runs perform them without asking. They come only from the approved
	// create_task arguments and can only be cleared afterwards.
	Grants []RoutineGrant `json:"grants,omitempty"`
	// Script, when set, is the saved chat script every occurrence's model
	// runs first with run_command, pinned to the version the user approved.
	// Only an approved create_task or set_script sets it.
	Script *RoutineScript `json:"script,omitempty"`
	// CreatedBy is the person who created the routine: the creating run's
	// ActingUser. Its occurrences act for that person, and only that person
	// changes its script or resumes it. Empty means no recorded creator.
	CreatedBy string `json:"createdBy,omitempty"`
	// Event, when set, makes this an event routine: its occurrences start
	// when a matching event arrives instead of on a schedule.
	Event *RoutineEventTrigger `json:"event,omitempty"`
}

// scheduledTaskAppID matches host-assigned app identifiers.
var scheduledTaskAppID = regexp.MustCompile(`^app-[a-f0-9]{64}$`)

func (t *ScheduledAgentTask) Validate() error {
	if t == nil || strings.TrimSpace(t.Goal) == "" || len(t.Goal) > 16000 || !validOpaqueIdentifier(t.ConversationID, 256) || !validOpaqueIdentifier(t.SourceMessageID, 256) {
		return errors.New("scheduled task requires bounded work and an originating conversation message")
	}
	if t.CreatedBy != "" && !validOpaqueIdentifier(t.CreatedBy, 256) {
		return errors.New("scheduled task creator is invalid")
	}
	if t.AppID != "" && !scheduledTaskAppID.MatchString(t.AppID) {
		return errors.New("scheduled task app must be an app ID from this chat")
	}
	if _, err := normalizeRoutineGrants(t.Grants, false); err != nil {
		return err
	}
	if t.Script != nil {
		if err := t.Script.Validate(); err != nil {
			return err
		}
	}
	if t.Event != nil {
		if err := t.Event.Validate(); err != nil {
			return err
		}
	}
	return validation.ValidateNoSensitiveInput(t.Goal)
}
func cloneScheduledAgentTask(t *ScheduledAgentTask) *ScheduledAgentTask {
	if t == nil {
		return nil
	}
	copy := *t
	copy.Grants = cloneRoutineGrants(t.Grants)
	copy.Script = cloneRoutineScript(t.Script)
	copy.Event = cloneRoutineEventTrigger(t.Event)
	return &copy
}

func cloneRoutineEventTrigger(t *RoutineEventTrigger) *RoutineEventTrigger {
	if t == nil {
		return nil
	}
	copy := *t
	if t.Filter != nil {
		copy.Filter = make([]RoutineEventCondition, len(t.Filter))
		for i, condition := range t.Filter {
			condition.Values = append([]string(nil), condition.Values...)
			copy.Filter[i] = condition
		}
	}
	if t.Calendar != nil {
		calendar := *t.Calendar
		copy.Calendar = &calendar
	}
	if t.Webhook != nil {
		webhook := *t.Webhook
		copy.Webhook = &webhook
	}
	return &copy
}
func activationGoal(a *RunbookActivation, o *Objective) string {
	if a.Task != nil {
		if a.Task.AppID != "" {
			return a.Task.Goal + "\n\nThis routine keeps app " + a.Task.AppID + " up to date: save its results with save_app_data for that appId so the app shows them."
		}
		return a.Task.Goal
	}
	return o.Goal
}

// ErrRoutineReportingUnavailable means a routine can no longer report to the
// chat it was made in. Retrying does not help; the routine must be paused.
var ErrRoutineReportingUnavailable = errors.New("routine reporting chat is unavailable")

func scheduledTaskConversation(ctx context.Context, store ConversationStore, scope Scope, owner ObjectiveOwner, task *ScheduledAgentTask) (*Conversation, error) {
	if err := task.Validate(); err != nil {
		return nil, err
	}
	c, err := store.GetConversation(ctx, scope, task.ConversationID)
	if err != nil {
		return nil, err
	}
	if c == nil || c.Scope != scope || c.Owner != owner || c.Status != ConversationStatusActive {
		return nil, fmt.Errorf("%w: its chat is archived or gone", ErrRoutineReportingUnavailable)
	}
	m, err := store.GetChannelMessage(ctx, scope, c.ID, task.SourceMessageID)
	if err != nil {
		return nil, err
	}
	if m == nil || m.Scope != scope || m.ConversationID != c.ID || m.Sender.Type != ConversationParticipantUser || m.Audience.Kind != ConversationAudienceChannel {
		return nil, fmt.Errorf("%w: its request message is gone or not visible in the chat", ErrRoutineReportingUnavailable)
	}
	return c, nil
}

func validateScheduledTaskTarget(ctx context.Context, reader RunbookActivationReader, catalog RunbookDefinitionCatalog, a *RunbookActivation) error {
	if err := a.Validate(); err != nil {
		return err
	}
	d, err := catalog.GetDeployment(ctx, capability.ScopeReference{Kind: a.Scope.Kind, ID: a.Scope.ID}, a.AssignedAgentID)
	if err != nil {
		return fmt.Errorf("%w: assigned Agent is unavailable", ErrRunbookActivationUnverified)
	}
	if d == nil || d.Scope.Kind != a.Scope.Kind || d.Scope.ID != a.Scope.ID || d.RolloutStatus != kernelagent.RolloutActive {
		return fmt.Errorf("%w: assigned Agent is inactive", ErrRunbookActivationUnverified)
	}
	// A Team routine runs as the member that made it only while that member is
	// on the roster; otherwise the scheduler pauses it.
	onRoster, err := AgentOnChatRoster(ctx, teamRosterReader(reader), a.Scope, a.Owner, a.AssignedAgentID)
	if err != nil {
		return err
	}
	if !onRoster {
		return fmt.Errorf("%w: assigned Agent is not on the chat's roster", ErrRunbookActivationUnverified)
	}
	store, ok := reader.(ConversationStore)
	if !ok {
		return fmt.Errorf("%w: reporting store unavailable", ErrRunbookActivationUnverified)
	}
	if _, err := scheduledTaskConversation(ctx, store, a.Scope, a.Owner, a.Task); err != nil {
		return fmt.Errorf("%w: %v", ErrRunbookActivationUnverified, err)
	}
	return nil
}

func prepareActivationReporting(ctx context.Context, store ConversationStore, a *RunbookActivation, runID string, values map[string]interface{}) (*Conversation, string, error) {
	if a.Task == nil {
		return prepareRunReporting(ctx, store, a.Scope, a.Owner, a.Trigger.Reporting, runID, values)
	}
	if store == nil {
		return nil, "", errors.New("scheduled task reporting store unavailable")
	}
	c, err := scheduledTaskConversation(ctx, store, a.Scope, a.Owner, a.Task)
	if err != nil {
		return nil, "", err
	}
	values[conversationRunContextConversationID] = c.ID
	values[conversationRunContextTriggerID] = a.Task.SourceMessageID
	values[runReportingContextRootRunID] = runID
	values[runReportingContextMilestones] = []interface{}{string(runbook.ReportingCompleted), string(runbook.ReportingFailed)}
	values[scheduledTaskContextKey] = true
	if a.Task.Script != nil {
		// The occurrence runs the version pinned when it started, even if the
		// routine is approved for a newer one while it runs.
		values[RoutineScriptContextKey] = routineScriptMap(a.Task.Script)
	}
	return c, "", nil
}

// scheduledTaskCreateAction creates a routine. The user always approves its
// brief first, in every approval mode. Its grants are pinned to the connected
// tools and destinations the user approved, and a routine may run a saved,
// tested script from its chat's files, which each run runs first with
// run_command; the kernel pins the script's current version when the
// routine is proposed.
func scheduledTaskCreateAction() skill.Action {
	return skill.Action{Name: RunbookActionCreateTask, Description: "Create and activate a routine when the user wants future, repeated or event-driven work. The current Agent executes the supplied goal with its authorized tools on each occurrence and posts results to this chat. This creates a real routine, not a sleeping chat reply. Tool access and approval requirements still apply on each run." +
		" To start the work when something happens rather than on a clock, call event_sources first and pass trigger.event with a source from it; prefer an event over a frequent schedule whenever a source exists. Put conditions in filter rather than in goal. Use maximumOccurrences 1 for a one-time follow-up such as 'tell me when Ana replies'. In goal, say what to do with the event; the event itself is given to each run." +
		" To keep an app's data fresh, pass its appId; each occurrence then saves results to that app with save_app_data." +
		" The user always approves a brief before the routine is created: always pass brief with a one-sentence summary of what it does and when, deliverTo naming where results go, and grants listing only the actions its runs need to perform without asking (for example web search or page reads and an email to the user). Runs ask in this chat before any other side effect, and actions that always ask, such as paying, cannot be granted." +
		" To grant a connected app's tool, use that tool's own exact action name from your tools, for example mcp.<connection>.<tool>; the generic action connected tools run through (such as openseal.mcp.call) cannot be granted." +
		" Each run's result is posted automatically as a new message in the chat or Slack conversation the routine was created from, so never grant a send action to deliver results there. To post to any other channel, grant that sending action (for example skill-slack.slack-send-message) with toDestinations naming each channel, and name it in the label, for example \"Post to #ai-news\"; runs then post there without asking, and anywhere else asks first." +
		" For work that is the same steps with new data each time (pull data, compute, chart, write a file), make the routine run a saved script instead of redoing the work: first write the script in this chat's files and test it in this chat's sandbox with run_command until it works, then pass script with its path and the exact command that ran it. Approving the brief pins the script as it is then; each run then starts by running that exact version with run_command, and you write the report from its output, so say in goal what to report from the script's results.",
		Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired, Retry: skill.ActionRetryPolicy{MaxAttempts: 2}, Review: skill.ActionReviewAlways,
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"title", "goal", "trigger", "brief"}, "properties": map[string]interface{}{
			"title": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 240}, "goal": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 16000, "description": "Self-contained work to perform on every occurrence, including the source to use and requested result. Do not include credentials or request another schedule."},
			"trigger": scheduledTaskTriggerSchema(),
			"appId": map[string]interface{}{
				"type": "string", "pattern": `^app-[a-f0-9]{64}$`, "description": "Optional app from this chat that each occurrence updates.",
			},
			"brief": map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"summary", "grants"},
				"description": "Required. The brief the user approves before the routine is created.",
				"properties": map[string]interface{}{
					"summary":   map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumRoutineBriefSummary, "description": "One plain sentence: what the routine does and when."},
					"deliverTo": map[string]interface{}{"type": "string", "maxLength": maximumRoutineDeliverTo, "description": "Where results go, in plain words, for example \"kev@example.com, from your Gmail\" or \"this chat\"."},
					"grants": map[string]interface{}{
						"type": "array", "maxItems": maximumRoutineGrants,
						"description": "The fewest actions each run must perform without asking. Use exact action names from your tools: skillId.action, or a connected tool's own name such as mcp.<connection>.<tool>, granting each connected tool separately. List what it reads (for example web search) and each sending action; for an email to the user set toOwnerOnly; for a post to another channel set toDestinations. Never grant a send to this chat: results are posted here automatically.",
						"items": map[string]interface{}{
							"type": "object", "additionalProperties": false, "required": []interface{}{"action"},
							"properties": map[string]interface{}{
								"action":        map[string]interface{}{"type": "string", "minLength": 3, "maxLength": 384, "description": "Exact action name: skillId.action, or a connected tool's own name, mcp.<connection>.<tool>."},
								"label":         map[string]interface{}{"type": "string", "maxLength": maximumRoutineGrantLabel, "description": "Plain words for the user, for example \"Email you the summary\"."},
								"toOwnerOnly":   map[string]interface{}{"type": "boolean", "description": "For a sending action: only sends whose every recipient is the user's own address run without asking."},
								"toolArguments": map[string]interface{}{"type": "object", "maxProperties": maximumRoutineGrantToolArguments, "description": "Filled in by the system for a connected tool; omit it."},
								"toDestinations": map[string]interface{}{
									"type": "array", "minItems": 1, "maxItems": maximumRoutineGrantDestinations,
									"items":       map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumRoutineGrantDestinationBytes},
									"description": "Required for an action that posts to a channel or conversation: each destination its runs post to, exactly as the runs will pass it as the action's channel argument (the channel ID, or its #name). Only posts to these run without asking.",
								},
							},
						},
					},
				},
			},
			"script": map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"path", "command"},
				"description": "Optional. A tested script from this chat's files that each run runs first with run_command, in this chat's sandbox.",
				"properties": map[string]interface{}{
					"path":           map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1024, "description": "The script's path in this chat's files, for example scripts/weekly_signups.py."},
					"command":        map[string]interface{}{"type": "string", "minLength": 1, "maxLength": maximumRoutineScriptCommand, "description": "Exactly how to run it: python3, node or bash, then the script's path, then any plain arguments without quotes or shell characters, for example python3 scripts/weekly_signups.py --days 7."},
					"timeoutSeconds": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumRoutineScriptSeconds, "description": "Optional limit for one run of the script. Defaults to 300."},
					"digest":         map[string]interface{}{"type": "string", "description": "Filled in by the system; omit it."},
					"filesVersion":   map[string]interface{}{"type": "integer", "minimum": 1, "description": "Filled in by the system; omit it."},
				},
			},
		}}, OutputSchema: scheduledTaskResultSchema(RunbookActionCreateTask)}
}

// scheduledTaskTriggerSchema is create_task's trigger: exactly one of a
// schedule or an event.
func scheduledTaskTriggerSchema() map[string]interface{} {
	kinds := make([]interface{}, 0, len(RoutineEventKinds))
	for _, kind := range RoutineEventKinds {
		kinds = append(kinds, string(kind))
	}
	maximumOccurrences := func() map[string]interface{} {
		return map[string]interface{}{"type": []interface{}{"integer", "null"}, "minimum": 1, "maximum": 1000000, "description": "Omit or supply null for an ongoing routine; supply a positive integer only when the user requested a bounded number of executions."}
	}
	return map[string]interface{}{
		"type": "object", "additionalProperties": false, "minProperties": 1, "maxProperties": 1,
		"description": "Exactly one of schedule or event.",
		"properties": map[string]interface{}{
			"schedule": map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"cron", "timezone"},
				"description": "Run on a clock. Use a six-field cron including seconds and an explicit IANA timezone; clarify ambiguous timing.",
				"properties": map[string]interface{}{
					"cron":               map[string]interface{}{"type": "string", "pattern": `^\S+\s+\S+\s+\S+\s+\S+\s+\S+\s+\S+$`, "description": "Six-field cron: seconds minutes hours day-of-month month weekday."},
					"timezone":           map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 100},
					"maximumOccurrences": maximumOccurrences(),
				},
			},
			"event": map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"kind", "source"},
				"properties": map[string]interface{}{
					"kind":   map[string]interface{}{"type": "string", "enum": kinds},
					"source": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 256, "description": "A source from event_sources. For a webhook use \"new\"."},
					"filter": map[string]interface{}{
						"type": "array", "maxItems": RoutineEventMaxConditions,
						"items": map[string]interface{}{
							"type": "object", "additionalProperties": false, "required": []interface{}{"field", "op"},
							"properties": map[string]interface{}{
								"field":  map[string]interface{}{"type": "string", "maxLength": 256},
								"op":     map[string]interface{}{"type": "string", "enum": []interface{}{"equals", "contains", "one_of", "glob", "exists"}},
								"value":  map[string]interface{}{"type": "string", "maxLength": RoutineEventConditionValueMax},
								"values": map[string]interface{}{"type": "array", "maxItems": RoutineEventConditionMaxValues, "items": map[string]interface{}{"type": "string", "maxLength": RoutineEventConditionValueMax}},
								"not":    map[string]interface{}{"type": "boolean"},
								"label":  map[string]interface{}{"type": "string", "maxLength": RoutineEventConditionLabelMax, "description": "Plain words for the person, e.g. \"#support\"."},
							},
						},
					},
					"calendar": map[string]interface{}{
						"type": "object", "additionalProperties": false, "required": []interface{}{"on"},
						"properties": map[string]interface{}{
							"on":      map[string]interface{}{"type": "string", "enum": []interface{}{RoutineCalendarStartsIn, RoutineCalendarCreated, RoutineCalendarUpdated, RoutineCalendarEnded}},
							"minutes": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": RoutineCalendarMaxMinutesBefore},
						},
					},
					"includeBots":        map[string]interface{}{"type": "boolean"},
					"maxPerHour":         map[string]interface{}{"type": "integer", "minimum": 1, "maximum": RoutineEventMaxPerHourLimit, "description": "Most runs per hour; extra events wait and run together. Default 20."},
					"maximumOccurrences": map[string]interface{}{"type": []interface{}{"integer", "null"}, "minimum": 1, "maximum": 1000000},
				},
			},
		},
	}
}

func scheduledTaskListAction() skill.Action {
	return skill.Action{Name: RunbookActionList, Description: "List routines owned by this Agent or Team, including exact activation IDs, schedules, status, revisions, next occurrence and originating chat. Use these IDs to start, pause, resume, cancel or replace a schedule.", Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported, InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"offset": map[string]interface{}{"type": "integer", "minimum": 0}}}, OutputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"activations": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "object"}}, "nextOffset": map[string]interface{}{"type": "integer"}}, "required": []interface{}{"activations"}}}
}
func scheduledTaskStatusAction() skill.Action {
	return skill.Action{Name: RunbookActionSetStatus, Description: "Pause, resume or cancel future occurrences of an owned routine using its exact activationId and expectedRevision from list. active resumes; paused suspends; retired cancels permanently. Existing runs continue separately and can be controlled with Run tools. To change timing, retire then replace_schedule; the work and reporting destination remain unchanged.", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired, InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"activationId", "expectedRevision", "status", "reason"}, "properties": map[string]interface{}{"activationId": map[string]interface{}{"type": "string", "minLength": 1}, "expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1}, "status": map[string]interface{}{"type": "string", "enum": []interface{}{"active", "paused", "retired"}}, "reason": map[string]interface{}{"type": "string", "minLength": 1}}}, OutputSchema: scheduledTaskResultSchema(RunbookActionSetStatus)}
}

// scheduledTaskSetScriptAction re-pins a script routine to its chat file's
// current version. Like creating the routine, it always shows the brief.
func scheduledTaskSetScriptAction() skill.Action {
	return skill.Action{Name: RunbookActionSetScript, Description: "Make a routine that runs a saved script run the script's current version from this chat's files. A routine keeps running the version approved in its brief after the script changes; use this only when the person wants the new version, after you tested it with run_command. The person approves the routine's brief again before it changes. Use the exact activationId and expectedRevision from list.", Risk: skill.RiskLevelWrite, SideEffect: skill.SideEffectWrite, Idempotency: skill.IdempotencyRequired, Review: skill.ActionReviewAlways,
		InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"activationId", "expectedRevision"}, "properties": map[string]interface{}{
			"activationId": map[string]interface{}{"type": "string", "minLength": 1}, "expectedRevision": map[string]interface{}{"type": "integer", "minimum": 1},
			"digest":       map[string]interface{}{"type": "string", "description": "Filled in by the system; omit it."},
			"filesVersion": map[string]interface{}{"type": "integer", "minimum": 1, "description": "Filled in by the system; omit it."},
		}}, OutputSchema: scheduledTaskResultSchema(RunbookActionSetScript)}
}

func scheduledTaskResultSchema(operation string) map[string]interface{} {
	return map[string]interface{}{"type": "object", "additionalProperties": false, "properties": map[string]interface{}{"resourceType": map[string]interface{}{"type": "string", "const": runbookActivationResourceType}, "operation": map[string]interface{}{"type": "string", "const": operation}, "activation": map[string]interface{}{"type": "object"}, "objective": map[string]interface{}{"type": "object"}, "replayed": map[string]interface{}{"type": "boolean"}}, "required": []interface{}{"resourceType", "operation", "activation", "replayed"}}
}

type scheduledTaskCreateArguments struct {
	Title   string                         `json:"title"`
	Goal    string                         `json:"goal"`
	Trigger *scheduledTaskTriggerArguments `json:"trigger"`
	AppID   string                         `json:"appId,omitempty"`
	Brief   *RoutineBrief                  `json:"brief,omitempty"`
	Script  *RoutineScript                 `json:"script,omitempty"`
}

type scheduledTaskTriggerArguments struct {
	Schedule *scheduledTaskScheduleArguments `json:"schedule,omitempty"`
	Event    *scheduledTaskEventArguments    `json:"event,omitempty"`
}

type scheduledTaskScheduleArguments struct {
	Cron               string `json:"cron"`
	Timezone           string `json:"timezone"`
	MaximumOccurrences *int64 `json:"maximumOccurrences,omitempty"`
}

type scheduledTaskEventArguments struct {
	Kind               RoutineEventKind        `json:"kind"`
	Source             string                  `json:"source"`
	Filter             []RoutineEventCondition `json:"filter,omitempty"`
	Calendar           *RoutineCalendarTiming  `json:"calendar,omitempty"`
	IncludeBots        bool                    `json:"includeBots,omitempty"`
	MaxPerHour         int                     `json:"maxPerHour,omitempty"`
	MaximumOccurrences *int64                  `json:"maximumOccurrences,omitempty"`
}

// scheduledTaskCreatesEventRoutine reports whether create_task arguments
// ask for an event routine.
func scheduledTaskCreatesEventRoutine(arguments map[string]interface{}) bool {
	trigger, _ := arguments["trigger"].(map[string]interface{})
	_, ok := trigger["event"]
	return ok
}

type scheduledTaskStatusArguments struct {
	ActivationID     string                  `json:"activationId"`
	ExpectedRevision int64                   `json:"expectedRevision"`
	Status           RunbookActivationStatus `json:"status"`
	Reason           string                  `json:"reason"`
}

func decodeScheduledTaskArguments(arguments map[string]interface{}, target interface{}) error {
	b, err := json.Marshal(arguments)
	if err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

// resolveScheduledTaskCreate checks create_task arguments and returns the
// routine's task with its schedule, or with Task.Event set for an event
// routine (then the schedule is nil). activationID is the routine's ID when
// it is known (dispatch); a webhook routine's hook is named after it.
func resolveScheduledTaskCreate(ctx context.Context, store runbookActionStore, files ChatFileResolver, events routineEventActions, run *AgentRun, activationID string, args map[string]interface{}) (*scheduledTaskCreateArguments, *ScheduledAgentTask, *runbook.Schedule, error) {
	var a scheduledTaskCreateArguments
	if err := decodeScheduledTaskArguments(args, &a); err != nil {
		return nil, nil, nil, err
	}
	a.Title, a.Goal = strings.TrimSpace(a.Title), strings.TrimSpace(a.Goal)
	if a.Title == "" || len(a.Title) > 240 {
		return nil, nil, nil, errors.New("scheduled task title is invalid")
	}
	if a.Trigger == nil || (a.Trigger.Schedule == nil) == (a.Trigger.Event == nil) {
		return nil, nil, nil, errors.New("a routine needs exactly one trigger: schedule or event")
	}
	if err := validation.ValidateNoSensitiveInput(a.Title); err != nil {
		return nil, nil, nil, err
	}
	if run == nil || run.Kind != RunKindConversation || run.AssignedAgentID == "" {
		return nil, nil, nil, errors.New("scheduled task creation requires a chat whose roster includes this Agent")
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	messageID, _ := run.Context[conversationRunContextTriggerID].(string)
	if isTeamMemberRun(run) {
		// A teammate's hand-off started this Run; the routine's source is the
		// person's request that the hand-off answered.
		trigger, err := store.GetChannelMessage(ctx, run.Scope, conversationID, messageID)
		if err != nil {
			return nil, nil, nil, err
		}
		request, err := ResolveTeamHandoffRequest(ctx, store, run, trigger)
		if err != nil {
			return nil, nil, nil, err
		}
		if request != nil {
			messageID = request.ID
		}
	}
	task := &ScheduledAgentTask{Goal: a.Goal, ConversationID: conversationID, SourceMessageID: messageID, AppID: strings.TrimSpace(a.AppID), CreatedBy: run.ActingUser}
	// A routine always carries the brief the user approved.
	brief, err := normalizeRoutineBrief(a.Brief, false)
	if err != nil {
		return nil, nil, nil, err
	}
	a.Brief, task.Grants = brief, cloneRoutineGrants(brief.Grants)
	if a.Script != nil {
		// The kernel pinned this version from the chat's files when the
		// routine was proposed; it must still be that snapshot's file.
		if err := verifyRoutineScriptPin(ctx, files, run.Scope, run.Owner, conversationID, a.Script); err != nil {
			return nil, nil, nil, err
		}
		task.Script = cloneRoutineScript(a.Script)
	}
	conversation, err := scheduledTaskConversation(ctx, store, run.Scope, run.Owner, task)
	if err != nil {
		return nil, nil, nil, err
	}
	if ConversationRunAgentID(conversation, run) != run.AssignedAgentID {
		return nil, nil, nil, errors.New("scheduled task creation requires a chat whose roster includes this Agent")
	}
	if task.AppID != "" {
		if err := validateScheduledTaskApp(ctx, store, run, task); err != nil {
			return nil, nil, nil, err
		}
	}
	if a.Trigger.Event != nil {
		if activationID == "" {
			// Validation before the call exists: the hook ID is only checked
			// for shape here and named after the real activation at dispatch.
			activationID = "proposal:" + run.ID
		}
		event, err := resolveRoutineEventTrigger(ctx, store, events, run, activationID, a.Trigger.Event)
		if err != nil {
			return nil, nil, nil, err
		}
		task.Event = event
		if err := task.Validate(); err != nil {
			return nil, nil, nil, err
		}
		return &a, task, nil, nil
	}
	schedule := &runbook.Schedule{Cron: strings.TrimSpace(a.Trigger.Schedule.Cron), Timezone: strings.TrimSpace(a.Trigger.Schedule.Timezone)}
	if limit := a.Trigger.Schedule.MaximumOccurrences; limit != nil {
		if *limit < 1 || *limit > 1000000 {
			return nil, nil, nil, errors.New("scheduled task occurrence limit is invalid")
		}
		schedule.MaximumOccurrences = *limit
	}
	if err := schedule.Validate(); err != nil {
		return nil, nil, nil, err
	}
	return &a, task, schedule, nil
}
func resolveOwnedTaskStatus(ctx context.Context, store runbookActionStore, run *AgentRun, arguments map[string]interface{}) (*scheduledTaskStatusArguments, *RunbookActivation, error) {
	var args scheduledTaskStatusArguments
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(args.Reason) == "" || args.ActivationID == "" || args.ExpectedRevision < 1 {
		return nil, nil, errors.New("routine status change requires an ID, revision and reason")
	}
	if args.Status != RunbookActivationActive && args.Status != RunbookActivationPaused && args.Status != RunbookActivationRetired {
		return nil, nil, errors.New("unsupported routine status")
	}
	a, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
	if err != nil {
		return nil, nil, err
	}
	if a == nil || a.Owner != run.Owner {
		return nil, nil, errors.New("routine is not owned by this chat")
	}
	if a.Revision != args.ExpectedRevision {
		return nil, nil, ErrRunbookActivationRevision
	}
	if a.Status == RunbookActivationRetired && args.Status != RunbookActivationRetired {
		return nil, nil, errors.New("cancelled routine cannot be resumed; replace its schedule")
	}
	if args.Status == RunbookActivationActive {
		if err := authorizeRoutineChange(a, run); err != nil {
			return nil, nil, err
		}
	}
	return &args, a, nil
}

// ErrRoutineNotCreator refuses a person other than a routine's creator when
// they change its script or resume it. Pausing and cancelling stay open.
var ErrRoutineNotCreator = errors.New("Only the person who created this routine can change it.")

// authorizeRoutineChange lets only the person who created a routine change
// its script or resume it: its occurrences act for that person. A routine
// with no recorded creator becomes the routine of whoever changes it next
// (routineChangeCreator stamps them).
func authorizeRoutineChange(a *RunbookActivation, run *AgentRun) error {
	if a == nil || a.Task == nil || a.Task.CreatedBy == "" || run != nil && a.Task.CreatedBy == run.ActingUser {
		return nil
	}
	return ErrRoutineNotCreator
}

// routineChangeCreator is the creator an authorized change records: the
// changing run's person when the routine has none yet, else "".
func routineChangeCreator(a *RunbookActivation, run *AgentRun) string {
	if a == nil || a.Task == nil || a.Task.CreatedBy != "" || run == nil {
		return ""
	}
	return run.ActingUser
}
func validateScheduledTaskAction(ctx context.Context, store runbookActionStore, files ChatFileResolver, teams RoutineTeamCatalog, events routineEventActions, input ActionProposalValidationInput) (map[string]interface{}, error) {
	if err := runbookActionOwner(ctx, teams, input.Run, input.Bound); err != nil {
		return nil, err
	}
	switch input.Bound.Action.Name {
	case RunbookActionCreateTask:
		args, task, schedule, err := resolveScheduledTaskCreate(ctx, store, files, events, input.Run, "", input.Arguments)
		if err != nil {
			return nil, err
		}
		preview := map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionCreateTask, "title": args.Title, "goal": task.Goal, "conversationId": task.ConversationID}
		if task.Event != nil {
			preview["event"] = routineEventPreview(task.Event)
		} else {
			preview["schedule"] = schedule
		}
		if args.Brief != nil {
			preview["brief"] = routineBriefPreview(args.Brief)
		}
		if task.Script != nil {
			// The brief's Runs line: the exact file version and command approved.
			preview["script"] = routineScriptMap(task.Script)
		}
		return preview, nil
	case RunbookActionSetScript:
		_, a, script, err := resolveOwnedTaskScript(ctx, store, files, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		if err := authorizeRoutineChange(a, input.Run); err != nil {
			return nil, err
		}
		return routineScriptChangePreview(ctx, store, a, script)
	case RunbookActionList, RunbookActionEventSources:
		return nil, nil
	case RunbookActionSendTestEvent:
		a, _, err := resolveRoutineTestEvent(ctx, store, events, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSendTestEvent, "activationId": a.ID, "event": routineEventPreview(a.Task.Event)}, nil
	case RunbookActionSetStatus:
		args, a, err := resolveOwnedTaskStatus(ctx, store, input.Run, input.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetStatus, "activationId": a.ID, "currentStatus": a.Status, "status": args.Status, "reason": args.Reason}, nil
	}
	return nil, errors.New("unsupported scheduled task action")
}
func dispatchScheduledTaskAction(ctx context.Context, store runbookActionStore, files ChatFileResolver, teams RoutineTeamCatalog, events routineEventActions, input ActionDispatchInput) (map[string]interface{}, error) {
	run, err := store.GetAgentRun(ctx, input.Call.Scope, input.Call.RunID)
	if err != nil {
		return nil, err
	}
	if input.Call.ID == "" || run == nil || run.Scope != input.Run.Scope || run.ID != input.Run.ID || input.Call.DeploymentID != input.Bound.Binding.DeploymentID {
		return nil, errors.New("routine execution does not match its persisted Agent run")
	}
	if err := runbookActionOwner(ctx, teams, run, input.Bound); err != nil {
		return nil, err
	}
	switch input.Bound.Action.Name {
	case RunbookActionList:
		var args struct {
			Offset int `json:"offset"`
		}
		if err := decodeScheduledTaskArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		if args.Offset < 0 {
			return nil, errors.New("invalid routine offset")
		}
		values, err := store.ListRunbookActivations(ctx, RunbookActivationFilter{Scope: run.Scope, Owner: &run.Owner, Limit: 50, Offset: args.Offset})
		if err != nil {
			return nil, err
		}
		result := map[string]interface{}{"activations": values}
		if len(values) == 50 {
			result["nextOffset"] = args.Offset + 50
		}
		return result, nil
	case RunbookActionEventSources:
		return dispatchRoutineEventSources(ctx, store, events, run, input.Arguments)
	case RunbookActionSendTestEvent:
		return dispatchRoutineTestEvent(ctx, store, events, run, input.Arguments)
	case RunbookActionCreateTask:
		if input.Call.IdempotencyKey == "" {
			return nil, errors.New("scheduled task requires action idempotency")
		}
		// The ID is known before Create so a webhook routine's hook can be
		// named after it and made first.
		activationID := RunbookActivationIDForKey(run.Scope, input.Call.IdempotencyKey)
		args, task, schedule, err := resolveScheduledTaskCreate(ctx, store, files, events, run, activationID, input.Arguments)
		if err != nil {
			return nil, err
		}
		if task.Script != nil {
			// Applying the approved brief approves its script version now.
			approved := time.Now().UTC()
			task.Script.ApprovedAt = &approved
		}
		objective, err := NewPortfolioService(store).CreateObjectiveIdempotent(ctx, CreateObjectiveRequest{Scope: run.Scope, Owner: run.Owner, Title: args.Title, Goal: task.Goal, Status: ObjectiveStatusActive, IdempotencyKey: input.Call.IdempotencyKey, ExecutionPolicy: &ObjectiveExecutionPolicy{MaximumConcurrentRuns: 1}, Actor: ActivityActor{Type: "agent", ID: run.AssignedAgentID}, Visibility: ActivityVisibilityScope})
		if err != nil {
			return nil, err
		}
		request := CreateRunbookActivationRequest{ID: activationID, Scope: run.Scope, Owner: run.Owner, ObjectiveID: objective.Objective.ID, AssignedAgentID: run.AssignedAgentID, Task: task, TriggerID: "schedule", Trigger: runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: schedule}, MaximumConcurrent: 1, IdempotencyKey: input.Call.IdempotencyKey}
		if task.Event != nil {
			request.TriggerID, request.Trigger = "event", runbook.Trigger{Kind: runbook.TriggerEvent}
			if task.Event.Webhook != nil {
				// The hook exists before the activation commits; one whose
				// activation never commits is removed by PruneHooks.
				if _, err := store.CreateHook(ctx, &Hook{ID: task.Event.Webhook.HookID, Scope: run.Scope, ActivationID: activationID}); err != nil {
					return nil, err
				}
			}
		}
		activation, err := NewRunbookActivationService(store).Create(ctx, request)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionCreateTask, "activation": activation, "objective": objective.Objective, "replayed": !objective.Created}, nil
	case RunbookActionSetStatus:
		// Replay after the lifecycle update committed but the action receipt did not.
		var args scheduledTaskStatusArguments
		if err := decodeScheduledTaskArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		current, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
		if err != nil {
			return nil, err
		}
		if current == nil || current.Owner != run.Owner {
			return nil, errors.New("routine is not owned by this chat")
		}
		// The action checkpoint uses CAS; arbitrary stale revisions remain conflicts.
		if current.LifecycleActionID == input.Call.ID && current.Status == args.Status {
			return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetStatus, "activation": current, "replayed": true}, nil
		}
		_, owned, err := resolveOwnedTaskStatus(ctx, store, run, input.Arguments)
		if err != nil {
			return nil, err
		}
		update := UpdateRunbookActivationRequest{ExpectedRevision: args.ExpectedRevision, Status: args.Status, ActionID: input.Call.ID}
		if args.Status == RunbookActivationActive {
			update.CreatedBy = routineChangeCreator(owned, run)
		}
		activation, err := NewRunbookActivationService(store).Update(ctx, run.Scope, current.ID, update)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetStatus, "activation": activation, "replayed": false}, nil
	case RunbookActionSetScript:
		var args scheduledTaskSetScriptArguments
		if err := decodeScheduledTaskArguments(input.Arguments, &args); err != nil {
			return nil, err
		}
		current, err := store.GetRunbookActivation(ctx, run.Scope, args.ActivationID)
		if err != nil {
			return nil, err
		}
		if current == nil || current.Owner != run.Owner {
			return nil, errors.New("routine is not owned by this chat")
		}
		// Replay after the re-pin committed but the action receipt did not.
		if current.LifecycleActionID == input.Call.ID && current.Task != nil && current.Task.Script != nil && current.Task.Script.Digest == args.Digest && current.Task.Script.FilesVersion == args.FilesVersion {
			if err := authorizeRoutineChange(current, run); err != nil {
				return nil, err
			}
			return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetScript, "activation": current, "replayed": true}, nil
		}
		_, a, script, err := resolveOwnedTaskScript(ctx, store, files, run, input.Arguments)
		if err != nil {
			return nil, err
		}
		if err := authorizeRoutineChange(a, run); err != nil {
			return nil, err
		}
		activation, err := NewRunbookActivationService(store).RepinScript(ctx, run.Scope, a.ID, args.ExpectedRevision, script, input.Call.ID, routineChangeCreator(a, run))
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"resourceType": runbookActivationResourceType, "operation": RunbookActionSetScript, "activation": activation, "replayed": false}, nil
	}
	return nil, errors.New("unsupported scheduled task action")
}

// validateScheduledTaskApp proves the app exists, belongs to the routine's
// owner and was made in the routine's conversation.
func validateScheduledTaskApp(ctx context.Context, store runbookActionStore, run *AgentRun, task *ScheduledAgentTask) error {
	artifacts, ok := store.(interface {
		GetArtifact(context.Context, Scope, string, int64) (*Artifact, error)
	})
	if !ok {
		return errors.New("apps are unavailable for routines on this host")
	}
	app, err := artifacts.GetArtifact(ctx, run.Scope, task.AppID, 0)
	if err != nil || app == nil || app.Type != "app" || app.Provenance.Owner == nil || *app.Provenance.Owner != run.Owner ||
		app.Metadata["conversationId"] != task.ConversationID {
		return errors.New("scheduled task app must be an app from this chat")
	}
	return nil
}
