package authoring

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
)

// ErrSensitiveAuthoringInput is returned before an authoring prompt can be
// persisted or sent to a model. Credentials belong in an authorized binding,
// never in prompt text.
var ErrSensitiveAuthoringInput = errors.New("sensitive authoring input")

type SensitiveInputKind string

const (
	SensitiveInputPassword   SensitiveInputKind = "password"
	SensitiveInputAPIKey     SensitiveInputKind = "api_key"
	SensitiveInputToken      SensitiveInputKind = "token"
	SensitiveInputPrivateKey SensitiveInputKind = "private_key"
)

// SensitiveInputFinding deliberately carries classification only. It never
// contains the matched value, surrounding prompt text, or byte offsets.
type SensitiveInputFinding struct {
	Kind SensitiveInputKind `json:"kind"`
}

type SensitiveInputError struct {
	Findings []SensitiveInputFinding `json:"findings"`
}

func (e *SensitiveInputError) Error() string {
	return "authoring prompts cannot contain passwords, API keys, tokens, or private keys; configure an authorized credential binding instead"
}

func (e *SensitiveInputError) Unwrap() error { return ErrSensitiveAuthoringInput }

type sensitiveInputPattern struct {
	kind  SensitiveInputKind
	value int
	re    *regexp.Regexp
}

var sensitiveInputPatterns = []sensitiveInputPattern{
	{
		kind: SensitiveInputPassword, value: 2,
		re: regexp.MustCompile(`(?i)(?:^|[\s,{;])["']?(password|passwd|passcode|pwd)["']?\s*(?::|=|\bis\b)\s*["']?([^\s"',;}\]]{4,})`),
	},
	{
		kind: SensitiveInputAPIKey, value: 2,
		re: regexp.MustCompile(`(?i)(?:^|[\s,{;])["']?(api[\s_-]*key|secret[\s_-]*key|access[\s_-]*key|client[\s_-]*secret)["']?\s*(?::|=|\bis\b)\s*["']?([^\s"',;}\]]{4,})`),
	},
	{
		kind: SensitiveInputToken, value: 2,
		re: regexp.MustCompile(`(?i)(?:^|[\s,{;])["']?(access[\s_-]*token|refresh[\s_-]*token|auth[\s_-]*token|bearer[\s_-]*token|token)["']?\s*(?::|=|\bis\b)\s*["']?([^\s"',;}\]]{4,})`),
	},
	{
		kind: SensitiveInputToken, value: 1,
		re: regexp.MustCompile(`(?i)\bauthorization\s*:\s*bearer\s+([^\s,;]+)`),
	},
	{
		kind: SensitiveInputPassword, value: 1,
		re: regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^/\s:@]+:([^@\s/]+)@`),
	},
	{
		kind: SensitiveInputToken, value: 1,
		re: regexp.MustCompile(`\b((?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9_]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{16,}|AKIA[A-Z0-9]{16}))\b`),
	},
	{
		kind: SensitiveInputPrivateKey, value: 1,
		re: regexp.MustCompile(`(?s)(-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----)`),
	},
}

type sensitiveInputMatch struct {
	kind       SensitiveInputKind
	start, end int
}

var nonSecretPlaceholder = map[string]struct{}{
	"[redacted]": {}, "redacted": {}, "required": {}, "configured": {}, "configure": {}, "vault": {},
	"credential": {}, "credentials": {}, "secret": {}, "unknown": {}, "missing": {}, "none": {}, "true": {}, "false": {},
}

var credentialVocabulary = regexp.MustCompile(`(?i)\b(password|passwd|passcode|api[\s_-]*key|secret[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|bearer[\s_-]*token|private[\s_-]*key|username|login[\s_-]*credential)\b`)

const hardenedLegacyAgentPrompt = "Follow the Agent's purpose, objectives, authority, and installed Skills. Use only authorized credential bindings; never request, store, or reveal raw credential values."

func sensitiveInputMatches(value string) []sensitiveInputMatch {
	matches := make([]sensitiveInputMatch, 0)
	for _, pattern := range sensitiveInputPatterns {
		for _, indices := range pattern.re.FindAllStringSubmatchIndex(value, -1) {
			index := pattern.value * 2
			if index+1 >= len(indices) || indices[index] < 0 || indices[index+1] <= indices[index] {
				continue
			}
			candidate := strings.Trim(strings.TrimSpace(value[indices[index]:indices[index+1]]), `"'[]`)
			if _, placeholder := nonSecretPlaceholder[strings.ToLower(candidate)]; placeholder {
				continue
			}
			matches = append(matches, sensitiveInputMatch{kind: pattern.kind, start: indices[index], end: indices[index+1]})
		}
	}
	return matches
}

// ValidateAuthoringPrompt rejects likely raw credentials without returning or
// logging the sensitive value.
func ValidateAuthoringPrompt(prompt string) error {
	matches := sensitiveInputMatches(prompt)
	if len(matches) == 0 {
		return nil
	}
	kinds := make(map[SensitiveInputKind]struct{}, len(matches))
	for _, match := range matches {
		kinds[match.kind] = struct{}{}
	}
	ordered := make([]string, 0, len(kinds))
	for kind := range kinds {
		ordered = append(ordered, string(kind))
	}
	sort.Strings(ordered)
	findings := make([]SensitiveInputFinding, 0, len(ordered))
	for _, kind := range ordered {
		findings = append(findings, SensitiveInputFinding{Kind: SensitiveInputKind(kind)})
	}
	return &SensitiveInputError{Findings: findings}
}

// RedactSensitiveAuthoringPrompt is used by storage migrations to remove
// legacy prompt credentials. It returns only the sanitized text and whether a
// change occurred; callers never receive the extracted values.
func RedactSensitiveAuthoringPrompt(prompt string) (string, bool) {
	matches := sensitiveInputMatches(prompt)
	if len(matches) == 0 {
		return prompt, false
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start == matches[j].start {
			return matches[i].end > matches[j].end
		}
		return matches[i].start > matches[j].start
	})
	result, previousStart := prompt, len(prompt)
	for _, match := range matches {
		if match.end > previousStart {
			continue
		}
		result = result[:match.start] + "[REDACTED]" + result[match.end:]
		previousStart = match.start
	}
	return result, result != prompt
}

func validateRefinementSensitiveInput(value RefinementAnswerValue) error {
	if err := ValidateAuthoringPrompt(value.Text); err != nil {
		return err
	}
	for _, item := range value.Items {
		if err := ValidateAuthoringPrompt(item); err != nil {
			return err
		}
	}
	return nil
}

func sensitiveFindingsInValue(value interface{}, kinds map[SensitiveInputKind]struct{}) {
	switch typed := value.(type) {
	case string:
		for _, match := range sensitiveInputMatches(typed) {
			kinds[match.kind] = struct{}{}
		}
	case []interface{}:
		for _, child := range typed {
			sensitiveFindingsInValue(child, kinds)
		}
	case map[string]interface{}:
		for _, child := range typed {
			sensitiveFindingsInValue(child, kinds)
		}
	}
}

// ValidateWorkforceCandidateSensitiveInput prevents provider output from
// introducing model-visible credentials even when the source prompt was safe.
func ValidateWorkforceCandidateSensitiveInput(value *WorkforceCandidate) error {
	if value == nil {
		return nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var tree interface{}
	if err := json.Unmarshal(payload, &tree); err != nil {
		return err
	}
	kinds := make(map[SensitiveInputKind]struct{})
	sensitiveFindingsInValue(tree, kinds)
	if len(kinds) == 0 {
		return nil
	}
	ordered := make([]string, 0, len(kinds))
	for kind := range kinds {
		ordered = append(ordered, string(kind))
	}
	sort.Strings(ordered)
	findings := make([]SensitiveInputFinding, 0, len(ordered))
	for _, kind := range ordered {
		findings = append(findings, SensitiveInputFinding{Kind: SensitiveInputKind(kind)})
	}
	return &SensitiveInputError{Findings: findings}
}

func redactSensitiveStrings(value interface{}) (interface{}, bool) {
	switch typed := value.(type) {
	case string:
		redacted, changed := RedactSensitiveAuthoringPrompt(typed)
		return redacted, changed
	case []interface{}:
		changed := false
		for index, child := range typed {
			var childChanged bool
			typed[index], childChanged = redactSensitiveStrings(child)
			changed = changed || childChanged
		}
		return typed, changed
	case map[string]interface{}:
		changed := false
		for key, child := range typed {
			var childChanged bool
			typed[key], childChanged = redactSensitiveStrings(child)
			changed = changed || childChanged
		}
		return typed, changed
	default:
		return value, false
	}
}

func redactSensitiveStructuredValue(value interface{}) (bool, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	var tree interface{}
	if err := json.Unmarshal(payload, &tree); err != nil {
		return false, err
	}
	tree, changed := redactSensitiveStrings(tree)
	if !changed {
		return false, nil
	}
	payload, err = json.Marshal(tree)
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return false, err
	}
	return true, nil
}

// RedactSensitiveAgentDefinition removes legacy credentials from immutable
// Agent behavior. Callers must persist the returned refreshed digest together
// with the sanitized definition.
func RedactSensitiveAgentDefinition(value *agent.AgentDefinition) (bool, error) {
	if value == nil {
		return false, nil
	}
	changed, err := redactSensitiveStructuredValue(value)
	if err != nil || !changed {
		return changed, err
	}
	value.Digest = agent.DefinitionDigest(value)
	return true, nil
}

// HardenLegacySensitiveAgentDefinition handles provider paraphrases from
// ChangeSets already known to have contained a credential. Exact extraction is
// intentionally not attempted: replacing the affected behavior instruction is
// safer than retaining an unknown raw value or returning it to a caller.
func HardenLegacySensitiveAgentDefinition(value *agent.AgentDefinition) (bool, error) {
	if value == nil {
		return false, nil
	}
	changed, err := RedactSensitiveAgentDefinition(value)
	if err != nil {
		return false, err
	}
	if credentialVocabulary.MatchString(value.SystemPrompt) && value.SystemPrompt != hardenedLegacyAgentPrompt {
		value.SystemPrompt = hardenedLegacyAgentPrompt
		changed = true
	}
	if changed {
		value.Digest = agent.DefinitionDigest(value)
	}
	return changed, nil
}

// RedactSensitiveChangeSetPrompts removes legacy credentials from every
// user-authored or provider-produced text location that can later enter model
// context, refreshing every affected immutable content identity atomically.
func RedactSensitiveChangeSetPrompts(value *ChangeSet) bool {
	if value == nil {
		return false
	}
	changed := false
	sensitiveSource := strings.Contains(value.Prompt, "[REDACTED]")
	if prompt, redacted := RedactSensitiveAuthoringPrompt(value.Prompt); redacted {
		value.Prompt = prompt
		value.PromptDigest = digestString(prompt)
		changed = true
		sensitiveSource = true
	}
	if value.Generation != nil {
		sensitiveSource = sensitiveSource || strings.Contains(value.Generation.Request.Prompt, "[REDACTED]")
		if prompt, redacted := RedactSensitiveAuthoringPrompt(value.Generation.Request.Prompt); redacted {
			value.Generation.Request.Prompt = prompt
			changed = true
			sensitiveSource = true
		}
	}
	for index := range value.Refinement.Answers {
		answer := &value.Refinement.Answers[index].Value
		sensitiveSource = sensitiveSource || strings.Contains(answer.Text, "[REDACTED]")
		if text, redacted := RedactSensitiveAuthoringPrompt(answer.Text); redacted {
			answer.Text = text
			changed = true
			sensitiveSource = true
		}
		for item := range answer.Items {
			sensitiveSource = sensitiveSource || strings.Contains(answer.Items[item], "[REDACTED]")
			if text, redacted := RedactSensitiveAuthoringPrompt(answer.Items[item]); redacted {
				answer.Items[item] = text
				changed = true
				sensitiveSource = true
			}
		}
	}
	oldCandidateDigest := value.CandidateDigest
	candidateChanged, err := redactSensitiveStructuredValue(&value.Result.Candidate)
	if err == nil && sensitiveSource {
		for _, definition := range value.Result.Candidate.Agents {
			hardened, hardenErr := HardenLegacySensitiveAgentDefinition(definition)
			if hardenErr != nil {
				err = hardenErr
				break
			}
			candidateChanged = candidateChanged || hardened
		}
	}
	if err == nil && candidateChanged {
		for _, definition := range value.Result.Candidate.Agents {
			if definition != nil {
				definition.Digest = agent.DefinitionDigest(definition)
			}
		}
		if digest, digestErr := digestJSON(value.Result.Candidate); digestErr == nil {
			value.CandidateDigest = digest
			for index := range value.Evaluations {
				if value.Evaluations[index].CandidateDigest == oldCandidateDigest {
					value.Evaluations[index].CandidateDigest = digest
				}
			}
			if value.ApplyReceipt != nil && value.ApplyReceipt.CandidateDigest == oldCandidateDigest {
				value.ApplyReceipt.CandidateDigest = digest
			}
		}
		changed = true
	}
	return changed
}
