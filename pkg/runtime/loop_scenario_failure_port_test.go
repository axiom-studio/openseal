package runtime

// Run-loop scenarios ported from the old failure, evidence and answer-visual
// tests (run-loop section 12 §5.1, tests-T17b): a tool failure goes back to
// the model, a terminal failure is a typed Run.FailureExplanation with a
// visible chat message, the final answer checks answer with tool errors the
// model repairs, and the final answer tool carries visuals, silence,
// broadcast and generated files. Every scenario drives the real runloop.Loop
// through the loop fixture; only the model, the dispatcher, the host tools,
// the visual host checks and the evidence reviewer are fakes.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

// portVisualArtifactID is the stable artifact ID of the visual on the answer
// to trigger: a replayed answer must reference the visual its first attempt
// stored.
func portVisualArtifactID(runID, triggerID string) string {
	return "visual-" + strings.TrimPrefix(models.SHA256DigestString(runID+"\x00"+triggerID), "sha256:")[:40]
}

// ---------------------------------------------------------------------------
// Helpers

// portAgentMessages is the conversation's messages from the fixture Agent.
func portAgentMessages(f *looptest.Fixture, conversationID string) []*models.ChannelMessage {
	f.T.Helper()
	var out []*models.ChannelMessage
	for _, message := range f.Messages(conversationID) {
		if message.Sender.Type == models.ConversationParticipantAgent && message.Sender.ID == looptest.FixtureAgent {
			out = append(out, message)
		}
	}
	return out
}

// portFinalEntries is the Run's turn_final_answer tool entries in log order.
func portFinalEntries(f *looptest.Fixture, runID string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		if entry.Type == models.SessionEntryTool && entry.Tool != nil && entry.Tool.Name == string(models.ControlToolFinalAnswer) && entry.RunID == runID {
			out = append(out, entry)
		}
	}
	return out
}

func portEntryText(entry *models.SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil {
		return ""
	}
	return looptest.PartsText(entry.Tool.Result.Content)
}

// portLastToolFailed reports whether the newest tool result the step sees is
// an error.
func portLastToolFailed(v looptest.StepView) bool {
	results := v.ToolResultsSinceUser()
	return len(results) > 0 && results[len(results)-1].Tool != nil && results[len(results)-1].Tool.Result.IsError
}

// portRequireTypedFailure checks the D4 contract of a terminal failure: a
// typed code and message on the Run, and no failure state anywhere else.
func portRequireTypedFailure(t *testing.T, run *models.AgentRun) {
	t.Helper()
	if run.FailureExplanation == nil || strings.TrimSpace(run.FailureExplanation.Code) == "" || strings.TrimSpace(run.FailureExplanation.Message) == "" {
		t.Fatalf("failure = %#v, want a typed code and message", run.FailureExplanation)
	}
	for key := range run.Checkpoint {
		if strings.Contains(strings.ToLower(key), "failure") {
			t.Fatalf("run state carries a failure key %q; the failure lives only in FailureExplanation", key)
		}
	}
}

// portReportFailure delivers the terminal report of a failed chat Run the way
// the worker's reporting outbox does, and returns the Agent's last message.
func portReportFailure(f *looptest.Fixture, conversationID string, failed *models.AgentRun) *models.ChannelMessage {
	f.T.Helper()
	if err := conversationrun.ProjectFailedConversationReply(f.Ctx, f.Chats, failed); err != nil {
		f.T.Fatalf("report failed run: %v", err)
	}
	answers := portAgentMessages(f, conversationID)
	if len(answers) != 1 {
		f.T.Fatalf("agent messages after the report = %s, want one", looptest.DescribeMessages(f.Messages(conversationID)))
	}
	return answers[0]
}

// ---------------------------------------------------------------------------
// Tool failures

// TestLoopToolFailureIsExplainedByTheModelNotTheKernel: a failed governed
// call is a tool error the model reads and answers from; the Run completes
// and carries no failure (ported intent of TestFailedDispatchReturnsToTheModel
// and the action_failure_feedback tests).
func TestLoopToolFailureIsExplainedByTheModelNotTheKernel(t *testing.T) {
	looptest.EachStore(t, "port_tool_failure", func(t *testing.T, store models.KernelStore) {
		var seen string
		f := looptest.NewFixture(t, store, looptest.WithRules(
			&looptest.ModelRule{Name: "explode", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.Act("Reading the service.", looptest.Call(looptest.GovernedTool(v, "explode"), nil))
			}},
			&looptest.ModelRule{Name: "explain", When: portLastToolFailed, Reply: func(v looptest.StepView) models.ModelStepResult {
				results := v.ToolResultsSinceUser()
				seen = looptest.PartsText(results[len(results)-1].Tool.Result.Content)
				return looptest.Say("I couldn't read it: the fixture service exploded.")
			}},
		))
		conversation, run := f.StartChat("read the fixture service")
		done := f.DriveToRest(run.ID, 3)
		f.MustStatus(done, models.AgentRunStatusCompleted)
		if done.FailureExplanation != nil || done.Error != "" {
			t.Fatalf("completed run carries failure %#v / %q", done.FailureExplanation, done.Error)
		}
		var failedEntry *models.SessionEntry
		for _, entry := range f.Entries(run.ID) {
			if entry.Type == models.SessionEntryTool && entry.Tool != nil && strings.HasSuffix(entry.Tool.Name, "explode") {
				failedEntry = entry
			}
		}
		if failedEntry == nil || failedEntry.State != models.ToolEntryError || !failedEntry.Tool.Result.IsError {
			t.Fatalf("explode entry = %#v", failedEntry)
		}
		if !strings.Contains(seen, "the fixture service exploded") || seen != portEntryText(failedEntry) {
			t.Fatalf("the model saw %q, the entry holds %q", seen, portEntryText(failedEntry))
		}
		if calls := f.Calls(run.ID); len(calls) != 1 || calls[0].Status != models.ActionCallStatusFailed {
			t.Fatalf("action calls = %#v", calls)
		}
		answers := portAgentMessages(f, conversation.ID)
		if len(answers) != 1 || answers[0].Content != "I couldn't read it: the fixture service exploded." {
			t.Fatalf("channel = %s", looptest.DescribeMessages(f.Messages(conversation.ID)))
		}
		if len(f.Model.Calls(models.ModelStepPurposeExplanation)) != 0 {
			t.Fatal("a tool failure ran the kernel's explanation step")
		}
	})
}

// TestLoopTerminalFailureCarriesTypedFailureExplanation: the ways a Run ends
// in failure all record a typed Run.FailureExplanation and leave a visible
// chat message: the model's explanation when the failure is explainable,
// otherwise the kernel's reply from the terminal report.
func TestLoopTerminalFailureCarriesTypedFailureExplanation(t *testing.T) {
	t.Run("host_unavailable_exhausted", func(t *testing.T) {
		looptest.EachStore(t, "port_host_exhausted", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "down",
				Err: &modelstep.TurnHostFailure{Code: "host_draining", Message: "The step host is restarting.", Retryable: true}}))
			conversation, run := f.StartChat("check issue 1")
			const hostRetries = 3 // the run loop requeues a retryable host failure three times
			failed := f.DriveToRest(run.ID, hostRetries+2)
			f.MustStatus(failed, models.AgentRunStatusFailed)
			portRequireTypedFailure(t, failed)
			if failed.FailureExplanation.Code != runloop.StepFailureHostUnavailable || failed.FailureExplanation.Explained {
				t.Fatalf("failure = %#v, want an unexplained %s", failed.FailureExplanation, runloop.StepFailureHostUnavailable)
			}
			if got := len(f.Model.Calls("")); got != hostRetries+1 {
				t.Fatalf("model calls = %d, want %d (3 requeues, then the failure)", got, hostRetries+1)
			}
			if len(portAgentMessages(f, conversation.ID)) != 0 {
				t.Fatal("the loop posted a reply for an unexplained failure")
			}
			reply := portReportFailure(f, conversation.ID, failed)
			if want := runtimeconversation.TerminalRunFailureReply(f.Ctx, f.Raw, f.Raw, failed); reply.Content != want || strings.Contains(reply.Content, "restarting") {
				t.Fatalf("reply = %q, want the kernel reply %q", reply.Content, want)
			}
		})
	})

	t.Run("doom_loop", func(t *testing.T) {
		looptest.EachStore(t, "port_doom_loop", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(
				&looptest.ModelRule{Name: "lookup", When: stepPurpose, Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Checking.", looptest.Call(looptest.GovernedTool(v, "lookup"), repeatLookupArgs))
				}},
				&looptest.ModelRule{Name: "explain", When: purposeIs(models.ModelStepPurposeExplanation), Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.Say("I kept getting the same answer for issue 6815, so I stopped.")
				}},
			))
			conversation, run := f.StartChat("what is the status of 6815?")
			failed := f.DriveToRest(run.ID, 4)
			f.MustStatus(failed, models.AgentRunStatusFailed)
			portRequireTypedFailure(t, failed)
			if failed.FailureExplanation.Code != action.RunFailureCodeRepeatLimit || !failed.FailureExplanation.Explained {
				t.Fatalf("failure = %#v, want an explained %s", failed.FailureExplanation, action.RunFailureCodeRepeatLimit)
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || !strings.Contains(answers[0].Content, "so I stopped") {
				t.Fatalf("channel = %s, want the model's explanation", looptest.DescribeMessages(f.Messages(conversation.ID)))
			}
		})
	})

	t.Run("second_length_rejection", func(t *testing.T) {
		looptest.EachStore(t, "port_length_twice", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(
				lookupRule("lookup", 5, nil, nil),
				&looptest.ModelRule{Name: "summary", When: purposeIs(models.ModelStepPurposeSummary),
					Reply: func(looptest.StepView) models.ModelStepResult { return looptest.Say("Issues 1 to 5 were looked up.") }},
				&looptest.ModelRule{Name: "too long", When: purposeIs(models.ModelStepPurposeStep), Err: contextExceeded()},
			))
			f.Dispatch.On("lookup", bigLookup)
			conversation, run := f.StartChat("check issues 1 to 5")
			exit, failed := f.RunClaimWith(f.Ctx, run.ID, f.WithLoopCompactor)
			if exit.Reason != runloop.ExitFailed || exit.Err != nil {
				t.Fatalf("exit = %s (%v), want failed", exit.Reason, exit.Err)
			}
			f.MustStatus(failed, models.AgentRunStatusFailed)
			portRequireTypedFailure(t, failed)
			if failed.FailureExplanation.Explained || len(f.Model.Calls(models.ModelStepPurposeExplanation)) != 0 {
				t.Fatalf("failure = %#v; a context overflow is not explained by the model", failed.FailureExplanation)
			}
			if steps := f.Model.Calls(models.ModelStepPurposeStep); len(steps) != 7 || steps[6].Attempt != 1 {
				t.Fatalf("step calls = %d, want 5 lookups and two attempts of the sixth step", len(steps))
			}
			if reply := portReportFailure(f, conversation.ID, failed); reply.Content != runtimeconversation.TerminalRunFailureReply(f.Ctx, f.Raw, f.Raw, failed) {
				t.Fatalf("reply = %q", reply.Content)
			}
		})
	})

	// A spent budget pauses the Run (the owner can raise the budget and
	// resume); it is not a failure. Spec 12 §5.1 lists it among the failures,
	// but the loop pauses with budget.exhausted (TestRunLoopBudgetExhaustion
	// PausesAfterStep); this subtest pins that the pause records no failure.
	t.Run("budget_exhausted_pauses", func(t *testing.T) {
		looptest.EachStore(t, "port_budget", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(lookupRule("lookup", 0, purposeIs(models.ModelStepPurposeStep), nil)))
			run := f.StartBoundedRun("Check issues 1 to 3", models.BudgetPolicy{MaxTurns: 2})
			paused := f.DriveToRest(run.ID, 2)
			f.MustStatus(paused, models.AgentRunStatusPaused)
			if paused.BudgetState != models.BudgetStateExhausted || paused.FailureExplanation != nil {
				t.Fatalf("paused run budget %s failure %#v", paused.BudgetState, paused.FailureExplanation)
			}
			if len(f.Events(run.ID, "budget.exhausted")) != 1 || len(f.Model.Calls(models.ModelStepPurposeStep)) != 2 {
				t.Fatalf("budget.exhausted = %d, steps = %d", len(f.Events(run.ID, "budget.exhausted")), len(f.Model.Calls(models.ModelStepPurposeStep)))
			}
		})
	})
}

// ---------------------------------------------------------------------------
// Final answer checks

// TestLoopCompletionChecksReturnToolErrors: a final answer the checks refuse
// is an error result on its turn_final_answer entry; the model repairs it in
// the next step and only the accepted answer is delivered.
func TestLoopCompletionChecksReturnToolErrors(t *testing.T) {
	chatCheck := func(t *testing.T, prefix, claim, code string) {
		looptest.EachStore(t, prefix, func(t *testing.T, store models.KernelStore) {
			var rejection string
			f := looptest.NewFixture(t, store, looptest.WithRules(
				&looptest.ModelRule{Name: "claim", Times: 1, Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": claim})
				}},
				&looptest.ModelRule{Name: "repair", When: portLastToolFailed, Reply: func(v looptest.StepView) models.ModelStepResult {
					rejection, _, _ = v.ToolResult(string(models.ControlToolFinalAnswer))
					return looptest.FinalAnswer(map[string]any{"answer": "I have not done that yet; nothing was sent or submitted."})
				}},
			))
			conversation, run := f.StartChat("please email Ana the report")
			done := f.DriveToRest(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			finals := portFinalEntries(f, run.ID)
			if len(finals) != 2 || finals[0].State != models.ToolEntryError || !finals[0].Tool.Result.IsError || finals[1].State != models.ToolEntryCompleted {
				t.Fatalf("final entries = %v", f.EntryTypes(run.ID))
			}
			if text := portEntryText(finals[0]); !strings.Contains(text, code) || text != rejection {
				t.Fatalf("rejection = %q (model saw %q), want code %s", text, rejection, code)
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || !strings.HasPrefix(answers[0].Content, "I have not done that yet") {
				t.Fatalf("channel = %s, want only the repaired answer", looptest.DescribeMessages(f.Messages(conversation.ID)))
			}
		})
	}
	t.Run("unevidenced_completion_claim", func(t *testing.T) {
		// The claim vocabulary is claimsExternalMutation's; "I updated the
		// status" (spec 12 §5.1) is not in it.
		chatCheck(t, "port_unevidenced", "Done: I sent the email to Ana.", "unevidenced_external_claim")
	})
	t.Run("unverified_approval_claim", func(t *testing.T) {
		chatCheck(t, "port_approval_claim", "Your approval request has been submitted.", "approval_claim")
	})

}

// ---------------------------------------------------------------------------
// Final answer fields

// TestLoopFinalAnswerToolCarriesVisualSilentAndFiles: what the final answer
// tool carries reaches the chat (ported from conversation_answer_visual_test.go
// and the spoken and broadcast cases of the old runner).
func TestLoopFinalAnswerToolCarriesVisualSilentAndFiles(t *testing.T) {
	t.Run("visual", func(t *testing.T) {
		looptest.EachStore(t, "port_visual", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Revenue is up.", "visual": failurePortVisual()})
			}}))
			content := &looptest.AnswerVisualContent{}
			convRunner(f, conversationrun.TurnRunnerConfig{AttachmentContent: content})
			conversation, run := f.StartChat("how is revenue?")
			trigger := f.Messages(conversation.ID)[0]
			done := f.DriveToRest(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			id := portVisualArtifactID(run.ID, trigger.ID)
			answers := portAgentMessages(f, conversation.ID)
			want := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}, {Kind: models.ConversationReferenceArtifact, ID: id, Version: 1}}
			if len(answers) != 1 || answers[0].Content != "Revenue is up." || answers[0].Intent != models.MessageIntentAnswer ||
				len(answers[0].References) != 2 || answers[0].References[0] != want[0] || answers[0].References[1] != want[1] {
				t.Fatalf("answers = %#v", answers)
			}
			for _, message := range f.Messages(conversation.ID) {
				if message.Sender.Type == models.ConversationParticipantService {
					t.Fatalf("a receipt was posted for the visual: %#v", message)
				}
			}
			artifact, err := f.Raw.(models.ArtifactStore).GetArtifact(f.Ctx, f.Scope, id, 1)
			if err != nil || artifact == nil || artifact.Type != presentation.ArtifactType || artifact.MediaType != presentation.MediaType ||
				artifact.Provenance.RunID != run.ID || artifact.Provenance.Producer != (models.ActivityActor{Type: "agent", ID: looptest.FixtureAgent}) {
				t.Fatalf("artifact = %#v, %v", artifact, err)
			}
			surface, err := presentation.Decode(content.Data)
			if err != nil || surface.Title != "Quarterly revenue" || content.Puts != 1 {
				t.Fatalf("stored visual = %#v (%d puts), %v", surface, content.Puts, err)
			}
			if _, ok := done.Output[agentrun.FinalOutputVisualKey]; ok {
				t.Fatalf("the visual stayed in the Run output: %#v", done.Output)
			}
		})
	})

	t.Run("visual_rejected_by_host", func(t *testing.T) {
		looptest.EachStore(t, "port_visual_rejected", func(t *testing.T, store models.KernelStore) {
			bound := failurePortVisual()
			bound.Components = append(bound.Components, presentation.Component{ID: "next", Type: "actions", Actions: []presentation.Action{
				{ID: "stop", Label: "Stop", Kind: presentation.ActionEntity, Command: "stop", Binding: &presentation.Binding{Kind: presentation.BindingRunbook, ID: "rb-1"}},
			}})
			f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "Revenue is up.", "visual": bound})
			}}))
			content := &looptest.AnswerVisualContent{}
			convRunner(f, conversationrun.TurnRunnerConfig{AttachmentContent: content, VisualChecks: func(*models.AgentRun) presentation.ValidateOptions {
				return presentation.ValidateOptions{Bindings: presentation.BindingResolverFunc(func(context.Context, presentation.Binding) error {
					return errors.New("not in this chat")
				})}
			}})
			conversation, run := f.StartChat("how is revenue?")
			done := f.DriveToRest(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			if finals := portFinalEntries(f, run.ID); len(finals) != 1 || finals[0].State != models.ToolEntryCompleted {
				t.Fatalf("final entries = %v, want one delivered answer", f.EntryTypes(run.ID))
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || answers[0].Content != "Revenue is up." || len(answers[0].References) != 1 || content.Puts != 0 {
				t.Fatalf("answers = %#v (%d visual puts), want the answer without the visual", answers, content.Puts)
			}
			if events := f.Events(run.ID, "turn.visual_rejected"); len(events) != 1 || events[0].Severity != models.ActivitySeverityWarning {
				t.Fatalf("visual rejected events = %#v", events)
			}
		})
	})

	t.Run("silent_spoken", func(t *testing.T) {
		looptest.EachStore(t, "port_silent", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "silent", Reply: func(v looptest.StepView) models.ModelStepResult {
				return looptest.FinalAnswer(map[string]any{"answer": "", "silent": true})
			}}))
			conversation := convChat(f, models.ObjectiveOwner{}, nil)
			spoken := convPost(f, conversation.ID, "mm, nice weather", func(r *runtimeconversation.PostChannelMessageRequest) {
				r.ResponseMode = "spoken"
				r.RequiresResponse = true
			})
			run := convDrive(f, convSchedule(f, conversation.ID, spoken).ID)
			if run.Output[agentrun.FinalOutputSilentKey] != true {
				t.Fatalf("output = %#v, want silent", run.Output)
			}
			if answers := portAgentMessages(f, conversation.ID); len(answers) != 0 {
				t.Fatalf("a silent spoken answer posted %s", looptest.DescribeMessages(answers))
			}
			if finals := portFinalEntries(f, run.ID); len(finals) != 1 || finals[0].State != models.ToolEntryCompleted {
				t.Fatalf("final entries = %v", f.EntryTypes(run.ID))
			}
		})
	})

	t.Run("generated_files", func(t *testing.T) {
		looptest.EachStore(t, "port_files", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(
				&looptest.ModelRule{Name: "export", Times: 1, Reply: func(v looptest.StepView) models.ModelStepResult {
					return looptest.Act("Exporting the report.", looptest.Call(looptest.GovernedTool(v, "lookup"), map[string]any{"id": "report"}))
				}},
				&looptest.ModelRule{Name: "answer", Reply: func(looptest.StepView) models.ModelStepResult {
					return looptest.FinalAnswer(map[string]any{"answer": "Here is the quarterly report.", "files": []any{"report-pdf", "report-csv"}})
				}},
			))
			f.Dispatch.On("lookup", func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"artifactRefs": []interface{}{
					map[string]interface{}{"id": "report-pdf", "version": 1},
					map[string]interface{}{"id": "report-csv", "version": 2},
				}}, nil
			})
			conversation, run := f.StartChat("export the quarterly report")
			done := f.DriveToRest(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 {
				t.Fatalf("channel = %s", looptest.DescribeMessages(f.Messages(conversation.ID)))
			}
			want := []models.ConversationReference{
				{Kind: models.ConversationReferenceRun, ID: run.ID},
				{Kind: models.ConversationReferenceArtifact, ID: "report-pdf", Version: 1},
				{Kind: models.ConversationReferenceArtifact, ID: "report-csv", Version: 2},
			}
			got := answers[0].References
			if len(got) != len(want) {
				t.Fatalf("references = %#v, want %#v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("references = %#v, want %#v", got, want)
				}
			}
		})
	})

	t.Run("broadcast", func(t *testing.T) {
		looptest.EachStore(t, "port_broadcast", func(t *testing.T, store models.KernelStore) {
			f := looptest.NewFixture(t, store, looptest.WithRules(&looptest.ModelRule{Name: "answer", Reply: func(v looptest.StepView) models.ModelStepResult {
				if !v.HasTool(string(models.ControlToolFinalAnswer)) {
					t.Errorf("no final answer tool: %s", v.Describe())
				}
				return looptest.FinalAnswer(map[string]any{"answer": "Shipped to everyone.", "broadcastToChannel": true})
			}}))
			conversation, run := f.StartChat("ship it")
			done := f.DriveToRest(run.ID, 3)
			f.MustStatus(done, models.AgentRunStatusCompleted)
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || !answers[0].BroadcastToChannel || done.Output[agentrun.FinalOutputBroadcastKey] != true {
				t.Fatalf("answers = %#v, output = %#v", answers, done.Output)
			}
		})
	})
}

// failurePortVisual is a copy of the conversation run tests' finalDeliveryVisual; change them
// together.
func failurePortVisual() *presentation.Visual {
	return &presentation.Visual{Title: "Quarterly revenue", Components: []presentation.Component{
		{ID: "revenue", Type: "stat_row", Summary: "Revenue grew 12%.", StatRow: &presentation.StatRow{Stats: []presentation.Stat{
			{Label: "Revenue", Value: 1200000, Format: presentation.StatCurrency, Currency: "USD"},
		}}},
	}}
}
