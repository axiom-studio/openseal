package runtime

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ApprovalPresentation is the reviewer-facing description of exactly what an
// approval decides: the same copy the web approval card shows, computed once
// by the kernel so every connector renders it verbatim. It is built only from
// model-written prose (review summary, action intent), the host-written policy
// reason and display-safe facts. Arguments, element references and identifiers
// are never shown, and redacted prose is dropped rather than echoed.
type ApprovalPresentation struct {
	// Title names the action and, once decided, its outcome.
	Title string `json:"title"`
	// Lines are the supporting sentences, in display order.
	Lines []string `json:"lines,omitempty"`
}

// Text is the plain-text form: the title followed by each line.
func (p ApprovalPresentation) Text() string {
	return strings.Join(append([]string{p.Title}, p.Lines...), "\n")
}

// Payload is the portable JSON form connectors receive as "presentation".
func (p ApprovalPresentation) Payload() map[string]interface{} {
	lines := make([]interface{}, 0, len(p.Lines))
	for _, line := range p.Lines {
		lines = append(lines, line)
	}
	return map[string]interface{}{"title": p.Title, "lines": lines}
}

const approvalPayAction = "live-browser-pay"

var (
	approvalGenericSummary = regexp.MustCompile(`^Proposed \S+$`)
	approvalProgressPhrase = regexp.MustCompile(`^(Loaded \d+ tools or instruction sets for the next step\.|Reading your request and available context\.|Working on your request\.)$`)
	approvalCardEnding     = regexp.MustCompile(`(?i)\bending(?: in)? (\d{4})\b`)
	approvalLast4          = regexp.MustCompile(`^\d{4}$`)
	approvalAmount         = regexp.MustCompile(`^\d+(\.\d{1,4})?$`)
	approvalCurrency       = regexp.MustCompile(`^[A-Za-z]{3}$`)
	approvalIdentifierGap  = regexp.MustCompile(`[._\s-]+`)
)

var approvalRiskLabels = map[string]string{
	"read": "Reads only", "write": "Makes changes", "external": "Affects outside services",
	"production": "Affects production", "destructive": "Destructive change",
}

var approvalFallbackTitles = map[string]string{
	"external":    "Your agent wants to act outside this chat",
	"production":  "Your agent wants to change a production system",
	"destructive": "Your agent wants to make a destructive change",
}

var approvalCurrencySymbols = map[string]string{"INR": "₹", "USD": "$", "EUR": "€", "GBP": "£", "JPY": "¥", "AUD": "A$", "CAD": "CA$", "SGD": "S$"}

// PresentApproval describes an approval for a reviewer. A pending approval
// asks; a decided one states its outcome while keeping what was decided.
func PresentApproval(approval *ApprovalCheckpoint) ApprovalPresentation {
	if approval == nil {
		return ApprovalPresentation{Title: "Your agent wants to make a change"}
	}
	proposed := approvalRecord(approval.ProposedAction)
	review := approvalRecord(proposed["reviewContext"])
	args := approvalRecord(proposed["arguments"])
	skillID, _ := proposed["skillId"].(string)
	action, _ := proposed["action"].(string)
	alwaysAsks := proposed["review"] == "always"
	risk := string(approval.Risk)
	if action == approvalPayAction {
		return presentPayment(approval, proposed, review, args, alwaysAsks)
	}
	public := func(value interface{}) string {
		text := approvalProse(value, 300)
		if text == "" || approvalGenericSummary.MatchString(text) || approvalProgressPhrase.MatchString(text) || approvalNamesIdentifier(text, skillID, action) {
			return ""
		}
		return text
	}
	base := ""
	for _, value := range []interface{}{review["summary"], args["intent"], approval.Summary} {
		if base = strings.TrimRight(public(value), ". \t\n"); base != "" {
			break
		}
	}
	site := approvalHost(args["url"])
	if site == "" {
		site = approvalHost(approvalRecord(proposed["externalOperation"])["resource"])
	}
	title := base
	switch {
	case base != "" && site != "" && !approvalMentions(base, site):
		title = base + " on " + site
	case base == "":
		title = approvalFallbackTitles[risk]
		if title == "" {
			title = "Your agent wants to make a change"
		}
	}
	target := approvalProse(review["target"], 200)
	details := []string{}
	if alwaysAsks && approval.Status == ApprovalStatusPending {
		details = append(details, "Always asks, even with approvals skipped")
	}
	if label := approvalRiskLabels[risk]; label != "" {
		details = append(details, label)
	}
	if target != "" && !approvalMentions(title, target) {
		details = append(details, target)
	}
	result := ApprovalPresentation{Title: title}
	if outcome := approvalOutcome(approval.Status); outcome != "" {
		result.Title = outcome + ": " + title
	}
	if len(details) > 0 {
		result.Lines = append(result.Lines, strings.Join(details, " · "))
	}
	return approvalWithDecisionReason(approval, result)
}

func presentPayment(approval *ApprovalCheckpoint, proposed, review, args map[string]interface{}, alwaysAsks bool) ApprovalPresentation {
	amount := ""
	raw := ""
	switch value := args["amount"].(type) {
	case string:
		raw = strings.TrimSpace(value)
	case float64:
		raw = strconv.FormatFloat(value, 'f', -1, 64)
	}
	currency, _ := args["currency"].(string)
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if !approvalCurrency.MatchString(currency) {
		currency = ""
	}
	if approvalAmount.MatchString(raw) {
		switch symbol := approvalCurrencySymbols[currency]; {
		case symbol != "":
			amount = symbol + raw
		case currency != "":
			amount = currency + " " + raw
		default:
			amount = raw
		}
	}
	merchant := approvalHost(args["merchant"])
	if merchant == "" {
		merchant = approvalHost(review["merchant"])
	}
	if merchant == "" {
		merchant = approvalHost(args["url"])
	}
	display := approvalRecord(proposed["displaySummary"])
	last4 := ""
	for _, value := range []interface{}{review["cardLast4"], display["cardLast4"], proposed["cardLast4"], args["cardLast4"], proposed["displaySummary"], review["summary"], approval.PolicyReason, approval.Summary} {
		if last4 = approvalLast4Of(value); last4 != "" {
			break
		}
	}
	shownMerchant, shownAmount, shownCard := merchant, amount, "No saved card entered"
	if shownMerchant == "" {
		shownMerchant = "Unknown site"
	}
	if shownAmount == "" {
		shownAmount = "Amount not shown"
	}
	if last4 != "" {
		shownCard = "Card ending " + last4
	}
	if outcome := approvalOutcome(approval.Status); outcome != "" {
		// "Payment approved · ₹312.00 to amazon.in · card ending 1001"
		facts := []string{"Payment " + strings.ToLower(outcome)}
		switch {
		case amount != "" && merchant != "":
			facts = append(facts, amount+" to "+merchant)
		case amount != "":
			facts = append(facts, amount)
		case merchant != "":
			facts = append(facts, "to "+merchant)
		}
		if last4 != "" {
			facts = append(facts, "card ending "+last4)
		}
		return approvalWithDecisionReason(approval, ApprovalPresentation{Title: strings.Join(facts, " · ")})
	}
	result := ApprovalPresentation{Title: "Payment — needs your approval"}
	// The host-written policy reason is the sentence the user consents to.
	if reason := approvalProse(approval.PolicyReason, 300); reason != "" {
		result.Lines = append(result.Lines, reason)
	}
	result.Lines = append(result.Lines, "Merchant: "+shownMerchant+" · Amount: "+shownAmount+" · Card: "+shownCard)
	if alwaysAsks {
		result.Lines = append(result.Lines, "Always asks, even with approvals skipped")
	}
	return result
}

func approvalOutcome(status ApprovalStatus) string {
	switch status {
	case ApprovalStatusApproved:
		return "Approved"
	case ApprovalStatusRejected:
		return "Declined"
	case ApprovalStatusExpired:
		return "Expired"
	case ApprovalStatusCanceled:
		return "Canceled"
	default:
		return ""
	}
}

func approvalWithDecisionReason(approval *ApprovalCheckpoint, result ApprovalPresentation) ApprovalPresentation {
	if approval.Status != ApprovalStatusApproved && approval.Status != ApprovalStatusPending {
		if reason := approvalProse(approval.DecisionReason, 300); reason != "" {
			result.Lines = append(result.Lines, reason)
		}
	}
	return result
}

func approvalRecord(value interface{}) map[string]interface{} {
	record, _ := value.(map[string]interface{})
	if record == nil {
		return map[string]interface{}{}
	}
	return record
}

func approvalProse(value interface{}, limit int) string {
	text, _ := value.(string)
	text = strings.Join(strings.Fields(text), " ")
	if text == "" || utf8.RuneCountInString(text) > limit || strings.Contains(strings.ToUpper(text), "[REDACTED]") {
		return ""
	}
	return text
}

func approvalHost(value interface{}) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	lower := strings.ToLower(text)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return ""
	}
	parsed, err := url.Parse(text)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(parsed.Hostname(), "www.")
}

func approvalLast4Of(value interface{}) string {
	text, _ := value.(string)
	text = strings.TrimSpace(text)
	if approvalLast4.MatchString(text) {
		return text
	}
	if match := approvalCardEnding.FindStringSubmatch(text); match != nil {
		return match[1]
	}
	return ""
}

func approvalMentions(text, value string) bool {
	return value != "" && strings.Contains(strings.ToLower(text), strings.ToLower(value))
}

// Public prose never humanizes a tool identifier.
func approvalNamesIdentifier(text string, identifiers ...string) bool {
	normalized := approvalIdentifierGap.ReplaceAllString(strings.ToLower(text), " ")
	for _, identifier := range identifiers {
		name := approvalIdentifierGap.ReplaceAllString(strings.ToLower(identifier), " ")
		if name != "" && (normalized == name || strings.ContainsAny(identifier, "_-.") && strings.Contains(normalized, name)) {
			return true
		}
	}
	return false
}
