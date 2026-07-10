package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

type mutableWorkerScopeSource struct {
	mu     sync.Mutex
	scopes []Scope
	err    error
}

func (s *mutableWorkerScopeSource) ListWorkerScopes(context.Context) ([]Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Scope(nil), s.scopes...), s.err
}

func (s *mutableWorkerScopeSource) set(scopes []Scope, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = append([]Scope(nil), scopes...)
	s.err = err
}

func TestAgentRunWorkerSupervisorExecutesOnlyConfiguredKindAcrossScopes(t *testing.T) {
	store := NewMemoryStore(50)
	ctx := context.Background()
	scope1 := Scope{Kind: "tenant", ID: "1"}
	scope2 := Scope{Kind: "tenant", ID: "2"}
	portfolio := NewPortfolioService(store)
	conversationRuns := make([]*AgentRun, 0, 2)
	for _, scope := range []Scope{scope1, scope2} {
		run, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, Kind: RunKindConversation, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-" + scope.ID},
			Goal: "Coordinate channel", Source: RunSourceChat,
		})
		if err != nil {
			t.Fatal(err)
		}
		conversationRuns = append(conversationRuns, run)
	}
	agentRun, err := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
		Scope: scope1, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Ordinary work",
	})
	if err != nil {
		t.Fatal(err)
	}

	seen := make(chan *AgentRun, 2)
	resolver := TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		seen <- cloneAgentRun(run)
		return &TurnRunnerBinding{Runner: TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) {
			return &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, OutputSummary: "Conversation coordinated"}, nil
		})}, nil
	})
	source := &mutableWorkerScopeSource{scopes: []Scope{scope2, scope1, scope1}}
	supervisor, err := NewAgentRunWorkerSupervisor(store, resolver, source, zap.NewNop().Sugar(), DynamicAgentRunWorkerConfig{
		Kind: RunKindConversation, Concurrency: 1, PollInterval: 5 * time.Millisecond,
		ReconcileInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		WorkerIDPrefix: "conversation",
	})
	if err != nil {
		t.Fatal(err)
	}
	supervisor.Start(ctx)
	supervisor.Start(ctx)
	defer supervisor.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		completed := 0
		for _, run := range conversationRuns {
			current, getErr := portfolio.GetAgentRun(ctx, run.Scope, run.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if current.Status == AgentRunStatusCompleted {
				completed++
			}
		}
		if completed == len(conversationRuns) && supervisor.ScopeCount() == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if supervisor.ScopeCount() != 2 {
		t.Fatalf("scope pools = %d", supervisor.ScopeCount())
	}
	for range conversationRuns {
		select {
		case run := <-seen:
			if run.Kind != RunKindConversation {
				t.Fatalf("resolver received run kind %q", run.Kind)
			}
		case <-time.After(time.Second):
			t.Fatal("conversation run was not executed")
		}
	}
	currentAgent, err := portfolio.GetAgentRun(ctx, scope1, agentRun.ID)
	if err != nil || currentAgent.Status != AgentRunStatusQueued {
		t.Fatalf("agent work was claimed by conversation worker: %#v, %v", currentAgent, err)
	}

	source.set([]Scope{scope2, Scope{Kind: "", ID: "invalid"}}, nil)
	deadline = time.Now().Add(time.Second)
	for supervisor.ScopeCount() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if supervisor.ScopeCount() != 1 {
		t.Fatalf("removed scope was not stopped: %d", supervisor.ScopeCount())
	}
	source.set(nil, errors.New("temporary scope failure"))
	time.Sleep(20 * time.Millisecond)
	if supervisor.ScopeCount() != 1 {
		t.Fatalf("source failure removed healthy pool: %d", supervisor.ScopeCount())
	}
	supervisor.Stop()
	supervisor.Stop()
	if supervisor.ScopeCount() != 0 {
		t.Fatalf("scope pools remained after stop: %d", supervisor.ScopeCount())
	}
}
