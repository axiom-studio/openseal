package looptest

// Run-loop test kit, part 3: reading what happened (a Run's session, steps,
// events and channel), and the helpers scenarios share on top of the fixture
// (bounded and runbook Runs, approvals, commands, previews).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
)

func (f *Fixture) MustRun(runID string) *models.AgentRun {
	f.T.Helper()
	run, err := f.Raw.GetAgentRun(f.Ctx, f.Scope, runID)
	if err != nil || run == nil {
		f.T.Fatalf("get run %s: %#v, %v", runID, run, err)
	}
	return run
}

func (f *Fixture) Session(runID string) *Session {
	f.T.Helper()
	run := f.MustRun(runID)
	if run.SessionID == "" {
		f.T.Fatalf("run %s has no session", runID)
	}
	session, err := f.Raw.(models.AgentSessionStore).GetAgentSession(f.Ctx, f.Scope, run.SessionID)
	if err != nil {
		f.T.Fatal(err)
	}
	return session
}

// Session is the agent session log of a Run.
type Session = models.AgentSession

// Entries is the Run's full session, ordered by sequence.
func (f *Fixture) Entries(runID string) []*models.SessionEntry {
	f.T.Helper()
	session := f.Session(runID)
	var out []*models.SessionEntry
	var after int64
	for {
		page, err := f.Raw.(models.AgentSessionStore).ListAgentSessionEntries(f.Ctx, models.SessionEntryFilter{
			Scope: f.Scope, SessionID: session.ID, AfterSequence: after, Limit: 1000,
		})
		if err != nil {
			f.T.Fatal(err)
		}
		out = append(out, page...)
		if len(page) < 1000 {
			break
		}
		after = page[len(page)-1].Sequence
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

// EntryTypes is the log shape: one string per entry, "tool:<state>" for tool
// entries.
// EntryTypes is the Run's entry log. Capabilities notes are left out: they
// follow the claim's catalog, not the loop mechanics these scenarios check
// (capability_cache_test.go covers them).
func (f *Fixture) EntryTypes(runID string) []string {
	f.T.Helper()
	var out []string
	for _, entry := range f.Entries(runID) {
		if entry.Note != nil && entry.Note.Kind == models.NoteCapabilities {
			continue
		}
		kind := string(entry.Type)
		if entry.Type == models.SessionEntryTool {
			kind = "tool:" + string(entry.State)
		}
		out = append(out, kind)
	}
	return out
}

func (f *Fixture) Steps(runID string) []*models.AgentTurn {
	f.T.Helper()
	steps, err := f.Raw.ListAgentTurns(f.Ctx, models.AgentTurnFilter{Scope: f.Scope, RunID: runID})
	if err != nil {
		f.T.Fatal(err)
	}
	return steps
}

func (f *Fixture) Calls(runID string) []*models.ActionCall {
	f.T.Helper()
	calls, err := f.Raw.ListActionCalls(f.Ctx, models.ActionFilter{Scope: f.Scope, RunID: runID})
	if err != nil {
		f.T.Fatal(err)
	}
	return calls
}

// Events is the Run's activity in sequence order, of these types when given.
// It pages ascending and filters here: the stores' ascending reads take only
// the Run and the sequence.
func (f *Fixture) Events(runID string, types ...string) []*models.ActivityEvent {
	f.T.Helper()
	want := map[string]bool{}
	for _, eventType := range types {
		want[eventType] = true
	}
	const page = 500
	var out []*models.ActivityEvent
	var after int64
	for {
		events, err := f.Raw.ListActivity(f.Ctx, models.ActivityFilter{Scope: f.Scope, RunID: runID, AfterSequence: after, Limit: page})
		if err != nil {
			f.T.Fatal(err)
		}
		for _, event := range events {
			if len(want) == 0 || want[event.EventType] {
				out = append(out, event)
			}
		}
		if len(events) < page {
			return out
		}
		after = events[len(events)-1].Sequence
	}
}

// Messages is the conversation's channel, oldest first.
func (f *Fixture) Messages(conversationID string) []*models.ChannelMessage {
	f.T.Helper()
	messages, err := f.Convs.ListChannelMessages(f.Ctx, models.ChannelMessageFilter{Scope: f.Scope, ConversationID: conversationID, Limit: 1000})
	if err != nil {
		f.T.Fatal(err)
	}
	return messages
}

func (f *Fixture) MustStatus(run *models.AgentRun, want models.AgentRunStatus) {
	f.T.Helper()
	current := f.MustRun(run.ID)
	if current.Status != want {
		f.T.Fatalf("run %s status = %s, want %s (failure: %#v)\nlog: %v", run.ID, current.Status, want, current.FailureExplanation, f.EntryTypes(run.ID))
	}
}

// ReviewedCall calls an external (reviewed) governed action: its arguments go
// in the review envelope the governed schema offers, with summary as the
// review summary and content.
func ReviewedCall(name string, args map[string]any, summary string) ToolCall {
	return Call(name, map[string]any{
		"arguments":     args,
		"reviewContext": map[string]any{"summary": summary, "content": summary},
	})
}

// GovernedTool is the offered tool name of a fixture action.
func GovernedTool(v StepView, action string) string {
	for _, tool := range v.Req.Tools {
		if strings.HasSuffix(tool.Name, action) && strings.HasPrefix(tool.Name, GovernedToolPrefix) {
			return tool.Name
		}
	}
	return GovernedToolPrefix + action
}

// GovernedToolPrefix starts every governed tool name the model is offered.
// The kit spells it out so its tests pin the names the model sees.
const GovernedToolPrefix = "action_"

func DescribeMessages(messages []*models.ChannelMessage) string {
	parts := make([]string, len(messages))
	for i, m := range messages {
		raw, _ := json.Marshal(map[string]string{"from": m.Sender.ID, "text": m.Content})
		parts[i] = string(raw)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// RunClaimWith claims the Run, lets mutate adjust the resolved binding (a
// compactor, a deterministic producer) and drives one claim.
func (f *Fixture) RunClaimWith(ctx context.Context, runID string, mutate func(*agentrun.TurnRunnerBinding)) (runloop.Exit, *models.AgentRun) {
	f.T.Helper()
	claimed := f.Claim(runID)
	binding := f.Binding(claimed)
	if binding.Session != nil && claimed.SessionID != binding.Session.ID {
		claimed = f.MustRun(runID)
	}
	if mutate != nil {
		mutate(binding)
	}
	exit := f.Loop.Drive(ctx, runloop.Claim{Run: claimed, WorkerID: f.WorkerID, Binding: binding})
	return exit, f.MustRun(runID)
}

// WithLoopCompactor attaches the production compactor the catalog resolver
// builds: the step host, the session store, the step's own preparer as the
// request builder and the Run activity budget.
func (f *Fixture) WithLoopCompactor(binding *agentrun.TurnRunnerBinding) {
	f.T.Helper()
	built, ok := binding.Compactor.(*modelstep.LoopCompactor)
	if !ok {
		f.T.Fatalf("binding compactor is %T, want the loop compactor", binding.Compactor)
	}
	activity := activity.NewRunActivityService(f.Store, f.Store, models.WithClock(f.Clock.Now))
	binding.Compactor = &modelstep.LoopCompactor{
		Host: binding.Host, Sessions: f.Store.(models.AgentSessionStore), Requests: built.Requests,
		Budget: modelstep.NewRunActivityCompactionBudget(f.Store, activity, f.Clock.Now), Now: f.Clock.Now,
	}
}

// StartBoundedRun creates an objective Run for finn with a budget.
func (f *Fixture) StartBoundedRun(goal string, budget models.BudgetPolicy) *models.AgentRun {
	f.T.Helper()
	created, err := f.Portfolio.CreateAgentRun(f.Ctx, models.CreateAgentRunRequest{
		Scope: f.Scope, Kind: models.RunKindAgentWork, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: FixtureAgent},
		AssignedAgentID: FixtureAgent, Goal: goal, Source: models.RunSourceObjective, Budget: &budget,
	})
	if err != nil {
		f.T.Fatalf("create bounded run: %v", err)
	}
	return created
}

// SortedSteps is the Run's step rows by sequence.
func (f *Fixture) SortedSteps(runID string) []*models.AgentTurn {
	f.T.Helper()
	steps := f.Steps(runID)
	sort.Slice(steps, func(i, j int) bool { return steps[i].Sequence < steps[j].Sequence })
	return steps
}

// RunningStep is the Run's running step row (the loop holds at most one).
func (f *Fixture) RunningStep(runID string) *models.AgentTurn {
	f.T.Helper()
	var running *models.AgentTurn
	for _, step := range f.Steps(runID) {
		if step.Status == models.AgentTurnStatusRunning {
			if running != nil {
				f.T.Fatalf("run %s has two running steps: %s and %s", runID, running.ID, step.ID)
			}
			running = step
		}
	}
	return running
}

// EntriesOfType counts session entries of one type ("note:<kind>" for notes).
func (f *Fixture) EntriesOfType(runID, kind string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		switch {
		case entry.Type == models.SessionEntryNote && entry.Note != nil && kind == "note:"+string(entry.Note.Kind):
			out = append(out, entry)
		case string(entry.Type) == kind:
			out = append(out, entry)
		}
	}
	return out
}

// StepTransitions are the run.transitioned events that applied stepID.
func (f *Fixture) StepTransitions(runID, stepID string) []*models.ActivityEvent {
	f.T.Helper()
	var out []*models.ActivityEvent
	for _, event := range f.Events(runID) {
		if event.TurnID == stepID && strings.HasPrefix(event.EventType, "run.") {
			out = append(out, event)
		}
	}
	return out
}

const scriptedLookupCallID = "det-lookup"

// ScriptedRunbookProducer is a deterministic producer: step 1 calls the
// governed lookup, step 2 reads its result from the session and finishes
// (or parks when park is set). Each step writes its engine state.
type ScriptedRunbookProducer struct {
	f      *Fixture
	lookup capability.ModelAction
	park   bool
	Steps  []map[string]interface{} // Run.Checkpoint as each step saw it
	Result *modelstep.LoopToolResult
}

func (p *ScriptedRunbookProducer) Kind() models.StepProducerKind {
	return models.StepProducerDeterministic
}

func (p *ScriptedRunbookProducer) Produce(ctx context.Context, in agentrun.StepInput) (agentrun.StepOutput, error) {
	p.Steps = append(p.Steps, maps.Clone(in.Run.Checkpoint))
	if p.park {
		return agentrun.StepOutput{
			Park:        &agentrun.LoopPark{Status: models.AgentRunStatusWaitingForEvent, Wake: &models.WakeCondition{Type: "user_message"}, Summary: "Waiting for the requester"},
			KernelState: map[string]interface{}{"phase": "waiting"},
		}, nil
	}
	result, _, err := modelstep.ProducerToolResult(ctx, p.f.Raw, in.Run, in.Path, scriptedLookupCallID)
	if err != nil {
		return agentrun.StepOutput{}, err
	}
	if result == nil {
		call, err := modelstep.ProducerGovernedToolCall(scriptedLookupCallID, &p.lookup, map[string]interface{}{"id": "5"},
			&agentrun.ToolCallDirective{IdempotencyKey: "scripted:" + in.Run.ID + ":lookup", Summary: "Look up issue 5"})
		if err != nil {
			return agentrun.StepOutput{}, err
		}
		return agentrun.StepOutput{ToolCalls: []agentrun.LoopToolCall{call}, KernelState: map[string]interface{}{"phase": "lookup"}}, nil
	}
	p.Result = result
	title, _ := result.Output["title"].(string)
	return agentrun.StepOutput{
		Final:       &agentrun.LoopFinal{Answer: title + " is open.", Output: map[string]interface{}{"title": title}},
		KernelState: map[string]interface{}{"phase": "done"},
	}, nil
}

func (f *Fixture) ScriptedRunbook(park bool) *ScriptedRunbookProducer {
	f.T.Helper()
	for _, action := range f.Offered {
		if action.Action == "lookup" {
			return &ScriptedRunbookProducer{f: f, lookup: action, park: park}
		}
	}
	f.T.Fatal("fixture offers no lookup")
	return nil
}

// PreviewFrame is one turn.answer_preview event of a Run, read from its
// payload.
type PreviewFrame struct {
	Attempt  string
	Sequence string
	Text     string
	Reset    bool
}

func (p PreviewFrame) String() string {
	if p.Reset {
		return fmt.Sprintf("reset(%s#%s)", p.Attempt, p.Sequence)
	}
	return fmt.Sprintf("text(%s#%s %q)", p.Attempt, p.Sequence, p.Text)
}

func (f *Fixture) PreviewFrames(runID string) []PreviewFrame {
	f.T.Helper()
	var out []PreviewFrame
	for _, event := range f.Events(runID, "turn.answer_preview") {
		frame := PreviewFrame{
			Attempt: fmt.Sprint(event.Payload["attemptId"]), Sequence: fmt.Sprint(event.Payload["sequence"]),
		}
		frame.Text, _ = event.Payload["text"].(string)
		frame.Reset, _ = event.Payload["reset"].(bool)
		out = append(out, frame)
	}
	return out
}

const ApprovalScenarioTrigger = "Please close issue 6815"

// ParkedApproval starts the chat and drives it to the approval park.
func (f *Fixture) ParkedApproval() (*models.Conversation, *models.AgentRun) {
	f.T.Helper()
	conversation, run := f.StartChat(ApprovalScenarioTrigger)
	parked := f.DriveToRest(run.ID, 3)
	f.MustStatus(parked, models.AgentRunStatusWaitingForApproval)
	return conversation, parked
}

func (f *Fixture) Approvals(runID string) []*models.ApprovalCheckpoint {
	f.T.Helper()
	approvals, err := f.Raw.ListApprovals(f.Ctx, models.ApprovalFilter{Scope: f.Scope, RunID: runID})
	if err != nil {
		f.T.Fatal(err)
	}
	return approvals
}

// ToolEntries is the Run's tool entries in log order.
func (f *Fixture) ToolEntries(runID string) []*models.SessionEntry {
	f.T.Helper()
	var out []*models.SessionEntry
	for _, entry := range f.Entries(runID) {
		if entry.Type == models.SessionEntryTool {
			out = append(out, entry)
		}
	}
	return out
}

func (f *Fixture) AgentReplies(conversationID string) []string {
	f.T.Helper()
	var out []string
	for _, message := range f.Messages(conversationID) {
		if message.Sender.Type == models.ConversationParticipantAgent {
			out = append(out, message.Content)
		}
	}
	return out
}

// LastStepRequest is the newest model step request.
func (f *Fixture) LastStepRequest() StepView {
	f.T.Helper()
	requests := f.Model.Calls(models.ModelStepPurposeStep)
	if len(requests) == 0 {
		f.T.Fatal("no model step ran")
	}
	return StepView{Req: requests[len(requests)-1]}
}

func (f *Fixture) CancelRun(run *models.AgentRun) *models.AgentRun {
	f.T.Helper()
	current := f.MustRun(run.ID)
	commands := agentrun.NewRunCommandService(f.Chats, models.WithClock(f.Clock.Now))
	result, err := commands.CommandAgentRun(f.Ctx, agentrun.CommandRequest{
		Scope: f.Scope, RunID: run.ID, ExpectedRevision: current.Revision, Kind: agentrun.CommandCancel,
		Actor: models.ActivityActor{Type: "user", ID: FixtureUser},
	})
	if err != nil {
		f.T.Fatalf("cancel run: %v", err)
	}
	return result.Run
}

func (f *Fixture) ConversationRuns(conversationID string) []*models.AgentRun {
	f.T.Helper()
	runs, err := f.Raw.ListAgentRuns(f.Ctx, models.AgentRunFilter{Scope: f.Scope, Limit: 100})
	if err != nil {
		f.T.Fatal(err)
	}
	var out []*models.AgentRun
	for _, run := range runs {
		if id, _ := run.Context[models.ConversationRunConversationIDContextKey].(string); id == conversationID {
			out = append(out, run)
		}
	}
	return out
}

// OnlyRunID is the scope's single Run.
func (f *Fixture) OnlyRunID() string {
	f.T.Helper()
	runs, err := f.Raw.ListAgentRuns(f.Ctx, models.AgentRunFilter{Scope: f.Scope, Limit: 10})
	if err != nil || len(runs) != 1 {
		f.T.Fatalf("runs = %d, %v", len(runs), err)
	}
	return runs[0].ID
}

// StartChatWith is StartChat with a hook that sees the conversation before
// the trigger is posted, and a trigger that may require a response.
func (f *Fixture) StartChatWith(text string, created func(*models.Conversation), requiresResponse bool) (*models.Conversation, *models.AgentRun) {
	f.T.Helper()
	conversation, _, err := f.Convs.CreateConversation(f.Ctx, runtimeconversation.CreateConversationRequest{
		Scope: f.Scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: FixtureAgent}, Title: "Loop fixture chat",
		IdempotencyKey: "loop-chat-with",
	})
	if err != nil {
		f.T.Fatal(err)
	}
	if created != nil {
		created(conversation)
	}
	posted, err := f.Convs.PostChannelMessage(f.Ctx, runtimeconversation.PostChannelMessageRequest{
		Scope: f.Scope, ConversationID: conversation.ID, ExpectedRevision: conversation.Revision,
		Sender: models.ConversationParticipant{Type: models.ConversationParticipantUser, ID: FixtureUser}, Intent: models.MessageIntentQuestion,
		Content: text, Audience: models.ConversationAudience{Kind: models.ConversationAudienceChannel}, RequiresResponse: requiresResponse,
		IdempotencyKey: "loop-chat-with-trigger",
	})
	if err != nil {
		f.T.Fatalf("post trigger: %v", err)
	}
	return conversation, f.ChatRun(conversation, posted.Message)
}

// AnswerVisualContent is the artifact content store visuals are written to.
type AnswerVisualContent struct {
	Puts int
	Data []byte
}

func (s *AnswerVisualContent) Put(_ context.Context, req models.ArtifactContentWrite) (models.ArtifactStoredContent, error) {
	s.Puts++
	s.Data, _ = io.ReadAll(req.Reader)
	return models.ArtifactStoredContent{ContentRef: "visual-content", SizeBytes: int64(len(s.Data)), Digest: models.SHA256Digest(s.Data)}, nil
}

func (s *AnswerVisualContent) Open(context.Context, models.Scope, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.Data)), nil
}

func (f *Fixture) CommandRun(runID string, kind agentrun.CommandKind) *models.AgentRun {
	f.T.Helper()
	current := f.MustRun(runID)
	commands := agentrun.NewRunCommandService(f.Chats, models.WithClock(f.Clock.Now))
	result, err := commands.CommandAgentRun(f.Ctx, agentrun.CommandRequest{
		Scope: f.Scope, RunID: runID, ExpectedRevision: current.Revision, Kind: kind,
		Actor: models.ActivityActor{Type: "user", ID: FixtureUser},
	})
	if err != nil {
		f.T.Fatalf("%s run: %v", kind, err)
	}
	return result.Run
}

// ExpireApproval lets the Run's single approval run out and resolves it the
// way the approval sweep does.
func (f *Fixture) ExpireApproval(runID string) *models.ApprovalResolutionResult {
	f.T.Helper()
	approvals := f.Approvals(runID)
	if len(approvals) != 1 {
		f.T.Fatalf("approvals = %d, want 1", len(approvals))
	}
	approval := approvals[0]
	f.Clock.Advance(approval.ExpiresAt.Sub(f.Clock.Now()) + time.Second)
	result, err := f.Approve.ResolveTimeout(f.Ctx, f.Scope, approval.ID, approval.Revision, approval.ID)
	if err != nil {
		f.T.Fatalf("expire approval: %v", err)
	}
	return result
}
