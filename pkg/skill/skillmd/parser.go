package skillmd

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

const maxSkillSize = 50 * 1024

// suspiciousPatterns are basic heuristics for detecting potentially malicious content.
var suspiciousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\{\{`),        // template injection markers
	regexp.MustCompile(`(?i)<script`),     // script injection
	regexp.MustCompile(`(?i)\beval\(`),    // eval calls
	regexp.MustCompile(`(?i)\bexec\(`),    // exec calls
	regexp.MustCompile(`(?i)javascript:`), // javascript: URLs
}

// ParseResult wraps a parsed skill with optional warnings.
type ParseResult struct {
	Skill    *ParsedSkill
	Warnings []string
}

// ParseSkillMDWithWarnings parses SKILL.md content and returns the result with any warnings.
// Warnings include size truncation and suspicious pattern detection.
func ParseSkillMDWithWarnings(content []byte) (*ParseResult, error) {
	var warnings []string

	// Size check: truncate to 50KB if larger
	if len(content) > maxSkillSize {
		warnings = append(warnings, fmt.Sprintf("SKILL.md truncated from %d bytes to %d bytes (max %dKB)", len(content), maxSkillSize, maxSkillSize/1024))
		content = content[:maxSkillSize]
	}

	// Suspicious pattern scan
	for _, pat := range suspiciousPatterns {
		if loc := pat.FindIndex(content); loc != nil {
			// Calculate approximate line number
			lineNum := 1
			for _, b := range content[:loc[0]] {
				if b == '\n' {
					lineNum++
				}
			}
			warnings = append(warnings, fmt.Sprintf("suspicious pattern %q detected at line %d", pat.String(), lineNum))
		}
	}

	skill, err := ParseSkillMD(content)
	if err != nil {
		return nil, err
	}
	skill.Warnings = warnings

	return &ParseResult{
		Skill:    skill,
		Warnings: warnings,
	}, nil
}

type frontmatterRaw struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Version     string      `yaml:"version"`
	Metadata    interface{} `yaml:"metadata"`
}

func ParseSkillMD(content []byte) (*ParsedSkill, error) {
	if len(content) == 0 {
		return nil, fmt.Errorf("empty content")
	}

	fm, body, err := extractFrontmatter(content)
	if err != nil {
		return nil, err
	}

	var raw frontmatterRaw
	if err := yaml.Unmarshal(fm, &raw); err != nil {
		return nil, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}

	if raw.Name == "" {
		return nil, fmt.Errorf("missing required field: name")
	}
	if raw.Description == "" {
		return nil, fmt.Errorf("missing required field: description")
	}
	if !slugPattern.MatchString(raw.Name) {
		return nil, fmt.Errorf("invalid name %q: must match pattern ^[a-z0-9][a-z0-9-]*$", raw.Name)
	}

	meta, err := parseMetadata(raw.Metadata)
	if err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return &ParsedSkill{
		Name:        raw.Name,
		Description: raw.Description,
		Version:     raw.Version,
		Metadata:    meta,
		Body:        body,
		RawContent:  content,
		SizeBytes:   len(content),
	}, nil
}

func ValidateSkillMD(parsed *ParsedSkill) []ValidationError {
	var errors []ValidationError

	if parsed.Name == "" {
		errors = append(errors, ValidationError{
			Field:   "name",
			Message: "name is required",
		})
	} else if !slugPattern.MatchString(parsed.Name) {
		errors = append(errors, ValidationError{
			Field:   "name",
			Message: fmt.Sprintf("name %q does not match slug pattern ^[a-z0-9][a-z0-9-]*$", parsed.Name),
		})
	}

	if parsed.Description == "" {
		errors = append(errors, ValidationError{
			Field:   "description",
			Message: "description is required",
		})
	}

	if parsed.SizeBytes > maxSkillSize {
		errors = append(errors, ValidationError{
			Field:   "content",
			Message: fmt.Sprintf("SKILL.md is large (%d bytes, >50KB); consider splitting", parsed.SizeBytes),
		})
	}

	// Scan raw content for suspicious patterns
	if len(parsed.RawContent) > 0 {
		for _, pat := range suspiciousPatterns {
			if loc := pat.FindIndex(parsed.RawContent); loc != nil {
				lineNum := 1
				for _, b := range parsed.RawContent[:loc[0]] {
					if b == '\n' {
						lineNum++
					}
				}
				errors = append(errors, ValidationError{
					Field:      "content",
					Message:    fmt.Sprintf("suspicious pattern detected at line %d", lineNum),
					LineNumber: lineNum,
				})
			}
		}
	}

	return errors
}

func extractFrontmatter(content []byte) (frontmatter []byte, body string, err error) {
	text := string(content)

	if !strings.HasPrefix(text, "---") {
		return nil, "", fmt.Errorf("missing frontmatter delimiters: content must start with ---")
	}

	rest := text[3:]
	endIdx := strings.Index(rest, "\n---")
	delimLen := 4
	if endIdx == -1 {
		endIdx = strings.Index(rest, "---")
		delimLen = 3
		if endIdx == -1 {
			return nil, "", fmt.Errorf("missing closing frontmatter delimiter")
		}
	}

	fm := rest[:endIdx]
	bodyStart := endIdx + delimLen
	if bodyStart < len(rest) && rest[bodyStart] == '\n' {
		bodyStart++
	}
	if bodyStart < len(rest) {
		body = rest[bodyStart:]
	}

	return []byte(fm), body, nil
}

func parseMetadata(raw interface{}) (SkillMetadata, error) {
	if raw == nil {
		return SkillMetadata{}, nil
	}

	var jsonStr string

	switch v := raw.(type) {
	case string:
		jsonStr = v
	case map[string]interface{}:
		if openclaw, ok := v["openclaw"]; ok {
			return parseOpenClawJSON(openclaw)
		}
		data, err := json.Marshal(v)
		if err != nil {
			return SkillMetadata{}, fmt.Errorf("failed to marshal metadata: %w", err)
		}
		jsonStr = string(data)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return SkillMetadata{}, fmt.Errorf("failed to marshal metadata: %w", err)
		}
		jsonStr = string(data)
	}

	jsonStr = strings.TrimSpace(jsonStr)
	if jsonStr == "" {
		return SkillMetadata{}, nil
	}

	var wrapper map[string]interface{}
	if err := json.Unmarshal([]byte(jsonStr), &wrapper); err != nil {
		return SkillMetadata{}, fmt.Errorf("metadata is not valid JSON: %w", err)
	}

	if openclaw, ok := wrapper["openclaw"]; ok {
		return parseOpenClawJSON(openclaw)
	}

	return SkillMetadata{}, nil
}

func parseOpenClawJSON(raw interface{}) (SkillMetadata, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return SkillMetadata{}, fmt.Errorf("failed to marshal openclaw metadata: %w", err)
	}
	var oc openclawFlat
	if err := json.Unmarshal(data, &oc); err != nil {
		return SkillMetadata{}, fmt.Errorf("failed to unmarshal openclaw metadata: %w", err)
	}
	return SkillMetadata{
		RequiresEnv:    oc.RequiresEnv,
		RequiresBins:   oc.RequiresBins,
		RequiresConfig: oc.RequiresConfig,
		RequiresAnyBin: oc.RequiresAnyBin,
		PrimaryEnv:     oc.PrimaryEnv,
		Emoji:          oc.Emoji,
		Homepage:       oc.Homepage,
		OS:             oc.OS,
		Always:         oc.Always,
		Install:        oc.Install,
		OpenClaw: &OpenClawMetadata{
			Requires: RequiresConfig{
				Env:     oc.RequiresEnv,
				Bins:    oc.RequiresBins,
				AnyBins: oc.RequiresAnyBin,
				Config:  oc.RequiresConfig,
			},
		},
	}, nil
}

type openclawFlat struct {
	RequiresEnv    []string      `json:"requires_env"`
	RequiresBins   []string      `json:"requires_bins"`
	RequiresConfig []string      `json:"requires_config"`
	RequiresAnyBin []string      `json:"requires_any_bin"`
	PrimaryEnv     string        `json:"primary_env"`
	Emoji          string        `json:"emoji"`
	Homepage       string        `json:"homepage"`
	OS             []string      `json:"os"`
	Always         bool          `json:"always"`
	Install        []InstallSpec `json:"install"`
}
