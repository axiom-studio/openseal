package runtime

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

type snakesAndLaddersState struct {
	mu     sync.Mutex
	winner string
}

func (state *snakesAndLaddersState) winnerID() string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.winner
}

func (state *snakesAndLaddersState) declareWinner(agentID string) string {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.winner == "" {
		state.winner = agentID
	}
	return state.winner
}

type snakesAndLaddersRunner struct {
	state *snakesAndLaddersState
	rolls map[string][]int
	board map[int]int
}

func (runner *snakesAndLaddersRunner) RunTurn(_ context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	agentID := input.Run.AssignedAgentID
	if winner := runner.state.winnerID(); winner != "" && winner != agentID {
		return &TurnOutcome{
			NextRunStatus: AgentRunStatusCompleted,
			OutputSummary: fmt.Sprintf("%s observed that %s won", agentID, winner),
			RunOutput: map[string]interface{}{
				"winner": winner, "result": "lost", "rollLog": checkpointList(input.Run.Checkpoint, "rollLog"),
			},
		}, nil
	}

	position := checkpointInt(input.Run.Checkpoint, "position")
	rollIndex := checkpointInt(input.Run.Checkpoint, "rollIndex")
	sequence := runner.rolls[agentID]
	if len(sequence) == 0 {
		return nil, fmt.Errorf("no deterministic dice sequence for %s", agentID)
	}
	roll := sequence[rollIndex%len(sequence)]
	landing := position
	if position+roll <= 100 {
		landing = position + roll
	}
	destination := landing
	if jump, ok := runner.board[landing]; ok {
		destination = jump
	}
	log := append(checkpointList(input.Run.Checkpoint, "rollLog"), map[string]interface{}{
		"turn": rollIndex + 1, "roll": roll, "from": position, "landed": landing, "to": destination,
	})
	checkpoint := map[string]interface{}{"position": destination, "rollIndex": rollIndex + 1, "rollLog": log}
	summary := fmt.Sprintf("%s rolled %d and moved from %d to %d", agentID, roll, position, destination)

	if destination == 100 {
		winner := runner.state.declareWinner(agentID)
		result := "lost"
		if winner == agentID {
			result = "won"
		}
		return &TurnOutcome{
			NextRunStatus:          AgentRunStatusCompleted,
			OutputSummary:          summary,
			ContinuationCheckpoint: checkpoint,
			RunOutput: map[string]interface{}{
				"winner": winner, "result": result, "position": destination, "rollLog": log,
			},
		}, nil
	}
	if rollIndex == 0 {
		wakeAt := time.Now().UTC().Add(150 * time.Millisecond)
		return &TurnOutcome{
			NextRunStatus:          AgentRunStatusSleeping,
			WakeCondition:          &WakeCondition{Type: "timer", WakeAt: &wakeAt, Reference: "next-dice-roll"},
			OutputSummary:          summary + "; waiting durably for the next turn",
			ContinuationCheckpoint: checkpoint,
		}, nil
	}
	return &TurnOutcome{
		NextRunStatus:          AgentRunStatusRunning,
		OutputSummary:          summary,
		ContinuationCheckpoint: checkpoint,
	}, nil
}

func checkpointInt(checkpoint map[string]interface{}, key string) int {
	switch value := checkpoint[key].(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func checkpointList(checkpoint map[string]interface{}, key string) []interface{} {
	values, _ := checkpoint[key].([]interface{})
	return append([]interface{}(nil), values...)
}

func TestThreeAgentSnakesAndLaddersDurableTeamAcceptance(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "snakes-and-ladders.db")
	scope := Scope{Kind: "tenant", ID: "games"}
	capabilityScope := capability.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	agentIDs := []string{"player-alpha", "player-beta", "player-gamma"}

	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	agents := kernelagent.NewRegistryWithStore(store)
	for _, agentID := range agentIDs {
		definition, registerErr := agents.RegisterDefinition(ctx, &kernelagent.AgentDefinition{
			ID: agentID, Version: "1.0.0", DisplayName: strings.ReplaceAll(agentID, "-", " "),
			Purpose: "Play a durable turn-based game", SystemPrompt: "Take one bounded game turn and preserve the roll log.",
			Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		})
		if registerErr != nil {
			t.Fatal(registerErr)
		}
		if _, _, createErr := agents.CreateDeployment(ctx, &kernelagent.AgentDeployment{
			ID: agentID, Scope: capabilityScope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: kernelagent.RolloutActive, Environment: "acceptance",
			Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 1, MaxQueuedRuns: 2},
		}, "test", "acceptance", "create tournament player"); createErr != nil {
			t.Fatal(createErr)
		}
	}

	teams := kernelteam.NewRegistryWithStore(store, agents)
	teamDefinition, err := teams.RegisterDefinition(ctx, &kernelteam.Definition{
		ID: "board-game-team", Version: "1.0.0", DisplayName: "Board Game Team",
		Purpose: "Play independently and report the winner with a durable roll log",
		Roles: []kernelteam.RoleSlot{{
			ID: "player", DisplayName: "Player", Purpose: "Take bounded turns", MinimumMembers: 3, MaximumMembers: 3,
			ChannelParticipation: kernelteam.RoleChannelActive,
		}},
		Coordination: kernelteam.CoordinationPolicy{
			Mode: kernelteam.CoordinationPeer, MaximumSpeakersPerRound: 1, QuietByDefault: true,
			RequireRoleRelevance: true, SuppressDuplicateContent: true,
		},
		Delegation: kernelteam.DelegationPolicy{MaximumDepth: 1, MaximumConcurrent: 3},
		Approvals:  kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead},
	})
	if err != nil {
		t.Fatal(err)
	}
	roster := make([]kernelteam.RosterAssignment, 0, len(agentIDs))
	for _, agentID := range agentIDs {
		roster = append(roster, kernelteam.RosterAssignment{
			ID: agentID + "-seat", RoleID: "player", AgentDeploymentID: agentID, DisplayName: agentID,
		})
	}
	teamDeployment, _, err := teams.CreateDeployment(ctx, &kernelteam.Deployment{
		ID: "board-game-team", Scope: capabilityScope, DefinitionID: teamDefinition.ID, ActiveVersion: teamDefinition.Version,
		Roster: roster, Status: kernelteam.DeploymentActive,
		Restrictions: kernelteam.DeploymentRestrictions{MaximumRisk: capability.RiskLevelRead, MaximumConcurrency: 3},
	}, "test", "acceptance", "create tournament")
	if err != nil {
		t.Fatal(err)
	}

	portfolio := NewPortfolioService(store)
	runs := make(map[string]string, len(agentIDs))
	for _, agentID := range agentIDs {
		owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: agentID}
		gameObjective, createErr := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
			Scope: scope, Owner: owner, Title: "Play Snakes and Ladders", Goal: "Take bounded turns until one player wins",
			Status: ObjectiveStatusActive, Priority: 100,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		if _, createErr = portfolio.CreateObjective(ctx, CreateObjectiveRequest{
			Scope: scope, Owner: owner, Title: "Report tournament outcome", Goal: "Preserve and share the final result",
			Status: ObjectiveStatusActive, Priority: 50,
		}); createErr != nil {
			t.Fatal(createErr)
		}
		run, createErr := portfolio.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, ObjectiveID: gameObjective.ID, Owner: owner, AssignedAgentID: agentID,
			Goal: "Play the supplied board and retain every dice roll", Source: RunSourceObjective,
			Budget: &BudgetPolicy{MaxTurns: 20, MaxAttempts: 20, MaxDurationMS: int64((4 * time.Hour) / time.Millisecond)},
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		runs[agentID] = run.ID
	}
	if _, createErr := portfolio.CreateObjective(ctx, CreateObjectiveRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: teamDeployment.ID},
		Title: "Coordinate tournament", Goal: "Keep the channel quiet until a winner can report evidence",
		Status: ObjectiveStatusActive, Priority: 90,
	}); createErr != nil {
		t.Fatal(createErr)
	}
	conversations := NewConversationService(store)
	conversation, _, err := conversations.CreateConversation(ctx, CreateConversationRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: teamDeployment.ID},
		Title: "Snakes and Ladders", IdempotencyKey: "board-game-channel",
	})
	if err != nil {
		t.Fatal(err)
	}

	firstState := &snakesAndLaddersState{}
	firstPool := snakesAndLaddersWorkerPool(t, store, scope, snakesAndLaddersTestRunner(firstState))
	firstContext, stopFirst := context.WithCancel(ctx)
	firstPool.Start(firstContext)
	waitForGameRuns(t, store, scope, runs, func(run *AgentRun) bool {
		return run.Status == AgentRunStatusSleeping && run.LastAppliedTurn == 1
	})
	stopFirst()
	firstPool.Stop()
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restartedAgents := kernelagent.NewRegistryWithStore(restarted)
	restartedTeams := kernelteam.NewRegistryWithStore(restarted, restartedAgents)
	if _, getErr := restartedTeams.GetDeployment(ctx, capabilityScope, teamDeployment.ID); getErr != nil {
		t.Fatalf("Team did not survive restart: %v", getErr)
	}
	for _, agentID := range agentIDs {
		if _, getErr := restartedAgents.GetDeployment(ctx, capabilityScope, agentID); getErr != nil {
			t.Fatalf("Agent %s did not survive restart: %v", agentID, getErr)
		}
	}

	time.Sleep(175 * time.Millisecond)
	gameState := &snakesAndLaddersState{}
	secondPool := snakesAndLaddersWorkerPool(t, restarted, scope, snakesAndLaddersTestRunner(gameState))
	secondContext, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondPool.Start(secondContext)
	defer secondPool.Stop()
	waitForGameRuns(t, restarted, scope, runs, func(run *AgentRun) bool {
		return run.Status == AgentRunStatusCompleted
	})

	winner := gameState.winnerID()
	if winner != "player-alpha" {
		t.Fatalf("winner = %q, want player-alpha", winner)
	}
	winnerRun, err := restarted.GetAgentRun(ctx, scope, runs[winner])
	if err != nil {
		t.Fatal(err)
	}
	rollLog := winnerRun.Output["rollLog"]
	if winnerRun.Output["result"] != "won" || winnerRun.Output["position"] != float64(100) && winnerRun.Output["position"] != 100 {
		t.Fatalf("winner output = %#v", winnerRun.Output)
	}
	turns, err := restarted.ListAgentTurns(ctx, AgentTurnFilter{Scope: scope, RunID: winnerRun.ID})
	if err != nil || len(turns) < 3 {
		t.Fatalf("winner turns = %#v, err = %v", turns, err)
	}

	restartedConversation, err := NewConversationService(restarted).GetConversation(ctx, scope, conversation.ID)
	if err != nil {
		t.Fatal(err)
	}
	messageContent := fmt.Sprintf("%s won Snakes and Ladders. Durable roll log: %v", winner, rollLog)
	posted, err := NewConversationService(restarted).PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: scope, ConversationID: restartedConversation.ID, ExpectedRevision: restartedConversation.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: winner},
		Intent: MessageIntentUpdate, Content: messageContent,
		Audience:       ConversationAudience{Kind: ConversationAudienceChannel},
		IdempotencyKey: "winner-report",
	})
	if err != nil {
		t.Fatal(err)
	}
	if posted.Message.Sender.ID != winner || !strings.Contains(posted.Message.Content, "Durable roll log") {
		t.Fatalf("winner report = %#v", posted.Message)
	}
	messages, err := NewConversationService(restarted).ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: scope, ConversationID: conversation.ID, Limit: 10,
	})
	if err != nil || len(messages) != 1 || messages[0].ID != posted.Message.ID {
		t.Fatalf("Team channel messages = %#v, err = %v", messages, err)
	}
}

func snakesAndLaddersTestRunner(state *snakesAndLaddersState) *snakesAndLaddersRunner {
	return &snakesAndLaddersRunner{
		state: state,
		rolls: map[string][]int{
			"player-alpha": {2, 6, 4},
			"player-beta":  {3, 4, 2, 5},
			"player-gamma": {1, 5, 3, 2},
		},
		board: map[int]int{2: 90, 15: 5, 28: 55, 60: 22, 74: 96, 98: 79},
	}
}

func snakesAndLaddersWorkerPool(t *testing.T, store KernelStore, scope Scope, runner TurnRunner) *AgentRunWorkerPool {
	t.Helper()
	pool, err := NewAgentRunWorkerPool(store, TurnRunnerResolverFunc(func(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
		return &TurnRunnerBinding{DefinitionID: run.AssignedAgentID, DefinitionVersion: "1.0.0", Runner: runner}, nil
	}), nil, AgentRunWorkerConfig{
		Scope: scope, Concurrency: 3, MaxTurnsPerClaim: 1,
		PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func waitForGameRuns(t *testing.T, store PortfolioStore, scope Scope, runIDs map[string]string, ready func(*AgentRun) bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allReady := true
		for _, runID := range runIDs {
			run, err := store.GetAgentRun(t.Context(), scope, runID)
			if err != nil {
				t.Fatal(err)
			}
			if run == nil || !ready(run) {
				allReady = false
				break
			}
		}
		if allReady {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("game Runs did not reach the expected state: %#v", runIDs)
}
