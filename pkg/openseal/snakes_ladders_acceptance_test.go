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
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/store/sqlite"
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
	host          *loopStepHost
	mu            sync.RWMutex
	engine        *Engine
	store         *sqlite.Store
	recovered     bool
	gameRunIDs    map[string]string
	sideRunIDs    map[string]string
	gameObjective map[string]string
	channelID     string
}

// gameMoveMarker starts the line of a player's assistant text that records its
// authoritative move. The replay reads these lines back from the session log.
const gameMoveMarker = "authoritative move: "

// TestThreeAgentSnakesAndLaddersSurvivesRestartAndPublishesReplay is the
// multi-agent restart acceptance test on the run loop. Three Agents play
// through ordinary model steps against a three-rule StepHost fake: a side
// objective waits for the restart, a player that cannot move waits, and the
// player whose turn it is records its move in its assistant text. The kernel
// is stopped mid-game, the store is reopened, and the game finishes from the
// session logs alone.
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
	harness.host = &loopStepHost{decide: harness.decide}
	engine := harness.open(ctx)
	harness.composeWorkforce(ctx, engine)
	harness.startWork(ctx, engine)
	if err := engine.Start(ctx); err != nil {
		t.Fatal(err)
	}

	harness.waitFor(ctx, func() bool {
		moves, err := harness.replay(ctx)
		return err == nil && len(moves) >= 3 && harness.allParked(ctx)
	}, "three legal moves and every Run parked before restart")
	preRestartMoves, err := harness.replay(ctx)
	if err != nil || len(preRestartMoves) != 3 {
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
	// Engine.Start runs the run-loop cutover first: these Runs are loop Runs
	// with sessions, so it must leave every one of them to continue.
	if err := restarted.Start(ctx); err != nil {
		t.Fatal(err)
	}
	harness.waitFor(ctx, func() bool {
		moves, err := harness.replay(ctx)
		if err != nil || len(moves) != len(gameDice) {
			return false
		}
		for _, runID := range harness.allRunIDs() {
			run, loadErr := restarted.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "acceptance"}, runID)
			if loadErr != nil {
				return false
			}
			if run.Status == AgentRunStatusFailed || run.Status == AgentRunStatusCanceled || run.Status == AgentRunStatusPaused {
				t.Fatalf("run %s ended %s after restart: %s %+v", run.ID, run.Status, run.Error, run.FailureExplanation)
			}
			if run.Status != AgentRunStatusCompleted {
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
	harness.assertGameRunsObservedWinner(ctx, winner)
	artifact := harness.publishGameLog(ctx, restarted, winner, positions, moves)
	harness.announceWinner(ctx, restarted, winner, artifact)
	harness.assertDurableOutcome(ctx, restarted, winner, artifact, moves)
	restarted.Stop()
	if err := harness.store.Close(); err != nil {
		t.Fatal(err)
	}

	finalStore, err := sqlite.NewSQLiteStore(harness.path)
	if err != nil {
		t.Fatal(err)
	}
	defer finalStore.Close()
	finalEngine, err := New(WithPersistentStore(finalStore))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { finalEngine.Close() })
	harness.setEngine(finalEngine, finalStore)
	finalMoves, err := harness.replay(ctx)
	if err != nil || len(finalMoves) != len(gameDice) {
		t.Fatalf("replay after the final reopen: moves=%#v err=%v", finalMoves, err)
	}
	harness.assertDurableOutcome(ctx, finalEngine, winner, artifact, moves)
}

func (h *snakesAcceptance) open(ctx context.Context) *Engine {
	h.t.Helper()
	store, err := sqlite.NewSQLiteStore(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	var engine *Engine
	engine, err = New(
		WithPersistentStore(store),
		WithAgentRunWorkers(AgentRunWorkerConfig{
			Scope: Scope{Kind: "tenant", ID: "acceptance"}, Concurrency: 3, MaxActiveForAgent: 1,
			PollInterval: 5 * time.Millisecond, LeaseDuration: time.Second, TurnLeaseDuration: time.Second,
		}, loopResolver(&engine, h.host)),
	)
	if err != nil {
		_ = store.Close()
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { engine.Close() })
	h.setEngine(engine, store)
	return engine
}

func (h *snakesAcceptance) allRunIDs() []string {
	ids := make([]string, 0, 2*len(gamePlayers))
	for _, player := range gamePlayers {
		ids = append(ids, h.gameRunIDs[player], h.sideRunIDs[player])
	}
	return ids
}

func (h *snakesAcceptance) playerOf(runID string) (player string, side bool) {
	for _, candidate := range gamePlayers {
		if h.gameRunIDs[candidate] == runID {
			return candidate, false
		}
		if h.sideRunIDs[candidate] == runID {
			return candidate, true
		}
	}
	return "", false
}

// decide is the fake model: three rules over the request and the durable
// replay of the game, which is all a referee-guided player could know.
func (h *snakesAcceptance) decide(ctx context.Context, req ModelStepRequest) (loopReply, error) {
	player, side := h.playerOf(req.RunID)
	if player == "" {
		return loopReply{}, fmt.Errorf("model step for an unknown Run %s", req.RunID)
	}
	// Rule 1: an independent objective keeps waiting until the kernel has
	// restarted, then finishes.
	if side {
		if !h.hasRecovered() {
			return loopReply{Text: "Independent objective recorded; waiting for the kernel restart.", Calls: []ModelToolCall{loopWaitCall(1)}}, nil
		}
		return loopReply{Text: "Independent objective continued after recovery."}, nil
	}
	moves, err := h.replay(ctx)
	if err != nil {
		return loopReply{}, err
	}
	winner, positions, err := replayGame(moves)
	if err != nil {
		return loopReply{}, err
	}
	// Rule 2: nothing to play now. After a win, observe it and finish;
	// otherwise wait for this player's turn (and, mid-game, for the restart).
	if winner != "" {
		return loopReply{Text: fmt.Sprintf("Observed %s win; no additional move made.", winner)}, nil
	}
	expected := gamePlayers[len(moves)%len(gamePlayers)]
	if player != expected || (len(moves) >= 3 && !h.hasRecovered()) {
		return loopReply{Text: fmt.Sprintf("Waiting for %s's authoritative turn.", expected), Calls: []ModelToolCall{loopWaitCall(1)}}, nil
	}
	// Rule 3: take the authoritative move and record it.
	if len(moves) >= len(gameDice) {
		return loopReply{}, fmt.Errorf("deterministic dice exhausted before a winner")
	}
	move := applyGameMove(len(moves)+1, player, positions[player], gameDice[len(moves)])
	comment, usage, err := h.commentator.Comment(ctx, move)
	if err != nil {
		return loopReply{}, err
	}
	encoded, err := json.Marshal(move)
	if err != nil {
		return loopReply{}, err
	}
	text := comment + "\n" + gameMoveMarker + string(encoded)
	if move.To == gameTarget {
		return loopReply{Text: text, Usage: usage}, nil
	}
	return loopReply{Text: text, Calls: []ModelToolCall{loopWaitCall(1)}, Usage: usage}, nil
}

// replay reads every recorded move back from the game Runs' session logs.
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
		entries, err := engine.Store().ListAgentSessionEntries(ctx, models.SessionEntryFilter{
			Scope: Scope{Kind: "tenant", ID: "acceptance"}, RunID: runID,
			Types: []models.SessionEntryType{models.SessionEntryAssistant}, Limit: 1000,
		})
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			for _, line := range strings.Split(entry.Assistant.Text, "\n") {
				encoded, ok := strings.CutPrefix(line, gameMoveMarker)
				if !ok {
					continue
				}
				var move gameMove
				if err := json.Unmarshal([]byte(encoded), &move); err != nil {
					return nil, fmt.Errorf("entry %s: %w", entry.ID, err)
				}
				if move.Player != player {
					return nil, fmt.Errorf("entry %s: %s's session records a move by %s", entry.ID, player, move.Player)
				}
				moves = append(moves, move)
			}
		}
	}
	sort.Slice(moves, func(i, j int) bool { return moves[i].Number < moves[j].Number })
	_, _, err := replayGame(moves)
	return moves, err
}

func (h *snakesAcceptance) assertGameRunsObservedWinner(ctx context.Context, winner string) {
	h.t.Helper()
	engine := h.currentEngine()
	for _, player := range gamePlayers {
		run, err := engine.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "acceptance"}, h.gameRunIDs[player])
		if err != nil {
			h.t.Fatal(err)
		}
		summary, _ := run.Output["summary"].(string)
		if player == winner {
			if !strings.Contains(summary, gameMoveMarker) || !strings.Contains(summary, fmt.Sprintf(`"to":%d`, gameTarget)) {
				h.t.Fatalf("winner %s did not finish on its winning move: %#v", player, run.Output)
			}
		} else if summary != fmt.Sprintf("Observed %s win; no additional move made.", winner) {
			h.t.Fatalf("player %s did not observe the win: %#v", player, run.Output)
		}
	}
}

func (h *snakesAcceptance) setEngine(engine *Engine, store *sqlite.Store) {
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
	seedSpace(h.t, engine, Scope{Kind: scope.Kind, ID: scope.ID}, "operator")
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
	roster := make([]TeamRosterAssignment, 0, len(gamePlayers))
	for index, player := range gamePlayers {
		id := fmt.Sprintf("player-%d", index+1)
		roster = append(roster, TeamRosterAssignment{ID: id, AgentDeploymentID: player, DisplayName: id, Purpose: "Play by authoritative game rules", Approver: index == 0})
	}
	definition, err := engine.RegisterTeamDefinition(ctx, &TeamDefinition{
		ID: gameTeamID, Version: "1", DisplayName: "Snakes and Ladders Team", Purpose: "Complete a replayable multi-agent game",
		Delegation: TeamDelegationPolicy{MaximumDepth: 1, MaximumConcurrent: 3},
		Approvals:  TeamApprovalPolicy{MaximumRisk: capability.RiskLevelWrite},
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
			Context: map[string]interface{}{"acceptanceKind": "side"},
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
			Context: map[string]interface{}{"acceptanceKind": "game", "teamId": gameTeamID},
		})
		if err != nil {
			h.t.Fatal(err)
		}
		h.gameObjective[player], h.gameRunIDs[player] = objective.ID, run.ID
	}
}

// allParked reports whether every Run is sleeping on its wait between steps,
// so the restart happens between steps, not inside one.
func (h *snakesAcceptance) allParked(ctx context.Context) bool {
	engine := h.currentEngine()
	for _, runID := range h.allRunIDs() {
		run, err := engine.GetAgentRun(ctx, Scope{Kind: "tenant", ID: "acceptance"}, runID)
		if err != nil || run.Status != AgentRunStatusSleeping || run.LastAppliedTurn < 1 {
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
			if run.Status != AgentRunStatusCompleted || run.Output["summary"] != "Independent objective continued after recovery." || run.LastAppliedTurn < 2 {
				h.t.Fatalf("independent objective for %s did not continue: %#v", player, run)
			}
			// The step after the restart saw the wait it parked on before it.
			steps := h.host.requestsFor(run.ID)
			last := steps[len(steps)-1]
			if text := loopRequestText(last); !strings.Contains(text, "Independent objective recorded") || !strings.Contains(text, `"status":"elapsed"`) {
				h.t.Fatalf("independent objective for %s resumed without its session: %s", player, text)
			}
		} else if run.LastAppliedTurn < 1 || run.Status != AgentRunStatusSleeping || run.SessionID == "" {
			h.t.Fatalf("independent objective for %s was not parked before restart: %#v", player, run)
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
	logs := map[string]interface{}{}
	for _, runID := range h.allRunIDs() {
		entries, err := engine.Store().ListAgentSessionEntries(ctx, models.SessionEntryFilter{Scope: artifact.Scope, RunID: runID, Limit: 1000})
		if err != nil {
			h.t.Fatal(err)
		}
		steps, err := engine.ListAgentTurns(ctx, AgentTurnFilter{Scope: artifact.Scope, RunID: runID, Limit: 200})
		if err != nil {
			h.t.Fatal(err)
		}
		logs[runID] = map[string]interface{}{"entries": entries, "steps": steps}
	}
	state, err := json.Marshal(map[string]interface{}{"artifact": stored, "messages": messages, "events": events, "moves": moves, "runs": logs})
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
