package runloop

import (
	"strconv"
	"strings"
)

// The exact model-facing texts of the D9 repeat guard (section 08 §5.6).

const (
	repeatNoteTextFormat       = "You already made this exact call at step {N} and got the same result. Use that result instead of calling again."
	repeatNoteWaitHint         = " If you are waiting for something to change, use wait before checking again."
	repeatRefusedTextFormat    = "Not run: this exact call was already made {times} in this run with the same result (last at step {N}). Use the earlier result, try a different step, or say what is blocking you."
	repeatLimitTextFormat      = "Not run: this exact call has now been repeated {times} without progress, so this run is stopping."
	repeatSuppressedTextFormat = "Not run: this exact call already succeeded at step {N}, so it was not repeated. If it really has to happen again, tell the user; a new request can do it. Its result was:"
	repeatSameStepTextFormat   = "Not run: this is the same call as {priorToolCallId} earlier in this step. Use its result."
	repeatRunStoppingText      = "Not run: this run is stopping because a call was repeated without progress."
)

// repeatTimes is "twice" for 2 and "{k} times" otherwise.
func repeatTimes(k int) string {
	if k == 2 {
		return "twice"
	}
	return strconv.Itoa(k) + " times"
}

func repeatStepText(format string, priorStep int64) string {
	return strings.ReplaceAll(format, "{N}", strconv.FormatInt(priorStep, 10))
}

// repeatNoteText is the NoteRepeat text after the 2nd identical call with the
// same result. waitHint adds the wait sentence (the step offered wait and the
// call is a read or stateful one).
func repeatNoteText(priorStep int64, waitHint bool) string {
	text := repeatStepText(repeatNoteTextFormat, priorStep)
	if waitHint {
		text += repeatNoteWaitHint
	}
	return text
}

// repeatRefusedText is the repeat_refused text for the count-th identical
// call (3 or 4): "{times}" is count − 1.
func repeatRefusedText(count int, priorStep int64) string {
	return strings.ReplaceAll(repeatStepText(repeatRefusedTextFormat, priorStep), "{times}", repeatTimes(count-1))
}

// repeatLimitText is the repeat_limit text for the count-th identical call
// (5 or more), which fails the Run.
func repeatLimitText(count int) string {
	return strings.ReplaceAll(repeatLimitTextFormat, "{times}", repeatTimes(count))
}

// repeatSuppressedText is the duplicate_suppressed text; the prior entry's
// result blocks follow it.
func repeatSuppressedText(priorStep int64) string {
	return repeatStepText(repeatSuppressedTextFormat, priorStep)
}

// repeatSameStepText is the same_step_duplicate text.
func repeatSameStepText(priorToolCallID string) string {
	return strings.ReplaceAll(repeatSameStepTextFormat, "{priorToolCallId}", priorToolCallID)
}
