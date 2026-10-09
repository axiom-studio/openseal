package runtime

import (
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func payApprovalFixture(status ApprovalStatus) *ApprovalCheckpoint {
	return &ApprovalCheckpoint{
		Status: status, Risk: skill.RiskLevelWrite, Summary: "Working on your request.",
		PolicyReason: "Approve paying 312.00 INR to https://www.amazon.in",
		ProposedAction: map[string]interface{}{"action": "live-browser-pay", "review": "always", "skillId": "skill-live-browser",
			"arguments": map[string]interface{}{"amount": "312.00", "currency": "INR", "merchant": "https://www.amazon.in", "target": "s18:e1", "sessionId": "session",
				"intent": "Place the Amazon India order using the saved card ending in 1001."},
			"reviewContext": map[string]interface{}{"summary": "Place the Amazon India order using the saved card ending in 1001."}},
	}
}

func TestPaymentApprovalPresentationMatchesTheWebCard(t *testing.T) {
	got := PresentApproval(payApprovalFixture(ApprovalStatusPending))
	want := ApprovalPresentation{Title: "Payment — needs your approval", Lines: []string{
		"Approve paying 312.00 INR to https://www.amazon.in",
		"Merchant: amazon.in · Amount: ₹312.00 · Card: Card ending 1001",
		"Always asks, even with approvals skipped",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending payment = %#v", got)
	}
	if got := PresentApproval(payApprovalFixture(ApprovalStatusApproved)); got.Title != "Payment approved · ₹312.00 to amazon.in · card ending 1001" || len(got.Lines) != 0 {
		t.Fatalf("approved payment = %#v", got)
	}
	declined := payApprovalFixture(ApprovalStatusRejected)
	declined.DecisionReason = "Wrong address"
	if got := PresentApproval(declined); got.Title != "Payment declined · ₹312.00 to amazon.in · card ending 1001" || !reflect.DeepEqual(got.Lines, []string{"Wrong address"}) {
		t.Fatalf("declined payment = %#v", got)
	}
}

func TestApprovalPresentationNeverShowsProgressPhrasesIdentifiersOrRedactions(t *testing.T) {
	approval := &ApprovalCheckpoint{Status: ApprovalStatusPending, Risk: skill.RiskLevelExternal, Summary: "Working on your request.",
		ProposedAction: map[string]interface{}{"skillId": "skill-slack", "action": "send_message", "review": "always",
			"arguments": map[string]interface{}{"intent": "Post the weekly summary to #general", "url": "https://slack.com/x", "token": "secret"}}}
	got := PresentApproval(approval)
	if got.Title != "Post the weekly summary to #general on slack.com" ||
		!reflect.DeepEqual(got.Lines, []string{"Always asks, even with approvals skipped · Affects outside services"}) {
		t.Fatalf("presentation = %#v", got)
	}
	approval.Status = ApprovalStatusApproved
	if got := PresentApproval(approval); got.Title != "Approved: Post the weekly summary to #general on slack.com" || !reflect.DeepEqual(got.Lines, []string{"Affects outside services"}) {
		t.Fatalf("approved presentation = %#v", got)
	}
	for _, intent := range []string{"Send message", "send_message now", "Use [REDACTED] to sign in", "Proposed send_message"} {
		approval := &ApprovalCheckpoint{Status: ApprovalStatusPending, Risk: skill.RiskLevelExternal, Summary: "Working on your request.",
			ProposedAction: map[string]interface{}{"skillId": "skill-slack", "action": "send_message", "arguments": map[string]interface{}{"intent": intent}}}
		if got := PresentApproval(approval); got.Title != "Your agent wants to act outside this chat" {
			t.Fatalf("%q leaked into %#v", intent, got)
		}
	}
}
