package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	"github.com/google/uuid"
)

// The conversation seams of the RunLoop (run-loop section 05 §7, §13 on
// SPEC §2.4), on the SQLite store. A chat Run's binding comes from the
// conversation turn runner, as the worker resolves it. Runs are driven
// through the real RunLoop with a recording model producer, or the seams are
// called the way the loop calls them (appendStepContext is the loop's
// phase 3).

func TestChatRunAnswersFromSessionNotGoal(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		earlier := f.post("earlier", "user-1", "The launch is on Friday.")
		trigger := f.post("trigger", "user-1", "When is the launch?")
		run := f.startRun(trigger)
		producer := &chatLoopProducer{answers: []string{"Friday."}}
		binding := f.bind(run, producer)
		exit := f.drive(run, binding)
		if exit.Reason != RunLoopExitFinal || exit.Run.Status != AgentRunStatusCompleted {
			t.Fatalf("exit = %s, run %s", exit.Reason, exit.Run.Status)
		}
		if len(producer.paths) != 1 {
			t.Fatalf("model steps = %d", len(producer.paths))
		}
		rendered := producer.paths[0]
		chatLoopExpectShape(t, rendered, "user:"+earlier.ID, "user:"+trigger.ID, "note:run_context:"+run.ID)
		for _, entry := range rendered {
			text := chatLoopEntryText(entry)
			if entry.User != nil && entry.User.Origin != UserEntryChannelMessage || strings.Contains(text, run.Goal) || strings.Contains(text, `"goal"`) {
				t.Fatalf("goal JSON reached the step: %#v %q", entry, text)
			}
		}
		if !strings.HasSuffix(chatLoopEntryText(rendered[1]), "\nWhen is the launch?") || !strings.Contains(chatLoopEntryText(rendered[2]), trigger.ID) {
			t.Fatalf("trigger %q, note %q", chatLoopEntryText(rendered[1]), chatLoopEntryText(rendered[2]))
		}
		answers := f.agentMessages()
		if len(answers) != 1 || answers[0].Content != "Friday." || answers[0].ReplyToMessageID != trigger.ID ||
			answers[0].References[0] != (ConversationReference{Kind: ConversationReferenceRun, ID: run.ID}) {
			t.Fatalf("answers = %#v", answers)
		}
		session := f.run(run.ID).SessionID
		if session != binding.Session.ID || session != conversationSessionID(run.Scope, run.ConcurrencyKey) {
			t.Fatalf("run session %q, binding %q", session, binding.Session.ID)
		}
		// The read receipt follows the committed watermark.
		cursor, err := f.service.GetCursor(f.ctx, run.Scope, f.conversation.ID, ConversationParticipant{Type: ConversationParticipantAgent, ID: "agent-1"})
		if err != nil || cursor == nil || cursor.ReadSequence < trigger.Sequence {
			t.Fatalf("agent cursor = %#v, %v (trigger %d)", cursor, err, trigger.Sequence)
		}
	})
}

func TestSecondMessageSeesFirstExchangeAsLogEntries(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		first := f.post("first", "user-1", "Draft the agenda.")
		run1 := f.startRun(first)
		f.drive(run1, f.bind(run1, &chatLoopProducer{answers: []string{"Agenda: intro, demo, Q&A."}}))

		second := f.post("second", "user-1", "Add a break.")
		run2 := f.startRun(second)
		producer := &chatLoopProducer{answers: []string{"Added."}}
		exit := f.drive(run2, f.bind(run2, producer))
		if exit.Run.Status != AgentRunStatusCompleted || len(producer.paths) != 1 {
			t.Fatalf("exit = %#v, steps %d", exit, len(producer.paths))
		}
		rendered := producer.paths[0]
		chatLoopExpectShape(t, rendered,
			"user:"+first.ID, "note:run_context:"+run1.ID, "assistant:"+run1.ID,
			"user:"+second.ID, "note:run_context:"+run2.ID)
		if chatLoopEntryText(rendered[2]) != "Agenda: intro, demo, Q&A." || rendered[3].User.Author.Type != "user" {
			t.Fatalf("first exchange = %q / %#v", chatLoopEntryText(rendered[2]), rendered[3].User.Author)
		}
		if answers := f.agentMessages(); len(answers) != 2 || answers[1].Content != "Added." {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestFollowUpFromSamePersonAppendedAtNextStepOnce(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Summarize the report.")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		f.prepare(run, binding, 1)

		followUp := f.post("follow-up", "user-1", "Only the risks, please.")
		current := f.run(run.ID)
		updated := cloneAgentRun(current)
		updated.PendingInterventions = append(updated.PendingInterventions, AgentRunIntervention{
			ID: conversationFollowUpInterventionID(run.Scope, followUp.ID), Actor: ActivityActor{Type: "user", ID: "user-1"},
			Instruction: followUp.Content, CreatedAt: f.now,
			ConversationMessage: &ConversationMessageDelivery{ConversationID: f.conversation.ID, MessageID: followUp.ID, Sequence: followUp.Sequence, Sender: followUp.Sender},
		})
		updated.Revision++
		if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, &ActivityEvent{
			ID: "follow-up-" + uuid.NewString()[:8], Scope: run.Scope, EventType: "run.updated", Severity: ActivitySeverityInfo,
			RunID: run.ID, Actor: ActivityActor{Type: "worker", ID: chatLoopWorker}, Summary: "Follow-up delivered",
			Visibility: ActivityVisibilityScope, CreatedAt: f.now,
		}, nil); err != nil {
			t.Fatal(err)
		}

		second := f.prepare(run, binding, 2)
		if !second.CursorMoved {
			t.Fatal("the follow-up was not consumed")
		}
		f.prepare(run, binding, 3)
		chatLoopExpectShape(t, f.path(binding.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID, "user:"+followUp.ID)
		entries := f.path(binding.Session.ID)
		if !strings.Contains(chatLoopEntryText(entries[2]), "(user user-1)") || entries[2].Step != 2 {
			t.Fatalf("follow-up entry = %q at step %d", chatLoopEntryText(entries[2]), entries[2].Step)
		}
		if got := f.run(run.ID).ConsumedInterventions; got != 1 {
			t.Fatalf("consumed interventions = %d", got)
		}
	})
}

func TestOtherPersonsMessageStartsOwnRunAndInterleavesAfterApprovalPark(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		triggerA := f.post("a", "user-1", "Send the invoice.")
		runA := f.startRun(triggerA)
		bindingA := f.bind(runA, &chatLoopProducer{})
		f.prepare(runA, bindingA, 1)
		// A's governed call is held for approval; A parks.
		f.stepEntries(runA, bindingA.Session.ID, "a-step-1", 1,
			ModelToolCall{ID: "send", Name: "billing__send_invoice", Arguments: json.RawMessage(`{"invoice":"7"}`)})

		triggerB := f.post("b", "user-2", "What is on my calendar?")
		runB := f.startRun(triggerB)
		bindingB := f.bind(runB, &chatLoopProducer{})
		f.prepare(runB, bindingB, 1)
		f.stepEntries(runB, bindingB.Session.ID, "b-step-1", 1)

		// A resumes after the approval: nothing new is ingested for it.
		if result := f.prepare(runA, bindingA, 2); result.Append != nil && len(result.Append.Entries) > 0 {
			t.Fatalf("A ingested again: %#v", result.Append.Entries)
		}
		chatLoopExpectShape(t, f.path(bindingA.Session.ID),
			"user:"+triggerA.ID, "note:run_context:"+runA.ID, "assistant:"+runA.ID, "tool:"+runA.ID,
			"user:"+triggerB.ID, "note:run_context:"+runB.ID, "assistant:"+runB.ID)
		if bindingA.Session.ID != bindingB.Session.ID {
			t.Fatalf("one lane, two sessions: %s %s", bindingA.Session.ID, bindingB.Session.ID)
		}
		entries := f.path(bindingB.Session.ID)
		if entries[4].User.Author.ID != "user-2" || !strings.Contains(chatLoopEntryText(entries[5]), triggerB.ID) {
			t.Fatalf("B's trigger %#v, note %q", entries[4].User.Author, chatLoopEntryText(entries[5]))
		}
	})
}

// TestRunActionsPresentWithoutActiveWork: a chat with no active work keeps
// the run-control actions (the tool list does not change when work starts or
// ends); a call names no active work, so it is refused.
func TestRunActionsPresentWithoutActiveWork(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		definition := agentrun.RunManagementSkill()
		pause := capability.ModelAction{
			Name: "openseal_runs__pause", SkillID: RunManagementSkillID, Version: agentrun.RunManagementSkillVersion,
			Action: RunActionPause, InputSchema: definition.Actions[RunActionPause].InputSchema,
		}
		other := capability.ModelAction{Name: "reports__export", SkillID: "reports", Action: "export"}
		quiet := f.post("quiet", "user-1", "Hello")
		idle := f.startRun(quiet)
		binding := f.bind(idle, &chatLoopProducer{}, pause, other)
		if len(binding.ModelActions) != 2 || binding.ModelActions[0].Name != pause.Name {
			t.Fatalf("idle chat actions = %#v", binding.ModelActions)
		}
		refusal, err := binding.ToolCheck(f.ctx, agentrun.StepToolsInput{Run: idle, Binding: binding}, &SessionEntry{
			Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: "call",
			Tool: &ToolEntry{Name: pause.Name, Arguments: json.RawMessage(`{"runId":"nope"}`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(refusal, "is not active work in this conversation") {
			t.Fatalf("idle refusal = %q", refusal)
		}
	})
}

func TestRunControlInvalidRunIdIsToolError(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		definition := agentrun.RunManagementSkill()
		pause := capability.ModelAction{
			Name: "openseal_runs__pause", SkillID: RunManagementSkillID, Version: agentrun.RunManagementSkillVersion,
			Action: RunActionPause, InputSchema: definition.Actions[RunActionPause].InputSchema,
		}
		resume := pause
		resume.Name, resume.Action, resume.InputSchema = "openseal_runs__resume", RunActionResume, definition.Actions[RunActionResume].InputSchema
		other := capability.ModelAction{Name: "reports__export", SkillID: "reports", Action: "export"}

		trigger := f.post("pause", "user-1", "Pause the export.")
		run := f.startRun(trigger)
		portfolio := NewPortfolioService(f.store, models.WithClock(func() time.Time { return f.now }))
		work, err := portfolio.CreateAgentRun(f.ctx, CreateAgentRunRequest{
			Scope: run.Scope, Kind: RunKindAgentWork, ParentRunID: run.ID, Owner: run.Owner, AssignedAgentID: "agent-1",
			Goal: "Export the report", Source: RunSourceFork, IdempotencyKey: "work-" + run.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		binding := f.bind(run, &chatLoopProducer{}, pause, resume, other)
		if len(binding.ModelActions) != 3 {
			t.Fatalf("active chat actions = %#v", binding.ModelActions)
		}
		if _, ok := binding.ModelActions[0].InputSchema["properties"].(map[string]interface{})["runId"].(map[string]interface{})["enum"]; ok {
			t.Fatal("run id schema carries a per-claim enum")
		}
		check := func(name, runID string) string {
			t.Helper()
			refusal, err := binding.ToolCheck(f.ctx, agentrun.StepToolsInput{Run: run, Binding: binding}, &SessionEntry{
				Type: SessionEntryTool, State: ToolEntryPending, ToolCallID: "call",
				Tool: &ToolEntry{Name: name, Arguments: json.RawMessage(fmt.Sprintf(`{"runId":%q}`, runID))},
			})
			if err != nil {
				t.Fatal(err)
			}
			return refusal
		}
		if refusal := check(pause.Name, work.ID); refusal != "" {
			t.Fatalf("pausing active work refused: %q", refusal)
		}
		want := "Run nope is not active work in this conversation, or it cannot be paused now. Use a run id from the active work in the run context."
		if refusal := check(pause.Name, "nope"); refusal != want {
			t.Fatalf("refusal = %q", refusal)
		}
		if refusal := check(resume.Name, work.ID); !strings.Contains(refusal, "cannot be resumed now") {
			t.Fatalf("resuming running work = %q", refusal)
		}
		if refusal := check(pause.Name, ""); refusal != "A runId from the active work in the run context is required." {
			t.Fatalf("missing run id refusal = %q", refusal)
		}
		if refusal := check(other.Name, "nope"); refusal != "" {
			t.Fatalf("a non run-control call was refused: %q", refusal)
		}
	})
}

// A tool that saves files posts nothing: work-in-progress files (panels
// drawn for a comic) stay in the Library, and the final answer hands over
// the finished ones.
func TestNoMessageAfterAGovernedToolSavesFiles(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Export both reports.")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		f.prepare(run, binding, 1)
		entries := f.stepEntries(run, binding.Session.ID, "step-1", 1,
			ModelToolCall{ID: "a", Name: "reports__export", Arguments: json.RawMessage(`{"report":"a"}`)},
			ModelToolCall{ID: "b", Name: "reports__export", Arguments: json.RawMessage(`{"report":"b"}`)})
		for _, id := range []string{"a", "b"} {
			actionID := "action-" + id
			artifact := &Artifact{ID: "file-" + id, Version: 1, Scope: run.Scope, Name: id + ".pdf",
				Provenance: ArtifactProvenance{Producer: ActivityActor{Type: "agent", ID: "agent-1"}, Owner: &run.Owner, RunID: run.ID, ActionID: actionID},
				ContentRef: "object-store:" + id, Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10, Classification: ArtifactClassificationInternal}
			if err := f.store.CreateArtifactVersion(f.ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			f.actions.calls[actionID] = &ActionCall{ID: actionID, Scope: run.Scope, RunID: run.ID, Status: ActionCallStatusSucceeded,
				Output: map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": artifact.ID, "version": 1}}}}
			done := f.settle(run, entries[id], ToolEntryCompleted, actionID, &ToolResult{Status: ToolResultSucceeded, Content: []ContentPart{storetest.TextPart("saved")}})
			for range 2 { // the hook is replay-safe
				if err := binding.AfterTool(f.ctx, agentrun.StepToolsInput{Run: f.run(run.ID), Binding: binding}, done); err != nil {
					t.Fatal(err)
				}
			}
		}
		if posted := f.agentMessages(); len(posted) != 0 {
			t.Fatalf("posted after tools = %#v", posted)
		}
	})
}

// Participation is on when the Run is bound and stopped before its first
// step: the step's seams end the Run without a model call.
func TestParticipationStoppedBeforeFirstStepPostsNothing(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{participation: true}, func(t *testing.T, f *chatLoopFixture) {
		f.setParticipation(true)
		trigger := f.post("trigger", "user-1", "Anyone there?")
		run := f.startRun(trigger)
		producer := &chatLoopProducer{}
		binding := f.bind(run, producer)
		f.setParticipation(false)
		exit := f.drive(run, binding)
		if exit.Reason != RunLoopExitFinal || exit.Run.Status != AgentRunStatusCompleted || exit.Run.Output["participationSkipped"] != true {
			t.Fatalf("exit = %s %s %#v", exit.Reason, exit.Run.Status, exit.Run.Output)
		}
		if len(producer.paths) != 0 {
			t.Fatalf("the model was called %d times", len(producer.paths))
		}
		if messages := f.agentMessages(); len(messages) != 0 {
			t.Fatalf("posted = %#v", messages)
		}
	})
}

func TestTeamReplacedApprovalEndsMemberSilently(t *testing.T) {
	for _, followUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("follow-up=%v", followUp), func(t *testing.T) {
			forChatLoopStores(t, chatLoopOptions{team: true}, func(t *testing.T, f *chatLoopFixture) {
				trigger := f.post("trigger", "user-1", "Deploy the canary.")
				run := f.startRun(trigger)
				producer := &chatLoopProducer{answers: []string{"Answered the follow-up."}}
				binding := f.bind(run, producer)
				f.prepare(run, binding, 1)
				entries := f.stepEntries(run, binding.Session.ID, "member-step-1", 1,
					ModelToolCall{ID: "deploy", Name: "deploy__canary", Arguments: json.RawMessage(`{}`)})
				newer := f.post("newer", "user-1", "Actually, ask the other member.")
				f.actions.calls["action-deploy"] = &ActionCall{ID: "action-deploy", Scope: run.Scope, RunID: run.ID, Status: ActionCallStatusDenied, ApprovalID: "approval-deploy"}
				f.actions.approvals["approval-deploy"] = &ApprovalCheckpoint{ID: "approval-deploy", Scope: run.Scope, Status: ApprovalStatusSuperseded, SupersededByMessageID: newer.ID}
				f.settle(run, entries["deploy"], ToolEntryError, "action-deploy", governedJSONResult(ToolResultSuperseded, map[string]interface{}{
					"status": string(ToolResultSuperseded), "reason": action.ApprovalSupersededReason,
				}))
				if followUp {
					current := f.run(run.ID)
					updated := cloneAgentRun(current)
					updated.PendingInterventions = append(updated.PendingInterventions, AgentRunIntervention{
						ID: conversationFollowUpInterventionID(run.Scope, newer.ID), Actor: ActivityActor{Type: "user", ID: "user-1"},
						Instruction: newer.Content, CreatedAt: f.now,
						ConversationMessage: &ConversationMessageDelivery{ConversationID: f.conversation.ID, MessageID: newer.ID, Sequence: newer.Sequence, Sender: newer.Sender},
					})
					updated.Revision++
					if _, err := f.store.UpdateAgentRunWithEvent(f.ctx, updated, current.Revision, &ActivityEvent{
						ID: "follow-up", Scope: run.Scope, EventType: "run.updated", Severity: ActivitySeverityInfo, RunID: run.ID,
						Actor: ActivityActor{Type: "worker", ID: chatLoopWorker}, Summary: "Follow-up delivered", Visibility: ActivityVisibilityScope, CreatedAt: f.now,
					}, nil); err != nil {
						t.Fatal(err)
					}
				}
				exit := f.drive(run, binding)
				if exit.Run.Status != AgentRunStatusCompleted {
					t.Fatalf("exit = %s %s", exit.Reason, exit.Run.Status)
				}
				if followUp {
					if len(producer.paths) != 1 || exit.Run.Output["approvalReplaced"] != nil {
						t.Fatalf("follow-up member: steps %d output %#v", len(producer.paths), exit.Run.Output)
					}
					return
				}
				if len(producer.paths) != 0 || exit.Run.Output["approvalReplaced"] != true || exit.Run.Output["replacedByMessageId"] != newer.ID {
					t.Fatalf("replaced member: steps %d output %#v", len(producer.paths), exit.Run.Output)
				}
				if messages := f.agentMessages(); len(messages) != 0 {
					t.Fatalf("replaced member posted %#v", messages)
				}
			})
		})
	}
}

func TestPromotedBeforeFirstStepRendersTriggerAndRunContext(t *testing.T) {
	for _, when := range []string{"after-bind", "before-bind"} {
		t.Run(when, func(t *testing.T) {
			forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
				first := f.post("first", "user-1", "Draft the agenda.")
				run1 := f.startRun(first)
				f.drive(run1, f.bind(run1, &chatLoopProducer{answers: []string{"Agenda: intro, demo."}}))
				lane := f.run(run1.ID).SessionID

				// The second Run waited behind the first: it is promoted to a
				// child of the non-empty lane before its first step.
				second := f.post("second", "user-1", "Research the venues in depth.")
				run2 := f.startRun(second)
				var binding *agentrun.TurnRunnerBinding
				producer := &chatLoopProducer{answers: []string{"Venues: A, B."}}
				if when == "after-bind" {
					binding = f.bind(run2, producer)
					f.promote(run2)
				} else {
					f.promote(run2)
					binding = f.bind(f.run(run2.ID), producer)
				}
				f.drive(run2, binding)
				if len(producer.paths) == 0 {
					t.Fatal("the promoted Run made no model step")
				}
				child := f.run(run2.ID).SessionID
				if child == lane || child != RunAgentSessionID(run2.Scope, run2.ID) {
					t.Fatalf("run session %q, lane %q", child, lane)
				}
				chatLoopExpectShape(t, producer.paths[0],
					"user:"+first.ID, "note:run_context:"+run1.ID, "assistant:"+run1.ID,
					"user:"+second.ID, "note:run_context:"+run2.ID)
				if note := producer.paths[0][4]; note.SessionID != child || !strings.Contains(chatLoopEntryText(note), second.ID) {
					t.Fatalf("run_context note %q %q", note.SessionID, chatLoopEntryText(note))
				}
				for _, entry := range f.path(lane) {
					if entry.RunID == run2.ID && !(entry.Type == SessionEntryNote && entry.Note.Kind == NoteTaskPromoted) {
						t.Fatalf("the promoted Run wrote to the lane: %#v", entry)
					}
				}
			})
		})
	}
}

// A Run promoted on an empty lane gets a seeded child (only its task brief);
// its goal seed is not a sign it started, so StartRun still renders the
// trigger and the run_context note after the brief (§3.3, §4.5).
func TestPromotedBeforeFirstStepOnEmptyLaneSeedsTriggerAndRunContext(t *testing.T) {
	for _, when := range []string{"after-bind", "before-bind"} {
		t.Run(when, func(t *testing.T) {
			forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
				trigger := f.post("trigger", "user-1", "Research the venues in depth.")
				run := f.startRun(trigger)
				var binding *agentrun.TurnRunnerBinding
				producer := &chatLoopProducer{answers: []string{"Venues: A, B."}}
				if when == "after-bind" {
					binding = f.bind(run, producer)
					f.promote(run)
				} else {
					f.promote(run)
					binding = f.bind(f.run(run.ID), producer)
				}
				f.drive(run, binding)
				if len(producer.paths) == 0 {
					t.Fatal("the promoted Run made no model step")
				}
				child := f.run(run.ID).SessionID
				if child != RunAgentSessionID(run.Scope, run.ID) {
					t.Fatalf("run session %q", child)
				}
				path := producer.paths[0]
				chatLoopExpectShape(t, path, "user:", "user:"+trigger.ID, "note:run_context:"+run.ID)
				if seed := path[0]; seed.User.Origin != UserEntryGoal || !strings.Contains(chatLoopEntryText(seed), "<brief>") {
					t.Fatalf("first entry is not the task brief: %#v", seed)
				}
				if note := path[2]; note.SessionID != child || !strings.Contains(chatLoopEntryText(note), trigger.ID) {
					t.Fatalf("run_context note %q %q", note.SessionID, chatLoopEntryText(note))
				}
			})
		})
	}
}

func TestRunPromotedMidFlightCountsAsStartedInChild(t *testing.T) {
	forChatLoopStores(t, chatLoopOptions{}, func(t *testing.T, f *chatLoopFixture) {
		trigger := f.post("trigger", "user-1", "Compile the quarterly numbers.")
		run := f.startRun(trigger)
		binding := f.bind(run, &chatLoopProducer{})
		f.prepare(run, binding, 1)
		f.promote(run)
		promoted := f.bind(f.run(run.ID), &chatLoopProducer{})
		if promoted.Session.Kind != AgentSessionKindRun {
			t.Fatalf("promoted session = %#v", promoted.Session)
		}
		// The chat preparer counts the Run as started in its child: with no
		// steering it has nothing to add, where an unstarted Run would get
		// StartRun's trigger and run_context entries.
		if len(promoted.Preparers) != 2 {
			t.Fatalf("preparers = %d, want the run-start and chat preparers", len(promoted.Preparers))
		}
		step := &AgentTurn{ID: run.ID + "-step-2", Sequence: 2}
		part, err := promoted.Preparers[1].BeforeStep(f.ctx, agentrun.StepInput{Run: f.run(run.ID), Session: promoted.Session, Binding: promoted, WorkerID: chatLoopWorker, Step: step}, agentrun.SteeringPlan{})
		if err != nil || part != nil {
			t.Fatalf("the promoted Run did not count as started: %#v, %v", part, err)
		}
		// The second step appends nothing to the child: StartRun does not run again.
		if result := f.prepare(run, promoted, 2); result.Append != nil && len(result.Append.Entries) > 0 {
			t.Fatalf("the child re-ran StartRun: %#v", result.Append.Entries)
		}
		chatLoopExpectShape(t, f.path(promoted.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID)
	})
}
