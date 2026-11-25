package conversationrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/google/uuid"
)

// Final answer delivery for Agent chat Runs (run-loop section 05 §8, SPEC
// §3.3 step 3). The control section has already decoded and checked the
// answer (silent only on spoken channels, non-empty unless reactions said it,
// completion checks); delivery posts it to the chat under the Run's reply
// guard, or tells the loop why it cannot.

const (
	conversationSilentNotSpokenText = "silent is only allowed for spoken replies; write the answer."
	conversationEmptyAnswerText     = "The reply is empty. Write the user-visible answer."
	conversationEmptyAnswerFailure  = "Agent channel response did not contain user-visible output"
	conversationEmptyAnswerCode     = "empty_answer"
	// visualRepairTries is how often a Run asks again for blocks that
	// fail: with the first answer, three tries. After that they are left
	// out without a word in the answer.
	visualRepairTries = 2
	// answerRejectionsAllowed keeps repairs under the run loop's bound
	// (runloop maximumFinalRejections, 3): its third rejected answer in a
	// row fails the Run, so a repair is not asked after two rejections of
	// any kind.
	answerRejectionsAllowed = 2
)

// conversationFinalHandler is the FinalAnswerHandler of an Agent chat Run
// (Agent channel Runs and Team member Runs). The Team routing Run posts
// nothing and does not use it.
type conversationFinalHandler struct {
	runner   *TurnRunner
	sessions runtimeconversation.LoopSessionEntryLister
	actions  models.ActionCallReader
	activity activityAppender
}

// activityAppender appends one activity event (turn.visual_rejected).
type activityAppender interface {
	AppendActivity(ctx context.Context, event *models.ActivityEvent) (*models.ActivityEvent, error)
}

// newConversationFinalHandler builds the chat delivery over the stores the
// loop binding already holds. sessions and actions read this Run's tool
// entries and governed calls (reactions, artifact references, prior delivery
// rejections); activity may be nil (no visual_rejected event).
func newConversationFinalHandler(runner *TurnRunner, sessions runtimeconversation.LoopSessionEntryLister, actions models.ActionCallReader, activity activityAppender) *conversationFinalHandler {
	return &conversationFinalHandler{runner: runner, sessions: sessions, actions: actions, activity: activity}
}

// Deliver is section 05 §8.2 on the SPEC §2.4 seam. Results:
//   - complete: posted (or silent, reactions only, participation stopped,
//     a kernel final with nothing to post);
//   - park: posted, then the loop parks the Run (OTP wait, FinalPark.Deliver);
//   - fail: the failure explanation was posted (explain mode), or an empty
//     answer came back a second time;
//   - retry_final: an is_error result the model corrects in its next step;
//   - ErrSteeringArrived (from the reply guard): continue.
func (h *conversationFinalHandler) Deliver(ctx context.Context, d agentrun.FinalDelivery) (agentrun.FinalDeliveryResult, error) {
	if h == nil || h.runner == nil || h.runner.conversations == nil {
		return agentrun.FinalDeliveryResult{}, runtimeconversation.ErrConversationCoordinationUnavailable
	}
	run := d.Run
	if run == nil {
		return agentrun.FinalDeliveryResult{}, models.ErrInvalidAgentRun
	}
	r := h.runner
	answer := d.Answer
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	triggerID, _ := run.Context[models.ConversationRunTriggerIDContextKey].(string)
	if !conversationRunHasActingAgent(run) || conversationID == "" || triggerID == "" {
		return agentrun.FinalDeliveryResult{}, fmt.Errorf("%w: chat delivery needs an acting Agent, a conversation and a trigger", models.ErrInvalidAgentRun)
	}
	// 2. Participation.
	if stopped, err := r.participationStopped(ctx, run); err != nil {
		return agentrun.FinalDeliveryResult{}, err
	} else if stopped {
		return participationStoppedDelivery(), nil
	}
	conversation, err := r.conversations.GetConversation(ctx, run.Scope, conversationID)
	if err != nil {
		return agentrun.FinalDeliveryResult{}, err
	}
	viewer := models.ConversationViewer{Participant: models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: run.AssignedAgentID}}
	trigger, err := r.conversations.GetVisibleChannelMessage(ctx, run.Scope, conversation.ID, triggerID, viewer)
	if err != nil {
		return agentrun.FinalDeliveryResult{}, err
	}
	base := map[string]interface{}{"conversationId": conversation.ID, "triggerMessageId": trigger.ID}
	content := strings.TrimSpace(answer.Answer)
	guard := d.Guard
	references := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}
	saved, err := runtimeconversation.RunActionArtifactReferences(ctx, h.sessions, h.actions, run)
	if err != nil {
		return agentrun.FinalDeliveryResult{}, err
	}

	// 3. Failure explanation: always a visible text reply, never a visual.
	if runtimeconversation.RunExplainingFailure(run) {
		if content == "" {
			content = runtimeconversation.TerminalRunFailureReply(ctx, h.sessions, h.actions, run)
		}
		files, err := r.resolveAnswerFiles(ctx, run, content, saved)
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		content = files.content
		references = append(references, files.references...)
		message, replayed, err := r.postAgentResponseWithReferences(ctx, run, conversation, trigger, content, references, &guard)
		if errors.Is(err, errConversationParticipationStopped) {
			return participationStoppedDelivery(), nil
		}
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		output := withConversationOutput(base, message, replayed)
		output[FinalOutputSummaryKey] = content
		return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryFail, Output: output}, nil
	}

	// A kernel final (a deterministic producer's or a preparer's early
	// result) carries its own Run output; it posts only an answer it has.
	kernelFinal := !answer.PlainText && answer.ToolEntryID == ""
	if kernelFinal && content == "" {
		return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryComplete, Output: maps.Clone(answer.Output)}, nil
	}

	// 4. Silent.
	if answer.Silent() {
		if trigger.ResponseMode != "spoken" {
			return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryRetryFinal, RejectionText: conversationSilentNotSpokenText}, nil
		}
		output := maps.Clone(base)
		output[agentrun.FinalOutputSilentKey] = true
		return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryComplete, Output: output}, nil
	}

	// 5. Visual blocks. A reply of only the blocks delivery asked to repair
	// goes into the answer it asked about; then every block is read with
	// the host checks.
	if answer.PlainText {
		repaired, ok, err := h.repairedAnswer(ctx, run, answer, content)
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		if ok {
			content = repaired
		}
	}
	// The files the answer links, stored as they are now. A link to a file
	// that is not in the chat is asked about once, then left as its text.
	files, err := r.resolveAnswerFiles(ctx, run, content, saved)
	if err != nil {
		return agentrun.FinalDeliveryResult{}, err
	}
	if len(files.unknown) > 0 {
		prior, err := h.priorDeliveryRejections(ctx, run, answer, isFileLinkRepairRequest)
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		if prior == 0 {
			return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryRetryFinal, RejectionText: fileLinkRepairRequest(files.unknown, saved)}, nil
		}
	}
	content = files.content
	references = append(references, files.references...)
	// The files the Run saved and does not show were made to build its
	// answer; the catalog remembers that so file lists can leave them out.
	// It is bookkeeping for listings, so it never holds an answer back.
	_ = models.MarkRunFiles(ctx, r.artifacts, run.Scope, saved, files.references)
	visual, drops := r.readAnswerBlocks(ctx, run, content)
	left := map[string]bool{}
	for _, drop := range drops {
		if drop.Whole {
			left[drop.BlockID] = true
		}
	}
	// Images the answer links show with it; blocks that only repeat them
	// would show each one twice.
	if visual != nil && presentation.PlainAnswer(content) != "" && visualRepeatsAttachedImages(*visual, references) {
		for _, component := range visual.Components {
			left[component.ID] = true
		}
		visual, drops = nil, nil
	}

	// 6. Reactions only (also an answer that only repeats the reacted emoji
	// and shows nothing else).
	if visual == nil && len(files.references) == 0 {
		reactions, err := h.runReactions(ctx, run)
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		if len(reactions) > 0 && runtimeconversation.ReactionEcho(presentation.PlainAnswer(content), reactions) {
			output := maps.Clone(base)
			output[agentrun.FinalOutputReactedKey] = true
			return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryComplete, Output: output}, nil
		}
	}

	// 7. Empty: once a correctable error, then a failure.
	if content == "" {
		prior, err := h.priorDeliveryRejections(ctx, run, answer, func(text string) bool { return text == conversationEmptyAnswerText })
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		if prior > 0 {
			return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryFail, Failure: &agentrun.LoopFailure{
				Kind: runtimeconversation.RunFailureKindExecution, Code: conversationEmptyAnswerCode, Message: conversationEmptyAnswerFailure,
			}}, nil
		}
		return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryRetryFinal, RejectionText: conversationEmptyAnswerText}, nil
	}

	// 8. Repair, up to visualRepairTries times per Run: blocks that failed
	// are asked for again on their own. The person keeps the streamed
	// answer meanwhile; nothing else of it is written again.
	if slices.ContainsFunc(drops, func(drop presentation.BlockDrop) bool { return drop.Whole }) {
		prior, err := h.priorDeliveryRejections(ctx, run, answer, isVisualRepairRequest)
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		rejected, err := h.priorDeliveryRejections(ctx, run, answer, func(string) bool { return true })
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		if prior < visualRepairTries && rejected < answerRejectionsAllowed {
			return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryRetryFinal, RejectionText: visualRepairRequest(drops)}, nil
		}
	}

	// 9. The blocks that are left, as one card the message references. A
	// block that still fails never costs the answer: its placeholder goes
	// without a word in the message, and the Run records every part left
	// out.
	if visual != nil {
		reference, err := r.storeAnswerVisual(ctx, run, run.AssignedAgentID,
			answerVisualArtifactID(run.ID, trigger.ID, answerVisualFollowUpKey(run)), *visual)
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		references = append(references, reference)
	}

	// 10. Post under the reply guard.
	posted := presentation.PlacedAnswer(content, left)
	message, replayed, err := r.postAgentResponseWithReferences(ctx, run, conversation, trigger, posted, references, &guard)
	if errors.Is(err, errConversationParticipationStopped) {
		return participationStoppedDelivery(), nil
	}
	if err != nil {
		return agentrun.FinalDeliveryResult{}, err
	}
	for _, drop := range drops {
		h.visualRejected(ctx, run, d.Step, guard.WorkerID, drop)
	}
	for _, target := range files.unknown {
		h.fileLinkRejected(ctx, run, d.Step, guard.WorkerID, target)
	}

	// 11. Output.
	output := maps.Clone(answer.Output)
	if output == nil {
		output = map[string]interface{}{}
	}
	for key, value := range withConversationOutput(base, message, replayed) {
		output[key] = value
	}
	output[FinalOutputSummaryKey] = presentation.PlainAnswer(content)
	kind := agentrun.FinalDeliveryComplete
	if answer.Park != nil && answer.Park.Deliver {
		// OTP: the answer asks for the code; the Run waits for the reply.
		kind = agentrun.FinalDeliveryPark
	}
	return agentrun.FinalDeliveryResult{Kind: kind, Output: output}, nil
}

func participationStoppedDelivery() agentrun.FinalDeliveryResult {
	return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryComplete, Output: maps.Clone(participationStoppedOutput())}
}

func withConversationOutput(base map[string]interface{}, message *models.ChannelMessage, replayed bool) map[string]interface{} {
	output := maps.Clone(base)
	output["messageId"] = message.ID
	output["replayed"] = replayed
	return output
}

// answerVisualFollowUpKey separates the visual of a continued Run's later
// answer from its first: "" without follow-ups, else the latest follow-up.
func answerVisualFollowUpKey(run *models.AgentRun) string {
	followUps := models.ConversationRunFollowUps(run)
	if len(followUps) == 0 {
		return ""
	}
	return "followup:" + followUps[len(followUps)-1].MessageID
}

// readAnswerBlocks reads the answer's blocks with the host checks (bindings,
// links, images, places) of TurnRunnerConfig.VisualChecks and stamps their
// places. A block that fails is left out on its own and listed in the
// drops; a place stamping failure costs every block, as it cannot say
// which one it was.
func (r *TurnRunner) readAnswerBlocks(ctx context.Context, run *models.AgentRun, content string) (*presentation.Visual, []presentation.BlockDrop) {
	var options presentation.ValidateOptions
	if r.config.VisualChecks != nil {
		options = r.config.VisualChecks(run)
	}
	options.SkipHostChecks = false
	visual, drops := presentation.ReadBlocks(ctx, content, options)
	if visual == nil {
		return nil, drops
	}
	surface := visual.Surface(presentation.StateReady)
	if len(presentation.PlaceIDs(surface)) == 0 {
		return visual, drops
	}
	fixes, err := presentation.ResolvePlaces(ctx, surface, options.PlaceProvenance)
	if err == nil {
		surface, err = presentation.StampPlaces(surface, fixes)
	}
	if err != nil {
		for _, component := range visual.Components {
			drops = append(drops, presentation.BlockDrop{BlockID: component.ID, Whole: true, Drop: presentation.Drop{ComponentID: component.ID, Reason: err.Error()}})
		}
		return nil, drops
	}
	return &presentation.Visual{Title: surface.Title, Components: surface.Components}, drops
}

// visualRepairLead starts the request to repair failing blocks; a Run
// asks it up to visualRepairTries times.
const visualRepairLead = "Some visual blocks in your answer could not be shown:"

func isVisualRepairRequest(text string) bool {
	return strings.HasPrefix(text, visualRepairLead)
}

// visualRepairRequest asks for the blocks that failed, by id, with why.
func visualRepairRequest(drops []presentation.BlockDrop) string {
	var b strings.Builder
	b.WriteString(visualRepairLead)
	for _, drop := range drops {
		if drop.Whole {
			fmt.Fprintf(&b, "\n- %s: %s", drop.BlockID, drop.Reason)
		}
	}
	b.WriteString("\nReply with only the corrected blocks, each whole with its ::: lines and the same #id, one per block above; " +
		"leave a block out to drop it. Your answer text stands; do not write it again.")
	return b.String()
}

// askedBlockIDs are the block ids a repair request lists ("- <id>: why").
func askedBlockIDs(request string) map[string]bool {
	ids := map[string]bool{}
	for _, line := range strings.Split(request, "\n") {
		if id, _, ok := strings.Cut(strings.TrimPrefix(line, "- "), ": "); ok && strings.HasPrefix(line, "- ") {
			ids[strings.TrimSpace(id)] = true
		}
	}
	return ids
}

// repairedAnswer merges a reply that holds only blocks delivery asked to
// repair into the answer it asked about: the reply of the step refused with
// this Run's latest repair request. ok is false for any other reply, which
// is an answer of its own.
func (h *conversationFinalHandler) repairedAnswer(ctx context.Context, run *models.AgentRun, answer agentrun.FinalAnswer, reply string) (string, bool, error) {
	if h.sessions == nil || !onlyBlocks(reply) {
		return "", false, nil
	}
	requests, err := h.visualRepairRequestNotes(ctx, run, answer)
	if err != nil || len(requests) == 0 {
		return "", false, err
	}
	return h.answerWithRepairs(ctx, run, requests, reply)
}

// onlyBlocks reports a reply of visual blocks and nothing else.
func onlyBlocks(reply string) bool {
	return len(presentation.ParseAnswer(reply)) > 0 && strings.TrimSpace(presentation.PlainAnswer(reply)) == ""
}

// answerWithRepairs swaps reply's blocks into the answer the last of
// requests asked about. That answer is itself a repair reply when an
// earlier request was answered with blocks that failed again; it goes into
// its own answer first. A block asked for and left out of reply is dropped.
func (h *conversationFinalHandler) answerWithRepairs(ctx context.Context, run *models.AgentRun, requests []*models.SessionEntry, reply string) (string, bool, error) {
	request := requests[len(requests)-1]
	asked := askedBlockIDs(request.Note.Text)
	repairs := make(map[string]string, len(asked))
	for _, block := range presentation.ParseAnswer(reply) {
		if !block.Closed || !asked[block.ID] {
			return "", false, nil
		}
		repairs[block.ID] = reply[block.Start:block.End]
	}
	for id := range asked {
		if _, ok := repairs[id]; !ok {
			repairs[id] = ""
		}
	}
	about, err := h.stepReply(ctx, run, request.StepID)
	if err != nil || about == "" {
		return "", false, err
	}
	if len(requests) > 1 && onlyBlocks(about) {
		merged, ok, err := h.answerWithRepairs(ctx, run, requests[:len(requests)-1], about)
		if err != nil || !ok {
			return "", false, err
		}
		about = merged
	}
	repaired, _ := presentation.ReplaceBlocks(about, repairs)
	return strings.TrimSpace(repaired), true, nil
}

// stepReply is the text of the step's reply without tool calls, or "".
func (h *conversationFinalHandler) stepReply(ctx context.Context, run *models.AgentRun, stepID string) (string, error) {
	entries, err := h.sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, StepID: stepID, Types: []models.SessionEntryType{models.SessionEntryAssistant}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return "", err
	}
	for index := len(entries) - 1; index >= 0; index-- {
		entry := entries[index]
		if entry != nil && entry.Assistant != nil && len(entry.Assistant.ToolCalls) == 0 {
			return strings.TrimSpace(entry.Assistant.Text), nil
		}
	}
	return "", nil
}

// visualRepairRequestNotes are this Run's repair requests (final_rejection
// notes on earlier steps), oldest first.
func (h *conversationFinalHandler) visualRepairRequestNotes(ctx context.Context, run *models.AgentRun, answer agentrun.FinalAnswer) ([]*models.SessionEntry, error) {
	notes, err := h.sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, NoteKinds: []models.NoteKind{models.NoteFinalRejection}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	var requests []*models.SessionEntry
	for _, note := range notes {
		if note != nil && note.Note != nil && note.StepID != answer.StepID && isVisualRepairRequest(strings.TrimSpace(note.Note.Text)) {
			requests = append(requests, note)
		}
	}
	return requests, nil
}

// visualRejected records one part of the answer's visual that was left
// out, or the whole visual (a drop with no path). The answer is already
// posted; a lost warning is not retried. The event id is stable per Run,
// step and drop, so a replayed delivery does not record it twice.
func (h *conversationFinalHandler) visualRejected(ctx context.Context, run *models.AgentRun, step *models.AgentTurn, workerID string, drop presentation.BlockDrop) {
	if h.activity == nil {
		return
	}
	reason := drop.Reason
	if runes := []rune(reason); len(runes) > 400 {
		reason = string(runes[:400]) + "…"
	}
	summary := fmt.Sprintf("%s block %q: %s", answerVisualRejectedSummary, drop.BlockID, reason)
	if !drop.Whole {
		summary = fmt.Sprintf("%s %s (component %q of block %q): %s", answerVisualPartRejectedSummary, drop.Path, drop.ComponentID, drop.BlockID, reason)
	}
	stepID := ""
	if step != nil {
		stepID = step.ID
	}
	event := &models.ActivityEvent{
		ID:    uuid.NewSHA1(uuid.NameSpaceOID, []byte(strings.Join([]string{"visual-rejected", run.ID, stepID, drop.BlockID, drop.Path, drop.Reason}, "\x00"))).String(),
		Scope: run.Scope, RunID: run.ID, TurnID: stepID, AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: models.TeamIDForRun(run),
		EventType: answerVisualRejectedEvent, Severity: models.ActivitySeverityWarning, CreatedAt: h.runner.now(),
		Summary: summary, Actor: models.ActivityActor{Type: "worker", ID: workerID},
		Payload:    map[string]interface{}{"blockId": drop.BlockID, "path": drop.Path, "componentId": drop.ComponentID, "reason": drop.Reason},
		Visibility: models.ActivityVisibilityScope,
	}
	_, _ = h.activity.AppendActivity(ctx, event)
}

// runReactions are the reactions this Run added: its completed react tool
// entries, in entry order.
func (h *conversationFinalHandler) runReactions(ctx context.Context, run *models.AgentRun) ([]runtimeconversation.TurnReaction, error) {
	if h.sessions == nil {
		return nil, nil
	}
	entries, err := h.sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, ToolNames: []string{string(models.ControlToolReact)},
		States: []models.ToolEntryState{models.ToolEntryCompleted}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return nil, err
	}
	reactions := make([]runtimeconversation.TurnReaction, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.Tool == nil || len(entry.Tool.Arguments) == 0 {
			continue
		}
		var reaction runtimeconversation.TurnReaction
		if json.Unmarshal(entry.Tool.Arguments, &reaction) != nil || strings.TrimSpace(reaction.Emoji) == "" {
			continue
		}
		reactions = append(reactions, reaction)
	}
	return reactions, nil
}

// priorDeliveryRejections counts this Run's earlier replies that delivery
// rejected with a text matching match (final_rejection notes). The answer
// being delivered is not counted.
func (h *conversationFinalHandler) priorDeliveryRejections(ctx context.Context, run *models.AgentRun, answer agentrun.FinalAnswer, match func(string) bool) (int, error) {
	if h.sessions == nil {
		return 0, nil
	}
	notes, err := h.sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, NoteKinds: []models.NoteKind{models.NoteFinalRejection}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, entry := range notes {
		if entry == nil || entry.Note == nil || answer.PlainText && entry.StepID == answer.StepID {
			continue
		}
		if match(strings.TrimSpace(entry.Note.Text)) {
			count++
		}
	}
	return count, nil
}

// FinalOutputSummaryKey is the final output field that holds the answer text.
const FinalOutputSummaryKey = "summary"
