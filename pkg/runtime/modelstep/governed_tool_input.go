package modelstep

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/capability"
	runtimeaction "github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// GovernedToolInput is a governed tool call's input after the kernel took
// the governance fields out of it.
type GovernedToolInput struct {
	Arguments         map[string]interface{}
	ReviewContext     *activity.ApprovalReviewContext
	ExternalOperation *activity.ExternalOperationIdentity
	EvidenceRefs      []string
	ReviewSummary     string // reviewContext.summary or _reviewSummary, trimmed
}

// governedInputDirective is the governance part of a deterministic
// producer's tool call directive (ToolCallDirective's Summary,
// ExternalOperation, ReviewContext and EvidenceRefs). With a directive the raw input is the
// plain action arguments, never the review envelope.
type GovernedInputDirective struct {
	Summary           string
	ExternalOperation *activity.ExternalOperationIdentity
	ReviewContext     *activity.ApprovalReviewContext
	EvidenceRefs      []string
}

// governedDirectiveOf is the governance part of a producer's tool call
// directive; nil for a model call.
func governedDirectiveOf(directive *agentrun.ToolCallDirective) *GovernedInputDirective {
	if directive == nil {
		return nil
	}
	return &GovernedInputDirective{
		Summary: directive.Summary, ExternalOperation: directive.ExternalOperation,
		ReviewContext: directive.ReviewContext, EvidenceRefs: directive.EvidenceRefs,
	}
}

const governedReviewSummaryInvalidMessage = GovernedReviewSummaryField + " must be one sentence of 1 to 600 characters."

// ParseGovernedToolInput decodes the model's (or a producer directive's)
// tool input. Errors are *ActionProposalRejection (model-visible).
func ParseGovernedToolInput(action capability.ModelAction, raw json.RawMessage, directive *GovernedInputDirective) (GovernedToolInput, error) {
	object, err := decodeGovernedObject(raw)
	if err != nil {
		return GovernedToolInput{}, err
	}
	var input GovernedToolInput
	switch {
	case directive != nil:
		input, err = governedDirectiveInput(action, object, directive)
	case GovernedToolReviewed(action):
		input, err = governedReviewedInput(action, raw)
	default:
		input, err = governedPlainInput(action, object)
	}
	if err != nil {
		return GovernedToolInput{}, err
	}
	// The host owns action idempotency; a provider may ignore its schema.
	delete(input.Arguments, "idempotencyKey")
	return input, nil
}

func decodeGovernedObject(raw json.RawMessage) (map[string]interface{}, error) {
	var object map[string]interface{}
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, &runtimeaction.ActionProposalRejection{Code: runtimeaction.ProposalRejectionToolArgumentsInvalid, Message: "The tool input must be a JSON object.", Cause: err}
	}
	return object, nil
}

func governedReviewedInput(action capability.ModelAction, raw json.RawMessage) (GovernedToolInput, error) {
	var envelope struct {
		Arguments         map[string]interface{}              `json:"arguments"`
		ExternalOperation *activity.ExternalOperationIdentity `json:"externalOperation"`
		ReviewContext     *activity.ApprovalReviewContext     `json:"reviewContext"`
		EvidenceRefs      []string                            `json:"evidenceRefs"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return GovernedToolInput{}, runtimeaction.RejectProposal(runtimeaction.ProposalRejectionToolArgumentsInvalid,
			fmt.Errorf("The tool input must be the object {arguments, reviewContext, externalOperation, evidenceRefs} with no other keys: %v", err))
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return GovernedToolInput{}, runtimeaction.RejectProposal(runtimeaction.ProposalRejectionToolArgumentsInvalid, errors.New("The tool input must be a single JSON object."))
	}
	if envelope.Arguments == nil {
		return GovernedToolInput{}, runtimeaction.RejectProposal(runtimeaction.ProposalRejectionToolArgumentsInvalid, errors.New("The tool input needs arguments: an object with the action's arguments."))
	}
	if err := runtimeaction.ValidateReviewedActionReviewContext(envelope.ReviewContext); err != nil {
		return GovernedToolInput{}, runtimeaction.RejectProposal(runtimeaction.ProposalRejectionReviewInvalid, fmt.Errorf("reviewContext is invalid: %w", err))
	}
	if err := checkGovernedExternalOperation(action, envelope.ExternalOperation); err != nil {
		return GovernedToolInput{}, err
	}
	return GovernedToolInput{
		Arguments:         envelope.Arguments,
		ReviewContext:     envelope.ReviewContext,
		ExternalOperation: envelope.ExternalOperation,
		EvidenceRefs:      envelope.EvidenceRefs,
		ReviewSummary:     strings.TrimSpace(envelope.ReviewContext.Summary),
	}, nil
}

func governedPlainInput(action capability.ModelAction, arguments map[string]interface{}) (GovernedToolInput, error) {
	input := GovernedToolInput{Arguments: arguments}
	if !GovernedToolNeedsReviewSummary(action) {
		return input, nil
	}
	value, present := arguments[GovernedReviewSummaryField]
	delete(arguments, GovernedReviewSummaryField)
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if !present || !utf8.ValidString(text) || text == "" || utf8.RuneCountInString(text) > maxGovernedReviewSummaryRunes {
		return GovernedToolInput{}, &runtimeaction.ActionProposalRejection{Code: runtimeaction.ProposalRejectionReviewInvalid, Message: governedReviewSummaryInvalidMessage}
	}
	input.ReviewSummary = text
	return input, nil
}

// governedDirectiveInput takes the governance fields from a kernel-authored
// directive. The review context is validated but its content is not required:
// deterministic producers do not author published text.
func governedDirectiveInput(action capability.ModelAction, arguments map[string]interface{}, directive *GovernedInputDirective) (GovernedToolInput, error) {
	if err := runtimeaction.ValidateApprovalReviewContext(directive.ReviewContext); err != nil {
		return GovernedToolInput{}, runtimeaction.RejectProposal(runtimeaction.ProposalRejectionReviewInvalid, fmt.Errorf("reviewContext is invalid: %w", err))
	}
	if err := checkGovernedExternalOperation(action, directive.ExternalOperation); err != nil {
		return GovernedToolInput{}, err
	}
	input := GovernedToolInput{
		Arguments:         arguments,
		ReviewContext:     runtimeaction.CloneApprovalReviewContext(directive.ReviewContext),
		ExternalOperation: cloneExternalOperationIdentity(directive.ExternalOperation),
		EvidenceRefs:      append([]string(nil), directive.EvidenceRefs...),
		ReviewSummary:     strings.TrimSpace(directive.Summary),
	}
	if input.ReviewSummary == "" && input.ReviewContext != nil {
		input.ReviewSummary = strings.TrimSpace(input.ReviewContext.Summary)
	}
	return input, nil
}

func checkGovernedExternalOperation(action capability.ModelAction, operation *activity.ExternalOperationIdentity) error {
	switch {
	case operation == nil && governedExternalOperationRequired(action):
		return &runtimeaction.ActionProposalRejection{Code: runtimeaction.ProposalRejectionExternalIdentityInvalid,
			Message: "This action needs externalOperation: the stable resource it changes and the operation it performs."}
	case operation != nil && !governedExternalOperationOffered(action):
		return &runtimeaction.ActionProposalRejection{Code: runtimeaction.ProposalRejectionExternalIdentityInvalid,
			Message: "This action does not take externalOperation; call it again without it."}
	case operation != nil:
		if err := operation.Validate(); err != nil {
			return runtimeaction.RejectProposal(runtimeaction.ProposalRejectionExternalIdentityInvalid, fmt.Errorf("externalOperation is invalid: %w", err))
		}
	}
	return nil
}

func cloneExternalOperationIdentity(operation *activity.ExternalOperationIdentity) *activity.ExternalOperationIdentity {
	if operation == nil {
		return nil
	}
	cloned := *operation
	return &cloned
}

// narrationSummary is the step's narration cut to its first sentence: up to
// the first line break or sentence end, at most 600 runes. It names a
// governed call that carries no review summary of its own.
func NarrationSummary(narration string) string {
	text := strings.TrimSpace(narration)
	cut := len(text)
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		cut = index
	}
	for _, end := range []string{". ", "! ", "? "} {
		if index := strings.Index(text, end); index >= 0 && index+1 < cut {
			cut = index + 1
		}
	}
	text = strings.TrimSpace(text[:cut])
	if runes := []rune(text); len(runes) > maxGovernedReviewSummaryRunes {
		text = strings.TrimSpace(string(runes[:maxGovernedReviewSummaryRunes]))
	}
	return text
}
