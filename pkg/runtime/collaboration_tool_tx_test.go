package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

// CTL-6a: AgentRequest create applies the delegating tool entry's transition
// and the RequireInterventionCount fence in its own transaction.

type delegateTxStore interface {
	agentSessionConformanceStore
	CollaborationStore
}

func TestDelegateCreateTransitionsEntryAtomically(t *testing.T) {
	backends := []struct {
		name string
		open func(t *testing.T) delegateTxStore
	}{
		{"Memory", func(*testing.T) delegateTxStore { return NewMemoryStore() }},
		{"SQLite", func(t *testing.T) delegateTxStore { return openTestSQLiteSessionStore(t) }},
	}
	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			for _, sub := range delegateTxSubtests {
				sub := sub
				t.Run(sub.name, func(t *testing.T) {
					store := backend.open(t)
					f := &sessionConformance{
						t: t, ctx: context.Background(), store: store,
						scope: Scope{Kind: "tenant", ID: "delegate-" + uuid.NewString()[:8]},
						now:   time.Now().UTC().Truncate(time.Millisecond),
					}
					sub.run(&delegateTxFixture{sessionConformance: f, collab: store})
				})
			}
		})
	}
}

type delegateTxFixture struct {
	*sessionConformance
	collab CollaborationStore
}

var delegateTxSubtests = []struct {
	name string
	run  func(*delegateTxFixture)
}{
	{"CommitsEntryRequestAndSourceTogether", delegateTxCommits},
	{"EntryConflictWritesNothing", delegateTxEntryConflict},
	{"NewInterventionRefused", delegateTxSteering},
	{"LostLeaseWritesNothing", delegateTxLeaseLost},
	{"TransitionOfAnotherRunRejected", delegateTxForeignRun},
}

// record builds the singular create record a delegate settle sends: the
// request, the source parked on agent_request_decision, and the entry moving
// pending -> awaiting_child.
func (f *delegateTxFixture) record(run *AgentRun, requestID string) AgentRequestCreateRecord {
	f.t.Helper()
	request := &AgentRequest{
		ID: requestID, Scope: f.scope, Kind: AgentRequestKindRequest, Status: AgentRequestStatusPending,
		Requester:   CollaborationParty{Type: OwnerTypeAgent, ID: run.AssignedAgentID},
		Recipient:   CollaborationParty{Type: OwnerTypeAgent, ID: "agent-b"},
		SourceRunID: run.ID, Goal: "Write the brief", AcceptancePolicy: AgentRequestAcceptanceRecipientReview,
		IdempotencyKey: "delegate-e1", Revision: 1, CreatedAt: f.now, UpdatedAt: f.now,
	}
	source, err := sourceAwaitingAgentRequestDecision(run, request, f.now)
	if err != nil {
		f.t.Fatalf("source wait: %v", err)
	}
	transition := ToolEntryTransitionRequest{
		Scope: f.scope, EntryID: "e1", RunID: run.ID, Lease: f.lease(),
		From: ToolEntryPending, To: ToolEntryAwaitingChild,
		Wait:   &ToolEntryWait{AgentRequestID: requestID},
		Kernel: &ToolEntryKernelPatch{Control: json.RawMessage(`{"kind":"delegate","requestId":"` + requestID + `"}`)},
		Now:    f.now,
	}
	zero := len(run.PendingInterventions)
	return AgentRequestCreateRecord{
		Request: request, SourceRun: source, ExpectedSourceRevision: run.Revision,
		Event:          collaborationEvent(run, request, "collaboration.requested", "Requested work from agent agent-b", request.Requester, f.now),
		ToolTransition: &transition, RequireInterventionCount: &zero,
	}
}

func (f *delegateTxFixture) assertNothingWritten(runID string, sourceRevision int64, entryState ToolEntryState) {
	f.t.Helper()
	if request, err := f.collab.GetAgentRequest(f.ctx, f.scope, "req-1"); err == nil && request != nil {
		f.t.Fatalf("request was created: %+v", request)
	}
	if found, err := f.collab.FindAgentRequestByIdempotencyKey(f.ctx, f.scope, "delegate-e1"); err == nil && found != nil {
		f.t.Fatalf("idempotency key was reserved: %+v", found)
	}
	run := f.run(runID)
	if run.Revision != sourceRevision || run.Status != AgentRunStatusRunning {
		f.t.Fatalf("source = %s rev %d, want running rev %d", run.Status, run.Revision, sourceRevision)
	}
	if entry := f.entry("e1"); entry.State != entryState || entry.Tool.Wait != nil {
		f.t.Fatalf("entry = %s wait %+v, want %s", entry.State, entry.Tool.Wait, entryState)
	}
}

func delegateTxCommits(f *delegateTxFixture) {
	run, _ := f.runWithPendingCall()
	if _, err := f.collab.CreateAgentRequest(f.ctx, f.record(run, "req-1")); err != nil {
		f.t.Fatalf("create: %v", err)
	}
	entry := f.entry("e1")
	if entry.State != ToolEntryAwaitingChild || entry.Revision != 2 || entry.Tool.Wait == nil || entry.Tool.Wait.AgentRequestID != "req-1" {
		f.t.Fatalf("entry = %s rev %d wait %+v", entry.State, entry.Revision, entry.Tool.Wait)
	}
	var control struct{ Kind, RequestID string }
	if err := json.Unmarshal(entry.Tool.Control, &control); err != nil || control.Kind != "delegate" || control.RequestID != "req-1" {
		f.t.Fatalf("control = %s, %v", entry.Tool.Control, err)
	}
	request, err := f.collab.GetAgentRequest(f.ctx, f.scope, "req-1")
	if err != nil || request.Status != AgentRequestStatusPending {
		f.t.Fatalf("request = %+v, %v", request, err)
	}
	source := f.run(run.ID)
	if source.Status != AgentRunStatusWaitingForAgent || source.WakeCondition == nil || source.WakeCondition.Reference != "req-1" || source.Revision != run.Revision+1 {
		f.t.Fatalf("source = %s %+v rev %d", source.Status, source.WakeCondition, source.Revision)
	}
}

func delegateTxEntryConflict(f *delegateTxFixture) {
	run, _ := f.runWithPendingCall()
	f.mustTransition("e1", run.ID, ToolEntryPending, ToolEntryRunning)
	run = f.run(run.ID)
	_, err := f.collab.CreateAgentRequest(f.ctx, f.record(run, "req-1"))
	expectErr(f.t, "create over a moved entry", err, ErrToolEntryStateConflict)
	if entry := f.entry("e1"); entry.State != ToolEntryRunning {
		f.t.Fatalf("entry = %s", entry.State)
	}
	if request, err := f.collab.GetAgentRequest(f.ctx, f.scope, "req-1"); err == nil && request != nil {
		f.t.Fatal("request was created")
	}
	if found, err := f.collab.FindAgentRequestByIdempotencyKey(f.ctx, f.scope, "delegate-e1"); err == nil && found != nil {
		f.t.Fatalf("idempotency key was reserved: %+v", found)
	}
	if source := f.run(run.ID); source.Revision != run.Revision || source.Status != AgentRunStatusRunning {
		f.t.Fatalf("source = %s rev %d", source.Status, source.Revision)
	}
}

func delegateTxSteering(f *delegateTxFixture) {
	run, _ := f.runWithPendingCall()
	record := f.record(run, "req-1") // count read at plan time: 0
	run = f.updateRun(run.ID, func(next *AgentRun) {
		next.PendingInterventions = append(next.PendingInterventions, AgentRunIntervention{
			ID: "i1", Actor: ActivityActor{Type: "user", ID: "u1"}, Instruction: "use the new deck", CreatedAt: f.now,
		})
	})
	// Re-plan the source write on the fresh revision, keeping the stale count.
	fresh := f.record(run, "req-1")
	fresh.RequireInterventionCount = record.RequireInterventionCount
	_, err := f.collab.CreateAgentRequest(f.ctx, fresh)
	expectErr(f.t, "create after a new intervention", err, ErrSteeringArrived)
	f.assertNothingWritten(run.ID, run.Revision, ToolEntryPending)

	// The current count is accepted.
	if _, err := f.collab.CreateAgentRequest(f.ctx, f.record(run, "req-1")); err != nil {
		f.t.Fatalf("create with current count: %v", err)
	}
	if entry := f.entry("e1"); entry.State != ToolEntryAwaitingChild {
		f.t.Fatalf("entry = %s", entry.State)
	}
}

func delegateTxLeaseLost(f *delegateTxFixture) {
	run, _ := f.runWithPendingCall()
	record := f.record(run, "req-1")
	record.ToolTransition.Lease = f.expiredLease()
	_, err := f.collab.CreateAgentRequest(f.ctx, record)
	expectErr(f.t, "create with an expired lease", err, ErrLeaseLost)
	f.assertNothingWritten(run.ID, run.Revision, ToolEntryPending)
}

func delegateTxForeignRun(f *delegateTxFixture) {
	run, _ := f.runWithPendingCall()
	record := f.record(run, "req-1")
	record.ToolTransition.RunID = "other-run"
	_, err := f.collab.CreateAgentRequest(f.ctx, record)
	expectErr(f.t, "create with another Run's entry", err, ErrInvalidToolEntryTransition)
	f.assertNothingWritten(run.ID, run.Revision, ToolEntryPending)
}
