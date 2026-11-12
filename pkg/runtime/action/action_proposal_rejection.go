package action

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// Codes of a proposal the model can correct.
const (
	ProposalRejectionToolArgumentsInvalid         = "tool_arguments_invalid"
	ProposalRejectionReviewInvalid                = "action_review_invalid"
	ProposalRejectionExternalIdentityInvalid      = "action_external_identity_invalid"
	ProposalRejectionReviewSummaryRequired        = "action_review_summary_required"
	ProposalRejectionNotAdmitted                  = "action_not_admitted"
	ProposalRejectionCapabilityUnavailable        = "capability_unavailable"
	ProposalRejectionUnknownTool                  = "unknown_tool"
	maximumProposalRejectionMessageBytes          = 1024
	proposalRejectionReviewSummaryRequiredMessage = "Before this can go to a person for approval, describe it in one short sentence (what it does and to what) and call the tool again."
	proposalRejectionDefaultMessage               = "The action was not accepted."
)

// ProposalRejection is a proposal the kernel refused for a reason the
// model can correct. It is returned to the model as a refused tool result and
// never persisted.
type ProposalRejection struct {
	// Code is one of the ProposalRejection* codes.
	Code string
	// Message is model-visible, at most 1024 bytes.
	Message string
	Cause   error
}

func (e *ProposalRejection) Error() string { return e.Message }

func (e *ProposalRejection) Unwrap() error { return e.Cause }

// RejectProposal turns err into a model-visible rejection with code. It is
// nil-safe and keeps an existing rejection unchanged.
func RejectProposal(code string, err error) error {
	if err == nil {
		return nil
	}
	var existing *ProposalRejection
	if errors.As(err, &existing) {
		return err
	}
	return &ProposalRejection{Code: code, Message: boundProposalRejectionMessage(err.Error()), Cause: err}
}

// boundProposalRejectionMessage trims message and cuts it on a rune
// boundary to the model-visible cap.
func boundProposalRejectionMessage(message string) string {
	message = strings.TrimSpace(strings.ToValidUTF8(message, ""))
	if message == "" {
		return proposalRejectionDefaultMessage
	}
	if len(message) <= maximumProposalRejectionMessageBytes {
		return message
	}
	cut := maximumProposalRejectionMessageBytes
	for cut > 0 && !utf8.RuneStart(message[cut]) {
		cut--
	}
	return strings.TrimSpace(message[:cut])
}
