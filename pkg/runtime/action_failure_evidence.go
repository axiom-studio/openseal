package runtime

import (
	"context"
	"maps"

	"github.com/axiom-studio/openseal/pkg/skillerror"
)

// A failed action may settle while its operator has paused the owning Run.
// Preserve that pause, then close the proven throttled attempt when explicitly
// resumed and claimed, before resolving credentials or calling a model.
func (p *AgentRunWorkerPool) stopSourceRateLimitedRun(ctx context.Context, workerID string, run *AgentRun) bool {
	code := terminalFailureCodeFromCheckpoint(run.Checkpoint)
	if code != "action_failed" && code != "execution_failed" {
		return false
	}
	failure := latestCanonicalActionFailure(run.Checkpoint)
	if failure == nil || !failure.HasRateLimitedSource() {
		return false
	}
	failed, _, err := p.activity.TransitionRun(ctx, run.Scope, run.ID, RunTransitionRequest{
		ExpectedRevision: run.Revision, Status: AgentRunStatusFailed, LeaseOwner: workerID,
		Error: failure.Error(), Summary: "Run stopped after a source rate limit", EventType: "run.failed",
		Actor: ActivityActor{Type: "worker", ID: workerID},
	})
	if err != nil {
		p.logger.Warnw("failed to settle source rate limit", "runId", run.ID, "error", err)
		return true
	}
	p.finalizeTerminalRun(ctx, failed)
	p.projectTerminalReporting(ctx, failed)
	p.resolveForkChild(ctx, failed)
	p.resolveCollaborationChild(ctx, failed)
	return true
}

// latestCanonicalActionFailure requires both protected projections to identify
// the same latest failed receipt with the exact sanitized type, message and
// details. Older failures and model-supplied text are not a current cause.
func latestCanonicalActionFailure(checkpoint map[string]interface{}) *skillerror.ActionError {
	last, _ := checkpoint["lastAction"].(map[string]interface{})
	lastID, _ := last["actionCallId"].(string)
	if lastID == "" {
		return nil
	}
	history := actionHistoryEntries(checkpoint)
	if len(history) == 0 {
		return nil
	}
	entry := history[len(history)-1]
	if entry["actionCallId"] != lastID {
		return nil
	}
	failure, recorded := canonicalActionFailureProjection(last), canonicalActionFailureProjection(entry)
	if failure == nil || recorded == nil || failure.Code() != recorded.Code() || !maps.Equal(failure.Details(), recorded.Details()) {
		return nil
	}
	return failure
}

func canonicalActionFailureProjection(entry map[string]interface{}) *skillerror.ActionError {
	if entry["status"] != ActionCallStatusFailed && entry["status"] != string(ActionCallStatusFailed) {
		return nil
	}
	code, _ := entry["errorCode"].(string)
	if code == "" || entry["failureKind"] != code {
		return nil
	}
	raw, _ := entry["errorDetails"].(map[string]interface{})
	details := make(map[string]string, len(raw))
	for key, value := range raw {
		text, ok := value.(string)
		if !ok {
			return nil
		}
		details[key] = text
	}
	failure := skillerror.NewActionError(code, "", details)
	if failure == nil || failure.Code() != code || entry["error"] != failure.Error() || !maps.Equal(details, failure.Details()) {
		return nil
	}
	return failure
}
