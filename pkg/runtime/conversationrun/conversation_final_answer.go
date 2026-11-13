package conversationrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
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
	// 9 (first part). The Run and the files its answer hands over.
	references := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: run.ID}}
	artifacts, err := runtimeconversation.RunActionArtifactReferences(ctx, h.sessions, h.actions, run)
	if err != nil {
		return agentrun.FinalDeliveryResult{}, err
	}
	handedOver, unknownFiles := runtimeconversation.HandedOverArtifacts(artifacts, answer.Output)
	references = append(references, handedOver...)
	if len(unknownFiles) > 0 && !runtimeconversation.RunExplainingFailure(run) {
		return agentrun.FinalDeliveryResult{Kind: agentrun.FinalDeliveryRetryFinal, RejectionText: runtimeconversation.UnknownHandedOverFilesText(unknownFiles, artifacts)}, nil
	}
	// The files the Run saved and does not hand over were made to build its
	// answer; the catalog remembers that so file lists can leave them out.
	// It is bookkeeping for listings, so it never holds an answer back.
	_ = models.MarkRunFiles(ctx, r.artifacts, run.Scope, artifacts, handedOver)

	// 3. Failure explanation: always a visible text reply, never a visual.
	if runtimeconversation.RunExplainingFailure(run) {
		if content == "" {
			content = runtimeconversation.TerminalRunFailureReply(ctx, h.sessions, h.actions, run)
		}
		message, replayed, err := r.postAgentResponseWithReferences(ctx, run, conversation, trigger, content, references, false, &guard)
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

	// 6. Reactions only (also an answer that only repeats the reacted emoji).
	visual := answer.Visual()
	// Images the Run already attached (generate_image, present) show with the
	// answer; a visual that only repeats them would show each one twice.
	if visual != nil && content != "" && visualRepeatsAttachedImages(*visual, references) {
		visual = nil
	}
	if visual == nil {
		reactions, err := h.runReactions(ctx, run)
		if err != nil {
			return agentrun.FinalDeliveryResult{}, err
		}
		if len(reactions) > 0 && visual == nil && runtimeconversation.ReactionEcho(content, reactions) {
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

	// 9 (second part). The visual, after the host checks.
	visualRejection, _ := answer.Output[FinalOutputVisualRejectionKey].(string)
	if visual != nil {
		checked, checkErr := r.checkAnswerVisual(ctx, run, visual)
		if checkErr == nil {
			reference, storeErr := r.storeAnswerVisual(ctx, run, run.AssignedAgentID,
				answerVisualArtifactID(run.ID, trigger.ID, answerVisualFollowUpKey(run)), *checked)
			checkErr = storeErr
			if storeErr == nil {
				references = append(references, reference)
			}
		}
		if checkErr != nil {
			if !errors.Is(checkErr, errInvalidAnswerVisual) {
				return agentrun.FinalDeliveryResult{}, checkErr
			}
			// A visual that fails the host checks never costs the answer, and
			// is not retried: a retry re-sends the whole answer, which the
			// chat would show vanishing and streaming in again.
			visualRejection = strings.TrimSpace(strings.TrimPrefix(checkErr.Error(), errInvalidAnswerVisual.Error()+":"))
		}
	}

	// 10. Post under the reply guard.
	message, replayed, err := r.postAgentResponseWithReferences(ctx, run, conversation, trigger, content, references, answer.BroadcastToChannel(), &guard)
	if errors.Is(err, errConversationParticipationStopped) {
		return participationStoppedDelivery(), nil
	}
	if err != nil {
		return agentrun.FinalDeliveryResult{}, err
	}
	if visualRejection != "" {
		h.visualRejected(ctx, run, d.Step, guard.WorkerID, visualRejection)
	}

	// 11. Output.
	output := maps.Clone(answer.Output)
	if output == nil {
		output = map[string]interface{}{}
	}
	delete(output, agentrun.FinalOutputVisualKey)
	delete(output, FinalOutputVisualRejectionKey)
	for key, value := range withConversationOutput(base, message, replayed) {
		output[key] = value
	}
	output[FinalOutputSummaryKey] = content
	if answer.BroadcastToChannel() {
		output[agentrun.FinalOutputBroadcastKey] = true
	} else {
		delete(output, agentrun.FinalOutputBroadcastKey)
	}
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

// checkAnswerVisual runs the host checks (bindings, links, images, places)
// of TurnRunnerConfig.VisualChecks on a structurally valid
// visual and stamps its places. A visual that fails them wraps
// errInvalidAnswerVisual.
func (r *TurnRunner) checkAnswerVisual(ctx context.Context, run *models.AgentRun, visual *presentation.Visual) (*presentation.Visual, error) {
	checked := runtimeconversation.CloneVisual(visual)
	var options presentation.ValidateOptions
	if r.config.VisualChecks != nil {
		options = r.config.VisualChecks(run)
	}
	options.SkipHostChecks = false
	surface := checked.Surface(presentation.StateReady)
	if _, err := presentation.Validate(ctx, surface, options); err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidAnswerVisual, err)
	}
	if len(presentation.PlaceIDs(surface)) == 0 {
		return checked, nil
	}
	fixes, err := presentation.ResolvePlaces(ctx, surface, options.PlaceProvenance)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidAnswerVisual, err)
	}
	stamped, err := presentation.StampPlaces(surface, fixes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errInvalidAnswerVisual, err)
	}
	return &presentation.Visual{Title: stamped.Title, Components: stamped.Components}, nil
}

// visualRejected records that the answer went out without its visual. The
// answer is already posted; a lost warning is not retried.
func (h *conversationFinalHandler) visualRejected(ctx context.Context, run *models.AgentRun, step *models.AgentTurn, workerID, reason string) {
	if h.activity == nil {
		return
	}
	if runes := []rune(reason); len(runes) > 400 {
		reason = string(runes[:400]) + "…"
	}
	event := &models.ActivityEvent{
		Scope: run.Scope, RunID: run.ID, AgentID: strings.TrimSpace(run.AssignedAgentID), TeamID: models.TeamIDForRun(run),
		EventType: answerVisualRejectedEvent, Severity: models.ActivitySeverityWarning,
		Summary: answerVisualRejectedSummary + " " + reason, Actor: models.ActivityActor{Type: "worker", ID: workerID},
		Visibility: models.ActivityVisibilityScope,
	}
	if step != nil {
		event.TurnID = step.ID
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

// priorDeliveryRejections counts this Run's earlier final attempts that
// delivery rejected with a text matching match: final tool entries in error
// and final_rejection notes. The answer being delivered is not counted.
func (h *conversationFinalHandler) priorDeliveryRejections(ctx context.Context, run *models.AgentRun, answer agentrun.FinalAnswer, match func(string) bool) (int, error) {
	if h.sessions == nil {
		return 0, nil
	}
	count := 0
	tools, err := h.sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, ToolNames: []string{string(models.ControlToolFinalAnswer)},
		States: []models.ToolEntryState{models.ToolEntryError}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return 0, err
	}
	for _, entry := range tools {
		if entry == nil || entry.ID == answer.ToolEntryID || entry.Tool == nil || entry.Tool.Result == nil {
			continue
		}
		for _, part := range entry.Tool.Result.Content {
			if part.Type == models.ContentPartText && match(strings.TrimSpace(part.Text)) {
				count++
				break
			}
		}
	}
	notes, err := h.sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: run.Scope, RunID: run.ID, NoteKinds: []models.NoteKind{models.NoteFinalRejection}, Limit: models.MaxSessionEntryListLimit,
	})
	if err != nil {
		return 0, err
	}
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

const FinalModeSpoken agentrun.FinalAnswerMode = "spoken" // conversation responseMode == "spoken"

// FinalOutputSummaryKey is the final output field that holds the answer text.
const FinalOutputSummaryKey = "summary"

// FinalOutputVisualRejectionKey is the final output field that records why
// the answer's visual was not posted.
const FinalOutputVisualRejectionKey = "visualRejection"
