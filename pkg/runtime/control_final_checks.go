package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Completion checks of a final answer (section 04 §11.4, SPEC A8/A9). They
// run in the turn_final_answer pipeline after the visual check and before the
// host extension fields; each failure is an is_error result (or, for plain
// text, a final_rejection note) the model can correct:
//
//  1. URLs the answer cites must appear in something the Run read.
//  2. An external mutation claim needs a succeeded ActionCall receipt with an
//     external operation.
//  3. An approval claim is refused once; the second one fails the Run.
//  4. Evidence mode: the claim ledger is checked against the Run's snapshot
//     and reviewed synchronously by the EvidenceGroundingReviewer. Nothing is
//     staged in the checkpoint; the draft lives in the tool entry.

func init() {
	registerFinalAnswerCheck(finalAnswerCheck{Order: finalCheckOrderCompletion, Name: "completion", Check: checkFinalAnswerCompletion})
	registerFinalAnswerCheck(finalAnswerCheck{Order: finalCheckOrderEvidence, Name: "evidence_grounding", Check: checkFinalEvidenceGrounding})
}

// Completion check error codes (stable; the model sees them).
const (
	finalUnsupportedURLCode     = "unsupported_url"
	finalUnevidencedClaimCode   = "unevidenced_external_claim"
	finalApprovalClaimCode      = "approval_claim"
	finalUnverifiedApprovalCode = "unverified_approval_claim"
	finalEvidenceRepairCode     = "evidence_repair_required"
)

// A backslash terminates a URL as well as ordinary whitespace: JSON-encoded
// text (the visual) carries a real newline as the two bytes `\n`, and
// accepting the backslash would append the next line to the URL.
var hostedTurnURLPattern = regexp.MustCompile(`https?://[^\s<>"'\\]+`)

func checkFinalAnswerCompletion(ctx context.Context, x *ControlExecutor, cc ControlContext, d *finalAnswerDraft) (*finalAnswerRejection, error) {
	content := d.content()
	if d.Args.Silent || d.ReactionsOnly {
		content = ""
	}
	// 1. URLs.
	if urls := finalAnswerURLs(content, d); len(urls) > 0 {
		corpus, err := x.finalEvidenceCorpus(ctx, cc)
		if err != nil {
			return nil, err
		}
		for _, url := range urls {
			if !strings.Contains(corpus, url) {
				return &finalAnswerRejection{Code: finalUnsupportedURLCode,
					Message: fmt.Sprintf("The answer cites %s, which does not appear in anything you read.", url)}, nil
			}
		}
	}
	// 2. External mutation claims.
	if claimsExternalMutation(content) {
		receipt, err := x.runHasExternalReceipt(ctx, cc)
		if err != nil {
			return nil, err
		}
		if !receipt {
			return &finalAnswerRejection{Code: finalUnevidencedClaimCode,
				Message: "The answer says an external change was made, but no action in this run made one. Say what actually happened."}, nil
		}
	}
	// 3. Approval claims. No approval of this Run is pending at final time
	// (a pending approval parks the Run before any final settle).
	if unverifiedApprovalClaim(content) || claimsPendingApproval(content) {
		prior, err := x.priorFinalErrorCodes(ctx, cc)
		if err != nil {
			return nil, err
		}
		if prior[finalApprovalClaimCode] == 0 {
			return &finalAnswerRejection{Code: finalApprovalClaimCode,
				Message: "No approval is pending in this Run; say what actually happened."}, nil
		}
		return &finalAnswerRejection{Code: finalUnverifiedApprovalCode,
			Message: "No approval is pending in this Run; say what actually happened.",
			Failure: &LoopFailure{Kind: RunFailureKindExecution, Code: finalUnverifiedApprovalCode,
				Message: "The agent said an approval was pending when none was, so the run stopped."}}, nil
	}
	return nil, nil
}

// finalAnswerURLs are the URLs of the answer text and of the visual, trimmed
// of trailing punctuation, in order, without duplicates.
func finalAnswerURLs(content string, d *finalAnswerDraft) []string {
	sources := []string{content}
	if d.Visual != nil {
		encoded, err := json.Marshal(d.Visual)
		if err == nil {
			sources = append(sources, string(encoded))
		}
	}
	seen := map[string]bool{}
	var urls []string
	for _, source := range sources {
		for _, candidate := range hostedTurnURLPattern.FindAllString(source, -1) {
			candidate = strings.TrimRight(candidate, ".,);]}")
			if candidate == "" || seen[candidate] {
				continue
			}
			seen[candidate] = true
			urls = append(urls, candidate)
		}
	}
	return urls
}

// finalEvidenceCorpus is the text the Run read: the Run goal and context and
// the session path's user, tool (arguments and results), note and compaction
// contents. Model-authored text is not evidence: assistant entries, the final
// tool's own arguments and results, and final_rejection notes (which quote the
// refused URL) are left out.
func (x *ControlExecutor) finalEvidenceCorpus(ctx context.Context, cc ControlContext) (string, error) {
	var b strings.Builder
	b.WriteString(cc.Run.Goal)
	b.WriteByte('\n')
	if len(cc.Run.Context) > 0 {
		if encoded, err := json.Marshal(cc.Run.Context); err == nil {
			b.Write(encoded)
			b.WriteByte('\n')
		}
	}
	path, err := x.controlSessionPath(ctx, cc)
	if err != nil {
		return "", err
	}
	if path == nil {
		return b.String(), nil
	}
	if path.Compaction != nil && path.Compaction.Compaction != nil {
		b.WriteString(path.Compaction.Compaction.Summary)
		b.WriteByte('\n')
	}
	for _, entry := range path.Entries {
		if entry == nil {
			continue
		}
		switch entry.Type {
		case SessionEntryUser:
			if entry.User != nil {
				writeContentParts(&b, entry.User.Content)
			}
		case SessionEntryTool:
			if entry.Tool == nil || entry.Tool.Name == string(ControlToolFinalAnswer) {
				continue
			}
			b.Write(entry.Tool.Arguments)
			b.WriteByte('\n')
			if entry.Tool.Result != nil {
				writeContentParts(&b, entry.Tool.Result.Content)
			}
		case SessionEntryNote:
			if entry.Note != nil && entry.Note.Kind != NoteFinalRejection {
				b.WriteString(entry.Note.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

func writeContentParts(b *strings.Builder, parts []ContentPart) {
	for _, part := range parts {
		for _, text := range []string{part.Text, string(part.Value), part.URI, part.Path, part.Note} {
			if text != "" {
				b.WriteString(text)
				b.WriteByte('\n')
			}
		}
	}
}

// controlSessionPath is the step's session path: cc.Path, else loaded.
func (x *ControlExecutor) controlSessionPath(ctx context.Context, cc ControlContext) (*AgentSessionPath, error) {
	if cc.Path != nil {
		return cc.Path, nil
	}
	sessionID := cc.Run.SessionID
	if cc.Session != nil {
		sessionID = cc.Session.ID
	}
	if sessionID == "" {
		return nil, nil
	}
	return x.Sessions.LoadAgentSessionPath(ctx, cc.Run.Scope, sessionID)
}

// runHasExternalReceipt: some tool entry of this Run references a succeeded
// ActionCall with an external operation receipt.
func (x *ControlExecutor) runHasExternalReceipt(ctx context.Context, cc ControlContext) (bool, error) {
	entries, err := x.Sessions.ListAgentSessionEntries(ctx, SessionEntryFilter{
		Scope: cc.Run.Scope, RunID: cc.Run.ID, Types: []SessionEntryType{SessionEntryTool}, Limit: maxSessionEntryListLimit,
	})
	if err != nil {
		return false, err
	}
	var actions ActionStore
	for _, entry := range entries {
		if entry == nil || entry.ActionCallID == "" {
			continue
		}
		if actions == nil {
			store, ok := x.Portfolio.(ActionStore)
			if !ok {
				return false, errors.New("control executor portfolio is not an action store; the external claim check needs the Run's calls")
			}
			actions = store
		}
		call, err := actions.GetActionCall(ctx, cc.Run.Scope, entry.ActionCallID)
		if errors.Is(err, ErrActionNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if call.RunID == cc.Run.ID && call.Status == ActionCallStatusSucceeded && strings.TrimSpace(call.ExternalOperationDigest) != "" {
			return true, nil
		}
	}
	return false, nil
}

// claimsExternalMutation: the text says an external effect happened.
func claimsExternalMutation(value string) bool {
	value = strings.ToLower(value)
	for _, phrase := range []string{
		"comment posted", "posted the comment", "comment submitted", "submitted the comment",
		"message sent", "sent the message", "email sent", "sent the email",
		"published the", "created the pull request", "transaction completed",
	} {
		if strings.Contains(value, phrase) {
			return true
		}
	}
	return false
}

// unverifiedApprovalClaim: the text says an approval request was submitted.
func unverifiedApprovalClaim(content string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(content), " "))
	for _, claim := range []string{
		"approval has been submitted",
		"approval was submitted",
		"approval request has been submitted",
		"approval request was submitted",
		"request has already been submitted",
		"request was already submitted",
		"awaiting approval",
		"pending approval",
	} {
		if strings.Contains(normalized, claim) {
			return true
		}
	}
	return false
}

// claimsPendingApproval: the text says something waits for an approval
// (moved from cortex cmd/atlas hosted_turn_executor.go).
func claimsPendingApproval(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || !strings.Contains(value, "approv") {
		return false
	}
	for _, pending := range []string{"await", "pending", "please approv", "need approval", "needs approval", "require approval", "requires approval", "waiting"} {
		if strings.Contains(value, pending) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Evidence grounding (§11.4.4)

// checkFinalEvidenceGrounding reviews an evidence-mode final against the Run's
// immutable snapshot. Findings (structural or the reviewer's) are an
// evidence_repair_required result listing them; an accepted review adds the
// normalized claims and the review to the output.
func checkFinalEvidenceGrounding(ctx context.Context, x *ControlExecutor, cc ControlContext, d *finalAnswerDraft) (*finalAnswerRejection, error) {
	if cc.Authority.finalMode() != FinalModeEvidence {
		return nil, nil
	}
	snapshot, err := evidenceSnapshotForGrounding(cc.Run.Context)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		// No snapshot to ground against: the schema already required the
		// report and the claim ledger.
		return nil, nil
	}
	claims, digest, findings := normalizeEvidenceClaims(d.Args.EvidenceClaims, snapshot)
	findings = append(findings, validateEvidenceGroundingDraft(d.Output, claims)...)
	if len(findings) > 0 {
		return evidenceRepairRejection(findings), nil
	}
	if x.Reviewer == nil {
		return nil, errors.New("evidence-backed completion requires a configured grounding reviewer")
	}
	request := EvidenceGroundingRequest{
		APIVersion: EvidenceGroundingAPIVersion, InvocationID: finalEvidenceInvocationID(cc),
		Scope: cc.Run.Scope, RunID: cc.Run.ID, AgentID: cc.Run.AssignedAgentID, Goal: cc.Run.Goal,
		Snapshot: *snapshot, Claims: append([]EvidenceClaim(nil), claims...), ClaimsDigest: digest,
		DraftSummary: strings.TrimSpace(d.Args.Answer), DraftOutput: cloneMap(d.Output),
		MaxOutputTokens: EvidenceGroundingReviewOutputLimit,
	}
	if cc.Step != nil {
		request.TurnID = cc.Step.ID
	}
	review, err := x.Reviewer.ReviewEvidenceGrounding(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("evidence grounding review: %w", err)
	}
	if err := validateEvidenceGroundingReview(review, request); err != nil {
		return nil, err
	}
	if !review.Accepted {
		findings := review.Findings
		if len(findings) == 0 {
			findings = []EvidenceGroundingFinding{{Status: EvidenceGroundingUncertain, Summary: "The review did not cover every claim."}}
		}
		return evidenceRepairRejection(findings), nil
	}
	d.Args.EvidenceClaims = claims
	d.Output[finalOutputEvidenceClaimsKey] = append([]EvidenceClaim(nil), claims...)
	d.Output[finalOutputEvidenceGroundingKey] = evidenceGroundingResultMap(review)
	return nil, nil
}

const finalOutputEvidenceGroundingKey = "evidenceGrounding"

// finalEvidenceInvocationID is stable across a replay of the same settle:
// the final tool entry, or the plain-text step.
func finalEvidenceInvocationID(cc ControlContext) string {
	if cc.Entry != nil && cc.Entry.ID != "" {
		return cc.Entry.ID
	}
	if cc.Step != nil {
		return cc.Step.ID
	}
	return cc.Run.ID
}

func evidenceRepairRejection(findings []EvidenceGroundingFinding) *finalAnswerRejection {
	lines := make([]string, 0, len(findings))
	for _, finding := range findings {
		line := finding.Summary
		if finding.ClaimID != "" {
			line = finding.ClaimID + ": " + line
		}
		if len(finding.EvidenceRefs) > 0 {
			line += " (" + strings.Join(finding.EvidenceRefs, ", ") + ")"
		}
		lines = append(lines, "- "+line)
	}
	encoded, _ := json.Marshal(findings)
	var projected []interface{}
	_ = json.Unmarshal(encoded, &projected)
	return &finalAnswerRejection{Code: finalEvidenceRepairCode,
		Message: "The report is not grounded in the evidence yet. Fix these and answer again:\n" + strings.Join(lines, "\n"),
		Extra:   map[string]interface{}{"findings": projected}}
}
