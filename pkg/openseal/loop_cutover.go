package openseal

import (
	"context"
	"errors"

	"github.com/axiom-studio/openseal/pkg/runtime"
)

// RunLoopCutoverReport counts what one cutover pass did.
type RunLoopCutoverReport = runtime.RunLoopCutoverReport

// RunLoopCutover moves the Engine's store onto the run loop (D16, A16): Runs
// created before the loop are adopted when they never started and canceled
// otherwise, with one visible message per chat lane. It must run before any
// worker starts and fails closed: on error the caller must not start workers.
func (e *Engine) RunLoopCutover(ctx context.Context) (RunLoopCutoverReport, error) {
	if e == nil || e.store == nil {
		return RunLoopCutoverReport{}, errors.New("run loop cutover: engine store is not configured")
	}
	store, ok := e.store.(runtime.RunLoopCutoverStore)
	if !ok {
		return RunLoopCutoverReport{}, errors.New("run loop cutover: the kernel store does not support the cutover")
	}
	var conversations runtime.RunLoopCutoverConversations
	if e.conversations != nil {
		conversations = e.conversations
	}
	return runtime.RunLoopCutover(ctx, store, conversations)
}
