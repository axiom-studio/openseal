package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/skillerror"
)

func sourceAccessChallengeTestCheckpoint(code string, details map[string]string) map[string]interface{} {
	call := &ActionCall{ID: "challenged-read", Status: ActionCallStatusFailed, SideEffect: skill.SideEffectRead,
		Arguments: map[string]interface{}{"url": "https://example.com/research"}, Error: "This source requires an access check before it can be read."}
	if failure := skillerror.NewActionError(code, "", details); failure != nil {
		call.Error, call.ErrorCode, call.ErrorDetails = failure.Error(), failure.Code(), failure.Details()
	}
	return checkpointTerminalAction(nil, call, nil)
}

func sourceAccessChallengeInstall(t *testing.T, f *foregroundClarificationFixture) {
	t.Helper()
	activity := NewRunActivityService(f.store, f.store)
	running, _, err := activity.TransitionRun(t.Context(), f.run.Scope, f.run.ID, RunTransitionRequest{
		ExpectedRevision: f.run.Revision, Status: AgentRunStatusRunning,
		Checkpoint: sourceAccessChallengeTestCheckpoint("source_access_challenge", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.run, _, err = activity.TransitionRun(t.Context(), f.run.Scope, f.run.ID, RunTransitionRequest{
		ExpectedRevision: running.Revision, Status: AgentRunStatusQueued,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func sourceAccessChallengeTestWait(question string) *TurnOutcome {
	return &TurnOutcome{NextRunStatus: AgentRunStatusWaitingForEvent, WakeCondition: &WakeCondition{Type: "user_message"},
		OutputSummary: question, RunOutput: map[string]interface{}{"summary": question}}
}

func sourceAccessChallengeTestAction() *TurnOutcome {
	return &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ProposedActions: []TurnAction{{Capability: "browser.read", InputRef: "/continuationCheckpoint/read"}}}
}

func sourceAccessChallengeAcceptAnswer(t *testing.T, f *foregroundClarificationFixture, question *ChannelMessage, key, content string) *ChannelMessage {
	t.Helper()
	answer := f.post(t, PostChannelMessageRequest{Sender: f.trigger.Sender, Intent: MessageIntentAnswer, Content: content,
		RequiresResponse: true, ReplyToMessageID: question.ID, ResolvesMessageID: question.ID, IdempotencyKey: key})
	accepted, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID)
	if err != nil || accepted == nil || accepted.Run == nil || accepted.Run.ID != f.run.ID || accepted.Run.Status != AgentRunStatusQueued {
		t.Fatalf("accept canonical answer: %#v %v", accepted, err)
	}
	f.run = accepted.Run
	return answer
}

func sourceAccessChallengeAssertPhase(t *testing.T, f *foregroundClarificationFixture, phase string) *SourceAccessChallengeInteraction {
	t.Helper()
	interaction, err := resolveSourceAccessChallengeInteraction(t.Context(), f.store, f.current(t))
	if err != nil || interaction == nil || interaction.Phase != phase {
		t.Fatalf("interaction = %#v, want phase %q: %v", interaction, phase, err)
	}
	return interaction
}

func TestSourceAccessChallengeCanonicalAnswerAllowsOnlyOneBridgeAcrossStores(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newForegroundClarificationFixture(t, store, true)
		sourceAccessChallengeInstall(t, f)
		interaction := sourceAccessChallengeAssertPhase(t, f, "question")
		if err := validateToolFeedbackCorrectionOutcome(f.run, sourceAccessChallengeTestAction(), interaction); err == nil {
			t.Fatal("action admitted before a question and answer")
		}
		f.ask(t, "Should I use the browser you selected for this source?", "user_message")
		question := f.question(t)
		state := f.run.Checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})[sourceAccessChallengeStateKey].(map[string]interface{})
		if state["questionTurnId"] != f.turn.ID || toolFeedbackInteger(state["questionTurnSequence"]) != int(f.turn.Sequence) || toolFeedbackInteger(state["questionsAsked"]) != 1 {
			t.Fatalf("question identity was not committed: %#v", state)
		}
		if interaction, err := resolveSourceAccessChallengeInteraction(t.Context(), store, f.current(t)); err != nil || interaction != nil {
			t.Fatalf("unanswered question granted continuation: %#v %v", interaction, err)
		}
		answer := sourceAccessChallengeAcceptAnswer(t, f, question, "challenge-answer", "Use the browser I selected.")
		interaction = sourceAccessChallengeAssertPhase(t, f, "answer")
		if err := validateToolFeedbackCorrectionOutcome(f.run, sourceAccessChallengeTestAction(), interaction); err == nil {
			t.Fatal("answer skipped its no-action interpretation turn")
		}
		bridge := &TurnOutcome{NextRunStatus: AgentRunStatusRunning, ContinuationCheckpoint: map[string]interface{}{
			ToolFeedbackCorrectionCheckpointKey: map[string]interface{}{sourceAccessChallengeStateKey: map[string]interface{}{"questionsAsked": -100, "answerTurnSequence": -100}},
		}}
		result, err := NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{
			Scope: f.run.Scope, RunID: f.run.ID, WorkerID: "foreground-worker",
		}, TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) { return bridge, nil }))
		if err != nil || result == nil || result.Run.Status != AgentRunStatusRunning || len(result.Turn.RequestedActions) != 0 {
			t.Fatalf("single no-action bridge: %#v %v", result, err)
		}
		f.run, f.turn = result.Run, result.Turn
		interaction = sourceAccessChallengeAssertPhase(t, f, "continue")
		if interaction.QuestionsAsked != 1 || interaction.QuestionTurnID == f.turn.ID {
			t.Fatalf("model checkpoint replaced question identity: %#v", interaction)
		}
		if err := validateToolFeedbackCorrectionOutcome(f.run, sourceAccessChallengeTestAction(), interaction); err != nil {
			t.Fatalf("verified bridge did not allow governed proposal validation: %v", err)
		}
		for range 2 {
			if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, answer.ID); err != nil {
				t.Fatal(err)
			}
			f.run = f.current(t)
			interaction = sourceAccessChallengeAssertPhase(t, f, "continue")
			if len(f.run.PendingInterventions) != 1 || validateToolFeedbackCorrectionOutcome(f.run, &TurnOutcome{NextRunStatus: AgentRunStatusRunning}, interaction) == nil {
				t.Fatal("answer replay replenished its interpretation turn")
			}
		}
		f.assertOnlyRun(t)
	})
}

func TestSourceAccessChallengeReaskIsBoundedAndOldAnswerCannotWakeIt(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newForegroundClarificationFixture(t, store, true)
		sourceAccessChallengeInstall(t, f)
		f.ask(t, "Which browser should I use for this source?", "user_message")
		firstQuestion := f.question(t)
		firstAnswer := sourceAccessChallengeAcceptAnswer(t, f, firstQuestion, "unclear-answer", "I am not sure which one you mean.")
		f.ask(t, "Should I use the browser already selected in this conversation?", "user_message")
		secondQuestion := f.question(t)
		if firstQuestion.ID == secondQuestion.ID {
			t.Fatal("reask reused the old question identity")
		}
		before := f.current(t)
		if _, _, err := f.scheduler.ScheduleMessage(t.Context(), f.run.Scope, f.conversation.ID, firstAnswer.ID); err != nil {
			t.Fatal(err)
		}
		after := f.current(t)
		if after.Status != AgentRunStatusWaitingForEvent || !reflect.DeepEqual(before.Checkpoint, after.Checkpoint) || !reflect.DeepEqual(before.PendingInterventions, after.PendingInterventions) {
			t.Fatal("old answer replay changed the later question")
		}
		if interaction, err := resolveSourceAccessChallengeInteraction(t.Context(), store, after); err != nil || interaction != nil {
			t.Fatalf("old answer authorized the later question: %#v %v", interaction, err)
		}
		sourceAccessChallengeAcceptAnswer(t, f, secondQuestion, "second-answer", "Please explain again.")
		interaction := sourceAccessChallengeAssertPhase(t, f, "answer")
		if interaction.QuestionsAsked != maximumSourceAccessChallengeQuestions {
			t.Fatalf("question allowance reset: %#v", interaction)
		}
		if err := validateToolFeedbackCorrectionOutcome(f.run, sourceAccessChallengeTestWait("A third question?"), interaction); err == nil {
			t.Fatal("third question exceeded the durable allowance")
		}
		if err := validateToolFeedbackCorrectionOutcome(f.run, &TurnOutcome{NextRunStatus: AgentRunStatusCompleted, RunOutput: map[string]interface{}{"summary": "I could not read this source."}}, interaction); err != nil {
			t.Fatalf("bounded clarification could not stop visibly: %v", err)
		}
	})
}

func TestSourceAccessChallengeRequiresExactCanonicalFailure(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		details    map[string]string
	}{
		{"typed 429", "source_rate_limited", nil},
		{"challenge with 429", "source_access_challenge", map[string]string{"httpStatus": "429"}},
		{"proxy authentication", "browser_proxy_authentication_failed", nil},
		{"proxy unavailable", "browser_proxy_unavailable", nil},
		{"untyped challenge wording", "", nil},
		{"batch challenge", "source_reads_failed", map[string]string{"failedCount": "1", "totalCount": "1", "failures": `[{"index":0,"failureKind":"source_access_challenge","httpStatus":403}]`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := &AgentRun{Status: AgentRunStatusRunning, Checkpoint: sourceAccessChallengeTestCheckpoint(tc.code, tc.details)}
			if hasCanonicalSourceAccessChallenge(run) || sourceAccessChallengeInteraction(run) != nil {
				t.Fatal("non-challenge receipt granted conversation continuation")
			}
			outcome := sourceAccessChallengeTestWait("Can I continue?")
			// Ordinary failures already carry the latest terminal marker. Match
			// the coordinator's validator order rather than bypassing that stop.
			err := validateFinalFailureExplanationOutcome(run, outcome)
			if err == nil {
				err = validateToolFeedbackCorrectionOutcome(run, outcome)
			}
			if err == nil {
				t.Fatal("non-challenge tool failure allowed a question")
			}
		})
	}
	for name, change := range map[string]func(*AgentRun){
		"stale last receipt": func(r *AgentRun) { r.Checkpoint["lastAction"].(map[string]interface{})["actionCallId"] = "old" },
		"stale correction": func(r *AgentRun) {
			r.Checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})["lastFailureId"] = "old"
		},
		"later success": func(r *AgentRun) {
			r.Checkpoint = checkpointTerminalAction(r.Checkpoint, &ActionCall{ID: "later-read", Status: ActionCallStatusSucceeded}, nil)
		},
		"final failure": func(r *AgentRun) { r.Checkpoint = checkpointFinalFailureExplanation(r.Checkpoint, "action", "Stopped") },
		"terminal run":  func(r *AgentRun) { r.Status = AgentRunStatusFailed },
	} {
		t.Run(name, func(t *testing.T) {
			run := &AgentRun{Status: AgentRunStatusRunning, Checkpoint: sourceAccessChallengeTestCheckpoint("source_access_challenge", nil)}
			change(run)
			if hasCanonicalSourceAccessChallenge(run) || sourceAccessChallengeInteraction(run) != nil {
				t.Fatal("stale or terminal evidence reopened clarification")
			}
		})
	}
}

func TestSourceAccessChallengeRejectsForeignAndReplayedReceipts(t *testing.T) {
	store := NewMemoryStore()
	f := newForegroundClarificationFixture(t, store, true)
	sourceAccessChallengeInstall(t, f)
	f.ask(t, "Use your selected browser?", "user_message")
	question := f.question(t)
	sourceAccessChallengeAcceptAnswer(t, f, question, "answer", "Yes")
	for name, change := range map[string]func(*AgentRun, *ConversationAnswerReceipt){
		"scope": func(_ *AgentRun, a *ConversationAnswerReceipt) { a.Scope.ID = "foreign" },
		"run":   func(_ *AgentRun, a *ConversationAnswerReceipt) { a.RunID = "foreign" },
		"agent": func(_ *AgentRun, a *ConversationAnswerReceipt) { a.AgentID = "foreign" },
		"actor": func(r *AgentRun, a *ConversationAnswerReceipt) {
			a.AuthenticatedActor.ID = "foreign"
			r.PendingInterventions[0].Actor.ID = "foreign"
		},
		"conversation":        func(_ *AgentRun, a *ConversationAnswerReceipt) { a.ConversationID = "foreign" },
		"source":              func(_ *AgentRun, a *ConversationAnswerReceipt) { a.SourceRunID = "foreign" },
		"thread":              func(_ *AgentRun, a *ConversationAnswerReceipt) { a.ThreadRootMessageID = "foreign" },
		"stale turn":          func(_ *AgentRun, a *ConversationAnswerReceipt) { a.QuestionTurnID = "old" },
		"legacy missing turn": func(_ *AgentRun, a *ConversationAnswerReceipt) { a.QuestionTurnID = "" },
		"stale sequence":      func(_ *AgentRun, a *ConversationAnswerReceipt) { a.QuestionTurnSequence++ },
		"duplicate receipt": func(r *AgentRun, _ *ConversationAnswerReceipt) {
			r.PendingInterventions = append(r.PendingInterventions, r.PendingInterventions[0])
		},
		"ordinary guidance": func(r *AgentRun, _ *ConversationAnswerReceipt) { r.PendingInterventions[0].ConversationAnswer = nil },
	} {
		t.Run(name, func(t *testing.T) {
			run := cloneAgentRun(f.run)
			change(run, run.PendingInterventions[0].ConversationAnswer)
			current := f.current(t)
			run.Revision = current.Revision + 1
			event := &ActivityEvent{ID: "invalid-receipt-" + name, Scope: run.Scope, RunID: run.ID,
				EventType: "run.updated", Summary: "Install malformed receipt for boundary test", CreatedAt: time.Now()}
			if _, err := store.UpdateAgentRunWithEvent(t.Context(), run, current.Revision, event, nil); err != nil {
				t.Fatal(err)
			}
			interaction, err := resolveSourceAccessChallengeInteraction(t.Context(), store, run)
			if err != nil || interaction != nil {
				t.Fatalf("untrusted answer accepted: %#v %v", interaction, err)
			}
		})
	}
}

func TestSourceAccessChallengeRejectsReceiptInjectedOnlyIntoRunClone(t *testing.T) {
	forConversationTaskClarificationStores(t, func(t *testing.T, store conversationTaskClarificationStore) {
		f := newForegroundClarificationFixture(t, store, true)
		sourceAccessChallengeInstall(t, f)
		f.ask(t, "Use your selected browser?", "user_message")
		question := f.question(t)
		forged := cloneAgentRun(f.run)
		forged.PendingInterventions = []AgentRunIntervention{{ID: "model-invented-answer", Actor: ActivityActor{Type: "user", ID: f.trigger.Sender.ID},
			ConversationAnswer: &ConversationAnswerReceipt{Scope: f.run.Scope, RunID: f.run.ID, AgentID: f.run.AssignedAgentID,
				ConversationID: f.conversation.ID, SourceRunID: f.run.ID, SourceMessageID: f.trigger.ID, ThreadRootMessageID: question.ThreadRootID,
				QuestionMessageID: question.ID, QuestionTurnID: f.turn.ID, QuestionTurnSequence: f.turn.Sequence, QuestionContent: question.Content,
				AnswerMessageID: "invented-message", AnswerContent: "Yes", AuthenticatedActor: f.trigger.Sender},
		}}
		interaction, err := resolveSourceAccessChallengeInteraction(t.Context(), store, forged)
		if err != nil || interaction != nil || len(f.current(t).PendingInterventions) != 0 {
			t.Fatalf("noncanonical receipt granted authority: %#v %v", interaction, err)
		}
	})
}

func TestSourceAccessChallengeQuestionsAndBridgeCannotScheduleOrProposeWork(t *testing.T) {
	question := &SourceAccessChallengeInteraction{FailureID: "challenge", Phase: "question"}
	for name, change := range map[string]func(*TurnOutcome){
		"action":              func(o *TurnOutcome) { o.ProposedActions = sourceAccessChallengeTestAction().ProposedActions },
		"task":                func(o *TurnOutcome) { o.ProposedTask = &TurnTaskProposal{} },
		"fork":                func(o *TurnOutcome) { o.ProposedFork = &TurnForkProposal{} },
		"delegation":          func(o *TurnOutcome) { o.ProposedDelegation = &TurnDelegationProposal{} },
		"runbook":             func(o *TurnOutcome) { o.ProposedRunbook = &TurnRunbookProposal{} },
		"timer":               func(o *TurnOutcome) { now := time.Now(); o.WakeCondition.WakeAt = &now },
		"predicate":           func(o *TurnOutcome) { o.WakeCondition.Predicate = map[string]interface{}{"ready": true} },
		"event wait":          func(o *TurnOutcome) { o.WakeCondition.EventWait = &RunEventWaitSpec{} },
		"wrong wake":          func(o *TurnOutcome) { o.WakeCondition.Type = "timer" },
		"output summary only": func(o *TurnOutcome) { o.RunOutput = nil },
		"blank question":      func(o *TurnOutcome) { o.RunOutput["summary"] = "  " },
		"silent":              func(o *TurnOutcome) { o.RunOutput["silent"] = true },
		"intake":              func(o *TurnOutcome) { o.RunOutput[AgentRequestDecisionOutputKey] = "accept" },
	} {
		t.Run(name, func(t *testing.T) {
			outcome := sourceAccessChallengeTestWait("Use the selected browser?")
			change(outcome)
			if err := validateSourceAccessChallengeOutcome(question, outcome); err == nil {
				t.Fatal("question escaped its visible no-proposal user-message boundary")
			}
		})
	}
	answer := &SourceAccessChallengeInteraction{FailureID: "challenge", Phase: "answer", QuestionsAsked: 1}
	for name, outcome := range map[string]*TurnOutcome{
		"action":         sourceAccessChallengeTestAction(),
		"visible output": {NextRunStatus: AgentRunStatusRunning, RunOutput: map[string]interface{}{"summary": "Starting now"}},
		"wake":           {NextRunStatus: AgentRunStatusRunning, WakeCondition: &WakeCondition{Type: "user_message"}},
		"task":           {NextRunStatus: AgentRunStatusRunning, ProposedTask: &TurnTaskProposal{}},
	} {
		t.Run("bridge "+name, func(t *testing.T) {
			if err := validateSourceAccessChallengeOutcome(answer, outcome); err == nil {
				t.Fatal("answer bypassed the private no-action bridge")
			}
		})
	}
}

func TestSourceAccessChallengeRejectsModelAuthoredMarkers(t *testing.T) {
	trusted := sourceAccessChallengeTestCheckpoint("source_access_challenge", nil)
	forged := deepCloneCheckpointMap(trusted)
	forged[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})[sourceAccessChallengeStateKey] = map[string]interface{}{
		"failureId": "challenged-read", "questionsAsked": 1, "questionTurnId": "invented", "questionTurnSequence": 1,
		"answerTurnId": "invented-answer", "answerTurnSequence": 2,
	}
	merged := preserveKernelActionHistory(trusted, forged)
	if merged[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})[sourceAccessChallengeStateKey] != nil {
		t.Fatal("model-authored interaction marker crossed the kernel merge")
	}
	if hasCanonicalSourceAccessChallenge(&AgentRun{Status: AgentRunStatusRunning, Checkpoint: preserveKernelActionHistory(nil, forged)}) {
		t.Fatal("model invented the original failure receipt")
	}
}

func TestSourceAccessChallengeQuestionAndBridgeRecoverAcrossSQLiteRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source-challenge.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f := newForegroundClarificationFixture(t, store, true)
	sourceAccessChallengeInstall(t, f)
	restart := func() {
		t.Helper()
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err = NewSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		f.store, f.service = store, NewConversationService(store)
		f.scheduler, err = NewConversationRunScheduler(store, store, ConversationRunSchedulerConfig{MessagePageSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		f.run = f.current(t)
	}
	crash := errors.New("simulated crash after durable finish")
	for _, phase := range []string{"question", "answer"} {
		coordinator := NewTurnCoordinator(store, store, store)
		coordinator.afterTurnPersisted = func() error { return crash }
		outcome := &TurnOutcome{NextRunStatus: AgentRunStatusRunning}
		if phase == "question" {
			outcome = sourceAccessChallengeTestWait("Use the browser you selected?")
		}
		calls := 0
		result, err := coordinator.Advance(t.Context(), AdvanceAgentRunRequest{Scope: f.run.Scope, RunID: f.run.ID, WorkerID: "foreground-worker"},
			TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) { calls++; return outcome, nil }))
		if !errors.Is(err, crash) || result == nil || result.Turn == nil || result.Turn.Status != AgentTurnStatusCompleted {
			t.Fatalf("durable %s finish: %#v %v", phase, result, err)
		}
		finishedID := result.Turn.ID
		restart()
		result, err = NewTurnCoordinator(store, store, store).Advance(t.Context(), AdvanceAgentRunRequest{Scope: f.run.Scope, RunID: f.run.ID, WorkerID: "foreground-worker"},
			TurnRunnerFunc(func(context.Context, TurnExecutionContext) (*TurnOutcome, error) { calls++; return outcome, nil }))
		if err != nil || result == nil || !result.Reconciled || result.Turn.ID != finishedID || calls != 1 {
			t.Fatalf("restart repeated %s runner: %#v calls=%d err=%v", phase, result, calls, err)
		}
		f.run, f.turn = result.Run, result.Turn
		if phase == "question" {
			if f.run.Status != AgentRunStatusWaitingForEvent {
				t.Fatalf("recovered question status = %s", f.run.Status)
			}
			sourceAccessChallengeAcceptAnswer(t, f, f.question(t), "restart-answer", "Use the selected browser.")
			sourceAccessChallengeAssertPhase(t, f, "answer")
		} else {
			interaction := sourceAccessChallengeAssertPhase(t, f, "continue")
			if interaction.QuestionsAsked != 1 || validateToolFeedbackCorrectionOutcome(f.run, &TurnOutcome{NextRunStatus: AgentRunStatusRunning}, interaction) == nil {
				t.Fatal("restart replenished the question or bridge allowance")
			}
		}
	}
}
