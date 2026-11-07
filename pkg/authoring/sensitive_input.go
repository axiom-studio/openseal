package authoring

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/validation"
)

// The sensitive-input check lives in pkg/validation.
var ErrSensitiveAuthoringInput = validation.ErrSensitiveInput

type (
	SensitiveInputKind    = validation.SensitiveInputKind
	SensitiveInputFinding = validation.SensitiveInputFinding
	SensitiveInputError   = validation.SensitiveInputError
)

const (
	SensitiveInputPassword   = validation.SensitiveInputPassword
	SensitiveInputAPIKey     = validation.SensitiveInputAPIKey
	SensitiveInputToken      = validation.SensitiveInputToken
	SensitiveInputPrivateKey = validation.SensitiveInputPrivateKey
)

var credentialVocabulary = regexp.MustCompile(`(?i)\b(password|passwd|passcode|api[\s_-]*key|secret[\s_-]*key|access[\s_-]*token|refresh[\s_-]*token|bearer[\s_-]*token|private[\s_-]*key|username|login[\s_-]*credential)\b`)

const hardenedLegacyAgentPrompt = "Follow the Agent's purpose, objectives, authority, and installed Skills. Use only authorized credential bindings; never request, store, or reveal raw credential values."

func ValidateAuthoringPrompt(prompt string) error {
	return validation.ValidateNoSensitiveInput(prompt)
}

// RedactSensitiveAuthoringPrompt is used by storage migrations to remove
// legacy prompt credentials.
func RedactSensitiveAuthoringPrompt(prompt string) (string, bool) {
	return validation.RedactSensitiveInput(prompt)
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
		var sensitive *SensitiveInputError
		if errors.As(ValidateAuthoringPrompt(typed), &sensitive) {
			for _, finding := range sensitive.Findings {
				kinds[finding.Kind] = struct{}{}
			}
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
