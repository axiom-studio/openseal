package openseal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/kerneltest"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
	"github.com/google/uuid"
)

// cutoverOrderStore records whether the run loop cutover had finished when a
// worker first claimed a Run.
type cutoverOrderStore struct {
	*sqlite.Store
	failList bool

	mu               sync.Mutex
	cutoverDone      bool
	claims           int
	claimsBeforeCut  int
	listedCandidates int
}

var errCutoverListFailed = errors.New("cutover candidates are unreadable")

func (s *cutoverOrderStore) ListRunLoopCutoverCandidates(ctx context.Context) ([]models.RunLoopCutoverRef, error) {
	if s.failList {
		return nil, errCutoverListFailed
	}
	refs, err := s.Store.ListRunLoopCutoverCandidates(ctx)
	s.mu.Lock()
	s.listedCandidates += len(refs)
	s.mu.Unlock()
	return refs, err
}

func (s *cutoverOrderStore) WithRunLoopCutoverLock(ctx context.Context, fn func(context.Context) error) error {
	err := s.Store.WithRunLoopCutoverLock(ctx, fn)
	if err == nil {
		s.mu.Lock()
		s.cutoverDone = true
		s.mu.Unlock()
	}
	return err
}

func (s *cutoverOrderStore) noteClaim() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claims++
	if !s.cutoverDone {
		s.claimsBeforeCut++
	}
}

func (s *cutoverOrderStore) ClaimNextAgentRun(ctx context.Context, claim models.AgentRunClaim) (*models.AgentRun, error) {
	s.noteClaim()
	return s.Store.ClaimNextAgentRun(ctx, claim)
}

func (s *cutoverOrderStore) ClaimNextAgentRunWithDecision(ctx context.Context, claim models.AgentRunClaim) (*models.AgentRunAdmissionDecision, error) {
	s.noteClaim()
	return s.Store.ClaimNextAgentRunWithDecision(ctx, claim)
}

func (s *cutoverOrderStore) counts() (claims, beforeCut int, done bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.claimsBeforeCut, s.cutoverDone
}

var engineCutoverScope = Scope{Kind: "tenant", ID: "engine-cutover"}

// oldEngineRun creates a Run and rewrites it as the old engine left it: no
// Engine marker, optionally claimed by an old worker first.
func oldEngineRun(t *testing.T, store *sqlite.Store, key string, claimed bool) *models.AgentRun {
	t.Helper()
	ctx := context.Background()
	owner := models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent-1"}
	created, err := agentrun.NewRunCommandService(store).CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: engineCutoverScope, Owner: owner, AssignedAgentID: owner.ID, ConcurrencyKey: "agent-1:" + key, Goal: "Do " + key,
		Source: models.RunSourceManual, IdempotencyKey: key, Actor: models.ActivityActor{Type: "user", ID: "7"},
	})
	if err != nil {
		t.Fatalf("create %s: %v", key, err)
	}
	if claimed {
		run, err := store.ClaimNextAgentRun(ctx, models.AgentRunClaim{
			Scope: engineCutoverScope, WorkerID: "old-worker", Now: time.Now(), LeaseDuration: time.Hour, AgingInterval: time.Minute,
		})
		if err != nil || run == nil || run.ID != created.Run.ID {
			t.Fatalf("old claim of %s = %#v, %v", key, run, err)
		}
	}
	current, err := store.GetAgentRun(ctx, engineCutoverScope, created.Run.ID)
	if err != nil || current == nil {
		t.Fatalf("load %s: %#v, %v", key, current, err)
	}
	old := *current
	old.Engine = ""
	old.SessionID = ""
	old.Revision = current.Revision + 1
	event := &models.ActivityEvent{
		ID: uuid.NewString(), Scope: old.Scope, EventType: "run.test_old_engine", Severity: models.ActivitySeverityInfo, RunID: old.ID,
		Actor: models.ActivityActor{Type: "system", ID: "test"}, Summary: "Old engine Run", Visibility: models.ActivityVisibilityScope, CreatedAt: time.Now(),
	}
	if _, err := store.UpdateAgentRunWithEvent(ctx, &old, current.Revision, event, nil); err != nil {
		t.Fatalf("rewrite %s as old: %v", key, err)
	}
	stored, _ := store.GetAgentRun(ctx, engineCutoverScope, old.ID)
	return stored
}

func TestEngineStartRunsCutoverBeforeWorkers(t *testing.T) {
	newEngine := func(t *testing.T, store *cutoverOrderStore, resolved chan<- *models.AgentRun) *Engine {
		t.Helper()
		resolver := conversation.TurnRunnerResolverFunc(func(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
			copied := *run
			select {
			case resolved <- &copied:
			default:
			}
			return nil, errors.New("no binding in this test")
		})
		engine, err := New(
			WithStore(store),
			WithAgentRunWorkers(runworker.AgentRunWorkerConfig{
				Scope: engineCutoverScope, Concurrency: 1, PollInterval: 10 * time.Millisecond, LeaseDuration: time.Minute,
			}, resolver),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { engine.Close() })
		return engine
	}

	t.Run("adopts and cancels before the first claim", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := &cutoverOrderStore{Store: kerneltest.NewStore(t)}
		running := oldEngineRun(t, store.Store, "running", true)
		queued := oldEngineRun(t, store.Store, "queued", false)
		resolved := make(chan *models.AgentRun, 4)
		engine := newEngine(t, store, resolved)

		if err := engine.Start(ctx); err != nil {
			t.Fatalf("start: %v", err)
		}
		defer engine.Stop()

		select {
		case run := <-resolved:
			if run.ID != queued.ID || run.Engine != models.RunEngineLoop {
				t.Fatalf("worker resolved %s with engine %q; want the adopted %s", run.ID, run.Engine, queued.ID)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the adopted Run was never claimed")
		}
		claims, beforeCut, done := store.counts()
		if !done || claims == 0 || beforeCut != 0 {
			t.Fatalf("claims = %d, before cutover = %d, cutover done = %v", claims, beforeCut, done)
		}
		canceled, err := store.GetAgentRun(ctx, engineCutoverScope, running.ID)
		if err != nil || canceled == nil || canceled.Status != models.AgentRunStatusCanceled {
			t.Fatalf("old running Run = %#v, %v; want canceled", canceled, err)
		}
		if canceled.FailureExplanation == nil || canceled.FailureExplanation.Code != runloop.CutoverCode {
			t.Fatalf("canceled Run explanation = %#v", canceled.FailureExplanation)
		}

		// A second start finds nothing left to cut over.
		engine.Stop()
		store.mu.Lock()
		store.listedCandidates = 0
		store.mu.Unlock()
		again := newEngine(t, store, make(chan *models.AgentRun, 1))
		if err := again.Start(ctx); err != nil {
			t.Fatalf("second start: %v", err)
		}
		again.Stop()
		store.mu.Lock()
		listed := store.listedCandidates
		store.mu.Unlock()
		if listed != 0 {
			t.Fatalf("second start listed %d cutover candidates", listed)
		}
	})

	t.Run("fails closed", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := &cutoverOrderStore{Store: kerneltest.NewStore(t), failList: true}
		oldEngineRun(t, store.Store, "queued", false)
		resolved := make(chan *models.AgentRun, 1)
		engine := newEngine(t, store, resolved)

		if err := engine.Start(ctx); !errors.Is(err, errCutoverListFailed) {
			t.Fatalf("start err = %v; want the cutover failure", err)
		}
		defer engine.Stop()
		time.Sleep(100 * time.Millisecond)
		if claims, _, _ := store.counts(); claims != 0 {
			t.Fatalf("workers claimed %d times after a failed cutover", claims)
		}
		select {
		case run := <-resolved:
			t.Fatalf("worker resolved %s after a failed cutover", run.ID)
		default:
		}
	})
}
