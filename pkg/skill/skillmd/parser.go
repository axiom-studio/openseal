package skillmd

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

const warningSkillSize = 50 * 1024

var suspiciousPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\{\{`),
	regexp.MustCompile(`(?i)<script`),
	regexp.MustCompile(`(?i)javascript:`),
}

type ParseResult struct {
	Skill    *ParsedSkill
	Warnings []string
}

func ParseSkillMDWithWarnings(content []byte) (*ParseResult, error) {
	return parseSkillMDWithWarnings(content, "")
}

// ParseSkillMDWithCanonicalName preserves the source-authored display name
// while allowing a trusted registry adapter to supply its verified slug as
// the portable capability identity. Standalone parsing remains strict.
func ParseSkillMDWithCanonicalName(content []byte, canonicalName string) (*ParseResult, error) {
	return parseSkillMDWithWarnings(content, canonicalName)
}

func parseSkillMDWithWarnings(content []byte, canonicalName string) (*ParseResult, error) {
	skill, err := parseSkillMD(content, canonicalName)
	if err != nil {
		return nil, err
	}
	warnings := scanWarnings(content)
	skill.Warnings = append([]string(nil), warnings...)
	return &ParseResult{Skill: skill, Warnings: warnings}, nil
}

func ParseSkillMD(content []byte) (*ParsedSkill, error) {
	return parseSkillMD(content, "")
}

func parseSkillMD(content []byte, canonicalName string) (*ParsedSkill, error) {
	if len(content) == 0 {
		return nil, fmt.Errorf("empty content")
	}
	fm, body, err := extractFrontmatter(content)
	if err != nil {
		return nil, err
	}
	frontmatter := make(map[string]interface{})
	if err := yaml.Unmarshal(fm, &frontmatter); err != nil {
		return nil, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	name := stringValue(frontmatter, "name")
	description := stringValue(frontmatter, "description")
	identity := name
	if err := validateName(name); err != nil && strings.TrimSpace(canonicalName) != "" {
		identity = strings.TrimSpace(canonicalName)
	}
	if err := validateIdentity(identity, description); err != nil {
		return nil, err
	}
	metadata, err := parseMetadata(frontmatter["metadata"])
	if err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}
	version := stringValue(frontmatter, "version")
	if version == "" {
		version = metadataString(metadata.Raw, "version")
	}
	homepage := stringValue(frontmatter, "homepage")
	if homepage == "" {
		homepage = metadata.Homepage
	}
	dispatch, err := parseCommandDispatch(frontmatter)
	if err != nil {
		return nil, err
	}
	return &ParsedSkill{
		Name: name, CanonicalName: identity, Description: description, License: stringValue(frontmatter, "license"),
		Compatibility: stringValue(frontmatter, "compatibility"), AllowedTools: strings.Fields(stringValue(frontmatter, "allowed-tools")),
		Version: version, Homepage: homepage, Metadata: metadata, Frontmatter: cloneMap(frontmatter),
		Invocation: InvocationPolicy{
			UserInvocable:          boolValue(frontmatter, "user-invocable", true),
			DisableModelInvocation: boolValue(frontmatter, "disable-model-invocation", false),
		},
		CommandDispatch: dispatch, Body: body, RawContent: append([]byte(nil), content...), SizeBytes: len(content),
	}, nil
}

func ValidateSkillMD(parsed *ParsedSkill) []ValidationError {
	if parsed == nil {
		return []ValidationError{{Field: "skill", Message: "skill is required"}}
	}
	var result []ValidationError
	identity := parsed.CanonicalName
	if identity == "" {
		identity = parsed.Name
	}
	if err := validateName(identity); err != nil {
		result = append(result, ValidationError{Field: "name", Message: err.Error()})
	}
	if parsed.Description == "" || len(parsed.Description) > 1024 {
		result = append(result, ValidationError{Field: "description", Message: "description must contain 1 to 1024 characters"})
	}
	if parsed.Compatibility != "" && len(parsed.Compatibility) > 500 {
		result = append(result, ValidationError{Field: "compatibility", Message: "compatibility must not exceed 500 characters"})
	}
	for _, warning := range scanWarnings(parsed.RawContent) {
		result = append(result, ValidationError{Field: "content", Message: warning})
	}
	return result
}

func extractFrontmatter(content []byte) ([]byte, string, error) {
	text := strings.TrimPrefix(string(content), "\ufeff")
	lines := strings.SplitAfter(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, "", fmt.Errorf("missing frontmatter delimiters: content must start with ---")
	}
	offset := len(lines[0])
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			frontmatter := text[offset : offset+sumLengths(lines[1:i])]
			bodyOffset := offset + sumLengths(lines[1:i+1])
			return []byte(frontmatter), text[bodyOffset:], nil
		}
	}
	return nil, "", fmt.Errorf("missing closing frontmatter delimiter")
}

func parseMetadata(raw interface{}) (SkillMetadata, error) {
	if raw == nil {
		return SkillMetadata{}, nil
	}
	value := raw
	if encoded, ok := raw.(string); ok {
		var decoded map[string]interface{}
		if err := yaml.Unmarshal([]byte(encoded), &decoded); err != nil {
			return SkillMetadata{}, fmt.Errorf("metadata is not valid YAML/JSON5-compatible data: %w", err)
		}
		value = decoded
	}
	root, ok := stringMap(value)
	if !ok {
		return SkillMetadata{}, fmt.Errorf("metadata must be an object")
	}
	openRaw, exists := root["openclaw"]
	if !exists {
		openRaw = root["clawdbot"]
	}
	metadata := SkillMetadata{Raw: cloneMap(root)}
	if openRaw == nil {
		return metadata, nil
	}
	open, ok := stringMap(openRaw)
	if !ok {
		return SkillMetadata{}, fmt.Errorf("metadata.openclaw must be an object")
	}
	requires, _ := stringMap(open["requires"])
	metadata.Always = looseBool(open["always"])
	metadata.SkillKey = firstString(open, "skillKey", "skill_key")
	metadata.PrimaryEnv = firstString(open, "primaryEnv", "primary_env")
	metadata.Emoji = firstString(open, "emoji")
	metadata.Homepage = firstString(open, "homepage")
	metadata.OS = stringSlice(open["os"])
	metadata.RequiresEnv = firstSlice(requires, open, "env", "requires_env")
	metadata.RequiresBins = firstSlice(requires, open, "bins", "requires_bins")
	metadata.RequiresAnyBin = firstSlice(requires, open, "anyBins", "requires_any_bin")
	metadata.RequiresConfig = firstSlice(requires, open, "config", "requires_config")
	metadata.Install = parseInstallSpecs(open["install"])
	metadata.OpenClaw = &OpenClawMetadata{
		Always: metadata.Always, SkillKey: metadata.SkillKey, PrimaryEnv: metadata.PrimaryEnv,
		Emoji: metadata.Emoji, Homepage: metadata.Homepage, OS: append([]string(nil), metadata.OS...),
		Requires: RequiresConfig{Env: append([]string(nil), metadata.RequiresEnv...), Bins: append([]string(nil), metadata.RequiresBins...), AnyBins: append([]string(nil), metadata.RequiresAnyBin...), Config: append([]string(nil), metadata.RequiresConfig...)},
		Install:  append([]InstallSpec(nil), metadata.Install...),
	}
	return metadata, nil
}

func parseInstallSpecs(raw interface{}) []InstallSpec {
	items, ok := raw.([]interface{})
	if !ok {
		return nil
	}
	result := make([]InstallSpec, 0, len(items))
	for _, item := range items {
		value, ok := stringMap(item)
		if !ok {
			continue
		}
		spec := InstallSpec{ID: firstString(value, "id"), Kind: firstString(value, "kind"), Formula: firstString(value, "formula", "cask"), Package: firstString(value, "package"), Module: firstString(value, "module"), URL: firstString(value, "url"), Archive: firstString(value, "archive"), TargetDir: firstString(value, "targetDir", "target_dir"), Bins: stringSlice(value["bins"]), Label: firstString(value, "label"), OS: stringSlice(value["os"])}
		if extract, ok := value["extract"].(bool); ok {
			spec.Extract = &extract
		}
		if strip, ok := intValue(value["stripComponents"]); ok {
			spec.StripComponents = &strip
		}
		result = append(result, spec)
	}
	return result
}

func parseCommandDispatch(frontmatter map[string]interface{}) (*CommandDispatch, error) {
	kind := firstString(frontmatter, "command-dispatch")
	tool := firstString(frontmatter, "command-tool")
	argumentMode := firstString(frontmatter, "command-arg-mode")
	if kind == "" {
		if tool != "" || argumentMode != "" {
			return nil, fmt.Errorf("command-tool and command-arg-mode require command-dispatch: tool")
		}
		return nil, nil
	}
	if kind != "tool" {
		return nil, fmt.Errorf("unsupported command-dispatch %q", kind)
	}
	if tool == "" {
		return nil, fmt.Errorf("command-dispatch tool requires command-tool")
	}
	if argumentMode == "" {
		argumentMode = "raw"
	}
	if argumentMode != "raw" {
		return nil, fmt.Errorf("unsupported command-arg-mode %q", argumentMode)
	}
	return &CommandDispatch{Kind: kind, ToolName: tool, ArgMode: argumentMode}, nil
}

func validateIdentity(name, description string) error {
	if err := validateName(name); err != nil {
		return err
	}
	if description == "" || len(description) > 1024 {
		return fmt.Errorf("description must contain 1 to 1024 characters")
	}
	return nil
}

func validateName(name string) error {
	if len(name) < 1 || len(name) > 64 || !slugPattern.MatchString(name) {
		return fmt.Errorf("name must be 1 to 64 lowercase alphanumeric or single-hyphen characters")
	}
	return nil
}

func scanWarnings(content []byte) []string {
	var result []string
	if len(content) > warningSkillSize {
		result = append(result, fmt.Sprintf("SKILL.md is large (%d bytes); use progressive disclosure for supporting material", len(content)))
	}
	for _, pattern := range suspiciousPatterns {
		if loc := pattern.FindIndex(content); loc != nil {
			line := 1 + strings.Count(string(content[:loc[0]]), "\n")
			result = append(result, fmt.Sprintf("suspicious pattern %q detected at line %d", pattern.String(), line))
		}
	}
	return result
}

func stringMap(value interface{}) (map[string]interface{}, bool) {
	result, ok := value.(map[string]interface{})
	return result, ok
}

func stringValue(values map[string]interface{}, key string) string { return firstString(values, key) }

func firstString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func metadataString(values map[string]interface{}, key string) string {
	return firstString(values, key)
}

func stringSlice(value interface{}) []string {
	items, ok := value.([]interface{})
	if !ok {
		if typed, ok := value.([]string); ok {
			return append([]string(nil), typed...)
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if value, ok := item.(string); ok && strings.TrimSpace(value) != "" {
			result = append(result, strings.TrimSpace(value))
		}
	}
	return result
}

func firstSlice(nested, flat map[string]interface{}, nestedKey, legacyKey string) []string {
	if values := stringSlice(nested[nestedKey]); len(values) > 0 {
		return values
	}
	return stringSlice(flat[legacyKey])
}

func boolValue(values map[string]interface{}, key string, fallback bool) bool {
	value, ok := values[key]
	if !ok {
		return fallback
	}
	if typed, ok := value.(bool); ok {
		return typed
	}
	if typed, ok := value.(string); ok {
		return strings.EqualFold(strings.TrimSpace(typed), "true")
	}
	return fallback
}

func looseBool(value interface{}) bool {
	typed, _ := value.(bool)
	return typed
}

func intValue(value interface{}) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	default:
		return 0, false
	}
}

func cloneMap(value map[string]interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	encoded, _ := yaml.Marshal(value)
	var result map[string]interface{}
	_ = yaml.Unmarshal(encoded, &result)
	return result
}

func sumLengths(lines []string) int {
	total := 0
	for _, line := range lines {
		total += len(line)
	}
	return total
}
