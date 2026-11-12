package models

import (
	"testing"
	"time"
)

func TestAgentRunSchedulingUsesAgingDeadlineAndFIFO(t *testing.T) {
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	oldLow := &AgentRun{ID: "old", Priority: 1, QueueEnteredAt: now.Add(-10 * time.Minute)}
	newHigh := &AgentRun{ID: "new", Priority: 5, QueueEnteredAt: now.Add(-time.Minute)}
	if !agentRunSchedulesBefore(oldLow, newHigh, now, time.Minute) {
		t.Fatal("wait aging did not prevent starvation")
	}
	early := now.Add(time.Hour)
	late := now.Add(2 * time.Hour)
	left := &AgentRun{ID: "left", Priority: 5, QueueEnteredAt: now, Deadline: &early}
	right := &AgentRun{ID: "right", Priority: 5, QueueEnteredAt: now, Deadline: &late}
	if !agentRunSchedulesBefore(left, right, now, time.Minute) {
		t.Fatal("earlier deadline did not break an equal-score tie")
	}
	left.Deadline, right.Deadline = nil, nil
	left.QueueEnteredAt = now.Add(-time.Second)
	if !agentRunSchedulesBefore(left, right, now, time.Minute) {
		t.Fatal("FIFO did not break an equal-score tie")
	}
}

func TestResourceQuantityValidationDistinguishesCapacityFromRequirement(t *testing.T) {
	objective := &ObjectiveExecutionPolicy{ResourceCapacities: map[string]int{"browser": 0}}
	if err := objective.Validate(); err != nil {
		t.Fatalf("zero capacity should disable a resource explicitly: %v", err)
	}
	run := &AgentRun{
		ID: "run", Scope: Scope{Kind: "local", ID: "test"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		Goal: "work", Status: AgentRunStatusQueued, Source: RunSourceManual, ResourceRequirements: map[string]int{"browser": 0},
		Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), AvailableAt: time.Now().UTC(), QueueEnteredAt: time.Now().UTC(),
	}
	if err := run.Validate(); err == nil {
		t.Fatal("zero Run resource requirement must be rejected")
	}
}
