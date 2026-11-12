package models

import (
	"testing"
)

func TestScheduledTaskCloneCopiesEventTrigger(t *testing.T) {
	original := &ScheduledAgentTask{
		Goal: "Summarize the new message.", ConversationID: "conversation-1", SourceMessageID: "message-1", CreatedBy: "user-1",
		Event: &RoutineEventTrigger{
			Kind: RoutineEventCalendar, SourceRef: RoutineGoogleSourceRef("user-1", "google-sub"), SourceLabel: "your calendar", MaxPerHour: 20,
			Filter:   []RoutineEventCondition{{Field: "title", Op: RoutineFilterOneOf, Values: []string{"Standup", "Review"}}},
			Calendar: &RoutineCalendarTiming{On: RoutineCalendarStartsIn, Minutes: 15},
			Webhook:  &RoutineWebhook{HookID: "hk_abc"},
		},
	}
	clone := CloneScheduledAgentTask(original)
	clone.Event.Filter[0].Values[0] = "Changed"
	clone.Event.Calendar.Minutes = 30
	clone.Event.Webhook.HookID = "hk_changed"
	clone.Event.SourceLabel = "changed"
	if original.Event.Filter[0].Values[0] != "Standup" || original.Event.Calendar.Minutes != 15 || original.Event.Webhook.HookID != "hk_abc" || original.Event.SourceLabel != "your calendar" {
		t.Fatalf("clone shares event trigger state: %#v", original.Event)
	}
	invalid := CloneScheduledAgentTask(original)
	invalid.Event.Webhook = nil
	invalid.Event.MaxPerHour = 0
	if err := invalid.Validate(); err == nil {
		t.Fatal("scheduled task accepted an invalid event trigger")
	}
}
