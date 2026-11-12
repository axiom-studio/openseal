package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestLoopProposeRejectionMessageIsBounded(t *testing.T) {
	long := strings.Repeat("é", maximumProposalRejectionMessageBytes)
	err := rejectProposal(ProposalRejectionNotAdmitted, errors.New(long))
	var rejection *ActionProposalRejection
	if !errors.As(err, &rejection) || len(rejection.Message) > maximumProposalRejectionMessageBytes || !strings.HasPrefix(long, rejection.Message) {
		t.Fatalf("bounded rejection = %#v", err)
	}
	if again := rejectProposal(ProposalRejectionToolArgumentsInvalid, err); again != err {
		t.Fatalf("existing rejection was rewrapped: %#v", again)
	}
	if rejectProposal(ProposalRejectionNotAdmitted, nil) != nil {
		t.Fatal("nil error became a rejection")
	}
}
