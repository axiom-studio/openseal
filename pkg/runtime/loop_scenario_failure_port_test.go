package runtime

// Run-loop scenarios ported from the old failure, evidence and answer-visual
// tests (run-loop section 12 §5.1, tests-T17b): a tool failure goes back to
// the model, a terminal failure is a typed Run.FailureExplanation with a
// visible chat message, the final answer checks answer with tool errors the
// model repairs, and the final answer tool carries visuals, silence,
// broadcast and generated files. Every scenario drives the real RunLoop
// through the loop fixture; only the model, the dispatcher, the host tools,
// the visual host checks and the evidence reviewer are fakes.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// portVisualArtifactID is the stable artifact ID of the visual on the answer
// to trigger: a replayed answer must reference the visual its first attempt
// stored.
func portVisualArtifactID(runID, triggerID string) string {
	return "visual-" + strings.TrimPrefix(models.SHA256DigestString(runID+"\x00"+triggerID), "sha256:")[:40]
}

// answerVisualContent is the artifact content store visuals are written to.
type answerVisualContent struct {
	puts int
	data []byte
}

func (s *answerVisualContent) Put(_ context.Context, req ArtifactContentWrite) (ArtifactStoredContent, error) {
	s.puts++
	s.data, _ = io.ReadAll(req.Reader)
	return ArtifactStoredContent{ContentRef: "visual-content", SizeBytes: int64(len(s.data)), Digest: hashBytes(s.data)}, nil
}

func (s *answerVisualContent) Open(context.Context, Scope, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data)), nil
}

// ---------------------------------------------------------------------------
// Helpers

// portAgentMessages is the conversation's messages from the fixture Agent.
func portAgentMessages(f *loopFixture, conversationID string) []*ChannelMessage {
	f.t.Helper()
	var out []*ChannelMessage
	for _, message := range f.messages(conversationID) {
		if message.Sender.Type == ConversationParticipantAgent && message.Sender.ID == loopFixtureAgent {
			out = append(out, message)
		}
	}
	return out
}

// portFinalEntries is the Run's turn_final_answer tool entries in log order.
func portFinalEntries(f *loopFixture, runID string) []*SessionEntry {
	f.t.Helper()
	var out []*SessionEntry
	for _, entry := range f.entries(runID) {
		if entry.Type == SessionEntryTool && entry.Tool != nil && entry.Tool.Name == string(ControlToolFinalAnswer) && entry.RunID == runID {
			out = append(out, entry)
		}
	}
	return out
}

func portEntryText(entry *SessionEntry) string {
	if entry == nil || entry.Tool == nil || entry.Tool.Result == nil {
		return ""
	}
	return kitPartsText(entry.Tool.Result.Content)
}

// portLastToolFailed reports whether the newest tool result the step sees is
// an error.
func portLastToolFailed(v stepView) bool {
	results := v.ToolResultsSinceUser()
	return len(results) > 0 && results[len(results)-1].Tool != nil && results[len(results)-1].Tool.Result.IsError
}

// portRequireTypedFailure checks the D4 contract of a terminal failure: a
// typed code and message on the Run, and no failure state anywhere else.
func portRequireTypedFailure(t *testing.T, run *AgentRun) {
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
func portReportFailure(f *loopFixture, conversationID string, failed *AgentRun) *ChannelMessage {
	f.t.Helper()
	if err := projectFailedConversationReply(f.ctx, f.chats, failed); err != nil {
		f.t.Fatalf("report failed run: %v", err)
	}
	answers := portAgentMessages(f, conversationID)
	if len(answers) != 1 {
		f.t.Fatalf("agent messages after the report = %s, want one", kitDescribeMessages(f.messages(conversationID)))
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
	eachStore(t, "port_tool_failure", func(t *testing.T, store KernelStore) {
		var seen string
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "explode", times: 1, reply: func(v stepView) ModelStepResult {
				return act("Reading the service.", call(kitGovernedTool(v, "explode"), nil))
			}},
			&modelRule{name: "explain", when: portLastToolFailed, reply: func(v stepView) ModelStepResult {
				results := v.ToolResultsSinceUser()
				seen = kitPartsText(results[len(results)-1].Tool.Result.Content)
				return say("I couldn't read it: the fixture service exploded.")
			}},
		))
		conversation, run := f.startChat("read the fixture service")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)
		if done.FailureExplanation != nil || done.Error != "" {
			t.Fatalf("completed run carries failure %#v / %q", done.FailureExplanation, done.Error)
		}
		var failedEntry *SessionEntry
		for _, entry := range f.entries(run.ID) {
			if entry.Type == SessionEntryTool && entry.Tool != nil && strings.HasSuffix(entry.Tool.Name, "explode") {
				failedEntry = entry
			}
		}
		if failedEntry == nil || failedEntry.State != ToolEntryError || !failedEntry.Tool.Result.IsError {
			t.Fatalf("explode entry = %#v", failedEntry)
		}
		if !strings.Contains(seen, "the fixture service exploded") || seen != portEntryText(failedEntry) {
			t.Fatalf("the model saw %q, the entry holds %q", seen, portEntryText(failedEntry))
		}
		if calls := f.calls(run.ID); len(calls) != 1 || calls[0].Status != ActionCallStatusFailed {
			t.Fatalf("action calls = %#v", calls)
		}
		answers := portAgentMessages(f, conversation.ID)
		if len(answers) != 1 || answers[0].Content != "I couldn't read it: the fixture service exploded." {
			t.Fatalf("channel = %s", kitDescribeMessages(f.messages(conversation.ID)))
		}
		if len(f.model.calls(ModelStepPurposeExplanation)) != 0 {
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
		eachStore(t, "port_host_exhausted", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(&modelRule{name: "down",
				err: &modelstep.TurnHostFailure{Code: "host_draining", Message: "The step host is restarting.", Retryable: true}}))
			conversation, run := f.startChat("check issue 1")
			const hostRetries = 3 // the run loop requeues a retryable host failure three times
			failed := f.driveToRest(run.ID, hostRetries+2)
			f.mustStatus(failed, AgentRunStatusFailed)
			portRequireTypedFailure(t, failed)
			if failed.FailureExplanation.Code != StepFailureHostUnavailable || failed.FailureExplanation.Explained {
				t.Fatalf("failure = %#v, want an unexplained %s", failed.FailureExplanation, StepFailureHostUnavailable)
			}
			if got := len(f.model.calls("")); got != hostRetries+1 {
				t.Fatalf("model calls = %d, want %d (3 requeues, then the failure)", got, hostRetries+1)
			}
			if len(portAgentMessages(f, conversation.ID)) != 0 {
				t.Fatal("the loop posted a reply for an unexplained failure")
			}
			reply := portReportFailure(f, conversation.ID, failed)
			if want := terminalRunFailureReply(f.ctx, f.raw, f.raw, failed); reply.Content != want || strings.Contains(reply.Content, "restarting") {
				t.Fatalf("reply = %q, want the kernel reply %q", reply.Content, want)
			}
		})
	})

	t.Run("doom_loop", func(t *testing.T) {
		eachStore(t, "port_doom_loop", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(
				&modelRule{name: "lookup", when: stepPurpose, reply: func(v stepView) ModelStepResult {
					return act("Checking.", call(kitGovernedTool(v, "lookup"), repeatLookupArgs))
				}},
				&modelRule{name: "explain", when: purposeIs(ModelStepPurposeExplanation), reply: func(stepView) ModelStepResult {
					return say("I kept getting the same answer for issue 6815, so I stopped.")
				}},
			))
			conversation, run := f.startChat("what is the status of 6815?")
			failed := f.driveToRest(run.ID, 4)
			f.mustStatus(failed, AgentRunStatusFailed)
			portRequireTypedFailure(t, failed)
			if failed.FailureExplanation.Code != action.RunFailureCodeRepeatLimit || !failed.FailureExplanation.Explained {
				t.Fatalf("failure = %#v, want an explained %s", failed.FailureExplanation, action.RunFailureCodeRepeatLimit)
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || !strings.Contains(answers[0].Content, "so I stopped") {
				t.Fatalf("channel = %s, want the model's explanation", kitDescribeMessages(f.messages(conversation.ID)))
			}
		})
	})

	t.Run("second_length_rejection", func(t *testing.T) {
		eachStore(t, "port_length_twice", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(
				lookupRule("lookup", 5, nil, nil),
				&modelRule{name: "summary", when: purposeIs(ModelStepPurposeSummary),
					reply: func(stepView) ModelStepResult { return say("Issues 1 to 5 were looked up.") }},
				&modelRule{name: "too long", when: purposeIs(ModelStepPurposeStep), err: contextExceeded()},
			))
			f.dispatch.on("lookup", bigLookup)
			conversation, run := f.startChat("check issues 1 to 5")
			exit, failed := f.runClaimWith(f.ctx, run.ID, f.withLoopCompactor)
			if exit.Reason != RunLoopExitFailed || exit.Err != nil {
				t.Fatalf("exit = %s (%v), want failed", exit.Reason, exit.Err)
			}
			f.mustStatus(failed, AgentRunStatusFailed)
			portRequireTypedFailure(t, failed)
			if failed.FailureExplanation.Explained || len(f.model.calls(ModelStepPurposeExplanation)) != 0 {
				t.Fatalf("failure = %#v; a context overflow is not explained by the model", failed.FailureExplanation)
			}
			if steps := f.model.calls(ModelStepPurposeStep); len(steps) != 7 || steps[6].Attempt != 1 {
				t.Fatalf("step calls = %d, want 5 lookups and two attempts of the sixth step", len(steps))
			}
			if reply := portReportFailure(f, conversation.ID, failed); reply.Content != terminalRunFailureReply(f.ctx, f.raw, f.raw, failed) {
				t.Fatalf("reply = %q", reply.Content)
			}
		})
	})

	// A spent budget pauses the Run (the owner can raise the budget and
	// resume); it is not a failure. Spec 12 §5.1 lists it among the failures,
	// but the loop pauses with budget.exhausted (TestRunLoopBudgetExhaustion
	// PausesAfterStep); this subtest pins that the pause records no failure.
	t.Run("budget_exhausted_pauses", func(t *testing.T) {
		eachStore(t, "port_budget", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(lookupRule("lookup", 0, purposeIs(ModelStepPurposeStep), nil)))
			run := f.startBoundedRun("Check issues 1 to 3", BudgetPolicy{MaxTurns: 2})
			paused := f.driveToRest(run.ID, 2)
			f.mustStatus(paused, AgentRunStatusPaused)
			if paused.BudgetState != BudgetStateExhausted || paused.FailureExplanation != nil {
				t.Fatalf("paused run budget %s failure %#v", paused.BudgetState, paused.FailureExplanation)
			}
			if len(f.events(run.ID, "budget.exhausted")) != 1 || len(f.model.calls(ModelStepPurposeStep)) != 2 {
				t.Fatalf("budget.exhausted = %d, steps = %d", len(f.events(run.ID, "budget.exhausted")), len(f.model.calls(ModelStepPurposeStep)))
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
		eachStore(t, prefix, func(t *testing.T, store KernelStore) {
			var rejection string
			f := newLoopFixture(t, store, withRules(
				&modelRule{name: "claim", times: 1, reply: func(stepView) ModelStepResult {
					return finalAnswer(map[string]any{"answer": claim})
				}},
				&modelRule{name: "repair", when: portLastToolFailed, reply: func(v stepView) ModelStepResult {
					rejection, _, _ = v.ToolResult(string(ControlToolFinalAnswer))
					return finalAnswer(map[string]any{"answer": "I have not done that yet; nothing was sent or submitted."})
				}},
			))
			conversation, run := f.startChat("please email Ana the report")
			done := f.driveToRest(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			finals := portFinalEntries(f, run.ID)
			if len(finals) != 2 || finals[0].State != ToolEntryError || !finals[0].Tool.Result.IsError || finals[1].State != ToolEntryCompleted {
				t.Fatalf("final entries = %v", f.entryTypes(run.ID))
			}
			if text := portEntryText(finals[0]); !strings.Contains(text, code) || text != rejection {
				t.Fatalf("rejection = %q (model saw %q), want code %s", text, rejection, code)
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || !strings.HasPrefix(answers[0].Content, "I have not done that yet") {
				t.Fatalf("channel = %s, want only the repaired answer", kitDescribeMessages(f.messages(conversation.ID)))
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
		eachStore(t, "port_visual", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(&modelRule{name: "answer", reply: func(stepView) ModelStepResult {
				return finalAnswer(map[string]any{"answer": "Revenue is up.", "visual": finalDeliveryVisual()})
			}}))
			content := &answerVisualContent{}
			convRunner(f, ConversationRunTurnRunnerConfig{AttachmentContent: content})
			conversation, run := f.startChat("how is revenue?")
			trigger := f.messages(conversation.ID)[0]
			done := f.driveToRest(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			id := portVisualArtifactID(run.ID, trigger.ID)
			answers := portAgentMessages(f, conversation.ID)
			want := []ConversationReference{{Kind: ConversationReferenceRun, ID: run.ID}, {Kind: ConversationReferenceArtifact, ID: id, Version: 1}}
			if len(answers) != 1 || answers[0].Content != "Revenue is up." || answers[0].Intent != MessageIntentAnswer ||
				len(answers[0].References) != 2 || answers[0].References[0] != want[0] || answers[0].References[1] != want[1] {
				t.Fatalf("answers = %#v", answers)
			}
			for _, message := range f.messages(conversation.ID) {
				if message.Sender.Type == ConversationParticipantService {
					t.Fatalf("a receipt was posted for the visual: %#v", message)
				}
			}
			artifact, err := f.raw.(ArtifactStore).GetArtifact(f.ctx, f.scope, id, 1)
			if err != nil || artifact == nil || artifact.Type != presentation.ArtifactType || artifact.MediaType != presentation.MediaType ||
				artifact.Provenance.RunID != run.ID || artifact.Provenance.Producer != (ActivityActor{Type: "agent", ID: loopFixtureAgent}) {
				t.Fatalf("artifact = %#v, %v", artifact, err)
			}
			surface, err := presentation.Decode(content.data)
			if err != nil || surface.Title != "Quarterly revenue" || content.puts != 1 {
				t.Fatalf("stored visual = %#v (%d puts), %v", surface, content.puts, err)
			}
			if _, ok := done.Output[agentrun.FinalOutputVisualKey]; ok {
				t.Fatalf("the visual stayed in the Run output: %#v", done.Output)
			}
		})
	})

	t.Run("visual_rejected_by_host", func(t *testing.T) {
		eachStore(t, "port_visual_rejected", func(t *testing.T, store KernelStore) {
			bound := finalDeliveryVisual()
			bound.Components = append(bound.Components, presentation.Component{ID: "next", Type: "actions", Actions: []presentation.Action{
				{ID: "stop", Label: "Stop", Kind: presentation.ActionEntity, Command: "stop", Binding: &presentation.Binding{Kind: presentation.BindingRunbook, ID: "rb-1"}},
			}})
			f := newLoopFixture(t, store, withRules(&modelRule{name: "answer", reply: func(stepView) ModelStepResult {
				return finalAnswer(map[string]any{"answer": "Revenue is up.", "visual": bound})
			}}))
			content := &answerVisualContent{}
			convRunner(f, ConversationRunTurnRunnerConfig{AttachmentContent: content, VisualChecks: func(*AgentRun) presentation.ValidateOptions {
				return presentation.ValidateOptions{Bindings: presentation.BindingResolverFunc(func(context.Context, presentation.Binding) error {
					return errors.New("not in this chat")
				})}
			}})
			conversation, run := f.startChat("how is revenue?")
			done := f.driveToRest(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			finals := portFinalEntries(f, run.ID)
			if len(finals) != 2 || finals[0].State != ToolEntryError || !strings.HasPrefix(portEntryText(finals[0]), "The visual failed its checks: ") {
				t.Fatalf("final entries = %v (%q)", f.entryTypes(run.ID), portEntryText(finals[0]))
			}
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || answers[0].Content != "Revenue is up." || len(answers[0].References) != 1 || content.puts != 0 {
				t.Fatalf("answers = %#v (%d visual puts), want the answer without the visual", answers, content.puts)
			}
			if events := f.events(run.ID, "turn.visual_rejected"); len(events) != 1 || events[0].Severity != ActivitySeverityWarning {
				t.Fatalf("visual rejected events = %#v", events)
			}
		})
	})

	t.Run("silent_spoken", func(t *testing.T) {
		eachStore(t, "port_silent", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(&modelRule{name: "silent", reply: func(v stepView) ModelStepResult {
				return finalAnswer(map[string]any{"answer": "", "silent": true})
			}}))
			conversation := convChat(f, ObjectiveOwner{}, nil)
			spoken := convPost(f, conversation.ID, "mm, nice weather", func(r *PostChannelMessageRequest) {
				r.ResponseMode = "spoken"
				r.RequiresResponse = true
			})
			run := convDrive(f, convSchedule(f, conversation.ID, spoken).ID)
			if run.Output[agentrun.FinalOutputSilentKey] != true {
				t.Fatalf("output = %#v, want silent", run.Output)
			}
			if answers := portAgentMessages(f, conversation.ID); len(answers) != 0 {
				t.Fatalf("a silent spoken answer posted %s", kitDescribeMessages(answers))
			}
			if finals := portFinalEntries(f, run.ID); len(finals) != 1 || finals[0].State != ToolEntryCompleted {
				t.Fatalf("final entries = %v", f.entryTypes(run.ID))
			}
		})
	})

	t.Run("generated_files", func(t *testing.T) {
		eachStore(t, "port_files", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(
				&modelRule{name: "export", times: 1, reply: func(v stepView) ModelStepResult {
					return act("Exporting the report.", call(kitGovernedTool(v, "lookup"), map[string]any{"id": "report"}))
				}},
				&modelRule{name: "answer", reply: func(stepView) ModelStepResult {
					return finalAnswer(map[string]any{"answer": "Here is the quarterly report.", "files": []any{"report-pdf", "report-csv"}})
				}},
			))
			f.dispatch.on("lookup", func(agentrun.ActionDispatchInput) (map[string]interface{}, error) {
				return map[string]interface{}{"artifactRefs": []interface{}{
					map[string]interface{}{"id": "report-pdf", "version": 1},
					map[string]interface{}{"id": "report-csv", "version": 2},
				}}, nil
			})
			conversation, run := f.startChat("export the quarterly report")
			done := f.driveToRest(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 {
				t.Fatalf("channel = %s", kitDescribeMessages(f.messages(conversation.ID)))
			}
			want := []ConversationReference{
				{Kind: ConversationReferenceRun, ID: run.ID},
				{Kind: ConversationReferenceArtifact, ID: "report-pdf", Version: 1},
				{Kind: ConversationReferenceArtifact, ID: "report-csv", Version: 2},
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
		eachStore(t, "port_broadcast", func(t *testing.T, store KernelStore) {
			f := newLoopFixture(t, store, withRules(&modelRule{name: "answer", reply: func(v stepView) ModelStepResult {
				if !v.HasTool(string(ControlToolFinalAnswer)) {
					t.Errorf("no final answer tool: %s", v.describe())
				}
				return finalAnswer(map[string]any{"answer": "Shipped to everyone.", "broadcastToChannel": true})
			}}))
			conversation, run := f.startChat("ship it")
			done := f.driveToRest(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			answers := portAgentMessages(f, conversation.ID)
			if len(answers) != 1 || !answers[0].BroadcastToChannel || done.Output[agentrun.FinalOutputBroadcastKey] != true {
				t.Fatalf("answers = %#v, output = %#v", answers, done.Output)
			}
		})
	})
}
