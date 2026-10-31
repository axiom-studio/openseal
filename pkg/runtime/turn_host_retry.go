package runtime

import "time"

// turnHostRetryCheckpointKey counts the consecutive Turns of one Run that
// failed because the turn host could not be reached. Only the kernel writes
// it; a completed Turn clears it.
const turnHostRetryCheckpointKey = "_opensealTurnHostRetries"

// turnHostRetryDelays are the waits before each requeued attempt. Once they
// are used up, the next unavailable host fails the Run.
var turnHostRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

func turnHostRetries(checkpoint map[string]interface{}) int {
	return toolFeedbackInteger(checkpoint[turnHostRetryCheckpointKey])
}

// checkpointTurnHostRetry records one more requeue on an otherwise unchanged
// checkpoint, so the retried Turn starts from the same state.
func checkpointTurnHostRetry(checkpoint map[string]interface{}) map[string]interface{} {
	result := deepCloneCheckpointMap(checkpoint)
	if result == nil {
		result = make(map[string]interface{})
	}
	result[turnHostRetryCheckpointKey] = turnHostRetries(checkpoint) + 1
	return result
}

// turnHostRetryAvailableAt is when a Run requeued by this failed Turn may be
// claimed again. It derives only from the durable Turn, so applying the Turn
// after a worker restart schedules the same attempt.
func turnHostRetryAvailableAt(turn *AgentTurn) *time.Time {
	if turn == nil || turn.Status != AgentTurnStatusFailed || turn.NextRunStatus != AgentRunStatusQueued || turn.CompletedAt == nil {
		return nil
	}
	retries := turnHostRetries(turn.ContinuationCheckpoint)
	if retries < 1 || retries > len(turnHostRetryDelays) {
		return nil
	}
	return new(turn.CompletedAt.Add(turnHostRetryDelays[retries-1]))
}
