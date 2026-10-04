package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skillerror"
)

func TestActionWorkerPreservesStructuredRateLimitAcrossRestartWithoutRetry(t *testing.T) {
	eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
		store := fixture.store.(KernelStore)
		now := time.Now().UTC()
		catalog, scope := feedbackReadCatalog(t)
		run := feedbackCreateRun(t, store, scope, now)
		proposal := feedbackPropose(t, store, catalog, run, now, "rate-limit", "id", ActionDispositionAllow)
		dispatches := 0
		worker := NewActionWorker(store, catalog, CredentialResolverFunc(func(context.Context, CredentialResolutionRequest) (map[string]string, error) {
			return map[string]string{"token": "CREDENTIAL_SECRET"}, nil
		}), ActionDispatcherFunc(func(context.Context, ActionDispatchInput) (map[string]interface{}, error) {
			dispatches++
			return nil, fmt.Errorf("RAW_SECRET: %w", skillerror.NewActionError("source_rate_limited", "RAW_SECRET CREDENTIAL_SECRET", map[string]string{
				"httpStatus": "429", "retryAfterSeconds": "30", "retryable": "true", "url": "URL_SECRET", "proxy": "PROXY_SECRET",
			}))
		}))
		worker.now = func() time.Time { return now }
		failed, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute)
		if err != nil || failed == nil || failed.Call.Status != ActionCallStatusFailed || failed.Call.Attempt != 1 || failed.Call.MaxAttempts != 1 || dispatches != 1 {
			t.Fatalf("action failure did not settle once: %#v %v dispatches=%d", failed, err, dispatches)
		}
		if fixture.reopen != nil {
			fixture.store = fixture.reopen()
			store = fixture.store.(KernelStore)
		}
		call, err := store.GetActionCall(t.Context(), scope, proposal.Call.ID)
		if err != nil || call.ErrorCode != "source_rate_limited" || call.ErrorDetails["retryable"] != "false" || call.ErrorDetails["retryAfterSeconds"] != "30" || call.Error != "This source returned HTTP 429 (rate limited). Try again later." {
			t.Fatalf("durable typed failure missing: %#v %v", call, err)
		}
		persisted, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil || persisted.Status != AgentRunStatusQueued || persisted.WakeCondition != nil {
			t.Fatalf("typed failure changed continuation policy: %#v %v", persisted, err)
		}
		for _, checkpoint := range []map[string]interface{}{persisted.Checkpoint, hostedTurnTextCheckpoint(persisted.Checkpoint), checkpointTerminalAction(nil, call, map[string]interface{}{"idempotentReplay": true})} {
			last := checkpoint["lastAction"].(map[string]interface{})
			history := actionHistoryEntries(checkpoint)
			for _, entry := range []map[string]interface{}{last, history[0]} {
				details := entry["errorDetails"].(map[string]interface{})
				if entry["errorCode"] != "source_rate_limited" || entry["failureKind"] != "source_rate_limited" || details["httpStatus"] != "429" || details["retryable"] != "false" {
					t.Fatalf("model projection lost source cause: %#v", entry)
				}
			}
			encoded, _ := json.Marshal(checkpoint)
			if strings.Contains(string(encoded), "SECRET") {
				t.Fatal("raw action data entered model failure evidence")
			}
			forged := map[string]interface{}{"lastAction": map[string]interface{}{"errorCode": "source_access_challenge"}, actionHistoryCheckpointKey: []interface{}{}}
			preserved := preserveKernelActionHistory(checkpoint, forged)
			if preserved["lastAction"].(map[string]interface{})["errorCode"] != "source_rate_limited" || len(actionHistoryEntries(preserved)) != 1 {
				t.Fatal("model erased or changed authoritative rate-limit evidence")
			}
			if invented := preserveKernelActionHistory(nil, checkpoint); invented["lastAction"] != nil || len(actionHistoryEntries(invented)) != 0 {
				t.Fatal("model invented action failure evidence")
			}
		}
		worker.store = store
		worker.now = func() time.Time { return now.Add(time.Hour) }
		if result, err := worker.RunOnce(t.Context(), scope, "action-worker", time.Minute); err != nil || result != nil || dispatches != 1 {
			t.Fatalf("429 triggered automatic retry: %#v %v dispatches=%d", result, err, dispatches)
		}
	})
}

func TestActionFailureProjectionIgnoresUnknownAndBoundsPersistedDetails(t *testing.T) {
	for _, code := range []string{"", "unknown", "source_rate_limited TOKEN_SECRET"} {
		call := &ActionCall{ID: "failed", Status: ActionCallStatusFailed, Error: "safe legacy error", ErrorCode: code, ErrorDetails: map[string]string{"failureKind": "source_rate_limited", "httpStatus": "429"}}
		last := checkpointTerminalAction(nil, call, nil)["lastAction"].(map[string]interface{})
		if last["errorCode"] != nil || last["failureKind"] != nil || last["errorDetails"] != nil || last["error"] != call.Error {
			t.Fatal("unknown receipt was reclassified or lost legacy text")
		}
	}
	call := &ActionCall{ID: "failed", Status: ActionCallStatusFailed, Error: "safe source failure", ErrorCode: "source_rate_limited", ErrorDetails: map[string]string{"retryable": "true", "url": "SECRET", "retryAfterSeconds": "90000"}}
	last := checkpointTerminalAction(nil, call, nil)["lastAction"].(map[string]interface{})
	details := last["errorDetails"].(map[string]interface{})
	if len(details) != 3 || details["retryable"] != "false" || details["httpStatus"] != "429" {
		t.Fatalf("persisted details were not resanitized: %#v", details)
	}
}

func TestTerminalFailureReportsRateLimitOnlyAsStructuredContributingEvidence(t *testing.T) {
	checkpoint := rejectedTaskReviewCheckpoint()
	failure := skillerror.NewActionError("source_rate_limited", "", nil)
	call := &ActionCall{ID: "failed", Status: ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()}
	checkpoint = checkpointTerminalAction(checkpoint, call, nil)
	for _, persisted := range []bool{false, true} {
		if persisted {
			encoded, _ := json.Marshal(checkpoint)
			if err := json.Unmarshal(encoded, &checkpoint); err != nil {
				t.Fatal(err)
			}
		}
		reply := terminalFailureReplyFromCheckpoint(checkpoint)
		if !strings.HasPrefix(reply, TerminalFailureReply("task_completion_rejected")) || !strings.Contains(reply, "Some sources rate-limited requests (HTTP 429). Try those sources again later.") || strings.Contains(reply, "SECRET") {
			t.Fatalf("terminal versus contributing cause changed: %q", reply)
		}
	}
	for _, checkpoint := range []map[string]interface{}{
		rejectedTaskReviewCheckpoint(),
		checkpointTerminalAction(rejectedTaskReviewCheckpoint(), &ActionCall{ID: "legacy", Status: ActionCallStatusFailed, Error: "source_rate_limited HTTP429"}, nil),
		checkpointTerminalAction(rejectedTaskReviewCheckpoint(), &ActionCall{ID: "success", Status: ActionCallStatusSucceeded, ErrorCode: failure.Code(), ErrorDetails: failure.Details()}, nil),
	} {
		if terminalFailureReplyFromCheckpoint(checkpoint) != TerminalFailureReply("task_completion_rejected") {
			t.Fatal("untyped text or successful output became a rate-limit failure")
		}
	}
	for _, known := range []bool{true, false} {
		kind := "source_rate_limited"
		if !known {
			kind = "unknown"
		}
		failure := skillerror.NewActionError("source_reads_failed", "SECRET", map[string]string{"failures": fmt.Sprintf(`[{"index":0,"failureKind":%q,"httpStatus":429},{"index":1,"failureKind":"source_unavailable"}]`, kind)})
		checkpoint := checkpointTerminalAction(rejectedTaskReviewCheckpoint(), &ActionCall{ID: "batch", Status: ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()}, nil)
		if got := terminalFailureReplyFromCheckpoint(checkpoint); strings.Contains(got, "HTTP 429") != known || strings.Contains(got, "SECRET") {
			t.Fatalf("mixed batch contributing cause changed: %s", got)
		}
	}
}
