package runtime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation/conversationtest"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// CTL-6a: AgentRequest create applies the delegating tool entry's transition
// and the RequireInterventionCount fence in its own transaction.

type delegateTxStore interface {
	storetest.AgentSessionContractStore
	models.CollaborationStore
}

func TestDelegateCreateTransitionsEntryAtomically(t *testing.T) {
	backends := []struct {
		name string
		open func(t *testing.T) delegateTxStore
	}{
		{"Memory", func(*testing.T) delegateTxStore { return conversationtest.NewStore(t) }},
		{"SQLite", func(t *testing.T) delegateTxStore { return openCollaborationSessionStore(t) }},
	}
	for _, backend := range backends {
		backend := backend
		t.Run(backend.name, func(t *testing.T) {
			for _, sub := range delegateTxSubtests {
				sub := sub
				t.Run(sub.name, func(t *testing.T) {
					store := backend.open(t)
					f := &storetest.SessionFixture{
						T: t, Ctx: context.Background(), Store: store,
						Scope: models.Scope{Kind: "tenant", ID: "delegate-" + uuid.NewString()[:8]},
						Now:   time.Now().UTC().Truncate(time.Millisecond),
					}
					sub.run(&delegateTxFixture{SessionFixture: f, collab: store})
				})
			}
		})
	}
}

type delegateTxFixture struct {
	*storetest.SessionFixture
	collab models.CollaborationStore
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
func (f *delegateTxFixture) record(run *models.AgentRun, requestID string) models.AgentRequestCreateRecord {
	f.T.Helper()
	request := &models.AgentRequest{
		ID: requestID, Scope: f.Scope, Kind: models.AgentRequestKindRequest, Status: models.AgentRequestStatusPending,
		Requester:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: run.AssignedAgentID},
		Recipient:   models.CollaborationParty{Type: models.OwnerTypeAgent, ID: "agent-b"},
		SourceRunID: run.ID, Goal: "Write the brief", AcceptancePolicy: models.AgentRequestAcceptanceRecipientReview,
		IdempotencyKey: "delegate-e1", Revision: 1, CreatedAt: f.Now, UpdatedAt: f.Now,
	}
	source, err := sourceAwaitingAgentRequestDecision(run, request, f.Now)
	if err != nil {
		f.T.Fatalf("source wait: %v", err)
	}
	transition := models.ToolEntryTransitionRequest{
		Scope: f.Scope, EntryID: "e1", RunID: run.ID, Lease: f.Lease(),
		From: models.ToolEntryPending, To: models.ToolEntryAwaitingChild,
		Wait:   &models.ToolEntryWait{AgentRequestID: requestID},
		Kernel: &models.ToolEntryKernelPatch{Control: json.RawMessage(`{"kind":"delegate","requestId":"` + requestID + `"}`)},
		Now:    f.Now,
	}
	zero := len(run.PendingInterventions)
	return models.AgentRequestCreateRecord{
		Request: request, SourceRun: source, ExpectedSourceRevision: run.Revision,
		Event:          collaborationEvent(run, request, "collaboration.requested", "Requested work from agent agent-b", request.Requester, f.Now),
		ToolTransition: &transition, RequireInterventionCount: &zero,
	}
}

func (f *delegateTxFixture) assertNothingWritten(runID string, sourceRevision int64, entryState models.ToolEntryState) {
	f.T.Helper()
	if request, err := f.collab.GetAgentRequest(f.Ctx, f.Scope, "req-1"); err == nil && request != nil {
		f.T.Fatalf("request was created: %+v", request)
	}
	if found, err := f.collab.FindAgentRequestByIdempotencyKey(f.Ctx, f.Scope, "delegate-e1"); err == nil && found != nil {
		f.T.Fatalf("idempotency key was reserved: %+v", found)
	}
	run := f.Run(runID)
	if run.Revision != sourceRevision || run.Status != models.AgentRunStatusRunning {
		f.T.Fatalf("source = %s rev %d, want running rev %d", run.Status, run.Revision, sourceRevision)
	}
	if entry := f.Entry("e1"); entry.State != entryState || entry.Tool.Wait != nil {
		f.T.Fatalf("entry = %s wait %+v, want %s", entry.State, entry.Tool.Wait, entryState)
	}
}

func delegateTxCommits(f *delegateTxFixture) {
	run, _ := f.RunWithPendingCall()
	if _, err := f.collab.CreateAgentRequest(f.Ctx, f.record(run, "req-1")); err != nil {
		f.T.Fatalf("create: %v", err)
	}
	entry := f.Entry("e1")
	if entry.State != models.ToolEntryAwaitingChild || entry.Revision != 2 || entry.Tool.Wait == nil || entry.Tool.Wait.AgentRequestID != "req-1" {
		f.T.Fatalf("entry = %s rev %d wait %+v", entry.State, entry.Revision, entry.Tool.Wait)
	}
	var control struct{ Kind, RequestID string }
	if err := json.Unmarshal(entry.Tool.Control, &control); err != nil || control.Kind != "delegate" || control.RequestID != "req-1" {
		f.T.Fatalf("control = %s, %v", entry.Tool.Control, err)
	}
	request, err := f.collab.GetAgentRequest(f.Ctx, f.Scope, "req-1")
	if err != nil || request.Status != models.AgentRequestStatusPending {
		f.T.Fatalf("request = %+v, %v", request, err)
	}
	source := f.Run(run.ID)
	if source.Status != models.AgentRunStatusWaitingForAgent || source.WakeCondition == nil || source.WakeCondition.Reference != "req-1" || source.Revision != run.Revision+1 {
		f.T.Fatalf("source = %s %+v rev %d", source.Status, source.WakeCondition, source.Revision)
	}
}

func delegateTxEntryConflict(f *delegateTxFixture) {
	run, _ := f.RunWithPendingCall()
	f.MustTransition("e1", run.ID, models.ToolEntryPending, models.ToolEntryRunning)
	run = f.Run(run.ID)
	_, err := f.collab.CreateAgentRequest(f.Ctx, f.record(run, "req-1"))
	storetest.ExpectErr(f.T, "create over a moved entry", err, models.ErrToolEntryStateConflict)
	if entry := f.Entry("e1"); entry.State != models.ToolEntryRunning {
		f.T.Fatalf("entry = %s", entry.State)
	}
	if request, err := f.collab.GetAgentRequest(f.Ctx, f.Scope, "req-1"); err == nil && request != nil {
		f.T.Fatal("request was created")
	}
	if found, err := f.collab.FindAgentRequestByIdempotencyKey(f.Ctx, f.Scope, "delegate-e1"); err == nil && found != nil {
		f.T.Fatalf("idempotency key was reserved: %+v", found)
	}
	if source := f.Run(run.ID); source.Revision != run.Revision || source.Status != models.AgentRunStatusRunning {
		f.T.Fatalf("source = %s rev %d", source.Status, source.Revision)
	}
}

func delegateTxSteering(f *delegateTxFixture) {
	run, _ := f.RunWithPendingCall()
	record := f.record(run, "req-1") // count read at plan time: 0
	run = f.UpdateRun(run.ID, func(next *models.AgentRun) {
		next.PendingInterventions = append(next.PendingInterventions, models.AgentRunIntervention{
			ID: "i1", Actor: models.ActivityActor{Type: "user", ID: "u1"}, Instruction: "use the new deck", CreatedAt: f.Now,
		})
	})
	// Re-plan the source write on the fresh revision, keeping the stale count.
	fresh := f.record(run, "req-1")
	fresh.RequireInterventionCount = record.RequireInterventionCount
	_, err := f.collab.CreateAgentRequest(f.Ctx, fresh)
	storetest.ExpectErr(f.T, "create after a new intervention", err, models.ErrSteeringArrived)
	f.assertNothingWritten(run.ID, run.Revision, models.ToolEntryPending)

	// The current count is accepted.
	if _, err := f.collab.CreateAgentRequest(f.Ctx, f.record(run, "req-1")); err != nil {
		f.T.Fatalf("create with current count: %v", err)
	}
	if entry := f.Entry("e1"); entry.State != models.ToolEntryAwaitingChild {
		f.T.Fatalf("entry = %s", entry.State)
	}
}

func delegateTxLeaseLost(f *delegateTxFixture) {
	run, _ := f.RunWithPendingCall()
	record := f.record(run, "req-1")
	record.ToolTransition.Lease = f.ExpiredLease()
	_, err := f.collab.CreateAgentRequest(f.Ctx, record)
	storetest.ExpectErr(f.T, "create with an expired lease", err, models.ErrLeaseLost)
	f.assertNothingWritten(run.ID, run.Revision, models.ToolEntryPending)
}

func delegateTxForeignRun(f *delegateTxFixture) {
	run, _ := f.RunWithPendingCall()
	record := f.record(run, "req-1")
	record.ToolTransition.RunID = "other-run"
	_, err := f.collab.CreateAgentRequest(f.Ctx, record)
	storetest.ExpectErr(f.T, "create with another Run's entry", err, models.ErrInvalidToolEntryTransition)
	f.assertNothingWritten(run.ID, run.Revision, models.ToolEntryPending)
}
