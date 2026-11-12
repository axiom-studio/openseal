package storetest

import (
	"fmt"
	"strings"
	"sync"

	"github.com/axiom-studio/openseal/pkg/models"
)

// Lane interleaving, compaction bound, concurrency (SPEC §4.2, §4.6, §4.7)

func conformParkedRunResumesAfterOtherRunAppended(f *SessionFixture) {
	runA, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, runA.ID, UserSessionEntry("uA", "first"))
	f.MustAppend(session.ID, runA.ID, AssistantSessionEntry("aA1", "sA1", "cA"), ToolSessionEntry("eA", "sA1", "cA", models.ToolEntryPending))
	f.MustTransition("eA", runA.ID, models.ToolEntryPending, models.ToolEntryAwaitingApproval)
	f.setStatus(runA.ID, models.AgentRunStatusWaitingForApproval)

	// The parked Run holds no lease.
	_, err := f.append(session.ID, runA.ID, UserSessionEntry("too-early", "x"))
	ExpectErr(f.T, "append by the parked run", err, models.ErrLeaseLost)

	runB := f.LeasedRun("agent-a", session.ID)
	if again := f.open(runB.ID, "agent-a"); again.ID != session.ID {
		f.T.Fatalf("lane run opened %s", again.ID)
	}
	f.MustAppend(session.ID, runB.ID, UserSessionEntry("uB", "second"))
	f.MustAppend(session.ID, runB.ID, AssistantSessionEntry("aB", "sB"))
	f.setStatus(runB.ID, models.AgentRunStatusCompleted)

	f.setStatus(runA.ID, models.AgentRunStatusRunning)
	f.MustTransition("eA", runA.ID, models.ToolEntryAwaitingApproval, models.ToolEntryRunning)
	f.MustTransition("eA", runA.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	f.MustAppend(session.ID, runA.ID, AssistantSessionEntry("aA2", "sA2"))

	path := f.Path(session.ID)
	if ids := PathEntryIDs(path); ids != "uA,aA1,eA,uB,aB,aA2" {
		f.T.Fatalf("interleaved path = %s", ids)
	}
	runs := make([]string, len(path.Entries))
	for i, entry := range path.Entries {
		runs[i] = map[string]string{runA.ID: "A", runB.ID: "B"}[entry.RunID]
	}
	if got := strings.Join(runs, ""); got != "AAABBA" {
		f.T.Fatalf("path runs = %s", got)
	}
	if path.Entries[2].State != models.ToolEntryCompleted {
		f.T.Fatalf("resumed tool entry state = %s", path.Entries[2].State)
	}
}

func conformCompactionCannotDropAssistantWithOpenToolEntry(f *SessionFixture) {
	runA, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, runA.ID, UserSessionEntry("u1", "a"))
	f.MustAppend(session.ID, runA.ID, AssistantSessionEntry("a1", "s1", "c1"), ToolSessionEntry("e1", "s1", "c1", models.ToolEntryPending))
	f.MustTransition("e1", runA.ID, models.ToolEntryPending, models.ToolEntryAwaitingApproval)
	f.setStatus(runA.ID, models.AgentRunStatusWaitingForApproval)

	// Another Run of the lane compacts: the open entry of Run A bounds the cut.
	runB := f.LeasedRun("agent-a", session.ID)
	f.MustAppend(session.ID, runB.ID, UserSessionEntry("u2", "b"))
	_, err := f.append(session.ID, runB.ID, compactionEntry("k1", "u2"))
	ExpectErr(f.T, "cut past an open call", err, models.ErrInvalidSessionEntry)
	_, err = f.append(session.ID, runB.ID, compactionEntry("k2", ""))
	ExpectErr(f.T, "keep nothing over an open call", err, models.ErrInvalidSessionEntry)
	_, err = f.append(session.ID, runB.ID, compactionEntry("k3", "e1"))
	ExpectErr(f.T, "cut at the tool entry", err, models.ErrInvalidSessionEntry)
	f.MustAppend(session.ID, runB.ID, compactionEntry("k4", "a1"))
	path := f.Path(session.ID)
	if path.Compaction == nil || path.Compaction.ID != "k4" || PathEntryIDs(path) != "a1,e1,u2" {
		f.T.Fatalf("path after compaction = %s", PathEntryIDs(path))
	}

	// Once the call settles, the cut may pass it.
	f.setStatus(runA.ID, models.AgentRunStatusRunning)
	f.MustTransition("e1", runA.ID, models.ToolEntryAwaitingApproval, models.ToolEntryRunning)
	f.MustTransition("e1", runA.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
	f.MustAppend(session.ID, runB.ID, compactionEntry("k5", "u2"))
	if ids := PathEntryIDs(f.Path(session.ID)); ids != "u2" {
		f.T.Fatalf("path after the second compaction = %s", ids)
	}
}

func conformLoadPathConsistentUnderConcurrentCompaction(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	f.MustAppend(session.ID, run.ID, UserSessionEntry("u0", "start"))
	const rounds = 25
	done := make(chan struct{})
	var writeErr error
	go func() {
		defer close(done)
		for i := 1; i <= rounds; i++ {
			id := fmt.Sprintf("u%d", i)
			if _, err := f.append(session.ID, run.ID, UserSessionEntry(id, id)); err != nil {
				writeErr = err
				return
			}
			if _, err := f.append(session.ID, run.ID, compactionEntry(fmt.Sprintf("k%d", i), id)); err != nil {
				writeErr = err
				return
			}
		}
	}()
	var wg sync.WaitGroup
	readErrs := make(chan error, 4)
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				path, err := f.Store.LoadAgentSessionPath(f.Ctx, f.Scope, session.ID)
				if err == nil {
					err = checkConcurrentCompactionPath(path)
				}
				if err != nil {
					readErrs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	<-done
	close(readErrs)
	if writeErr != nil {
		f.T.Fatalf("writer: %v", writeErr)
	}
	for err := range readErrs {
		f.T.Fatalf("reader: %v", err)
	}
	final := f.Path(session.ID)
	if final.Compaction == nil || final.Compaction.ID != fmt.Sprintf("k%d", rounds) || PathEntryIDs(final) != fmt.Sprintf("u%d", rounds) {
		f.T.Fatalf("final path = %s", PathEntryIDs(final))
	}
}

// checkConcurrentCompactionPath checks one path of the u_i / k_i(keep u_i)
// writer: it starts at the compaction's kept entry, holds at most that entry
// and the next one, never holds compactions, and its tokens add up.
func checkConcurrentCompactionPath(path *models.AgentSessionPath) error {
	var tokens int64
	if path.Compaction != nil {
		tokens = path.Compaction.Tokens
		if len(path.Entries) == 0 || path.Entries[0].ID != path.Compaction.Compaction.FirstKeptEntryID {
			return fmt.Errorf("path %s does not start at %s's kept entry", PathEntryIDs(path), path.Compaction.ID)
		}
		if path.Compaction.Sequence <= path.Entries[0].Sequence {
			return fmt.Errorf("compaction %s is not after its kept entry", path.Compaction.ID)
		}
	}
	if len(path.Entries) == 0 || len(path.Entries) > 2 {
		return fmt.Errorf("path %s has %d entries", PathEntryIDs(path), len(path.Entries))
	}
	for i, entry := range path.Entries {
		if entry.Type == models.SessionEntryCompaction {
			return fmt.Errorf("path holds compaction %s", entry.ID)
		}
		if i > 0 && entry.Sequence <= path.Entries[i-1].Sequence {
			return fmt.Errorf("path %s is out of order", PathEntryIDs(path))
		}
		if entry.Sequence > path.Session.NextSequence-1 {
			return fmt.Errorf("entry %s is past the session leaf", entry.ID)
		}
		tokens += entry.Tokens
	}
	if tokens != path.Tokens {
		return fmt.Errorf("path tokens = %d, entries add up to %d", path.Tokens, tokens)
	}
	return nil
}

func conformSixteenConcurrentTransitionsOneBatch(f *SessionFixture) {
	run, session := f.ConversationRun("agent-a")
	const calls = 16
	callIDs := make([]string, calls)
	for i := range callIDs {
		callIDs[i] = fmt.Sprintf("c%02d", i)
	}
	batch := []*models.SessionEntry{AssistantSessionEntry("a1", "s1", callIDs...)}
	for i, callID := range callIDs {
		batch = append(batch, ToolSessionEntry(fmt.Sprintf("e%02d", i), "s1", callID, models.ToolEntryPending))
	}
	f.MustAppend(session.ID, run.ID, batch...)
	var wg sync.WaitGroup
	errs := make([]error, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("e%02d", i)
			if _, err := f.transition(id, run.ID, models.ToolEntryPending, models.ToolEntryRunning); err != nil {
				errs[i] = err
				return
			}
			req := f.TransitionRequest(id, run.ID, models.ToolEntryRunning, models.ToolEntryCompleted)
			req.ExpectedRevision = 2
			req.Events = []*models.ActivityEvent{f.Event(run.ID, "Finished "+id)}
			_, errs[i] = f.Store.TransitionToolEntry(f.Ctx, req)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			f.T.Fatalf("transition %d: %v", i, err)
		}
	}
	open, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, SessionID: session.ID, States: openSessionToolStates()})
	if err != nil || len(open) != 0 {
		f.T.Fatalf("open entries after the batch = %s, %v", EntryIDs(open), err)
	}
	completed, err := f.Store.ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{Scope: f.Scope, StepID: "s1", States: []models.ToolEntryState{models.ToolEntryCompleted}})
	if err != nil || len(completed) != calls {
		f.T.Fatalf("completed entries = %d, %v", len(completed), err)
	}
	for _, entry := range completed {
		if entry.Revision != 3 {
			f.T.Fatalf("entry %s revision = %d", entry.ID, entry.Revision)
		}
	}
	if got := f.Session(session.ID).NextSequence; got != calls+2 {
		f.T.Fatalf("transitions changed NextSequence to %d", got)
	}
}
