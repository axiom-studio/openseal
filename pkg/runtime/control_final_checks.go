package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/conversation"
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

// Completion check error codes (stable; the model sees them).
const (
	finalUnsupportedURLCode     = "unsupported_url"
	finalUnevidencedClaimCode   = "unevidenced_external_claim"
	finalApprovalClaimCode      = "approval_claim"
	finalUnverifiedApprovalCode = "unverified_approval_claim"
)

// A backslash terminates a URL as well as ordinary whitespace: JSON-encoded
// text (the visual) carries a real newline as the two bytes `\n`, and
// accepting the backslash would append the next line to the URL.
var hostedTurnURLPattern = regexp.MustCompile(`https?://[^\s<>"'\\]+`)

func checkFinalAnswerCompletion(ctx context.Context, x *ControlExecutor, cc agentrun.ControlContext, d *finalAnswerDraft) (*finalAnswerRejection, error) {
	content := d.checkedText()
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
	if claimsPendingApproval(content) {
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
			Failure: &agentrun.LoopFailure{Kind: conversation.RunFailureKindExecution, Code: finalUnverifiedApprovalCode,
				Message: "The agent said an approval was pending when none was, so the run stopped."}}, nil
	}
	return nil, nil
}

// checkedText is every model-written text of the final: the answer (the
// persisted summary) and, when present, the report.
func (d *finalAnswerDraft) checkedText() string {
	answer, report := strings.TrimSpace(d.Args.Answer), strings.TrimSpace(d.Args.Report)
	switch {
	case report == "":
		return answer
	case answer == "":
		return report
	}
	return answer + "\n" + report
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
// contents. JSON (Run context, tool arguments, json parts) contributes its
// decoded string leaves, never its encoding: json.Marshal escapes '&' as
// \u0026, and the answer cites the URL as read. Model-authored text is not evidence: assistant entries, the final
// tool's own arguments and results, and final_rejection notes (which quote the
// refused URL) are left out.
func (x *ControlExecutor) finalEvidenceCorpus(ctx context.Context, cc agentrun.ControlContext) (string, error) {
	var b strings.Builder
	b.WriteString(cc.Run.Goal)
	b.WriteByte('\n')
	writeStringLeaves(&b, cc.Run.Context)
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
		case models.SessionEntryUser:
			if entry.User != nil {
				writeContentParts(&b, entry.User.Content)
			}
		case models.SessionEntryTool:
			if entry.Tool == nil || entry.Tool.Name == string(models.ControlToolFinalAnswer) {
				continue
			}
			writeJSONText(&b, entry.Tool.Arguments)
			if entry.Tool.Result != nil {
				writeContentParts(&b, entry.Tool.Result.Content)
			}
		case models.SessionEntryNote:
			if entry.Note != nil && entry.Note.Kind != models.NoteFinalRejection {
				b.WriteString(entry.Note.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String(), nil
}

func writeContentParts(b *strings.Builder, parts []models.ContentPart) {
	for _, part := range parts {
		writeJSONText(b, part.Value)
		for _, text := range []string{part.Text, part.URI, part.Path, part.Note} {
			if text != "" {
				b.WriteString(text)
				b.WriteByte('\n')
			}
		}
	}
}

// writeJSONText writes the decoded string leaves of a JSON document; bytes
// that are not JSON are written as they are.
func writeJSONText(b *strings.Builder, raw []byte) {
	if len(raw) == 0 {
		return
	}
	var value interface{}
	if err := json.Unmarshal(raw, &value); err != nil {
		b.Write(raw)
		b.WriteByte('\n')
		return
	}
	writeStringLeaves(b, value)
}

// writeStringLeaves writes every string (map keys included) of a decoded
// JSON value, one per line. Typed values are normalized through JSON first.
func writeStringLeaves(b *strings.Builder, value interface{}) {
	switch v := value.(type) {
	case nil, bool, float64, json.Number:
	case string:
		if v != "" {
			b.WriteString(v)
			b.WriteByte('\n')
		}
	case []interface{}:
		for _, item := range v {
			writeStringLeaves(b, item)
		}
	case map[string]interface{}:
		for key, item := range v {
			b.WriteString(key)
			b.WriteByte('\n')
			writeStringLeaves(b, item)
		}
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return
		}
		var decoded interface{}
		if json.Unmarshal(encoded, &decoded) == nil {
			writeStringLeaves(b, decoded)
		}
	}
}

// controlSessionPath is the step's session path: cc.Path, else loaded.
func (x *ControlExecutor) controlSessionPath(ctx context.Context, cc agentrun.ControlContext) (*models.AgentSessionPath, error) {
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

// runHasExternalReceipt: some succeeded ActionCall of this Run carries an
// external operation receipt.
func (x *ControlExecutor) runHasExternalReceipt(ctx context.Context, cc agentrun.ControlContext) (bool, error) {
	actions, ok := x.Portfolio.(ActionCallLister)
	if !ok {
		return false, errors.New("control executor portfolio is not an action store; the external claim check needs the Run's calls")
	}
	for offset := 0; ; offset += ActionCallPageSize {
		page, err := actions.ListActionCalls(ctx, models.ActionFilter{
			Scope: cc.Run.Scope, RunID: cc.Run.ID, Status: []models.ActionCallStatus{models.ActionCallStatusSucceeded},
			Limit: ActionCallPageSize, Offset: offset,
		})
		if err != nil {
			return false, fmt.Errorf("load the run's action calls: %w", err)
		}
		for _, call := range page {
			if call != nil && call.RunID == cc.Run.ID && call.Status == models.ActionCallStatusSucceeded &&
				strings.TrimSpace(call.ExternalOperationDigest) != "" {
				return true, nil
			}
		}
		if len(page) < ActionCallPageSize {
			return false, nil
		}
	}
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

// claimsPendingApproval: the text says an approval request was submitted or
// that something waits for an approval. Only whole phrases count, and a phrase
// negated within the three words before it ("there is no approval pending")
// does not: approval words near "pending" or "waiting" elsewhere, such as a
// setup form that is pending and asks to approve mail scopes, are no claim.
func claimsPendingApproval(content string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(content, "’", "'"))
	words := strings.FieldsFunc(normalized, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '\''
	})
	for i := range words {
		for _, claim := range pendingApprovalClaims {
			if !hasWordsAt(words, i, claim) || negatedBefore(words, i) {
				continue
			}
			return true
		}
	}
	return false
}

var pendingApprovalClaims = [][]string{
	{"approval", "has", "been", "submitted"},
	{"approval", "was", "submitted"},
	{"approval", "request", "has", "been", "submitted"},
	{"approval", "request", "was", "submitted"},
	{"request", "has", "already", "been", "submitted"},
	{"request", "was", "already", "submitted"},
	{"awaiting", "approval"},
	{"awaiting", "your", "approval"},
	{"pending", "approval"},
	{"pending", "your", "approval"},
	{"approval", "is", "pending"},
	{"approval", "pending"},
	{"waiting", "for", "approval"},
	{"waiting", "for", "your", "approval"},
	{"waiting", "on", "approval"},
	{"waiting", "on", "your", "approval"},
	{"needs", "approval"},
	{"needs", "your", "approval"},
	{"requires", "approval"},
	{"requires", "your", "approval"},
}

func hasWordsAt(words []string, i int, phrase []string) bool {
	if i+len(phrase) > len(words) {
		return false
	}
	for j, word := range phrase {
		if words[i+j] != word {
			return false
		}
	}
	return true
}

func negatedBefore(words []string, i int) bool {
	for j := max(0, i-3); j < i; j++ {
		switch words[j] {
		case "no", "not", "nothing", "never", "without", "isn't", "aren't", "wasn't", "nor":
			return true
		}
	}
	return false
}

// ActionCallPageSize pages a Run's failed action calls.
const ActionCallPageSize = 200

type ActionCallLister interface {
	ListActionCalls(ctx context.Context, filter models.ActionFilter) ([]*models.ActionCall, error)
}
