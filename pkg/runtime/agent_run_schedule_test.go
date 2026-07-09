package runtime

import (
	"context"
	"testing"
	"time"
)

func TestCreateAgentRunInitializesDurableQueueMetadata(t *testing.T) {
	store := NewMemoryStore(10)
	service := NewPortfolioService(store)
	now := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	available := now.Add(time.Hour)
	deadline := now.Add(2 * time.Hour)
	run, err := service.CreateAgentRun(context.Background(), CreateAgentRunRequest{
		Scope: Scope{Kind: "local", ID: "test"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
		AssignedAgentID: "agent", Goal: "scheduled work", Source: RunSourceSchedule,
		AvailableAt: &available, Deadline: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != AgentRunStatusQueued || !run.AvailableAt.Equal(available) || !run.QueueEnteredAt.Equal(now) ||
		run.Deadline == nil || !run.Deadline.Equal(deadline) || run.Revision != 1 {
		t.Fatalf("unexpected queued run: %#v", run)
	}
}

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
