package validation

import (
	"errors"
	"regexp"
	"sort"
	"strings"
)

// ErrSensitiveInput is returned before text that carries a likely raw
// credential can be persisted or sent to a model. Credentials belong in an
// authorized binding, never in prompt or profile text.
var ErrSensitiveInput = errors.New("sensitive input")

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
	return "text cannot contain passwords, API keys, tokens, or private keys; configure an authorized credential binding instead"
}

func (e *SensitiveInputError) Unwrap() error { return ErrSensitiveInput }

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
	"[redacted]": {}, "redacted": {}, "required": {}, "configured": {}, "configure": {},
	"credential": {}, "credentials": {}, "secret": {}, "unknown": {}, "missing": {}, "none": {}, "true": {}, "false": {},
}

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

// ValidateNoSensitiveInput rejects likely raw credentials without returning or
// logging the sensitive value.
func ValidateNoSensitiveInput(value string) error {
	matches := sensitiveInputMatches(value)
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

// RedactSensitiveInput replaces likely raw credential values with
// "[REDACTED]" and reports whether anything changed. Callers never receive the
// extracted values.
func RedactSensitiveInput(value string) (string, bool) {
	matches := sensitiveInputMatches(value)
	if len(matches) == 0 {
		return value, false
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].start == matches[j].start {
			return matches[i].end > matches[j].end
		}
		return matches[i].start > matches[j].start
	})
	result, previousStart := value, len(value)
	for _, match := range matches {
		if match.end > previousStart {
			continue
		}
		result = result[:match.start] + "[REDACTED]" + result[match.end:]
		previousStart = match.start
	}
	return result, result != value
}
