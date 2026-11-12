package action

import (
	"errors"
	"strings"
	"testing"
)

func TestLoopProposeRejectionMessageIsBounded(t *testing.T) {
	long := strings.Repeat("é", maximumProposalRejectionMessageBytes)
	err := RejectProposal(ProposalRejectionNotAdmitted, errors.New(long))
	var rejection *ProposalRejection
	if !errors.As(err, &rejection) || len(rejection.Message) > maximumProposalRejectionMessageBytes || !strings.HasPrefix(long, rejection.Message) {
		t.Fatalf("bounded rejection = %#v", err)
	}
	if again := RejectProposal(ProposalRejectionToolArgumentsInvalid, err); again != err {
		t.Fatalf("existing rejection was rewrapped: %#v", again)
	}
	if RejectProposal(ProposalRejectionNotAdmitted, nil) != nil {
		t.Fatal("nil error became a rejection")
	}
}
