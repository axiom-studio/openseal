package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/google/uuid"
)

// Cross-backend conformance suite of AgentSessionStore (run-loop section 01
// §10, SPEC §4.2/§4.6/§4.7). Each backend runs it from its own test:
// TestSQLiteAgentSessionStore and TestPostgresAgentSessionStore. The writer-model subtests of section 01 (2,
// 19, 30 and the promotion half of 31) are gone (A2); promotion is tested by
// store-8.

// AgentSessionContractStore is a backend under test. Runs are written with
// CreateAgentRun and leased with ClaimNextAgentRun.
type AgentSessionContractStore interface {
	models.AgentSessionStore
	models.PortfolioStore
	models.RunActivityStore
	models.AgentRunScheduleStore
}

// CreateRunSessionInOwnTx runs a backend's in-transaction
// createRunAgentSessionTx helper in a transaction of its own (forks).
type CreateRunSessionInOwnTx func(ctx context.Context, req models.CreateRunAgentSessionTxRequest) (*models.OpenAgentSessionResult, error)

// SessionWorker is the worker that leases the fixture's Runs.
const SessionWorker = "session-worker"

// SessionFixture writes Runs, sessions and entries to one store in one
// scope at one clock. Tests outside the suite build their own with T, Ctx,
// Store, Scope and Now set.
type SessionFixture struct {
	T     *testing.T
	Ctx   context.Context
	Store AgentSessionContractStore
	// createRunSession is set only by the conformance suite, for fork.
	createRunSession CreateRunSessionInOwnTx
	Scope            models.Scope
	Now              time.Time
}

// RunAgentSessionContract runs every subtest of the suite on a fresh store
// from open; open also returns the backend's in-transaction run-session
// create, which the fork subtests call.
func RunAgentSessionContract(t *testing.T, open func(t *testing.T) (AgentSessionContractStore, CreateRunSessionInOwnTx)) {
	t.Helper()
	for _, sub := range agentSessionConformanceSubtests {
		sub := sub
		t.Run(sub.name, func(t *testing.T) {
			store, createRunSession := open(t)
			f := &SessionFixture{
				T:                t,
				Ctx:              context.Background(),
				Store:            store,
				createRunSession: createRunSession,
				Scope:            models.Scope{Kind: "tenant", ID: "session-" + uuid.NewString()[:8]},
				Now:              time.Now().UTC().Truncate(time.Millisecond),
			}
			sub.run(f)
		})
	}
}

var agentSessionConformanceSubtests = []struct {
	name string
	run  func(*SessionFixture)
}{
	{"OpenConversationCreatesOnceAndSeedsFlag", conformOpenConversationCreatesOnce},
	{"OpenConversationRequiresLease", conformOpenConversationRequiresLease},
	{"TeamMembersGetSeparateSessions", conformTeamMembersGetSeparateSessions},
	{"AppendAssignsDenseSequences", conformAppendAssignsDenseSequences},
	{"AppendFencedByLease", conformAppendFencedByLease},
	{"AppendReplaysSameBatch", conformAppendReplaysSameBatch},
	{"AppendSkipsExistingSourceKey", conformAppendSkipsExistingSourceKey},
	{"AppendWatermarkIsMonotonic", conformAppendWatermarkIsMonotonic},
	{"AppendEventsAreAtomic", conformAppendEventsAreAtomic},
	{"AppendToolEntriesNeedTheirAssistant", conformAppendToolEntriesNeedTheirAssistant},
	{"AppendRejectsOversize", conformAppendRejectsOversize},
	{"ModelChangeUpdatesSessionModel", conformModelChangeUpdatesSessionModel},
	{"TransitionHappyPath", conformTransitionHappyPath},
	{"TransitionCASOneWinner", conformTransitionCASOneWinner},
	{"TransitionRejectsInvalidEdges", conformTransitionRejectsInvalidEdges},
	{"TransitionResultRules", conformTransitionResultRules},
	{"TransitionFencedByEntryRun", conformTransitionFencedByEntryRun},
	{"LoadPathNoCompaction", conformLoadPathNoCompaction},
	{"LoadPathFromLatestCompaction", conformLoadPathFromLatestCompaction},
	{"LoadPathCompactionKeepsNothing", conformLoadPathCompactionKeepsNothing},
	{"LoadPathAcrossFork", conformLoadPathAcrossFork},
	{"LoadPathCompactionInParentAboveFork", conformLoadPathCompactionInParentAboveFork},
	{"LoadPathChildCompactionKeepsParentEntries", conformLoadPathChildCompactionKeepsParentEntries},
	{"CompactionFirstKeptMustBeOnPathAndNotTool", conformCompactionFirstKeptMustBeOnPathAndNotTool},
	{"CreateRunSessionForksAtParentLeaf", conformCreateRunSessionForksAtParentLeaf},
	{"CreateRunSessionIsIdempotent", conformCreateRunSessionIsIdempotent},
	{"ForkDepthLimit", conformForkDepthLimit},
	{"ListOpenToolEntriesByRun", conformListOpenToolEntriesByRun},
	{"ListByActionCallID", conformListByActionCallID},
	{"ParkedRunResumesAfterOtherRunAppended", conformParkedRunResumesAfterOtherRunAppended},
	{"CompactionCannotDropAssistantWithOpenToolEntry", conformCompactionCannotDropAssistantWithOpenToolEntry},
	{"LoadPathConsistentUnderConcurrentCompaction", conformLoadPathConsistentUnderConcurrentCompaction},
	{"SixteenConcurrentTransitionsOneBatch", conformSixteenConcurrentTransitionsOneBatch},
	{"WatermarkCASConflict", conformWatermarkCASConflict},
	{"CursorAdvancesWithAppend", conformCursorAdvancesWithAppend},
	{"SessionMovedRejected", conformSessionMovedRejected},
	{"TerminalRunNoteWithoutLease", conformTerminalRunNoteWithoutLease},
}
