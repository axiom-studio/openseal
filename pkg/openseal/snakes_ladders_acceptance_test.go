package openseal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	artifactstore "github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

const (
	gameTeamID    = "snakes-ladders-team"
	gameChannelID = "snakes-ladders-channel"
	gameTarget    = 30
)

var (
	gamePlayers = []string{"player-one", "player-two", "player-three"}
	gameDice    = []int{3, 2, 4, 6, 3, 5, 2}
	gameLadders = map[int]int{3: 22, 5: 8, 11: 26, 20: 29}
	gameSnakes  = map[int]int{17: 4, 19: 7, 21: 9, 27: 1}
)

type gameMove struct {
	Number int    `json:"number"`
	Player string `json:"player"`
	Roll   int    `json:"roll"`
	From   int    `json:"from"`
	Landed int    `json:"landed"`
	To     int    `json:"to"`
	Effect string `json:"effect,omitempty"`
}

type gameLog struct {
	Board  int            `json:"board"`
	Dice   []int          `json:"dice"`
	Moves  []gameMove     `json:"moves"`
	Winner string         `json:"winner"`
	Final  map[string]int `json:"finalPositions"`
}

type moveCommentator interface {
	Identity() (string, string)
	Comment(context.Context, gameMove) (string, TurnUsage, error)
	SensitiveValues() []string
	AcceptanceTimeout() time.Duration
}

type deterministicCommentator struct {
	secret string
	mu     sync.Mutex
	calls  int
}

func (c *deterministicCommentator) Identity() (string, string) { return "fixture", "deterministic" }

func (c *deterministicCommentator) SensitiveValues() []string { return []string{c.secret} }

func (c *deterministicCommentator) AcceptanceTimeout() time.Duration { return 20 * time.Second }

func (c *deterministicCommentator) Comment(_ context.Context, move gameMove) (string, TurnUsage, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return fmt.Sprintf("%s rolled %d and moved from %d to %d.", move.Player, move.Roll, move.From, move.To), TurnUsage{InputTokens: 12, OutputTokens: 12}, nil
}

type snakesAcceptance struct {
	t             *testing.T
	path          string
	contentRoot   string
	commentator   moveCommentator
	mu            sync.RWMutex
	engine        *Engine
	store         *runtime.SQLiteStore
	recovered     bool
	gameRunIDs    map[string]string
	sideRunIDs    map[string]string
	gameObjective map[string]string
	channelID     string
}

func TestThreeAgentSnakesAndLaddersSurvivesRestartAndPublishesReplay(t *testing.T) {
	commentator := &deterministicCommentator{secret: "fixture-provider-secret-must-never-persist"}
	runSnakesAndLaddersAcceptance(t, commentator)
	commentator.mu.Lock()
	calls := commentator.calls
	commentator.mu.Unlock()
	if calls != len(gameDice) {
		t.Fatalf("commentary calls = %d, want one for each of %d authoritative moves", calls, len(gameDice))
	}
}

func runSnakesAndLaddersAcceptance(t *testing.T, commentator moveCommentator) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commentator.AcceptanceTimeout())
	defer cancel()
	directory := t.TempDir()
	harness := &snakesAcceptance{
		t: t, path: filepath.Join(directory, "kernel.db"), contentRoot: filepath.Join(directory, "artifacts"),
		commentator: commentator, gameRunIDs: map[string]string{}, sideRunIDs: map[string]string{}, gameObjective: map[string]string{},
	}
	engine := harness.open(ctx)
	harness.composeWorkforce(ctx, engine)
	harness.startWork(ctx, engine)
	engine.Start(ctx)

	harness.waitFor(ctx, func() bool {
		moves, err := harness.replay(ctx)
		return err == nil && len(moves) >= 3 && harness.gameRunsCheckpointed(ctx)
	}, "three legal moves before restart")
	preRestartMoves, err := harness.replay(ctx)
	if err != nil || len(preRestartMoves) < 3 || len(preRestartMoves) >= len(gameDice) {
		t.Fatalf("restart was not forced mid-game: moves=%#v err=%v", preRestartMoves, err)
	}
	harness.assertSideObjectivesProgressed(ctx, false)
	engine.Stop()
	if err := harness.store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted := harness.open(ctx)
	if _, err := restarted.GetTeamDeployment(ctx, SkillScope{Kind: "tenant", ID: "acceptance"}, gameTeamID); err != nil {
		t.Fatalf("Team did not survive restart: %v", err)
	}
	if _, err := restarted.GetConversation(ctx, Scope{Kind: "tenant", ID: "acceptance"}, harness.channelID); err != nil {
		t.Fatalf("Team channel did not survive restart: %v", err)
	}
	harness.setRecovered()
	woken, err := restarted.WakeAgentRuns(ctx, WakeSignal{
		ID: "acceptance-recovery-1", Scope: Scope{Kind: "tenant", ID: "acceptance"},
		Type: "acceptance-recovery", Reference: "kernel-restarted", Actor: ActivityActor{Type: "system", ID: "acceptance-harness"},
	})
	wokenCount := 0
	if woken != nil {
		wokenCount = len(woken.Runs)
	}
	wantWoken := len(gamePlayers) * 2
	if err != nil || wokenCount != wantWoken {
		t.Fatalf("restart recovery woke %d game and independent Runs, want %d: %v", wokenCount, wantWoken, err)
	}
	restarted.Start(ctx)
	harness.waitFor(ctx, func() bool {
		moves, err := harness.replay(ctx)
		if err != nil || len(moves) != len(gameDice) {
			return false
		}
		for _, player := range gamePlayers {
			run, loadErr := restarted.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "acceptance"}, harness.gameRunIDs[player])
			if loadErr != nil || run.Status != AgentRunStatusCompleted {
				return false
			}
			side, loadErr := restarted.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "acceptance"}, harness.sideRunIDs[player])
			if loadErr != nil || side.Status != AgentRunStatusCompleted {
				return false
			}
		}
		return true
	}, "one winner, all players observing completion, and independent objectives continuing")
	harness.assertSideObjectivesProgressed(ctx, true)

	moves, err := harness.replay(ctx)
	if err != nil {
		t.Fatal(err)
	}
	winner, positions := validateGameReplay(t, moves)
	artifact := harness.publishGameLog(ctx, restarted, winner, positions, moves)
	harness.announceWinner(ctx, restarted, winner, artifact)
	harness.assertDurableOutcome(ctx, restarted, winner, artifact, moves)
	restarted.Stop()
	if err := harness.store.Close(); err != nil {
		t.Fatal(err)
	}

	finalStore, err := runtime.NewSQLiteStore(harness.path)
	if err != nil {
		t.Fatal(err)
	}
	defer finalStore.Close()
	finalEngine, err := New(WithPersistentStore(finalStore))
	if err != nil {
		t.Fatal(err)
	}
	harness.setEngine(finalEngine, finalStore)
	harness.assertDurableOutcome(ctx, finalEngine, winner, artifact, moves)
}

func (h *snakesAcceptance) open(ctx context.Context) *Engine {
	h.t.Helper()
	store, err := runtime.NewSQLiteStore(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	engine, err := New(
		WithPersistentStore(store),
		WithAgentRunWorkers(AgentRunWorkerConfig{
			Scope: Scope{Kind: "tenant", ID: "acceptance"}, Concurrency: 1, MaxActiveForAgent: 1,
			MaxTurnsPerClaim: 1, PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		}, TurnRunnerResolverFunc(h.resolve)),
	)
	if err != nil {
		_ = store.Close()
		h.t.Fatal(err)
	}
	h.setEngine(engine, store)
	return engine
}

func (h *snakesAcceptance) setEngine(engine *Engine, store *runtime.SQLiteStore) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.engine, h.store = engine, store
}

func (h *snakesAcceptance) currentEngine() *Engine {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.engine
}

func (h *snakesAcceptance) setRecovered() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recovered = true
}

func (h *snakesAcceptance) hasRecovered() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.recovered
}

func (h *snakesAcceptance) composeWorkforce(ctx context.Context, engine *Engine) {
	h.t.Helper()
	scope := SkillScope{Kind: "tenant", ID: "acceptance"}
	for _, player := range gamePlayers {
		definition, err := engine.RegisterAgentDefinition(ctx, &AgentDefinition{
			ID: player, Version: "1", DisplayName: strings.ReplaceAll(player, "-", " "),
			Purpose:      "Pursue several durable objectives and communicate verified outcomes.",
			SystemPrompt: "Follow authoritative capability results. Never invent dice rolls, board state, winners, or artifacts.",
			Authority:    AgentAuthorityPolicy{MaximumRisk: capability.RiskLevelWrite, MaxConcurrentRuns: 2},
		})
		if err != nil {
			h.t.Fatal(err)
		}
		if _, _, err := engine.CreateAgentDeployment(ctx, &AgentDeployment{
			ID: player, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
			RolloutStatus: AgentRolloutActive, Environment: "acceptance", Capacity: AgentDeploymentCapacity{MaxConcurrentRuns: 2},
		}, "user", "operator", "compose acceptance workforce"); err != nil {
			h.t.Fatal(err)
		}
	}
	roles, roster := make([]TeamRoleSlot, 0, len(gamePlayers)), make([]TeamRosterAssignment, 0, len(gamePlayers))
	for index, player := range gamePlayers {
		role := fmt.Sprintf("player-%d", index+1)
		roles = append(roles, TeamRoleSlot{ID: role, DisplayName: role, Purpose: "Play by authoritative game rules", MinimumMembers: 1})
		roster = append(roster, TeamRosterAssignment{ID: role, RoleID: role, AgentDeploymentID: player})
	}
	definition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: gameTeamID, Version: "1", DisplayName: "Snakes and Ladders Team", Purpose: "Complete a replayable multi-agent game",
		Roles: roles, Coordination: TeamCoordinationPolicy{MaximumSpeakersPerRound: 1, QuietByDefault: true, RequireRoleRelevance: true},
		Delegation: TeamDelegationPolicy{MaximumDepth: 1, MaximumConcurrent: 3},
		Approvals:  TeamApprovalPolicy{MaximumRisk: capability.RiskLevelWrite, ApproverRoleIDs: []string{roles[0].ID}},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := engine.CreateTeamDeployment(ctx, &TeamDeployment{
		ID: gameTeamID, Scope: scope, DefinitionID: definition.ID, ActiveVersion: definition.Version,
		Status: TeamDeploymentActive, Roster: roster,
	}, "user", "operator", "activate acceptance Team"); err != nil {
		h.t.Fatal(err)
	}
	channel, _, err := engine.CreateConversation(ctx, CreateConversationRequest{
		Scope: Scope{Kind: "tenant", ID: "acceptance"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: gameTeamID},
		Title: "Snakes and Ladders", IdempotencyKey: gameChannelID,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.channelID = channel.ID
}

func (h *snakesAcceptance) startWork(ctx context.Context, engine *Engine) {
	h.t.Helper()
	scope := Scope{Kind: "tenant", ID: "acceptance"}
	// Side objectives are queued first so each Agent demonstrates independent,
	// resumable work before the game and continues it after the forced restart.
	for _, player := range gamePlayers {
		owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: player}
		objective, err := engine.CreateObjective(ctx, CreateObjectiveRequest{
			Scope: scope, Owner: owner, Title: "Maintain sportsmanship", Goal: "Keep a durable independent objective active while the Team game proceeds.", Status: ObjectiveStatusActive,
		})
		if err != nil {
			h.t.Fatal(err)
		}
		run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: player,
			Goal: "Record progress before restart and finish after recovery.", Source: RunSourceObjective,
			Context: map[string]interface{}{"acceptanceKind": "side"}, Budget: &BudgetPolicy{MaxTurns: 4, MaxTotalTokens: 1000},
		})
		if err != nil {
			h.t.Fatal(err)
		}
		h.sideRunIDs[player] = run.ID
	}
	for _, player := range gamePlayers {
		owner := ObjectiveOwner{Type: OwnerTypeAgent, ID: player}
		objective, err := engine.CreateObjective(ctx, CreateObjectiveRequest{
			Scope: scope, Owner: owner, Title: "Play the Team game", Goal: "Take legal authoritative turns until exactly one Team member wins.", Status: ObjectiveStatusActive,
		})
		if err != nil {
			h.t.Fatal(err)
		}
		run, err := engine.CreateAgentRun(ctx, CreateAgentRunRequest{
			Scope: scope, ObjectiveID: objective.ID, Owner: owner, AssignedAgentID: player,
			Goal: "Play Snakes and Ladders using only referee-provided state.", Source: RunSourceObjective,
			Context: map[string]interface{}{"acceptanceKind": "game", "teamId": gameTeamID}, Budget: &BudgetPolicy{MaxTurns: 30, MaxTotalTokens: 20000},
		})
		if err != nil {
			h.t.Fatal(err)
		}
		h.gameObjective[player], h.gameRunIDs[player] = objective.ID, run.ID
	}
}

func (h *snakesAcceptance) resolve(_ context.Context, run *AgentRun) (*TurnRunnerBinding, error) {
	provider, model := h.commentator.Identity()
	return &TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, DefinitionID: run.AssignedAgentID, DefinitionVersion: "1",
		ModelProvider: provider, Model: model, Runner: TurnRunnerFunc(h.runTurn),
	}, nil
}

func (h *snakesAcceptance) runTurn(ctx context.Context, input TurnExecutionContext) (*TurnOutcome, error) {
	if input.Run.Context["acceptanceKind"] == "side" {
		if input.Run.LastAppliedTurn == 0 {
			return &TurnOutcome{
				OutputSummary: "Independent objective checkpointed before recovery", NextRunStatus: AgentRunStatusWaitingForEvent,
				ContinuationCheckpoint: map[string]interface{}{"continuedAcrossRestart": true, "phase": "checkpointed"},
				WakeCondition:          &WakeCondition{Type: "acceptance-recovery", Reference: "kernel-restarted"},
			}, nil
		}
		return &TurnOutcome{
			OutputSummary: "Independent objective continued after recovery", NextRunStatus: AgentRunStatusCompleted,
			RunOutput: map[string]interface{}{"continuedAcrossRestart": true},
		}, nil
	}
	moves, err := h.replay(ctx)
	if err != nil {
		return nil, err
	}
	winner, positions, err := replayGame(moves)
	if err != nil {
		return nil, err
	}
	player := input.Run.AssignedAgentID
	if winner == "" && len(moves) >= 3 && !h.hasRecovered() {
		return &TurnOutcome{
			OutputSummary: "Game checkpointed for the required kernel restart", NextRunStatus: AgentRunStatusWaitingForEvent,
			WakeCondition: &WakeCondition{Type: "acceptance-recovery", Reference: "kernel-restarted"},
		}, nil
	}
	if winner != "" {
		return &TurnOutcome{
			OutputSummary: fmt.Sprintf("Observed %s win; no additional move made", winner), NextRunStatus: AgentRunStatusCompleted,
			RunOutput: map[string]interface{}{"winner": winner, "observed": true},
		}, nil
	}
	expected := gamePlayers[len(moves)%len(gamePlayers)]
	if player != expected {
		wake := time.Now().Add(50 * time.Millisecond)
		return &TurnOutcome{
			OutputSummary: fmt.Sprintf("Quietly waiting for %s's authoritative turn", expected), NextRunStatus: AgentRunStatusSleeping,
			WakeCondition: &WakeCondition{Type: "timer", Reference: "wait-for-authoritative-turn", WakeAt: &wake},
		}, nil
	}
	if len(moves) >= len(gameDice) {
		return nil, fmt.Errorf("deterministic dice exhausted before a winner")
	}
	move := applyGameMove(len(moves)+1, player, positions[player], gameDice[len(moves)])
	comment, usage, err := h.commentator.Comment(ctx, move)
	if err != nil {
		return nil, err
	}
	checkpoint := map[string]interface{}{"gameMove": moveMap(move)}
	if move.To == gameTarget {
		return &TurnOutcome{
			ModelProvider: identityProvider(h.commentator), Model: identityModel(h.commentator), Usage: usage,
			Decisions:     []TurnDecision{{Summary: comment, Rationale: "The deterministic referee owns dice and board transitions."}},
			OutputSummary: fmt.Sprintf("%s won on authoritative move %d", player, move.Number), NextRunStatus: AgentRunStatusCompleted,
			ContinuationCheckpoint: checkpoint, RunOutput: map[string]interface{}{"winner": player, "moveCount": move.Number},
		}, nil
	}
	wake := time.Now().Add(150 * time.Millisecond)
	return &TurnOutcome{
		ModelProvider: identityProvider(h.commentator), Model: identityModel(h.commentator), Usage: usage,
		Decisions:     []TurnDecision{{Summary: comment, Rationale: "The deterministic referee owns dice and board transitions."}},
		OutputSummary: comment, NextRunStatus: AgentRunStatusSleeping, ContinuationCheckpoint: checkpoint,
		WakeCondition: &WakeCondition{Type: "timer", Reference: "next-game-round", WakeAt: &wake},
	}, nil
}

func identityProvider(commentator moveCommentator) string {
	provider, _ := commentator.Identity()
	return provider
}
func identityModel(commentator moveCommentator) string {
	_, model := commentator.Identity()
	return model
}

func (h *snakesAcceptance) replay(ctx context.Context) ([]gameMove, error) {
	engine := h.currentEngine()
	if engine == nil {
		return nil, fmt.Errorf("acceptance engine is unavailable")
	}
	moves := make([]gameMove, 0, len(gameDice))
	for _, player := range gamePlayers {
		runID := h.gameRunIDs[player]
		if runID == "" {
			continue
		}
		turns, err := engine.ListAgentTurns(ctx, AgentTurnFilter{Scope: Scope{Kind: "tenant", ID: "acceptance"}, RunID: runID, Limit: 100})
		if err != nil {
			return nil, err
		}
		for _, turn := range turns {
			if turn.Status != AgentTurnStatusCompleted || turn.ContinuationCheckpoint == nil {
				continue
			}
			raw, ok := turn.ContinuationCheckpoint["gameMove"].(map[string]interface{})
			if !ok {
				continue
			}
			move, err := decodeGameMove(raw)
			if err != nil {
				return nil, fmt.Errorf("turn %s: %w", turn.ID, err)
			}
			moves = append(moves, move)
		}
	}
	sort.Slice(moves, func(i, j int) bool { return moves[i].Number < moves[j].Number })
	_, _, err := replayGame(moves)
	return moves, err
}

func (h *snakesAcceptance) gameRunsCheckpointed(ctx context.Context) bool {
	engine := h.currentEngine()
	for _, player := range gamePlayers {
		run, err := engine.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "acceptance"}, h.gameRunIDs[player])
		if err != nil || run.Status != AgentRunStatusWaitingForEvent {
			return false
		}
	}
	return true
}

func applyGameMove(number int, player string, from, roll int) gameMove {
	landed, to, effect := from+roll, from+roll, ""
	if landed > gameTarget {
		landed, to, effect = from, from, "exact landing required"
	} else if destination, ok := gameLadders[landed]; ok {
		to, effect = destination, fmt.Sprintf("ladder %d->%d", landed, destination)
	} else if destination, ok := gameSnakes[landed]; ok {
		to, effect = destination, fmt.Sprintf("snake %d->%d", landed, destination)
	}
	return gameMove{Number: number, Player: player, Roll: roll, From: from, Landed: landed, To: to, Effect: effect}
}

func replayGame(moves []gameMove) (string, map[string]int, error) {
	positions := map[string]int{}
	winner := ""
	for index, move := range moves {
		if winner != "" {
			return "", nil, fmt.Errorf("move %d occurred after winner %s", move.Number, winner)
		}
		if move.Number != index+1 || move.Player != gamePlayers[index%len(gamePlayers)] || index >= len(gameDice) || move.Roll != gameDice[index] {
			return "", nil, fmt.Errorf("move %d violates deterministic turn or dice order: %#v", index+1, move)
		}
		expected := applyGameMove(index+1, move.Player, positions[move.Player], gameDice[index])
		if move != expected {
			return "", nil, fmt.Errorf("illegal move %d: got %#v want %#v", index+1, move, expected)
		}
		positions[move.Player] = move.To
		if move.To == gameTarget {
			winner = move.Player
		}
	}
	return winner, positions, nil
}

func validateGameReplay(t *testing.T, moves []gameMove) (string, map[string]int) {
	t.Helper()
	winner, positions, err := replayGame(moves)
	if err != nil {
		t.Fatal(err)
	}
	if winner == "" || len(moves) != len(gameDice) {
		t.Fatalf("game did not produce exactly one deterministic winner: winner=%q moves=%#v", winner, moves)
	}
	winners := 0
	for _, position := range positions {
		if position == gameTarget {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("winner count = %d, positions = %#v", winners, positions)
	}
	return winner, positions
}

func (h *snakesAcceptance) publishGameLog(ctx context.Context, engine *Engine, winner string, positions map[string]int, moves []gameMove) *Artifact {
	h.t.Helper()
	content, err := artifactstore.NewLocalStore(h.contentRoot)
	if err != nil {
		h.t.Fatal(err)
	}
	payload, err := json.MarshalIndent(gameLog{Board: gameTarget, Dice: gameDice, Moves: moves, Winner: winner, Final: positions}, "", "  ")
	if err != nil {
		h.t.Fatal(err)
	}
	scope := Scope{Kind: "tenant", ID: "acceptance"}
	stored, err := content.Put(ctx, ArtifactContentWrite{Scope: scope, MediaType: "application/json", SizeBytes: int64(len(payload)), Reader: bytes.NewReader(payload)})
	if err != nil {
		h.t.Fatal(err)
	}
	owner := ObjectiveOwner{Type: OwnerTypeTeam, ID: gameTeamID}
	registered, err := engine.RegisterArtifact(ctx, RegisterArtifactRequest{Artifact: &Artifact{
		ID: "snakes-ladders-game-log", Version: 1, Scope: scope, Name: "snakes-ladders-game-log.json", Type: "game-log", MediaType: "application/json",
		ContentRef: stored.ContentRef, Digest: stored.Digest, SizeBytes: stored.SizeBytes, Classification: ArtifactClassInternal,
		Metadata: map[string]interface{}{"winner": winner, "moves": len(moves), "board": gameTarget},
		Provenance: ArtifactProvenance{
			Producer: ActivityActor{Type: "agent", ID: winner}, Owner: &owner, RunID: h.gameRunIDs[winner], ObjectiveID: h.gameObjective[winner],
		},
		Evidence: []ArtifactEvidenceLink{{Relation: ArtifactEvidenceOutputOf, TargetKind: ArtifactEvidenceTargetRun, TargetRef: h.gameRunIDs[winner]}},
	}})
	if err != nil {
		h.t.Fatal(err)
	}
	reader, err := content.Open(ctx, scope, registered.Artifact.ContentRef)
	if err != nil {
		h.t.Fatal(err)
	}
	defer reader.Close()
	readBack, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(payload, readBack) {
		h.t.Fatalf("game log content mismatch: err=%v", err)
	}
	return registered.Artifact
}

func (h *snakesAcceptance) announceWinner(ctx context.Context, engine *Engine, winner string, artifact *Artifact) {
	h.t.Helper()
	channel, err := engine.GetConversation(ctx, Scope{Kind: "tenant", ID: "acceptance"}, h.channelID)
	if err != nil {
		h.t.Fatal(err)
	}
	message, err := engine.PostChannelMessage(ctx, PostChannelMessageRequest{
		Scope: channel.Scope, ConversationID: channel.ID, ExpectedRevision: channel.Revision,
		Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: winner}, Intent: MessageIntentDecision,
		Content:  fmt.Sprintf("I won the authoritative Snakes and Ladders game. The complete replay is attached as %s.", artifact.Name),
		Audience: ConversationAudience{Kind: ConversationAudienceChannel},
		References: []ConversationReference{
			{Kind: ConversationReferenceRun, ID: h.gameRunIDs[winner]},
			{Kind: ConversationReferenceArtifact, ID: artifact.ID, Version: artifact.Version},
		},
		IdempotencyKey: "announce-snakes-ladders-winner",
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if message.Message.Sender.ID != winner {
		h.t.Fatalf("winner announcement sender = %#v", message.Message.Sender)
	}
	if _, err := engine.AppendActivity(ctx, &ActivityEvent{
		Scope: channel.Scope, TeamID: gameTeamID, AgentID: winner, RunID: h.gameRunIDs[winner], ObjectiveID: h.gameObjective[winner],
		EventType: "game.completed", Summary: "Snakes and Ladders completed with a replayable game log",
		Actor: ActivityActor{Type: "agent", ID: winner}, Visibility: ActivityVisibilityTeam,
		Payload: map[string]interface{}{"artifactId": artifact.ID, "winner": winner},
	}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *snakesAcceptance) assertSideObjectivesProgressed(ctx context.Context, completed bool) {
	h.t.Helper()
	engine := h.currentEngine()
	for _, player := range gamePlayers {
		run, err := engine.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "acceptance"}, h.sideRunIDs[player])
		if err != nil {
			h.t.Fatal(err)
		}
		if completed {
			if run.Status != AgentRunStatusCompleted || run.Output["continuedAcrossRestart"] != true || run.LastAppliedTurn != 2 {
				h.t.Fatalf("independent objective for %s did not continue: %#v", player, run)
			}
		} else if run.LastAppliedTurn != 1 || run.Status != AgentRunStatusWaitingForEvent {
			h.t.Fatalf("independent objective for %s was not checkpointed before restart: %#v", player, run)
		}
	}
}

func (h *snakesAcceptance) assertDurableOutcome(ctx context.Context, engine *Engine, winner string, artifact *Artifact, moves []gameMove) {
	h.t.Helper()
	stored, err := engine.GetArtifact(ctx, artifact.Scope, artifact.ID, artifact.Version)
	if err != nil || stored.Digest != artifact.Digest || stored.Provenance.Producer.ID != winner {
		h.t.Fatalf("durable artifact = %#v, err = %v", stored, err)
	}
	messages, err := engine.ListChannelMessages(ctx, ChannelMessageFilter{Scope: artifact.Scope, ConversationID: h.channelID})
	if err != nil || len(messages) != 1 || messages[0].Sender.ID != winner || len(messages[0].References) != 2 || messages[0].References[1].ID != artifact.ID {
		h.t.Fatalf("winner messages = %#v, err = %v", messages, err)
	}
	events, err := engine.ListActivity(ctx, ActivityFilter{Scope: artifact.Scope, RunID: h.gameRunIDs[winner], Limit: 200})
	if err != nil {
		h.t.Fatal(err)
	}
	completedEvents := 0
	for _, event := range events {
		if event.EventType == "game.completed" && event.Actor.ID == winner {
			completedEvents++
		}
	}
	if completedEvents != 1 {
		h.t.Fatalf("game completion events = %d, events = %#v", completedEvents, events)
	}
	state, err := json.Marshal(map[string]interface{}{"artifact": stored, "messages": messages, "events": events, "moves": moves})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, sensitive := range h.commentator.SensitiveValues() {
		if sensitive != "" && bytes.Contains(state, []byte(sensitive)) {
			h.t.Fatal("provider credential leaked into durable acceptance state")
		}
	}
}

func (h *snakesAcceptance) waitFor(ctx context.Context, condition func() bool, description string) {
	h.t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if condition() {
			return
		}
		select {
		case <-ctx.Done():
			h.t.Fatalf("timed out waiting for %s: %v", description, ctx.Err())
		case <-ticker.C:
		}
	}
}

func moveMap(move gameMove) map[string]interface{} {
	return map[string]interface{}{
		"number": move.Number, "player": move.Player, "roll": move.Roll, "from": move.From,
		"landed": move.Landed, "to": move.To, "effect": move.Effect,
	}
}

func decodeGameMove(value map[string]interface{}) (gameMove, error) {
	move := gameMove{Player: textValue(value["player"]), Effect: textValue(value["effect"])}
	var ok bool
	if move.Number, ok = intValue(value["number"]); !ok {
		return gameMove{}, fmt.Errorf("move number is invalid")
	}
	if move.Roll, ok = intValue(value["roll"]); !ok {
		return gameMove{}, fmt.Errorf("move roll is invalid")
	}
	if move.From, ok = intValue(value["from"]); !ok {
		return gameMove{}, fmt.Errorf("move origin is invalid")
	}
	if move.Landed, ok = intValue(value["landed"]); !ok {
		return gameMove{}, fmt.Errorf("move landing is invalid")
	}
	if move.To, ok = intValue(value["to"]); !ok {
		return gameMove{}, fmt.Errorf("move destination is invalid")
	}
	if move.Player == "" {
		return gameMove{}, fmt.Errorf("move player is invalid")
	}
	return move, nil
}

func intValue(value interface{}) (int, bool) {
	switch number := value.(type) {
	case int:
		return number, true
	case int64:
		return int(number), true
	case float64:
		return int(number), number == float64(int(number))
	default:
		return 0, false
	}
}

func textValue(value interface{}) string { text, _ := value.(string); return text }
