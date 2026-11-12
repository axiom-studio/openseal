package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

func TestCatchUpWatermarkConflictRetries(t *testing.T) {
	ForCatchUpStores(t, ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, func(t *testing.T, f *CatchUpFixture) {
		runA, session := f.open("")
		first := f.say("m1", "from Maya")
		// Run A parks; Run B of another person runs on the same lane session.
		runB := f.run(session.ID, true)
		second := f.say("m2", "from Lee")
		stale := cloneAgentSession(session)

		// B catches up through m2 while A is preparing from the stale watermark.
		bPreparer := &catchUpPreparer{f: f, through: second.Sequence}
		aPreparer := &catchUpPreparer{f: f, through: second.Sequence}
		aPreparer.before = func() {
			if _, err := appendStepContext(t.Context(), f.store, agentrun.StepInput{Run: runB, Step: &AgentTurn{Sequence: 1}, Session: stale, WorkerID: "w1"}, []agentrun.StepPreparer{bPreparer}, f.now); err != nil {
				t.Fatalf("run B context: %v", err)
			}
		}
		third := f.say("m3", "Maya again")
		aPreparer.through = third.Sequence
		result, err := appendStepContext(t.Context(), f.store, agentrun.StepInput{Run: runA, Step: &AgentTurn{Sequence: 2}, Session: stale, WorkerID: "w1"}, []agentrun.StepPreparer{aPreparer}, f.now)
		if err != nil {
			t.Fatalf("run A context: %v", err)
		}
		if aPreparer.calls != 2 {
			t.Fatalf("run A preparer ran %d times, want a retry after the watermark conflict", aPreparer.calls)
		}
		if result.Session.ChannelWatermark != third.Sequence {
			t.Fatalf("watermark = %d, want %d", result.Session.ChannelWatermark, third.Sequence)
		}
		entries := f.sessionEntries(session.ID)
		if got := CatchUpMessageIDs(entries); !reflect.DeepEqual(got, []string{first.ID, second.ID, third.ID}) {
			t.Fatalf("interleaved session entries = %v", got)
		}
		if entries[0].RunID != runB.ID || entries[2].RunID != runA.ID {
			t.Fatalf("entries ingested by %s, %s, %s", entries[0].RunID, entries[1].RunID, entries[2].RunID)
		}

		// A conflict on every try gives up with the error (the loop requeues).
		moving := &catchUpPreparer{f: f, through: third.Sequence}
		var bump func()
		bump = func() {
			next := f.say(fmt.Sprintf("bump-%d", moving.calls), "more")
			current, err := f.store.GetAgentSession(t.Context(), session.Scope, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			commitB := f.collect(f.request(runB, current, next.Sequence))
			f.commit(runB, current, commitB)
			moving.through = next.Sequence
			moving.before = bump
		}
		moving.before = bump
		if _, err := appendStepContext(t.Context(), f.store, agentrun.StepInput{Run: runA, Step: &AgentTurn{Sequence: 3}, Session: result.Session, WorkerID: "w1"}, []agentrun.StepPreparer{moving}, f.now); !errors.Is(err, ErrChannelWatermarkMoved) || moving.calls != stepContextAttempts {
			t.Fatalf("endless watermark conflicts = %v after %d tries, want ErrChannelWatermarkMoved after %d", err, moving.calls, stepContextAttempts)
		}
	})
}

// catchUpPreparer is the conversation preparer's catch-up part, enough to
// drive the loop's phase-3 append and its watermark-conflict retries.
type catchUpPreparer struct {
	f       *CatchUpFixture
	through int64
	calls   int
	before  func()
}

func (p *catchUpPreparer) BeforeStep(ctx context.Context, in agentrun.StepInput, _ agentrun.SteeringPlan) (*agentrun.StepContextAppend, error) {
	p.calls++
	if p.before != nil {
		before := p.before
		p.before = nil
		before()
	}
	result, err := p.f.catchUp.Collect(ctx, p.f.request(in.Run, in.Session, p.through))
	if err != nil {
		return nil, err
	}
	return result.StepContextAppend(in.Session), nil
}
