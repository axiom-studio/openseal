package runtime

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
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/runworker"
	"go.uber.org/zap"
)

type mutableWorkerScopeSource struct {
	mu     sync.Mutex
	scopes []models.Scope
	err    error
}

func (s *mutableWorkerScopeSource) ListWorkerScopes(context.Context) ([]models.Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]models.Scope(nil), s.scopes...), s.err
}

func (s *mutableWorkerScopeSource) set(scopes []models.Scope, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scopes = append([]models.Scope(nil), scopes...)
	s.err = err
}

func TestAgentRunWorkerSupervisorExecutesOnlyConfiguredKindAcrossScopes(t *testing.T) {
	store := kerneltest.NewStore(t)
	ctx := context.Background()
	scope1 := models.Scope{Kind: "tenant", ID: "1"}
	scope2 := models.Scope{Kind: "tenant", ID: "2"}
	portfolio := portfolio.NewPortfolioService(store)
	conversationRuns := make([]*models.AgentRun, 0, 2)
	for _, scope := range []models.Scope{scope1, scope2} {
		run, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
			Scope: scope, Kind: models.RunKindConversation, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-" + scope.ID},
			Goal: "Coordinate channel", Source: models.RunSourceChat,
		})
		if err != nil {
			t.Fatal(err)
		}
		conversationRuns = append(conversationRuns, run)
	}
	agentRun, err := portfolio.CreateAgentRun(ctx, models.CreateAgentRunRequest{
		Scope: scope1, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "agent"}, Goal: "Ordinary work",
	})
	if err != nil {
		t.Fatal(err)
	}

	seen := make(chan *models.AgentRun, 2)
	resolver := conversation.TurnRunnerResolverFunc(func(_ context.Context, run *models.AgentRun) (*agentrun.TurnRunnerBinding, error) {
		seen <- models.CloneAgentRun(run)
		return &agentrun.TurnRunnerBinding{Producer: &timerStepProducer{}}, nil
	})
	source := &mutableWorkerScopeSource{scopes: []models.Scope{scope2, scope1, scope1}}
	supervisor, err := runworker.NewAgentRunWorkerSupervisor(store, resolver, source, zap.NewNop().Sugar(), runworker.DynamicAgentRunWorkerConfig{
		Kind: models.RunKindConversation, Concurrency: 1, PollInterval: 5 * time.Millisecond,
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
			if current.Status == models.AgentRunStatusCompleted {
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
			if run.Kind != models.RunKindConversation {
				t.Fatalf("resolver received run kind %q", run.Kind)
			}
		case <-time.After(time.Second):
			t.Fatal("conversation run was not executed")
		}
	}
	currentAgent, err := portfolio.GetAgentRun(ctx, scope1, agentRun.ID)
	if err != nil || currentAgent.Status != models.AgentRunStatusQueued {
		t.Fatalf("agent work was claimed by conversation worker: %#v, %v", currentAgent, err)
	}

	source.set([]models.Scope{scope2, models.Scope{Kind: "", ID: "invalid"}}, nil)
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
