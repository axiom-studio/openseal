package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skillerror"
)

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
