package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func refusedLoopCallRequest(f *loopStoreFixture, prior *ActionCall, code string) RefusedLoopCallRequest {
	return RefusedLoopCallRequest{
		Scope: f.scope, Run: f.run, StepID: "step-5", DeploymentID: prior.DeploymentID,
		SkillID: prior.SkillID, SkillVersion: prior.SkillVersion, Action: prior.Action,
		Risk: prior.Risk, SideEffect: prior.SideEffect,
		InputSchema: map[string]interface{}{"properties": map[string]interface{}{"token": map[string]interface{}{"type": "string", "x-sensitive": true}}},
		Arguments:   map[string]interface{}{"path": prior.ID, "apiKey": "secret"},
		Code:        code, Message: "Not run: this exact call was already made twice in this run with the same result (last at step 2).",
		DuplicateOfActionCallID: prior.ID, PriorStep: 2, Count: 3,
		Lease: &AgentRunLeaseGuard{WorkerID: loopStoreWorker, Now: f.now},
	}
}

func TestNewRefusedLoopCallRow(t *testing.T) {
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		prior := f.propose(f.loopCall("prior"))
		coordinator := NewActionCoordinator(store, store, nil, nil)
		coordinator.now = func() time.Time { return f.now }

		result, err := coordinator.RecordRefusedLoopCall(f.ctx, refusedLoopCallRequest(f, prior, RefusedLoopCallRepeat))
		if err != nil || result == nil || !result.Created || result.Run != nil {
			t.Fatalf("record refused = %#v, %v", result, err)
		}
		call, err := store.GetActionCall(f.ctx, f.scope, result.Call.ID)
		if err != nil {
			t.Fatal(err)
		}
		if call.Status != ActionCallStatusRefused || call.Origin != ActionOriginLoop || call.Execution != ActionExecutionLoop ||
			call.TurnID != "step-5" || call.ErrorCode != RefusedLoopCallRepeat || call.DuplicateOfActionCallID != prior.ID ||
			call.MaxAttempts != 1 || call.CompletedAt == nil || call.ExternalOperationDigest != "" || call.IdempotencyKey != "" {
			t.Fatalf("refused row = %#v", call)
		}
		if call.ErrorDetails["step"] != "2" || call.ErrorDetails["count"] != "3" {
			t.Fatalf("error details = %#v", call.ErrorDetails)
		}
		if _, ok := call.Arguments["apiKey"]; ok || call.Arguments["path"] != prior.ID {
			t.Fatalf("arguments = %#v", call.Arguments)
		}
		if call.InvocationDigest == "" || call.SemanticDigest != ComputeActionSemanticDigest(call) {
			t.Fatalf("digests = %q %q", call.InvocationDigest, call.SemanticDigest)
		}
		event := result.Event
		if event == nil || event.EventType != ActivityEventActionRefused || event.Summary != "Skipped a repeated call" || event.TurnID != "step-5" ||
			event.Payload["actionCallId"] != call.ID || event.Payload["duplicateOfActionCallId"] != prior.ID || event.Payload["errorCode"] != RefusedLoopCallRepeat ||
			event.Payload["skillId"] != prior.SkillID || event.Payload["action"] != prior.Action {
			t.Fatalf("event = %#v", event)
		}
		// A refused row is terminal: no worker ever claims it.
		if claimed := f.poolClaim(f.now.Add(time.Hour)); claimed != nil && claimed.ID == call.ID {
			t.Fatalf("pool claimed refused row %#v", claimed)
		}
		if f.currentRun().Revision != f.run.Revision {
			t.Fatal("recording a refused row changed the Run")
		}
	})
}

func TestNewRefusedLoopCallDuplicateSuppressedSummary(t *testing.T) {
	f := newLoopStoreFixture(t, NewMemoryStore())
	prior := f.propose(f.loopCall("prior"))
	coordinator := NewActionCoordinator(f.store, f.store, nil, nil)
	_, event, err := coordinator.NewRefusedLoopCall(refusedLoopCallRequest(f, prior, RefusedLoopCallDuplicateSuppressed))
	if err != nil || event.Summary != "Already done; not repeated" {
		t.Fatalf("event = %#v, %v", event, err)
	}
	for _, code := range []string{"same_step_duplicate", "run_stopping", ""} {
		if _, _, err := coordinator.NewRefusedLoopCall(refusedLoopCallRequest(f, prior, code)); err == nil {
			t.Fatalf("code %q got a refused row", code)
		}
	}
	missingPrior := refusedLoopCallRequest(f, prior, RefusedLoopCallRepeatLimit)
	missingPrior.DuplicateOfActionCallID = ""
	if _, _, err := coordinator.NewRefusedLoopCall(missingPrior); err == nil {
		t.Fatal("a refused row without its prior call was built")
	}
}

func TestRefusedLoopCallRequiresLiveLease(t *testing.T) {
	f := newLoopStoreFixture(t, NewMemoryStore())
	prior := f.propose(f.loopCall("prior"))
	coordinator := NewActionCoordinator(f.store, f.store, nil, nil)
	req := refusedLoopCallRequest(f, prior, RefusedLoopCallRepeat)
	req.Lease = &AgentRunLeaseGuard{WorkerID: "other-worker", Now: f.now}
	if _, err := coordinator.RecordRefusedLoopCall(f.ctx, req); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("foreign lease err = %v", err)
	}
	req.Lease = nil
	if _, err := coordinator.RecordRefusedLoopCall(f.ctx, req); err == nil {
		t.Fatal("refused row recorded without a lease")
	}
}

func TestRefusedStatusValidatesWithoutExternalDigest(t *testing.T) {
	now := time.Now().UTC()
	call := &ActionCall{
		ID: "refused", Scope: Scope{Kind: "tenant", ID: "t"}, RunID: "run", TurnID: "step-3", DeploymentID: "agent",
		SkillID: "repo", SkillVersion: "1.0.0", Action: "read_file", Origin: ActionOriginLoop, Execution: ActionExecutionLoop,
		Status: ActionCallStatusRefused, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead,
		Arguments: map[string]interface{}{}, DuplicateOfActionCallID: "prior", MaxAttempts: 1, Revision: 1,
		AvailableAt: now, CreatedAt: now, UpdatedAt: now, CompletedAt: &now,
	}
	if err := call.Validate(); err != nil {
		t.Fatalf("refused duplicate without digest: %v", err)
	}
	call.Status = ActionCallStatusFailed
	if err := call.Validate(); err == nil {
		t.Fatal("a non-refused duplicate without an external operation digest validated")
	}
}

func TestRefusedDoesNotProtectExternalOperation(t *testing.T) {
	if externalOperationProtects(ActionCallStatusRefused) {
		t.Fatal("refused protects an external operation")
	}
	runLoopStoreScenario(t, func(t *testing.T, store KernelStore) {
		f := newLoopStoreFixture(t, store)
		digest := "sha256:0000000000000000000000000000000000000000000000000000000000000abc"
		refused := f.loopCall("refused")
		refused.Status, refused.ExternalOperationDigest, refused.CompletedAt = ActionCallStatusRefused, digest, &f.now
		refused.ErrorCode = RefusedLoopCallRepeat
		f.propose(refused)
		if _, err := store.GetActionCallByExternalOperation(f.ctx, f.scope, digest); !errors.Is(err, ErrActionNotFound) {
			t.Fatalf("refused row protects external operation: %v", err)
		}
		// The operation stays free: a real call may still claim it.
		claimant := f.loopCall("claimant")
		claimant.ExternalOperationDigest = digest
		f.propose(claimant)
		found, err := store.GetActionCallByExternalOperation(f.ctx, f.scope, digest)
		if err != nil || found.ID != "claimant" {
			t.Fatalf("external operation owner = %#v, %v", found, err)
		}
	})
}

func TestOutreachReconcilesRefusedActionAsCanceled(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "research"}
	project, runs := seedExecutableMonitorProject(t, store, scope)
	sources := NewSourceMonitorService(store, store, store, store)
	ingested, err := sources.Ingest(context.Background(), sourceObservationRequest(scope, project.ID, "monitor-a", runs["monitor-a"], 0, "cursor-1", "thread-7", "Setup is confusing"))
	if err != nil {
		t.Fatal(err)
	}
	actionReader := &fixedOutreachActionReader{}
	service := NewOutreachService(store, store, store, actionReader)
	disclosure := "Disclosure: I work on OpenSeal and am seeking product feedback."
	body := "What part of setup was hardest for you? " + disclosure
	arguments := map[string]interface{}{"threadUrl": ingested.Observation.SourceURI, "message": body}
	created, _, err := service.Create(context.Background(), CreateOutreachThreadRequest{Thread: &OutreachThread{
		Scope: scope, ProjectID: project.ID, SourceObservationID: ingested.Observation.ID, MonitorID: "monitor-a",
		StableSourceID: "thread-7", TargetURI: ingested.Observation.SourceURI, Owner: project.Owner, AssignedAgentID: "researcher",
		SourcePolicyRef: "approved-forums", ApprovalPolicyRef: "review-outreach",
		Identity: OutreachIdentity{ProfileRef: "profile:openseal-research", DisplayName: "OpenSeal Research", Affiliation: "OpenSeal", Disclosure: disclosure},
		Messages: []OutreachMessage{{ID: "message-1", Direction: OutreachMessageOutbound, Intent: OutreachIntentRequestFeedback, Body: body, Status: OutreachMessageDraft,
			Capability: &OutreachCapability{SkillID: "forum-outreach", SkillVersion: "1.0.0", Action: "reply", TargetArgument: "threadUrl", BodyArgument: "message", Arguments: arguments}}},
	}, IdempotencyKey: "draft-1", Actor: ActivityActor{Type: "user", ID: "operator"}})
	if err != nil {
		t.Fatal(err)
	}
	actionReader.call = &ActionCall{ID: "action-reply", Scope: scope, RunID: "run-outreach", DeploymentID: "researcher", SkillID: "forum-outreach", SkillVersion: "1.0.0", Action: "reply",
		Status: ActionCallStatusRefused, SideEffect: skill.SideEffectExternal, Arguments: arguments, Error: "Not run: this exact call already succeeded at step 2.",
		EvidenceRefs: []string{ingested.Observation.ID}, IdempotencyKey: "outreach:message-1"}
	reconciled, err := service.ReconcileAction(context.Background(), scope, created.ID, ReconcileOutreachActionRequest{
		MessageID: "message-1", ActionCallID: "action-reply", Actor: ActivityActor{Type: "worker", ID: "action-worker"},
	})
	if err != nil || reconciled.Thread.Messages[0].Status != OutreachMessageCanceled {
		t.Fatalf("reconciled = %#v, %v", reconciled, err)
	}
}
