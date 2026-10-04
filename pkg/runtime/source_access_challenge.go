package runtime

import (
	"context"
	"errors"
	"strings"
)

const maximumSourceAccessChallengeQuestions = 2
const sourceAccessChallengeStateKey = "sourceAccessChallenge"

// SourceAccessChallengeInteraction describes a bounded conversation continuation
// for one proven failed source read. It is host-only evidence, never consent,
// action authority, or an allowance to retry a failed transport request.
type SourceAccessChallengeInteraction struct {
	FailureID            string `json:"failureId"`
	Phase                string `json:"phase"` // question, answer, or continue
	QuestionsAsked       int    `json:"questionsAsked"`
	QuestionTurnID       string `json:"questionTurnId,omitempty"`
	QuestionTurnSequence int64  `json:"questionTurnSequence,omitempty"`
}

func hasCanonicalSourceAccessChallenge(run *AgentRun) bool {
	if run == nil || isTerminalAgentRunStatus(run.Status) || requiresFinalFailureExplanation(run.Checkpoint) {
		return false
	}
	feedback, active := ReadToolFeedbackCorrection(run.Checkpoint)
	failure := latestCanonicalActionFailure(run.Checkpoint)
	last, _ := run.Checkpoint["lastAction"].(map[string]interface{})
	return active && feedback.LastFailureID != "" && feedback.LastFailureID == last["actionCallId"] &&
		failure != nil && failure.Code() == "source_access_challenge" && failure.Details()["failures"] == "" && !failure.HasRateLimitedSource()
}

// sourceAccessChallengeInteraction reads only kernel-protected state and typed
// scheduler receipts. Callers must additionally prove conversation provenance.
func sourceAccessChallengeInteraction(run *AgentRun) *SourceAccessChallengeInteraction {
	if !hasCanonicalSourceAccessChallenge(run) {
		return nil
	}
	feedback, _ := ReadToolFeedbackCorrection(run.Checkpoint)
	result := &SourceAccessChallengeInteraction{FailureID: feedback.LastFailureID, Phase: "question"}
	correction := run.Checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	state, exists := correction[sourceAccessChallengeStateKey]
	if !exists {
		return result
	}
	question, ok := state.(map[string]interface{})
	if !ok || question["failureId"] != feedback.LastFailureID {
		return nil
	}
	result.QuestionsAsked = toolFeedbackInteger(question["questionsAsked"])
	result.QuestionTurnID, _ = question["questionTurnId"].(string)
	result.QuestionTurnSequence = int64(toolFeedbackInteger(question["questionTurnSequence"]))
	if result.QuestionsAsked < 1 || result.QuestionsAsked > maximumSourceAccessChallengeQuestions || result.QuestionTurnID == "" || result.QuestionTurnSequence < 1 {
		return nil
	}
	if sourceAccessChallengeAnswer(run, result) == nil {
		return nil
	}
	bridge := int64(toolFeedbackInteger(question["answerTurnSequence"]))
	if bridge == 0 && run.LastAppliedTurn == result.QuestionTurnSequence {
		result.Phase = "answer"
		return result
	}
	bridgeID, _ := question["answerTurnId"].(string)
	if bridgeID != "" && bridge == result.QuestionTurnSequence+1 && run.LastAppliedTurn == bridge {
		result.Phase = "continue"
		return result
	}
	return nil
}

func sourceAccessChallengeAnswer(run *AgentRun, interaction *SourceAccessChallengeInteraction) *ConversationAnswerReceipt {
	var accepted *ConversationAnswerReceipt
	for _, intervention := range run.PendingInterventions {
		answer := intervention.ConversationAnswer
		if answer == nil || answer.QuestionTurnID != interaction.QuestionTurnID {
			continue
		}
		if accepted != nil || answer.Scope != run.Scope || answer.RunID != run.ID || answer.AgentID != run.AssignedAgentID ||
			answer.QuestionTurnSequence != interaction.QuestionTurnSequence || answer.QuestionMessageID == "" || answer.AnswerMessageID == "" ||
			answer.AnswerMessageID == answer.QuestionMessageID || strings.TrimSpace(answer.QuestionContent) == "" || strings.TrimSpace(answer.AnswerContent) == "" ||
			answer.AuthenticatedActor.Type != ConversationParticipantUser || answer.AuthenticatedActor.ID == "" ||
			intervention.Actor.Type != "user" || intervention.Actor.ID != answer.AuthenticatedActor.ID {
			return nil
		}
		accepted = answer
	}
	return accepted
}

// resolveSourceAccessChallengeInteraction is also the action-worker admission
// boundary: callers may queue this interaction only after the failed receipt is
// protected and the original conversation is still a supported destination.
func resolveSourceAccessChallengeInteraction(ctx context.Context, portfolio PortfolioStore, run *AgentRun) (*SourceAccessChallengeInteraction, error) {
	interaction := sourceAccessChallengeInteraction(run)
	conversations, ok := portfolio.(ConversationStore)
	if interaction == nil || !ok {
		return nil, nil
	}
	source, conversation, trigger, err := conversationWorkOrigin(ctx, portfolio, conversations, run)
	if err != nil || source == nil || conversation == nil || trigger == nil || conversation.Owner.Type != OwnerTypeAgent || conversation.Owner.ID != run.AssignedAgentID {
		return nil, err
	}
	actor, valid := conversationMessageInitiatingUser(conversation, trigger)
	if !valid {
		return nil, nil
	}
	if interaction.Phase == "question" {
		return interaction, nil
	}
	canonical, err := portfolio.GetAgentRun(ctx, run.Scope, run.ID)
	if err != nil || canonical == nil || isTerminalAgentRunStatus(canonical.Status) {
		return nil, err
	}
	proof := cloneAgentRun(run)
	proof.PendingInterventions = canonical.PendingInterventions
	interaction = sourceAccessChallengeInteraction(proof)
	if interaction == nil {
		return nil, nil
	}
	answer := sourceAccessChallengeAnswer(proof, interaction)
	taskID, _ := run.Context[ConversationTaskContextKey].(string)
	thread := trigger.ThreadRootID
	if thread == "" {
		thread = trigger.ID
	}
	if answer == nil || answer.ConversationTaskID != taskID || answer.ConversationID != conversation.ID || answer.SourceRunID != source.ID ||
		answer.SourceMessageID != trigger.ID || answer.ThreadRootMessageID != thread || answer.AuthenticatedActor != actor {
		return nil, nil
	}
	return interaction, nil
}

func validateSourceAccessChallengeOutcome(interaction *SourceAccessChallengeInteraction, outcome *TurnOutcome) error {
	if interaction == nil {
		return errors.New("source access challenge has no verified conversation continuation")
	}
	if outcome == nil || outcome.ProposedFork != nil || outcome.ProposedDelegation != nil || outcome.ProposedRunbook != nil || outcome.ProposedTask != nil {
		return errors.New("source access challenge cannot fork, delegate, or start work")
	}
	if outcome.NextRunStatus == AgentRunStatusWaitingForEvent && outcome.WakeCondition != nil && outcome.WakeCondition.Type == "user_message" && outcome.WakeCondition.WakeAt == nil && len(outcome.WakeCondition.Predicate) == 0 && outcome.WakeCondition.EventWait == nil &&
		len(outcome.ProposedActions) == 0 && (interaction.Phase == "question" || interaction.Phase == "answer") && interaction.QuestionsAsked < maximumSourceAccessChallengeQuestions {
		question := conversationResultString(outcome.RunOutput, "summary")
		silent, _ := outcome.RunOutput["silent"].(bool)
		_, intake := outcome.RunOutput[AgentRequestDecisionOutputKey]
		if strings.TrimSpace(question) != "" && len(question) <= 64<<10 && !silent && !intake {
			return nil
		}
	}
	if interaction.Phase == "answer" && outcome.NextRunStatus == AgentRunStatusRunning && outcome.WakeCondition == nil && len(outcome.ProposedActions) == 0 && len(outcome.RunOutput) == 0 {
		return nil // One private answer-classification turn; it conveys no consent.
	}
	if interaction.Phase == "continue" && len(outcome.ProposedActions) == 1 && outcome.WakeCondition == nil {
		return nil // Ordinary action admission and the host's choice gate still apply.
	}
	return errors.New("source access challenge requires its bounded question or verified answer continuation")
}

func checkpointSourceAccessChallengeTurn(run *AgentRun, turn *AgentTurn, finish *FinishAgentTurnRequest) {
	if !hasCanonicalSourceAccessChallenge(run) {
		return
	}
	finish.ContinuationCheckpoint = preserveKernelActionHistory(run.Checkpoint, finish.ContinuationCheckpoint)
	interaction := sourceAccessChallengeInteraction(run)
	if interaction == nil || finish.Status != AgentTurnStatusCompleted {
		return
	}
	correction := finish.ContinuationCheckpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	if finish.NextRunStatus == AgentRunStatusWaitingForEvent && finish.WakeCondition != nil && finish.WakeCondition.Type == "user_message" {
		correction[sourceAccessChallengeStateKey] = map[string]interface{}{
			"failureId": interaction.FailureID, "questionsAsked": interaction.QuestionsAsked + 1,
			"questionTurnId": turn.ID, "questionTurnSequence": turn.Sequence,
		}
	} else if interaction.Phase == "answer" && finish.NextRunStatus == AgentRunStatusRunning && len(finish.RequestedActions) == 0 {
		state := correction[sourceAccessChallengeStateKey].(map[string]interface{})
		state["answerTurnId"], state["answerTurnSequence"] = turn.ID, turn.Sequence
	} else if interaction.Phase == "continue" && len(finish.RequestedActions) == 1 {
		state := correction[sourceAccessChallengeStateKey].(map[string]interface{})
		state["actionTurnId"], state["actionTurnSequence"] = turn.ID, turn.Sequence
	}
}

// The accepted proposal turn is required even if an operator manually resumes
// a wait and directly calls action admission. An answer still conveys no YES.
func validateSourceAccessChallengeAction(ctx context.Context, portfolio PortfolioStore, run *AgentRun, call *ActionCall) error {
	if !hasCanonicalSourceAccessChallenge(run) {
		return nil
	}
	correction := run.Checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	state, _ := correction[sourceAccessChallengeStateKey].(map[string]interface{})
	bridge := int64(toolFeedbackInteger(state["answerTurnSequence"]))
	if bridge < 1 || run.LastAppliedTurn != bridge+1 || int64(toolFeedbackInteger(state["actionTurnSequence"])) != run.LastAppliedTurn || call.TurnID == "" || state["actionTurnId"] != call.TurnID {
		return errors.New("source access challenge requires a verified answer and its accepted action proposal")
	}
	candidate := cloneAgentRun(run)
	candidate.LastAppliedTurn = bridge
	interaction, err := resolveSourceAccessChallengeInteraction(ctx, portfolio, candidate)
	if err != nil {
		return err
	}
	if interaction == nil || interaction.Phase != "continue" {
		return errors.New("source access challenge action has no verified answer lineage")
	}
	return nil
}

// The caller has already verified the exact accepted answer/proposal lineage.
// This one admission is distinct from disabled generic failure corrections.
func admitSourceAccessChallengeAction(checkpoint map[string]interface{}, call *ActionCall) (map[string]interface{}, string) {
	result := deepCloneCheckpointMap(checkpoint)
	state, _ := result[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	if state == nil || requiresFinalFailureExplanation(result) {
		return result, "source challenge interaction is unavailable"
	}
	if admitted, _ := state["admittedActionCallId"].(string); admitted != "" {
		if admitted == call.ID {
			return result, ""
		}
		return result, "The accepted source challenge continuation already admitted its action."
	}
	digest := ComputeActionSemanticDigest(call)
	if digests, ok := state["failedSemanticDigests"].([]interface{}); ok {
		for _, prior := range digests {
			if digest != "" && prior == digest {
				return result, "The unchanged failed request was not sent again."
			}
		}
	}
	state["admittedActionCallId"] = call.ID
	return result, ""
}

func sourceAccessChallengeActionAdmitted(ctx context.Context, portfolio PortfolioStore, run *AgentRun, call *ActionCall) bool {
	if !hasCanonicalSourceAccessChallenge(run) || call == nil {
		return false
	}
	state, _ := run.Checkpoint[ToolFeedbackCorrectionCheckpointKey].(map[string]interface{})
	return state["admittedActionCallId"] == call.ID && validateSourceAccessChallengeAction(ctx, portfolio, run, call) == nil
}

// Catalog resolution precedes the conversation adapter's private host flags.
// Recover the original persisted Run rather than trusting its lowered kind or
// a caller-supplied failure checkpoint to reopen a terminal attempt.
func resolveCatalogSourceAccessChallenge(ctx context.Context, catalog AgentTurnCatalog, run *AgentRun, store ConversationTaskKernelStore) (*SourceAccessChallengeInteraction, error) {
	if !hasCanonicalSourceAccessChallenge(run) {
		return nil, nil
	}
	if store == nil {
		store, _ = catalog.(ConversationTaskKernelStore)
		if provider, ok := catalog.(interface{ Store() KernelStore }); store == nil && ok {
			store, _ = provider.Store().(ConversationTaskKernelStore)
		}
	}
	if store == nil {
		return nil, nil
	}
	canonical, err := store.GetAgentRun(ctx, run.Scope, run.ID)
	if errors.Is(err, ErrRunNotFound) {
		return nil, nil
	}
	if err != nil || canonical == nil {
		return nil, err
	}
	if canonical.Scope != run.Scope || canonical.ID != run.ID || canonical.Revision != run.Revision || canonical.Owner != run.Owner ||
		canonical.AssignedAgentID != run.AssignedAgentID || canonical.RootRunID != run.RootRunID || canonical.ParentRunID != run.ParentRunID || canonical.LastAppliedTurn != run.LastAppliedTurn {
		return nil, nil
	}
	interaction, err := resolveSourceAccessChallengeInteraction(ctx, store, canonical)
	provided := sourceAccessChallengeInteraction(run)
	if err != nil || interaction == nil || provided == nil || *interaction != *provided {
		return nil, err
	}
	return interaction, nil
}
