package runtime

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUnsupportedURLRejected(t *testing.T) {
	f := newFinalFixture(t,
		LoopToolCall{ID: "read", Name: "action_web_read", Arguments: json.RawMessage(`{"query":"release notes"}`)},
		finalCall("bad", `{"answer":"See https://invented.test/notes for details."}`),
		finalCall("again", `{"answer":"As I said, https://invented.test/notes."}`),
		finalCall("ok", `{"answer":"The notes are at https://docs.example.test/notes."}`),
	)
	f.governedEntry("read", "web", "read", map[string]interface{}{"url": "https://docs.example.test/notes", "text": "v2 ships"})
	value := requireRejected(t, f, f.settleFinal("bad", ControlAuthority{}), "bad", finalUnsupportedURLCode)
	if value["message"] != "The answer cites https://invented.test/notes, which does not appear in anything you read." {
		t.Fatalf("message = %v", value["message"])
	}
	// The refused final (its arguments and its rejection) is not evidence.
	requireRejected(t, f, f.settleFinal("again", ControlAuthority{}), "again", finalUnsupportedURLCode)
	// Trailing punctuation is trimmed; the read tool's result holds the URL.
	final := requireFinal(t, f.settleFinal("ok", ControlAuthority{}))
	if !strings.Contains(final.Answer, "https://docs.example.test/notes") {
		t.Fatalf("final = %#v", final)
	}
	// A plain-text reply is checked the same way and refused with a note.
	requireRejectionNote(t, f.plain("step-2", 2, ControlAuthority{}, "Read https://invented.test/other"), "step-2", finalUnsupportedURLCode)
	// URLs in the Run goal count as read.
	g := newFinalFixture(t, finalCall("final", `{"answer":"Summarized https://goal.example.test/page."}`))
	g.run.Goal = "Summarize https://goal.example.test/page"
	requireFinal(t, g.settleFinal("final", ControlAuthority{}))
}

func TestUnsupportedURLDecodesJSONEvidence(t *testing.T) {
	// The governed result is JSON-encoded (& is escaped as \u0026); the answer
	// cites the URL as read.
	f := newFinalFixture(t,
		LoopToolCall{ID: "search", Name: "action_web_search", Arguments: json.RawMessage(`{"url":"https://args.test/a?x=1\u0026y=2"}`)},
		finalCall("final", `{"answer":"Results: https://x.test/s?q=a&page=2 and https://args.test/a?x=1&y=2."}`),
	)
	f.governedEntry("search", "web", "search", map[string]interface{}{"results": []interface{}{map[string]interface{}{"url": "https://x.test/s?q=a&page=2"}}})
	if raw := f.stored("search").Tool.Result.Content[0].Value; !strings.Contains(string(raw), `\u0026`) {
		t.Fatalf("result not escaped: %s", raw)
	}
	requireFinal(t, f.settleFinal("final", ControlAuthority{}))
}

func TestUnsupportedURLChecksAnswerBesideReport(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("final", `{"answer":"See https://invented.test/summary","report":"Nothing to cite."}`),
		finalCall("claim", `{"answer":"Email sent to Ana.","report":"Weekly status."}`),
	)
	requireRejected(t, f, f.settleFinal("final", ControlAuthority{}), "final", finalUnsupportedURLCode)
	requireRejected(t, f, f.settleFinal("claim", ControlAuthority{}), "claim", finalUnevidencedClaimCode)
}

func TestExternalClaimNeedsReceipt(t *testing.T) {
	f := newFinalFixture(t,
		LoopToolCall{ID: "send", Name: "action_mail_send", Arguments: json.RawMessage(`{"to":"a@example.test"}`)},
		finalCall("claim", `{"answer":"Done: email sent to Ana."}`),
		finalCall("claim2", `{"answer":"Done: email sent to Ana."}`),
	)
	f.governedEntry("send", "mail", "send", map[string]interface{}{"status": "queued"})
	requireRejected(t, f, f.settleFinal("claim", ControlAuthority{}), "claim", finalUnevidencedClaimCode)
	// The same call with an external operation receipt evidences the claim.
	f.store.mu.Lock()
	receipt := cloneActionCall(f.store.actions["action-send"])
	receipt.ExternalOperationDigest = "sha256:" + strings.Repeat("a", 64)
	f.store.actions[portfolioKey(f.run.Scope, receipt.ID)] = receipt
	f.store.mu.Unlock()
	requireFinal(t, f.settleFinal("claim2", ControlAuthority{}))
	// Without any governed call the claim is refused.
	g := newFinalFixture(t, finalCall("final", `{"answer":"I posted the comment."}`))
	requireRejected(t, g, g.settleFinal("final", ControlAuthority{}), "final", finalUnevidencedClaimCode)
}

func TestApprovalClaimOnceThenFailsRun(t *testing.T) {
	f := newFinalFixture(t,
		finalCall("first", `{"answer":"Your approval request has been submitted."}`),
		finalCall("second", `{"answer":"This is waiting for your approval."}`),
	)
	value := requireRejected(t, f, f.settleFinal("first", ControlAuthority{}), "first", finalApprovalClaimCode)
	if value["message"] != "No approval is pending in this Run; say what actually happened." {
		t.Fatalf("message = %v", value["message"])
	}
	result := f.settleFinal("second", ControlAuthority{})
	if result.Kind != ControlFailed || result.Failure == nil || result.Failure.Code != finalUnverifiedApprovalCode ||
		result.Failure.Kind != RunFailureKindExecution || result.Entry == nil || result.Entry.State != ToolEntryError {
		t.Fatalf("second = %#v", result)
	}
	requireControlError(t, f.stored("second").Tool.Result, ToolResultFailed, finalUnverifiedApprovalCode)
	// The plain-text path counts the same way.
	g := newFinalFixture(t)
	requireRejectionNote(t, g.plain("step-2", 2, ControlAuthority{}, "Pending approval from your admin."), "step-2", finalApprovalClaimCode)
	failed := g.plain("step-3", 3, ControlAuthority{}, "Awaiting approval.")
	if failed.Kind != ControlFailed || failed.Failure == nil || failed.Failure.Code != finalUnverifiedApprovalCode {
		t.Fatalf("plain second = %#v", failed)
	}
	// Saying something was approved is not a pending-approval claim.
	h := newFinalFixture(t, finalCall("final", `{"answer":"The change was approved and merged."}`))
	requireFinal(t, h.settleFinal("final", ControlAuthority{}))
}

// Prod run 46c87b15: a pending setup form that asks to approve mail scopes,
// and the agent's correction that no approval is pending, are no approval claims.
func TestClaimsPendingApprovalPhrases(t *testing.T) {
	for _, text := range []string{
		"The Google Workspace setup form is live in the chat now — status pending, waiting on you. Sign in and approve sending mail.",
		"Correction: there is no approval pending in this run. What exists is a setup form, status pending, awaiting your completion.",
		"Nothing is awaiting approval.",
		"The change was approved and merged.",
	} {
		if claimsPendingApproval(text) {
			t.Errorf("claim detected in %q", text)
		}
	}
	for _, text := range []string{
		"Your approval request has been submitted.",
		"This is waiting for your approval.",
		"Pending approval from your admin.",
		"Awaiting approval.",
		"The transfer needs your approval before it runs.",
		"The approval is pending with finance.",
	} {
		if !claimsPendingApproval(text) {
			t.Errorf("no claim detected in %q", text)
		}
	}
}
