package models

import (
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
)

func eventTaskActivationForValidation() *RunbookActivation {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	scope := Scope{Kind: "tenant", ID: "event-task"}
	hookID := HookID(scope, "activation-1")
	return &RunbookActivation{
		ID: "activation-1", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "researcher"}, ObjectiveID: "objective-1",
		AssignedAgentID: "researcher", TriggerID: "event", Trigger: runbook.Trigger{Kind: runbook.TriggerEvent},
		Task: &ScheduledAgentTask{
			Goal: "Summarize the new order.", ConversationID: "conversation-1", SourceMessageID: "message-1", CreatedBy: "user-1",
			Event: &RoutineEventTrigger{
				Kind: RoutineEventWebhook, SourceRef: HookSourceRef(hookID), SourceLabel: "your shop", MaxPerHour: 20,
				Webhook: &RoutineWebhook{HookID: hookID},
			},
		},
		Status: RunbookActivationActive, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func TestRunbookActivationEventTaskValidation(t *testing.T) {
	schedule := &runbook.Schedule{Cron: "0 0 9 * * *", Timezone: "UTC"}
	cases := []struct {
		name   string
		change func(*RunbookActivation)
		valid  bool
	}{
		{"event task", func(*RunbookActivation) {}, true},
		{"event task without creator", func(a *RunbookActivation) { a.Task.CreatedBy = "" }, false},
		{"event kind without event trigger", func(a *RunbookActivation) { a.Task.Event = nil }, false},
		{"event task with schedule", func(a *RunbookActivation) { a.Trigger.Schedule = schedule }, false},
		{"event task with entrypoint", func(a *RunbookActivation) { a.Trigger.Entrypoint = "run" }, false},
		{"event task with trigger reporting", func(a *RunbookActivation) {
			a.Trigger.Reporting = &runbook.ReportingPolicy{Channel: "orders", Title: "Orders"}
		}, false},
		{"event task with invalid event trigger", func(a *RunbookActivation) { a.Task.Event.MaxPerHour = 0 }, false},
		{"event task with cursor", func(a *RunbookActivation) {
			now := a.CreatedAt
			a.NextOccurrenceBase, a.NextRunAt = &now, &now
		}, false},
		{"event task with occurrence progress", func(a *RunbookActivation) { a.OccurrencesProcessed = 1 }, false},
		{"schedule task with event trigger", func(a *RunbookActivation) {
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: schedule}
		}, false},
		{"schedule task", func(a *RunbookActivation) {
			a.Task.Event = nil
			a.Task.CreatedBy = ""
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerSchedule, Schedule: schedule}
		}, true},
		{"definition schedule", func(a *RunbookActivation) {
			a.Task = nil
			a.DefinitionID, a.DefinitionVersion = "work", "1"
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerSchedule, Entrypoint: "run", Schedule: schedule}
		}, true},
		{"definition event activation", func(a *RunbookActivation) {
			a.Task = nil
			a.DefinitionID, a.DefinitionVersion = "work", "1"
			a.Trigger = runbook.Trigger{Kind: runbook.TriggerEvent, Entrypoint: "run"}
		}, false},
		{"paused with reason", func(a *RunbookActivation) {
			a.Status, a.PausedReason = RunbookActivationPaused, RoutinePausedNeedsSignIn
		}, true},
		{"active with reason", func(a *RunbookActivation) { a.PausedReason = RoutinePausedSourceRemoved }, false},
		{"paused with unknown reason", func(a *RunbookActivation) {
			a.Status, a.PausedReason = RunbookActivationPaused, "tired"
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			activation := eventTaskActivationForValidation()
			tc.change(activation)
			if err := activation.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	retired := eventTaskActivationForValidation()
	retired.Status = RunbookActivationRetired
	if retired.Callable() {
		t.Fatal("an event task the person retired is callable")
	}
	retired.Exhausted = true
	if !retired.Callable() || retired.Validate() != nil {
		t.Fatal("an exhausted event task is not callable on demand")
	}
	retired.Status = RunbookActivationPaused
	if retired.Callable() || retired.Validate() == nil {
		t.Fatal("a paused event task is callable or exhausted")
	}
}
