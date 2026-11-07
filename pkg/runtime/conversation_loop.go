package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// The conversation seams of the RunLoop (run-loop section 05 §7, §13 on
// SPEC §2.4). An Agent chat Run (an Agent channel's Run or a Team member's)
// gets, on top of the Agent's own binding:
//   - a StepPreparer: at the Run's first step, the catch-up of the channel
//     into the lane session and the run_context note, in the loop's one
//     context append; at later steps, the catch-up of follow-up messages the
//     steering plan covers, and the Team replaced-approval stop (§8.6);
//   - ToolCheck: run-control calls must name active work of this chat;
//   - AfterTool: the artifact receipt of each governed call (§8.4);
//   - PromptContribution: the static conversation system sections (§6.2);
//   - the conversation flags of the control authority (§7);
//   - Final: the chat delivery (CONV-08).
//
// bindConversationLoop also opens the lane session (get-or-create), because
// the loop loads the session path before any preparer runs.

// ConversationLoopAuthority is computed once per claim by the binding. It
// replaces the private TurnExecutionContext flags; apply folds it into the
// ControlAuthority the tools section reads.
type ConversationLoopAuthority struct {
	CanAskConversationQuestion bool // ask tool offered
	ExternalChannelReply       bool // answer-preview streaming for connector chats
	CanReact                   bool // react tool offered (true for every Agent chat Run)
	ApprovalMode               ConversationApprovalMode
	Spoken                     bool // the spoken system section; FinalModeSpoken comes from ResolveControlAuthority
}

// conversationLoopAuthority computes the §7 flags of an Agent chat Run.
func (r *ConversationRunTurnRunner) conversationLoopAuthority(ctx context.Context, run *AgentRun) (ConversationLoopAuthority, error) {
	if r == nil || r.conversations == nil {
		return ConversationLoopAuthority{}, ErrConversationCoordinationUnavailable
	}
	if run == nil {
		return ConversationLoopAuthority{}, fmt.Errorf("%w: run is required", ErrInvalidAgentRun)
	}
	authority := ConversationLoopAuthority{
		CanReact: true,
		Spoken:   run.Context["responseMode"] == "spoken",
	}
	_, conversation, trigger, err := conversationWorkOrigin(ctx, r.portfolio, r.conversations.store, run)
	if err != nil {
		return ConversationLoopAuthority{}, err
	}
	authority.CanAskConversationQuestion = conversation != nil && trigger != nil &&
		run.AssignedAgentID != "" && run.AssignedAgentID == ConversationRunAgentID(conversation, run)
	authority.ExternalChannelReply = externalChannelContext(conversation)
	if _, authority.ApprovalMode, err = RunConversationApprovalMode(ctx, r.portfolio, r.conversations.store, run); err != nil {
		return ConversationLoopAuthority{}, err
	}
	return authority, nil
}

// apply folds the conversation flags into the claim's control authority
// (ResolveControlAuthority's result). It only narrows: the ask and react
// rights are the intersection of both, so a Deterministic, decision or
// review authority, which offers neither, never gains them. Voice calls
// (no task start) and the spoken final mode are ResolveControlAuthority's.
func (a ConversationLoopAuthority) apply(authority ControlAuthority) ControlAuthority {
	authority.CanAskUser = authority.CanAskUser && a.CanAskConversationQuestion
	authority.CanReact = authority.CanReact && a.CanReact
	if mode := authority.finalMode(); mode != FinalModeDefault && mode != FinalModeSpoken && mode != FinalModeEvidence {
		return authority
	}
	authority.ExternalChannelReply = authority.ExternalChannelReply || a.ExternalChannelReply
	if authority.ApprovalMode == "" {
		authority.ApprovalMode = a.ApprovalMode
	}
	return authority
}

// conversationLoopActions is what the conversation seams read of governed
// calls: receipts (the call's artifacts) and the replaced-approval stop (the
// superseding message of the call's approval).
type conversationLoopActions interface {
	GetActionCall(ctx context.Context, scope Scope, id string) (*ActionCall, error)
	GetApproval(ctx context.Context, scope Scope, approvalID string) (*ApprovalCheckpoint, error)
}

// ConversationLoopStores are the stores the conversation seams use besides
// the runner's own.
type ConversationLoopStores struct {
	Sessions AgentSessionStore
	Actions  conversationLoopActions
	Activity activityAppender // nil: no turn.visual_rejected event
}

// conversationLoop holds one claim's conversation state. It is the
// conversation StepPreparer, and its methods are the binding's ToolCheck and
// AfterTool.
type conversationLoop struct {
	runner       *ConversationRunTurnRunner
	conversation *Conversation
	trigger      *ChannelMessage
	operations   []HostedRunbookOperation
	catchUp      *conversationCatchUp
	sessions     AgentSessionStore
	actions      conversationLoopActions
}

var _ StepPreparer = (*conversationLoop)(nil)

// recentRunContextMessages is how many visible messages the run_context note
// reads for card state and reactions.
const recentRunContextMessages = 50

// replacedApprovalScan bounds the newest tool entries the replaced-approval
// stop reads (one step's calls).
const replacedApprovalScan = 32

// bindConversationLoop fills the conversation seams of an Agent chat Run's
// binding (§13). binding holds the Agent's own resolution (model, actions,
// operations, control authority, the run-start preparer); run is the claimed
// conversation Run.
func (r *ConversationRunTurnRunner) bindConversationLoop(ctx context.Context, run *AgentRun, binding *TurnRunnerBinding, stores ConversationLoopStores) error {
	if r == nil || r.conversations == nil {
		return ErrConversationCoordinationUnavailable
	}
	if binding == nil || stores.Sessions == nil || stores.Actions == nil {
		return errors.New("conversation loop binding needs a binding, the session log and the action store")
	}
	if run == nil || !conversationRunHasActingAgent(run) {
		return fmt.Errorf("%w: a chat loop needs a Run with an acting Agent", ErrInvalidAgentRun)
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	triggerID, _ := run.Context[conversationRunContextTriggerID].(string)
	conversation, err := r.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return err
	}
	if conversation.Owner != run.Owner {
		return fmt.Errorf("%w: conversation Run owner does not match its channel", ErrInvalidAgentRun)
	}
	trigger, err := r.conversations.GetChannelMessage(ctx, run.Scope, conversation.ID, triggerID)
	if err != nil {
		return err
	}
	authority, err := r.conversationLoopAuthority(ctx, run)
	if err != nil {
		return err
	}
	session, err := r.openConversationLoopSession(ctx, stores.Sessions, run)
	if err != nil {
		return err
	}
	active, err := r.activeConversationRuns(ctx, conversation, trigger)
	if err != nil {
		return err
	}
	if len(active) == 0 {
		binding.ModelActions = withoutRunManagementActions(binding.ModelActions)
	}
	loop := &conversationLoop{
		runner: r, conversation: conversation, trigger: trigger,
		operations: cloneHostedRunbookOperations(binding.RunbookOperations),
		catchUp:    &conversationCatchUp{conversations: r.conversations, runs: r.portfolio, attachments: r.conversationAttachmentReader()},
		sessions:   stores.Sessions, actions: stores.Actions,
	}
	binding.Session = session
	binding.Authority = authority.apply(binding.Authority)
	binding.Preparers = append(binding.Preparers, loop)
	binding.ToolCheck = loop.checkToolCall
	binding.AfterTool = loop.afterToolEntry
	binding.Prompt.Sections = append(binding.Prompt.Sections, conversationSystemSections(conversation, run, authority.Spoken)...)
	binding.Final = conversationObservedFinal{loop: loop, inner: newConversationFinalHandler(r, stores.Sessions, stores.Actions, stores.Activity)}
	return nil
}

// conversationObservedFinal is the chat delivery (CONV-08) behind the read
// receipt: before delivering, the Agent is marked as having read the channel
// through the watermark its session committed (§13).
type conversationObservedFinal struct {
	loop  *conversationLoop
	inner *conversationFinalHandler
}

func (f conversationObservedFinal) Deliver(ctx context.Context, d FinalDelivery) (FinalDeliveryResult, error) {
	if d.Run != nil && d.Run.SessionID != "" {
		if session, err := f.loop.sessions.GetAgentSession(ctx, d.Run.Scope, d.Run.SessionID); err == nil {
			f.loop.markObserved(ctx, d.Run, session)
		}
	}
	return f.inner.Deliver(ctx, d)
}

// openConversationLoopSession resolves the Run's session and creates a lane
// session row that does not exist yet (C2 GetOrCreateSession). Child and
// run-owned sessions are already stored.
func (r *ConversationRunTurnRunner) openConversationLoopSession(ctx context.Context, sessions AgentSessionStore, run *AgentRun) (*AgentSession, error) {
	session, err := conversationRunSession(ctx, sessions, run)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, fmt.Errorf("%w: a chat Run of an Agent needs a session", ErrInvalidAgentRun)
	}
	if session.Kind != AgentSessionKindConversation {
		return session, nil
	}
	if strings.TrimSpace(run.LeaseOwner) == "" {
		return nil, fmt.Errorf("%w: the lane session opens only under the Run's lease", ErrInvalidAgentRun)
	}
	now := r.now().UTC()
	opened, err := sessions.OpenConversationAgentSession(ctx, OpenConversationAgentSessionRequest{
		Scope: run.Scope, ConversationID: session.ConversationID, ThreadRootID: session.ThreadRootID, AgentID: session.AgentID,
		RunID: run.ID, Lease: AgentRunLeaseGuard{WorkerID: run.LeaseOwner, Now: now}, Now: now,
	})
	if err != nil {
		return nil, err
	}
	return opened.Session, nil
}

// conversationAttachmentReader is the catch-up's attachment reader, nil when
// the runner has no artifact bytes (message text only).
func (r *ConversationRunTurnRunner) conversationAttachmentReader() *conversationAttachmentReader {
	if r.artifacts == nil || r.config.AttachmentContent == nil {
		return nil
	}
	return &conversationAttachmentReader{artifacts: r.artifacts, content: r.config.AttachmentContent}
}

// withoutRunManagementActions drops the run-control actions: a chat with no
// active work has nothing they could act on. With active work the catalog
// schema is kept as it is (no per-step enum) and checkToolCall refuses a run
// id that is not active work.
func withoutRunManagementActions(actions []capability.ModelAction) []capability.ModelAction {
	kept := make([]capability.ModelAction, 0, len(actions))
	for _, action := range actions {
		if action.SkillID != RunManagementSkillID {
			kept = append(kept, action)
		}
	}
	return kept
}

// BeforeStep is the conversation preparer (§13 StartRun / BeforeStep).
func (l *conversationLoop) BeforeStep(ctx context.Context, in StepInput, steering SteeringPlan) (*StepContextAppend, error) {
	if in.Run == nil || in.Session == nil || in.Step == nil {
		return nil, fmt.Errorf("%w: the conversation preparer needs a run, a session and a step", ErrInvalidAgentRun)
	}
	started, err := l.runStarted(ctx, in.Run, in.Session)
	if err != nil {
		return nil, err
	}
	// The session as loaded is committed: an earlier step's catch-up ingested
	// through its watermark.
	l.markObserved(ctx, in.Run, in.Session)
	if !started {
		return l.startRun(ctx, in, steering)
	}
	if output, replaced, err := l.replacedTeamApproval(ctx, in.Run); err != nil {
		return nil, err
	} else if replaced {
		return &StepContextAppend{EarlyFinal: &LoopFinal{Status: AgentRunStatusCompleted, Output: output}}, nil
	}
	if !l.chatSession(in.Session) || steering.ThroughMessageSequence <= in.Session.ChannelWatermark {
		// Nothing the steering plan covers is past what the session holds.
		return nil, nil
	}
	// Follow-ups arrive as catch-up user entries (message headers), in the
	// lane session or a promoted Run's child alike; the loop's own steering
	// entries for them are dropped by SourceKey.
	inherited, err := l.inheritedRuns(ctx, in.Session)
	if err != nil {
		return nil, err
	}
	result, err := l.catchUp.collect(ctx, catchUpRequest{
		Run: in.Run, Session: in.Session, Conversation: l.conversation, Viewer: conversationAgentViewer(in.Run), Trigger: l.trigger,
		Through: steering.ThroughMessageSequence, Step: in.Step.Sequence, InheritedRuns: inherited,
	})
	if err != nil {
		return nil, err
	}
	return result.stepContextAppend(in.Session), nil
}

// chatSession reports whether session reads this chat's channel: the lane
// session, or a child of it (a promoted Run's) that carries the chat.
func (l *conversationLoop) chatSession(session *AgentSession) bool {
	return session != nil && session.ConversationID != "" && session.ConversationID == l.conversation.ID && session.Scope == l.conversation.Scope
}

// inheritedRuns are the Runs whose assistant entries session renders from
// its parent path: a promoted Run's child forked at the lane leaf holds the
// lane's earlier answers, so their channel messages are not ingested again.
// It is nil for a session that inherits no path.
func (l *conversationLoop) inheritedRuns(ctx context.Context, session *AgentSession) (map[string]bool, error) {
	if session.ParentSessionID == "" || session.ForkSequence < 1 {
		return nil, nil
	}
	path, err := l.sessions.LoadAgentSessionPath(ctx, session.Scope, session.ID)
	if err != nil {
		return nil, err
	}
	runs := map[string]bool{}
	for _, entry := range path.Entries {
		if entry != nil && entry.SessionID != session.ID && entry.Type == SessionEntryAssistant && entry.RunID != "" {
			runs[entry.RunID] = true
		}
	}
	return runs, nil
}

// runStarted reports whether the session's rendered path already holds an
// entry of run: its own entries, or inherited parent entries up to the fork
// (a Run promoted mid-flight started in the lane). A Run starts at its first
// step with no such entry. The task_promoted note the promotion writes on
// the lane does not count: a Run promoted between its bind and its first
// append still starts (its lane append fails with ErrSessionMoved and the
// loop re-runs the preparers on the child).
func (l *conversationLoop) runStarted(ctx context.Context, run *AgentRun, session *AgentSession) (bool, error) {
	var through int64 // 0: the whole session
	for current := session; ; {
		// Oldest first; the one task_promoted note of run is skipped.
		entries, err := l.sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{Scope: run.Scope, SessionID: current.ID, RunID: run.ID, Limit: 2})
		if err != nil {
			return false, err
		}
		for _, entry := range entries {
			if entry == nil || entry.Type == SessionEntryNote && entry.Note != nil && entry.Note.Kind == NoteTaskPromoted {
				continue
			}
			if through == 0 || entry.Sequence <= through {
				return true, nil
			}
		}
		if current.ParentSessionID == "" || current.ForkSequence < 1 {
			// A root, or a lineage-only parent that is never rendered.
			return false, nil
		}
		through = current.ForkSequence
		if current, err = l.sessions.GetAgentSession(ctx, run.Scope, current.ParentSessionID); err != nil {
			return false, err
		}
	}
}

// startRun is §13 StartRun: the trigger as the Agent sees it, participation,
// then the catch-up and the run_context note.
func (l *conversationLoop) startRun(ctx context.Context, in StepInput, steering SteeringPlan) (*StepContextAppend, error) {
	r, run, session := l.runner, in.Run, in.Session
	conversation, err := r.conversations.GetConversation(ctx, run.Scope, l.conversation.ID)
	if err != nil {
		return nil, err
	}
	if conversation.Owner != run.Owner {
		return nil, fmt.Errorf("%w: conversation Run owner does not match its channel", ErrInvalidAgentRun)
	}
	_, viewer, trigger, err := r.agentConversationTurnContext(ctx, TurnExecutionContext{Run: run}, conversation, l.trigger.ID)
	if err != nil {
		return nil, err
	}
	if r.config.RequireParticipationOptIn && !conversationParticipationAllows(conversation, trigger) {
		return &StepContextAppend{EarlyFinal: &LoopFinal{Status: AgentRunStatusCompleted, Output: participationStoppedOutcome().RunOutput}}, nil
	}
	l.conversation, l.trigger = conversation, trigger
	if !l.chatSession(session) {
		return nil, fmt.Errorf("%w: session %s of chat Run %s does not carry its conversation", ErrInvalidAgentRun, session.ID, run.ID)
	}
	// A Run promoted before its first step starts in its child session: a
	// fork of the lane carries the lane's watermark, so the catch-up ingests
	// the trigger after the inherited history; a child of an empty lane has
	// only its task brief and watermark 0, and seeds like a new lane (§3.3).
	existing, err := l.sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{Scope: run.Scope, SessionID: session.ID, Limit: 1})
	if err != nil {
		return nil, err
	}
	inherited, err := l.inheritedRuns(ctx, session)
	if err != nil {
		return nil, err
	}
	through := max(trigger.Sequence, steering.ThroughMessageSequence)
	result, err := l.catchUp.collect(ctx, catchUpRequest{
		Run: run, Session: session, Conversation: conversation, Viewer: viewer, Trigger: trigger,
		Through: through, Seed: len(existing) == 0 || session.ChannelWatermark == 0 && session.ForkSequence == 0, Step: in.Step.Sequence,
		InheritedRuns: inherited,
	})
	if err != nil {
		return nil, err
	}
	part, err := l.runContextNote(ctx, in, viewer, result)
	if err != nil {
		return nil, err
	}
	if catchUp := result.stepContextAppend(session); catchUp != nil {
		catchUp.Entries = append(catchUp.Entries, part.Entries...)
		part = catchUp
	}
	return part, nil
}

// runContextNote builds the run_context note of the Run's start, which goes
// after the catch-up entries of catchUp.
func (l *conversationLoop) runContextNote(ctx context.Context, in StepInput, viewer ConversationViewer, catchUp *catchUpResult) (*StepContextAppend, error) {
	through := l.trigger.Sequence
	if catchUp != nil && catchUp.Watermark > through {
		through = catchUp.Watermark
	}
	recent, err := l.runner.conversations.ListChannelMessages(ctx, ChannelMessageFilter{
		Scope: l.conversation.Scope, ConversationID: l.conversation.ID, ThreadRootID: in.Session.ThreadRootID,
		BeforeSequence: through + 1, Limit: recentRunContextMessages, Descending: true, Viewer: &viewer,
	})
	if err != nil {
		return nil, err
	}
	reverseChannelMessages(recent)
	note, err := l.runner.conversationRunContextNote(ctx, conversationContextInput{
		Run: in.Run, Session: in.Session, Sessions: l.sessions, Conversation: l.conversation, Trigger: l.trigger,
		Viewer: viewer, Recent: recent, CatchUp: catchUp, Operations: l.operations, Step: in.Step.Sequence,
	})
	if err != nil {
		return nil, err
	}
	part := &StepContextAppend{}
	if note != nil {
		part.Entries = append(part.Entries, note)
	}
	return part, nil
}

// markObserved records that the Agent read the channel through the
// watermark session committed (§13: after a successful append). The loop
// has no hook after its append, so the committed watermark is marked at the
// next step, after each tool call and before delivery. Receipts are
// advisory: an error is dropped rather than failing the step.
func (l *conversationLoop) markObserved(ctx context.Context, run *AgentRun, session *AgentSession) {
	if l.runner.coordinator == nil || run == nil || !l.chatSession(session) || session.ChannelWatermark <= 0 {
		return
	}
	_ = l.runner.coordinator.markObserved(ctx, l.conversation, conversationAgentViewer(run).Participant, session.ChannelWatermark)
}

func conversationAgentViewer(run *AgentRun) ConversationViewer {
	return ConversationViewer{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: run.AssignedAgentID}}
}

// replacedTeamApproval is §8.6: a Team member whose held approval was
// superseded by a newer message that went to another member (not one of
// this Run's own follow-ups) stops without answering.
func (l *conversationLoop) replacedTeamApproval(ctx context.Context, run *AgentRun) (map[string]interface{}, bool, error) {
	if !isTeamMemberRun(run) {
		return nil, false, nil
	}
	entries, err := l.sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, Types: []SessionEntryType{SessionEntryTool}, Descending: true, Limit: replacedApprovalScan,
	})
	if err != nil {
		return nil, false, err
	}
	entry := supersededApprovalEntry(entries)
	if entry == nil {
		return nil, false, nil
	}
	messageID, err := l.supersedingMessageID(ctx, run.Scope, entry)
	if err != nil {
		return nil, false, err
	}
	output, replaced := replacedTeamApproval(run, messageID)
	return output, replaced, nil
}

// supersededApprovalEntry is the superseded governed call among the Run's
// newest step's tool entries (entries newest first), or nil.
func supersededApprovalEntry(entries []*SessionEntry) *SessionEntry {
	if len(entries) == 0 || entries[0] == nil {
		return nil
	}
	newestStep := entries[0].StepID
	for _, entry := range entries {
		if entry == nil || entry.StepID != newestStep {
			break
		}
		if entry.State == ToolEntryError && entry.Tool != nil && entry.Tool.Result != nil && entry.Tool.Result.Status == ToolResultSuperseded {
			return entry
		}
	}
	return nil
}

// supersedingMessageID is the message that superseded entry's approval: from
// its result when the result names it, else from the call's approval.
func (l *conversationLoop) supersedingMessageID(ctx context.Context, scope Scope, entry *SessionEntry) (string, error) {
	for _, part := range entry.Tool.Result.Content {
		if part.Type != ContentPartJSON {
			continue
		}
		var value struct {
			SupersededByMessageID string `json:"supersededByMessageId"`
		}
		if json.Unmarshal(part.Value, &value) == nil && value.SupersededByMessageID != "" {
			return value.SupersededByMessageID, nil
		}
	}
	if entry.ActionCallID == "" {
		return "", nil
	}
	call, err := l.actions.GetActionCall(ctx, scope, entry.ActionCallID)
	if errors.Is(err, ErrActionNotFound) {
		return "", nil
	}
	if err != nil || call == nil || call.ApprovalID == "" {
		return "", err
	}
	approval, err := l.actions.GetApproval(ctx, scope, call.ApprovalID)
	if errors.Is(err, ErrApprovalNotFound) {
		return "", nil
	}
	if err != nil || approval == nil || approval.Status != ApprovalStatusSuperseded {
		return "", err
	}
	return approval.SupersededByMessageID, nil
}

// replacedTeamApproval is the early-finish output of a Team member whose
// approval messageID superseded, unless that message is one of the Run's
// own follow-ups (then the member answers it).
func replacedTeamApproval(run *AgentRun, messageID string) (map[string]interface{}, bool) {
	if !isTeamMemberRun(run) || strings.TrimSpace(messageID) == "" || conversationRunHasFollowUp(run, messageID) {
		return nil, false
	}
	return map[string]interface{}{"approvalReplaced": true, "replacedByMessageId": messageID}, true
}

// checkToolCall is the binding's ToolCheck (§7): a run-control call must
// name active work of this chat that accepts the command now.
func (l *conversationLoop) checkToolCall(ctx context.Context, in StepToolsInput, entry *SessionEntry) (string, error) {
	if entry == nil || entry.Tool == nil || len(entry.Tool.Directive) > 0 {
		return "", nil
	}
	action, err := selectGovernedAction(in.Binding, entry.Tool.Name, nil)
	if err != nil || action == nil || action.SkillID != RunManagementSkillID {
		// An unresolvable call is refused by the executor itself.
		return "", nil
	}
	var arguments struct {
		RunID string `json:"runId"`
	}
	_ = json.Unmarshal(entry.Tool.Arguments, &arguments)
	runID := strings.TrimSpace(arguments.RunID)
	if runID == "" {
		return "A runId from the active work in the run context is required.", nil
	}
	active, err := l.runner.activeConversationRuns(ctx, l.conversation, l.trigger)
	if err != nil {
		return "", err
	}
	for _, run := range active {
		if run.ID == runID && slicesContainsRunCommand(run.AvailableControls, action.Action) {
			return "", nil
		}
	}
	return fmt.Sprintf("Run %s is not active work in this conversation, or it cannot be %s now. Use a run id from the active work in the run context.",
		runID, runCommandParticiple(action.Action)), nil
}

// runCommandParticiple is "paused", "resumed", "canceled".
func runCommandParticiple(action string) string {
	if strings.HasSuffix(action, "e") {
		return action + "d"
	}
	return action + "ed"
}

// afterToolEntry is the binding's AfterTool (§8.4): a governed call that
// completed gets its artifact receipt in the chat.
func (l *conversationLoop) afterToolEntry(ctx context.Context, in StepToolsInput, entry *SessionEntry) error {
	l.markObserved(ctx, in.Run, in.Session)
	if entry == nil || entry.ActionCallID == "" {
		return nil
	}
	return l.runner.postToolArtifactReceipt(ctx, l.actions, in.Run, l.conversation, l.trigger, entry)
}
