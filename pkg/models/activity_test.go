package models

import (
	"testing"
)

func TestActivityEventRejectsInvalidUsageDelta(t *testing.T) {
	event := &ActivityEvent{
		Scope: Scope{Kind: "tenant", ID: "one"}, RunID: "run", EventType: "turn.completed", Summary: "Completed",
		UsageDelta: &BudgetUsage{InputTokens: -1},
	}
	if err := event.Validate(); err == nil {
		t.Fatal("negative activity usage delta was accepted")
	}
}
