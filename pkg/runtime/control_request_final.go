package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/collaboration"
)

// agent_request_decision and agent_request_review (section 04 §11.7): the
// typed final tools of AgentRequest decision and completion-review Runs. They
// replace runOutput.agentRequestDecision /
// runOutput.agentRequestCompletionReviewDecision of the old Turn form. The
// settle validates the call with the same parsers ResolveDecisionRun and
// ResolveCompletionReviewRun use, then hands the loop a FinalAnswer whose
// Output is exactly the structured decision; the loop completes the Run with
// that Output through the binding's (non-conversation) final handler.

const (
	requestDecisionInvalidCode = "decision_invalid"
	// maximumRequestDecisionMessageRunes bounds the reason / question
	// (schema maxLength).
	maximumRequestDecisionMessageRunes = 4096
)

type requestFinalArgs struct {
	Decision string `json:"decision"`
	Message  string `json:"message"`
}

func requestFinalSchema(decisions []string, messageDescription string) map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"required": []string{"decision", "message"},
		"properties": map[string]interface{}{
			"decision": map[string]interface{}{"type": "string", "enum": append([]string(nil), decisions...)},
			"message": map[string]interface{}{
				"type": "string", "minLength": 1, "maxLength": maximumRequestDecisionMessageRunes,
				"description": messageDescription,
			},
		},
	}
}

type requestDecisionHandler struct{}

func (requestDecisionHandler) Spec(agentrun.ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: "Record your decision on the incoming request and finish this run. Call it alone.",
		InputSchema: requestFinalSchema(
			[]string{string(collaboration.AgentRequestDecisionAccept), string(collaboration.AgentRequestDecisionReject), string(collaboration.AgentRequestDecisionRequestClarification)},
			"accept/reject: a concise reason. request_clarification: one concrete question.",
		),
	}
}

func (requestDecisionHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return models.DecodeControlInto[requestFinalArgs](raw)
}

func (requestDecisionHandler) Run(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResult, error) {
	args, ok := cc.Arguments.(*requestFinalArgs)
	if !ok || args == nil {
		return ControlResult{}, errors.New("agent_request_decision arguments were not decoded")
	}
	output := requestFinalOutput(collaboration.AgentRequestDecisionOutputKey, args)
	_, message, err := collaboration.ParseAgentRequestDecisionOutput(output)
	return x.acceptRequestFinal(ctx, cc, output, message, err)
}

type completionReviewHandler struct{}

func (completionReviewHandler) Spec(agentrun.ControlAuthority) ControlToolSpec {
	return ControlToolSpec{
		Description: "Record your review of the delegated work and finish this run. Call it alone.",
		InputSchema: requestFinalSchema([]string{"approve", "reject"}, "A concise evidence-based reason."),
	}
}

func (completionReviewHandler) Decode(raw json.RawMessage) (interface{}, error) {
	return models.DecodeControlInto[requestFinalArgs](raw)
}

func (completionReviewHandler) Run(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext) (ControlResult, error) {
	args, ok := cc.Arguments.(*requestFinalArgs)
	if !ok || args == nil {
		return ControlResult{}, errors.New("agent_request_review arguments were not decoded")
	}
	output := requestFinalOutput(collaboration.AgentRequestCompletionReviewOutputKey, args)
	_, message, err := collaboration.ParseAgentRequestCompletionReviewOutput(output)
	return x.acceptRequestFinal(ctx, cc, output, message, err)
}

// requestFinalOutput is the Run output the resolvers consume: the trimmed
// decision under its one output key.
func requestFinalOutput(key string, args *requestFinalArgs) map[string]interface{} {
	return map[string]interface{}{key: map[string]interface{}{
		"decision": strings.TrimSpace(args.Decision),
		"message":  strings.TrimSpace(args.Message),
	}}
}

// acceptRequestFinal turns a parsed decision into the loop's FinalAnswer (the
// tool entry stays open; the loop marks it delivered and completes the Run
// with Output), or records a decision_invalid rejection the model corrects.
// Rejections count toward the loop's final-rejection bound like any other.
func (x *ControlExecutor) acceptRequestFinal(ctx context.Context, cc agentrun.ControlContext, output map[string]interface{}, message string, parseErr error) (ControlResult, error) {
	reason := ""
	switch {
	case parseErr != nil:
		reason = parseErr.Error() + "."
	case message == "":
		reason = "message must give a concise reason."
	case utf8.RuneCountInString(message) > maximumRequestDecisionMessageRunes:
		reason = fmt.Sprintf("message is longer than %d characters.", maximumRequestDecisionMessageRunes)
	}
	if reason != "" {
		return x.rejectFinal(ctx, cc, &finalAnswerDraft{}, &finalAnswerRejection{Code: requestDecisionInvalidCode, Message: reason})
	}
	final := &agentrun.FinalAnswer{
		StepID: cc.Entry.StepID, ToolEntryID: cc.Entry.ID, ToolCallID: cc.Entry.ToolCallID,
		Answer: message, Output: output,
	}
	return ControlResult{Kind: ControlFinal, Run: cc.Run, Entry: cc.Entry, Final: final}, nil
}
