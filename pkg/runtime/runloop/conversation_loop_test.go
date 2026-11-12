package runloop_test

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
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop/runlooptest"
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
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		earlier := f.Post("earlier", "user-1", "The launch is on Friday.")
		trigger := f.Post("trigger", "user-1", "When is the launch?")
		run := f.StartRun(trigger)
		producer := &runlooptest.ChatProducer{Answers: []string{"Friday."}}
		binding := f.Bind(run, producer)
		exit := f.Drive(run, binding)
		if exit.Reason != runloop.RunLoopExitFinal || exit.Run.Status != models.AgentRunStatusCompleted {
			t.Fatalf("exit = %s, run %s", exit.Reason, exit.Run.Status)
		}
		if len(producer.Paths) != 1 {
			t.Fatalf("model steps = %d", len(producer.Paths))
		}
		rendered := producer.Paths[0]
		runlooptest.ExpectChatShape(t, rendered, "user:"+earlier.ID, "user:"+trigger.ID, "note:run_context:"+run.ID)
		for _, entry := range rendered {
			text := runlooptest.ChatEntryText(entry)
			if entry.User != nil && entry.User.Origin != models.UserEntryChannelMessage || strings.Contains(text, run.Goal) || strings.Contains(text, `"goal"`) {
				t.Fatalf("goal JSON reached the step: %#v %q", entry, text)
			}
		}
		if !strings.HasSuffix(runlooptest.ChatEntryText(rendered[1]), "\nWhen is the launch?") || !strings.Contains(runlooptest.ChatEntryText(rendered[2]), trigger.ID) {
			t.Fatalf("trigger %q, note %q", runlooptest.ChatEntryText(rendered[1]), runlooptest.ChatEntryText(rendered[2]))
		}
		answers := f.AgentMessages()
		if len(answers) != 1 || answers[0].Content != "Friday." || answers[0].ReplyToMessageID != trigger.ID ||
			answers[0].References[0] != (models.ConversationReference{Kind: models.ConversationReferenceRun, ID: run.ID}) {
			t.Fatalf("answers = %#v", answers)
		}
		session := f.Run(run.ID).SessionID
		if session != binding.Session.ID || session != models.ConversationSessionID(run.Scope, run.ConcurrencyKey) {
			t.Fatalf("run session %q, binding %q", session, binding.Session.ID)
		}
		// The read receipt follows the committed watermark.
		cursor, err := f.Service.GetCursor(f.Ctx, run.Scope, f.Conversation.ID, models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: "agent-1"})
		if err != nil || cursor == nil || cursor.ReadSequence < trigger.Sequence {
			t.Fatalf("agent cursor = %#v, %v (trigger %d)", cursor, err, trigger.Sequence)
		}
	})
}

func TestSecondMessageSeesFirstExchangeAsLogEntries(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		first := f.Post("first", "user-1", "Draft the agenda.")
		run1 := f.StartRun(first)
		f.Drive(run1, f.Bind(run1, &runlooptest.ChatProducer{Answers: []string{"Agenda: intro, demo, Q&A."}}))

		second := f.Post("second", "user-1", "Add a break.")
		run2 := f.StartRun(second)
		producer := &runlooptest.ChatProducer{Answers: []string{"Added."}}
		exit := f.Drive(run2, f.Bind(run2, producer))
		if exit.Run.Status != models.AgentRunStatusCompleted || len(producer.Paths) != 1 {
			t.Fatalf("exit = %#v, steps %d", exit, len(producer.Paths))
		}
		rendered := producer.Paths[0]
		runlooptest.ExpectChatShape(t, rendered,
			"user:"+first.ID, "note:run_context:"+run1.ID, "assistant:"+run1.ID,
			"user:"+second.ID, "note:run_context:"+run2.ID)
		if runlooptest.ChatEntryText(rendered[2]) != "Agenda: intro, demo, Q&A." || rendered[3].User.Author.Type != "user" {
			t.Fatalf("first exchange = %q / %#v", runlooptest.ChatEntryText(rendered[2]), rendered[3].User.Author)
		}
		if answers := f.AgentMessages(); len(answers) != 2 || answers[1].Content != "Added." {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

func TestFollowUpFromSamePersonAppendedAtNextStepOnce(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		trigger := f.Post("trigger", "user-1", "Summarize the report.")
		run := f.StartRun(trigger)
		binding := f.Bind(run, &runlooptest.ChatProducer{})
		prepareChatStep(f, run, binding, 1)

		followUp := f.Post("follow-up", "user-1", "Only the risks, please.")
		current := f.Run(run.ID)
		updated := models.CloneAgentRun(current)
		updated.PendingInterventions = append(updated.PendingInterventions, models.AgentRunIntervention{
			ID: models.ConversationFollowUpInterventionID(run.Scope, followUp.ID), Actor: models.ActivityActor{Type: "user", ID: "user-1"},
			Instruction: followUp.Content, CreatedAt: f.Now,
			ConversationMessage: &models.ConversationMessageDelivery{ConversationID: f.Conversation.ID, MessageID: followUp.ID, Sequence: followUp.Sequence, Sender: followUp.Sender},
		})
		updated.Revision++
		if _, err := f.Store.UpdateAgentRunWithEvent(f.Ctx, updated, current.Revision, &models.ActivityEvent{
			ID: "follow-up-" + uuid.NewString()[:8], Scope: run.Scope, EventType: "run.updated", Severity: models.ActivitySeverityInfo,
			RunID: run.ID, Actor: models.ActivityActor{Type: "worker", ID: runlooptest.ChatWorker}, Summary: "Follow-up delivered",
			Visibility: models.ActivityVisibilityScope, CreatedAt: f.Now,
		}, nil); err != nil {
			t.Fatal(err)
		}

		second := prepareChatStep(f, run, binding, 2)
		if !second.CursorMoved {
			t.Fatal("the follow-up was not consumed")
		}
		prepareChatStep(f, run, binding, 3)
		runlooptest.ExpectChatShape(t, f.Path(binding.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID, "user:"+followUp.ID)
		entries := f.Path(binding.Session.ID)
		if !strings.Contains(runlooptest.ChatEntryText(entries[2]), "(user user-1)") || entries[2].Step != 2 {
			t.Fatalf("follow-up entry = %q at step %d", runlooptest.ChatEntryText(entries[2]), entries[2].Step)
		}
		if got := f.Run(run.ID).ConsumedInterventions; got != 1 {
			t.Fatalf("consumed interventions = %d", got)
		}
	})
}

func TestOtherPersonsMessageStartsOwnRunAndInterleavesAfterApprovalPark(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		triggerA := f.Post("a", "user-1", "Send the invoice.")
		runA := f.StartRun(triggerA)
		bindingA := f.Bind(runA, &runlooptest.ChatProducer{})
		prepareChatStep(f, runA, bindingA, 1)
		// A's governed call is held for approval; A parks.
		f.StepEntries(runA, bindingA.Session.ID, "a-step-1", 1,
			models.ModelToolCall{ID: "send", Name: "billing__send_invoice", Arguments: json.RawMessage(`{"invoice":"7"}`)})

		triggerB := f.Post("b", "user-2", "What is on my calendar?")
		runB := f.StartRun(triggerB)
		bindingB := f.Bind(runB, &runlooptest.ChatProducer{})
		prepareChatStep(f, runB, bindingB, 1)
		f.StepEntries(runB, bindingB.Session.ID, "b-step-1", 1)

		// A resumes after the approval: nothing new is ingested for it.
		if result := prepareChatStep(f, runA, bindingA, 2); result.Append != nil && len(result.Append.Entries) > 0 {
			t.Fatalf("A ingested again: %#v", result.Append.Entries)
		}
		runlooptest.ExpectChatShape(t, f.Path(bindingA.Session.ID),
			"user:"+triggerA.ID, "note:run_context:"+runA.ID, "assistant:"+runA.ID, "tool:"+runA.ID,
			"user:"+triggerB.ID, "note:run_context:"+runB.ID, "assistant:"+runB.ID)
		if bindingA.Session.ID != bindingB.Session.ID {
			t.Fatalf("one lane, two sessions: %s %s", bindingA.Session.ID, bindingB.Session.ID)
		}
		entries := f.Path(bindingB.Session.ID)
		if entries[4].User.Author.ID != "user-2" || !strings.Contains(runlooptest.ChatEntryText(entries[5]), triggerB.ID) {
			t.Fatalf("B's trigger %#v, note %q", entries[4].User.Author, runlooptest.ChatEntryText(entries[5]))
		}
	})
}

// TestRunActionsPresentWithoutActiveWork: a chat with no active work keeps
// the run-control actions (the tool list does not change when work starts or
// ends); a call names no active work, so it is refused.
func TestRunActionsPresentWithoutActiveWork(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		definition := agentrun.RunManagementSkill()
		pause := capability.ModelAction{
			Name: "openseal_runs__pause", SkillID: models.RunManagementSkillID, Version: agentrun.RunManagementSkillVersion,
			Action: models.RunActionPause, InputSchema: definition.Actions[models.RunActionPause].InputSchema,
		}
		other := capability.ModelAction{Name: "reports__export", SkillID: "reports", Action: "export"}
		quiet := f.Post("quiet", "user-1", "Hello")
		idle := f.StartRun(quiet)
		binding := f.Bind(idle, &runlooptest.ChatProducer{}, pause, other)
		if len(binding.ModelActions) != 2 || binding.ModelActions[0].Name != pause.Name {
			t.Fatalf("idle chat actions = %#v", binding.ModelActions)
		}
		refusal, err := binding.ToolCheck(f.Ctx, agentrun.StepToolsInput{Run: idle, Binding: binding}, &models.SessionEntry{
			Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: "call",
			Tool: &models.ToolEntry{Name: pause.Name, Arguments: json.RawMessage(`{"runId":"nope"}`)},
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
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		definition := agentrun.RunManagementSkill()
		pause := capability.ModelAction{
			Name: "openseal_runs__pause", SkillID: models.RunManagementSkillID, Version: agentrun.RunManagementSkillVersion,
			Action: models.RunActionPause, InputSchema: definition.Actions[models.RunActionPause].InputSchema,
		}
		resume := pause
		resume.Name, resume.Action, resume.InputSchema = "openseal_runs__resume", models.RunActionResume, definition.Actions[models.RunActionResume].InputSchema
		other := capability.ModelAction{Name: "reports__export", SkillID: "reports", Action: "export"}

		trigger := f.Post("pause", "user-1", "Pause the export.")
		run := f.StartRun(trigger)
		portfolio := portfolio.NewPortfolioService(f.Store, models.WithClock(func() time.Time { return f.Now }))
		work, err := portfolio.CreateAgentRun(f.Ctx, models.CreateAgentRunRequest{
			Scope: run.Scope, Kind: models.RunKindAgentWork, ParentRunID: run.ID, Owner: run.Owner, AssignedAgentID: "agent-1",
			Goal: "Export the report", Source: models.RunSourceFork, IdempotencyKey: "work-" + run.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		binding := f.Bind(run, &runlooptest.ChatProducer{}, pause, resume, other)
		if len(binding.ModelActions) != 3 {
			t.Fatalf("active chat actions = %#v", binding.ModelActions)
		}
		if _, ok := binding.ModelActions[0].InputSchema["properties"].(map[string]interface{})["runId"].(map[string]interface{})["enum"]; ok {
			t.Fatal("run id schema carries a per-claim enum")
		}
		check := func(name, runID string) string {
			t.Helper()
			refusal, err := binding.ToolCheck(f.Ctx, agentrun.StepToolsInput{Run: run, Binding: binding}, &models.SessionEntry{
				Type: models.SessionEntryTool, State: models.ToolEntryPending, ToolCallID: "call",
				Tool: &models.ToolEntry{Name: name, Arguments: json.RawMessage(fmt.Sprintf(`{"runId":%q}`, runID))},
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
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		trigger := f.Post("trigger", "user-1", "Export both reports.")
		run := f.StartRun(trigger)
		binding := f.Bind(run, &runlooptest.ChatProducer{})
		prepareChatStep(f, run, binding, 1)
		entries := f.StepEntries(run, binding.Session.ID, "step-1", 1,
			models.ModelToolCall{ID: "a", Name: "reports__export", Arguments: json.RawMessage(`{"report":"a"}`)},
			models.ModelToolCall{ID: "b", Name: "reports__export", Arguments: json.RawMessage(`{"report":"b"}`)})
		for _, id := range []string{"a", "b"} {
			actionID := "action-" + id
			artifact := &models.Artifact{ID: "file-" + id, Version: 1, Scope: run.Scope, Name: id + ".pdf",
				Provenance: models.ArtifactProvenance{Producer: models.ActivityActor{Type: "agent", ID: "agent-1"}, Owner: &run.Owner, RunID: run.ID, ActionID: actionID},
				ContentRef: "object-store:" + id, Digest: "sha256:" + strings.Repeat("a", 64), SizeBytes: 10, Classification: models.ArtifactClassificationInternal}
			if err := f.Store.CreateArtifactVersion(f.Ctx, artifact, 0); err != nil {
				t.Fatal(err)
			}
			f.Actions.Calls[actionID] = &models.ActionCall{ID: actionID, Scope: run.Scope, RunID: run.ID, Status: models.ActionCallStatusSucceeded,
				Output: map[string]interface{}{"artifactRefs": []interface{}{map[string]interface{}{"id": artifact.ID, "version": 1}}}}
			done := f.Settle(run, entries[id], models.ToolEntryCompleted, actionID, &models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{storetest.TextPart("saved")}})
			for range 2 { // the hook is replay-safe
				if err := binding.AfterTool(f.Ctx, agentrun.StepToolsInput{Run: f.Run(run.ID), Binding: binding}, done); err != nil {
					t.Fatal(err)
				}
			}
		}
		if posted := f.AgentMessages(); len(posted) != 0 {
			t.Fatalf("posted after tools = %#v", posted)
		}
	})
}

// Participation is on when the Run is bound and stopped before its first
// step: the step's seams end the Run without a model call.
func TestParticipationStoppedBeforeFirstStepPostsNothing(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{Participation: true}, func(t *testing.T, f *runlooptest.ChatFixture) {
		f.SetParticipation(true)
		trigger := f.Post("trigger", "user-1", "Anyone there?")
		run := f.StartRun(trigger)
		producer := &runlooptest.ChatProducer{}
		binding := f.Bind(run, producer)
		f.SetParticipation(false)
		exit := f.Drive(run, binding)
		if exit.Reason != runloop.RunLoopExitFinal || exit.Run.Status != models.AgentRunStatusCompleted || exit.Run.Output["participationSkipped"] != true {
			t.Fatalf("exit = %s %s %#v", exit.Reason, exit.Run.Status, exit.Run.Output)
		}
		if len(producer.Paths) != 0 {
			t.Fatalf("the model was called %d times", len(producer.Paths))
		}
		if messages := f.AgentMessages(); len(messages) != 0 {
			t.Fatalf("posted = %#v", messages)
		}
	})
}

func TestTeamReplacedApprovalEndsMemberSilently(t *testing.T) {
	for _, followUp := range []bool{false, true} {
		t.Run(fmt.Sprintf("follow-up=%v", followUp), func(t *testing.T) {
			runlooptest.ForChatStores(t, runlooptest.ChatOptions{Team: true}, func(t *testing.T, f *runlooptest.ChatFixture) {
				trigger := f.Post("trigger", "user-1", "Deploy the canary.")
				run := f.StartRun(trigger)
				producer := &runlooptest.ChatProducer{Answers: []string{"Answered the follow-up."}}
				binding := f.Bind(run, producer)
				prepareChatStep(f, run, binding, 1)
				entries := f.StepEntries(run, binding.Session.ID, "member-step-1", 1,
					models.ModelToolCall{ID: "deploy", Name: "deploy__canary", Arguments: json.RawMessage(`{}`)})
				newer := f.Post("newer", "user-1", "Actually, ask the other member.")
				f.Actions.Calls["action-deploy"] = &models.ActionCall{ID: "action-deploy", Scope: run.Scope, RunID: run.ID, Status: models.ActionCallStatusDenied, ApprovalID: "approval-deploy"}
				f.Actions.Approvals["approval-deploy"] = &models.ApprovalCheckpoint{ID: "approval-deploy", Scope: run.Scope, Status: models.ApprovalStatusSuperseded, SupersededByMessageID: newer.ID}
				f.Settle(run, entries["deploy"], models.ToolEntryError, "action-deploy", runloop.GovernedJSONResult(models.ToolResultSuperseded, map[string]interface{}{
					"status": string(models.ToolResultSuperseded), "reason": action.ApprovalSupersededReason,
				}))
				if followUp {
					current := f.Run(run.ID)
					updated := models.CloneAgentRun(current)
					updated.PendingInterventions = append(updated.PendingInterventions, models.AgentRunIntervention{
						ID: models.ConversationFollowUpInterventionID(run.Scope, newer.ID), Actor: models.ActivityActor{Type: "user", ID: "user-1"},
						Instruction: newer.Content, CreatedAt: f.Now,
						ConversationMessage: &models.ConversationMessageDelivery{ConversationID: f.Conversation.ID, MessageID: newer.ID, Sequence: newer.Sequence, Sender: newer.Sender},
					})
					updated.Revision++
					if _, err := f.Store.UpdateAgentRunWithEvent(f.Ctx, updated, current.Revision, &models.ActivityEvent{
						ID: "follow-up", Scope: run.Scope, EventType: "run.updated", Severity: models.ActivitySeverityInfo, RunID: run.ID,
						Actor: models.ActivityActor{Type: "worker", ID: runlooptest.ChatWorker}, Summary: "Follow-up delivered", Visibility: models.ActivityVisibilityScope, CreatedAt: f.Now,
					}, nil); err != nil {
						t.Fatal(err)
					}
				}
				exit := f.Drive(run, binding)
				if exit.Run.Status != models.AgentRunStatusCompleted {
					t.Fatalf("exit = %s %s", exit.Reason, exit.Run.Status)
				}
				if followUp {
					if len(producer.Paths) != 1 || exit.Run.Output["approvalReplaced"] != nil {
						t.Fatalf("follow-up member: steps %d output %#v", len(producer.Paths), exit.Run.Output)
					}
					return
				}
				if len(producer.Paths) != 0 || exit.Run.Output["approvalReplaced"] != true || exit.Run.Output["replacedByMessageId"] != newer.ID {
					t.Fatalf("replaced member: steps %d output %#v", len(producer.Paths), exit.Run.Output)
				}
				if messages := f.AgentMessages(); len(messages) != 0 {
					t.Fatalf("replaced member posted %#v", messages)
				}
			})
		})
	}
}

func TestPromotedBeforeFirstStepRendersTriggerAndRunContext(t *testing.T) {
	for _, when := range []string{"after-bind", "before-bind"} {
		t.Run(when, func(t *testing.T) {
			runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
				first := f.Post("first", "user-1", "Draft the agenda.")
				run1 := f.StartRun(first)
				f.Drive(run1, f.Bind(run1, &runlooptest.ChatProducer{Answers: []string{"Agenda: intro, demo."}}))
				lane := f.Run(run1.ID).SessionID

				// The second Run waited behind the first: it is promoted to a
				// child of the non-empty lane before its first step.
				second := f.Post("second", "user-1", "Research the venues in depth.")
				run2 := f.StartRun(second)
				var binding *agentrun.TurnRunnerBinding
				producer := &runlooptest.ChatProducer{Answers: []string{"Venues: A, B."}}
				if when == "after-bind" {
					binding = f.Bind(run2, producer)
					f.Promote(run2)
				} else {
					f.Promote(run2)
					binding = f.Bind(f.Run(run2.ID), producer)
				}
				f.Drive(run2, binding)
				if len(producer.Paths) == 0 {
					t.Fatal("the promoted Run made no model step")
				}
				child := f.Run(run2.ID).SessionID
				if child == lane || child != models.RunAgentSessionID(run2.Scope, run2.ID) {
					t.Fatalf("run session %q, lane %q", child, lane)
				}
				runlooptest.ExpectChatShape(t, producer.Paths[0],
					"user:"+first.ID, "note:run_context:"+run1.ID, "assistant:"+run1.ID,
					"user:"+second.ID, "note:run_context:"+run2.ID)
				if note := producer.Paths[0][4]; note.SessionID != child || !strings.Contains(runlooptest.ChatEntryText(note), second.ID) {
					t.Fatalf("run_context note %q %q", note.SessionID, runlooptest.ChatEntryText(note))
				}
				for _, entry := range f.Path(lane) {
					if entry.RunID == run2.ID && !(entry.Type == models.SessionEntryNote && entry.Note.Kind == models.NoteTaskPromoted) {
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
			runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
				trigger := f.Post("trigger", "user-1", "Research the venues in depth.")
				run := f.StartRun(trigger)
				var binding *agentrun.TurnRunnerBinding
				producer := &runlooptest.ChatProducer{Answers: []string{"Venues: A, B."}}
				if when == "after-bind" {
					binding = f.Bind(run, producer)
					f.Promote(run)
				} else {
					f.Promote(run)
					binding = f.Bind(f.Run(run.ID), producer)
				}
				f.Drive(run, binding)
				if len(producer.Paths) == 0 {
					t.Fatal("the promoted Run made no model step")
				}
				child := f.Run(run.ID).SessionID
				if child != models.RunAgentSessionID(run.Scope, run.ID) {
					t.Fatalf("run session %q", child)
				}
				path := producer.Paths[0]
				runlooptest.ExpectChatShape(t, path, "user:", "user:"+trigger.ID, "note:run_context:"+run.ID)
				if seed := path[0]; seed.User.Origin != models.UserEntryGoal || !strings.Contains(runlooptest.ChatEntryText(seed), "<brief>") {
					t.Fatalf("first entry is not the task brief: %#v", seed)
				}
				if note := path[2]; note.SessionID != child || !strings.Contains(runlooptest.ChatEntryText(note), trigger.ID) {
					t.Fatalf("run_context note %q %q", note.SessionID, runlooptest.ChatEntryText(note))
				}
			})
		})
	}
}

func TestRunPromotedMidFlightCountsAsStartedInChild(t *testing.T) {
	runlooptest.ForChatStores(t, runlooptest.ChatOptions{}, func(t *testing.T, f *runlooptest.ChatFixture) {
		trigger := f.Post("trigger", "user-1", "Compile the quarterly numbers.")
		run := f.StartRun(trigger)
		binding := f.Bind(run, &runlooptest.ChatProducer{})
		prepareChatStep(f, run, binding, 1)
		f.Promote(run)
		promoted := f.Bind(f.Run(run.ID), &runlooptest.ChatProducer{})
		if promoted.Session.Kind != models.AgentSessionKindRun {
			t.Fatalf("promoted session = %#v", promoted.Session)
		}
		// The chat preparer counts the Run as started in its child: with no
		// steering it has nothing to add, where an unstarted Run would get
		// StartRun's trigger and run_context entries.
		if len(promoted.Preparers) != 2 {
			t.Fatalf("preparers = %d, want the run-start and chat preparers", len(promoted.Preparers))
		}
		step := &models.AgentTurn{ID: run.ID + "-step-2", Sequence: 2}
		part, err := promoted.Preparers[1].BeforeStep(f.Ctx, agentrun.StepInput{Run: f.Run(run.ID), Session: promoted.Session, Binding: promoted, WorkerID: runlooptest.ChatWorker, Step: step}, agentrun.SteeringPlan{})
		if err != nil || part != nil {
			t.Fatalf("the promoted Run did not count as started: %#v, %v", part, err)
		}
		// The second step appends nothing to the child: StartRun does not run again.
		if result := prepareChatStep(f, run, promoted, 2); result.Append != nil && len(result.Append.Entries) > 0 {
			t.Fatalf("the child re-ran StartRun: %#v", result.Append.Entries)
		}
		runlooptest.ExpectChatShape(t, f.Path(promoted.Session.ID), "user:"+trigger.ID, "note:run_context:"+run.ID)
	})
}

// prepareChatStep runs the loop's phase 3 for run as the next step would.
func prepareChatStep(f *runlooptest.ChatFixture, run *models.AgentRun, binding *agentrun.TurnRunnerBinding, step int64) *runloop.StepContextResult {
	f.T.Helper()
	current, err := f.Store.GetAgentRun(f.Ctx, run.Scope, run.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	session, err := f.Store.GetAgentSession(f.Ctx, run.Scope, binding.Session.ID)
	if err != nil {
		f.T.Fatal(err)
	}
	in := agentrun.StepInput{
		Run: current, Session: session, Binding: binding, WorkerID: runlooptest.ChatWorker,
		Step: &models.AgentTurn{ID: fmt.Sprintf("%s-step-%d", run.ID, step), Sequence: step},
	}
	result, err := runloop.AppendStepContext(f.Ctx, f.Store, in, binding.Preparers, f.Now)
	if err != nil {
		f.T.Fatalf("step context of %s: %v", run.ID, err)
	}
	return result
}
