package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skillerror"
)

func TestTerminalSourceFailureLatestOutboxReportsExactCauseOnce(t *testing.T) {
	for _, code := range []string{"source_rate_limited", "source_reads_failed", "browser_proxy_authentication_failed", "browser_proxy_unavailable", "stale rate limit", "raw error"} {
		t.Run(code, func(t *testing.T) {
			eventWaitContractFixtures(t, func(t *testing.T, fixture eventWaitContractFixture) {
				store := terminalReportingContractFixture(t, fixture)
				now := eventWaitContractEpoch
				run, _, trigger := terminalFailureConversationFixture(t, store, now, true)
				kind := code
				if code == "stale rate limit" || code == "raw error" {
					kind = "source_rate_limited"
				}
				details := map[string]string{"url": "PRIVATE_URL", "token": "PRIVATE_TOKEN"}
				if code == "source_reads_failed" {
					details["failures"] = `[{"index":0,"failureKind":"source_rate_limited","httpStatus":429},{"index":1,"failureKind":"source_http_error","httpStatus":503}]`
				}
				failure := skillerror.NewActionError(kind, "PRIVATE_ERROR", details)
				checkpoint := checkpointTerminalAction(nil, &ActionCall{ID: "source-read", Status: ActionCallStatusFailed, Error: failure.Error(), ErrorCode: failure.Code(), ErrorDetails: failure.Details()}, nil)
				cause := failure.Error()
				if code == "stale rate limit" {
					checkpoint = checkpointTerminalAction(checkpoint, &ActionCall{ID: "newer-failed-action", Status: ActionCallStatusFailed, Error: "PRIVATE_CURRENT"}, nil)
					cause = "PRIVATE_CURRENT"
				}
				if code == "raw error" {
					cause = "PRIVATE_RAW HTTP 429"
				}
				activity := NewRunActivityService(store, store)
				activity.now = func() time.Time { return now }
				running, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: run.Revision, Status: AgentRunStatusRunning})
				if err != nil {
					t.Fatal(err)
				}
				failed, _, err := activity.TransitionRun(t.Context(), run.Scope, run.ID, RunTransitionRequest{ExpectedRevision: running.Revision, Status: AgentRunStatusFailed, Error: cause, Checkpoint: checkpoint})
				if err != nil {
					t.Fatal(err)
				}
				if fixture.reopen != nil {
					fixture.store = fixture.reopen()
					store = terminalReportingContractFixture(t, fixture)
				}
				if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 1 {
					t.Fatalf("outbox: count=%d err=%v", count, err)
				}
				message, err := store.FindChannelMessageByIdempotencyKey(t.Context(), run.Scope, trigger.ConversationID, terminalConversationFailureReplyKey(failed))
				if err != nil || message == nil || message.Scope != run.Scope || message.ThreadRootID != trigger.ID || message.ReplyToMessageID != trigger.ID || message.ResolvesMessageID != trigger.ID || message.ResponseMode != trigger.ResponseMode {
					t.Fatalf("source failure lost provenance: %#v %v", message, err)
				}
				if strings.Contains(message.Content, "PRIVATE") {
					t.Fatalf("untrusted diagnostic leaked: %q", message.Content)
				}
				if code == "source_rate_limited" || code == "source_reads_failed" {
					if !strings.Contains(message.Content, "HTTP 429") || !strings.Contains(message.Content, "Try again later") {
						t.Fatalf("source rate limit hidden: %q", message.Content)
					}
				} else if strings.HasPrefix(code, "browser_proxy") {
					if !strings.Contains(message.Content, "platform's browser connection") || strings.Contains(message.Content, "HTTP 429") {
						t.Fatalf("proxy cause misattributed: %q", message.Content)
					}
				} else if message.Content != TerminalFailureReply("action_failed") {
					t.Fatalf("stale or raw evidence attributed: %q", message.Content)
				}
				if count, err := terminalReportingContractWorker(t, store, now).ProcessBatch(t.Context()); err != nil || count != 0 {
					t.Fatalf("duplicate failure: count=%d err=%v", count, err)
				}
			})
		})
	}
}
