package openclaw

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealhttp "github.com/axiom-studio/openseal/pkg/httpaction"
	opensealprocess "github.com/axiom-studio/openseal/pkg/process"
	"github.com/axiom-studio/openseal/pkg/skill/skillmd"
)

const processCompilationRevision = "process.1"

const httpCompilationRevision = "http.1"

const NeedsActionAdapterDiagnostic = "needs_action_adapter"

const PromptCompiledDiagnostic = "prompt.compiled"

type Source struct {
	Registry     string
	Publisher    string
	Reference    string
	ExpectedName string
	Version      string
	Trust        map[string]interface{}
}

type File struct {
	Path    string
	Content []byte
}

type Bundle struct {
	SkillMD []byte
	Files   []File
	Source  Source
}

type Diagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

type Compilation struct {
	Definition   *capability.Definition
	Parsed       *skillmd.ParsedSkill
	Diagnostics  []Diagnostic
	SourceDigest string
	Artifact     Bundle
}

func Compile(bundle Bundle) (*Compilation, error) {
	canonicalName := canonicalSourceName(bundle.Source.Reference)
	parsedResult, err := skillmd.ParseSkillMDWithCanonicalName(bundle.SkillMD, canonicalName)
	if err != nil {
		return nil, fmt.Errorf("parse SKILL.md: %w", err)
	}
	parsed := parsedResult.Skill
	digest := bundleDigest(bundle)
	diagnostics := make([]Diagnostic, 0, len(parsedResult.Warnings)+2)
	for _, warning := range parsedResult.Warnings {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Code: "source.warning", Path: "SKILL.md", Message: warning})
	}
	if parsed.Name != parsed.CanonicalName {
		diagnostics = append(diagnostics, Diagnostic{Severity: "info", Code: "identity.normalized", Path: "SKILL.md", Message: fmt.Sprintf("source display name %q compiled with canonical registry identity %q", parsed.Name, parsed.CanonicalName)})
	}
	if expected := strings.TrimSpace(bundle.Source.ExpectedName); expected != "" {
		if expected != parsed.Name {
			return nil, fmt.Errorf("expected skill name %q does not match SKILL.md name %q", expected, parsed.Name)
		}
	}

	canonicalTrust := CanonicalTrust(bundle.Source.Trust)
	definition := &capability.Definition{
		ID: parsed.CanonicalName, Version: resolvedVersion(parsed, bundle.Source, digest, canonicalTrust), Name: parsed.Name,
		Description: parsed.Description, Icon: parsed.Metadata.Emoji, ConfigurationKey: parsed.Metadata.SkillKey,
		Actions: map[string]capability.Action{},
		Prompt: &capability.PromptModule{
			Instructions: parsed.Body, AlwaysActive: parsed.Metadata.Always,
			UserInvocable: parsed.Invocation.UserInvocable, DisableModelInvocation: parsed.Invocation.DisableModelInvocation,
			AllowedTools: append([]string(nil), parsed.AllowedTools...), Credentials: compilePromptCredentials(parsed.Metadata.PrimaryEnv),
		},
		Requirements: capability.Requirements{
			OperatingSystems: append([]string(nil), parsed.Metadata.OS...), Executables: append([]string(nil), parsed.Metadata.RequiresBins...),
			AnyExecutables: append([]string(nil), parsed.Metadata.RequiresAnyBin...), Environment: append([]string(nil), parsed.Metadata.RequiresEnv...),
			Configuration: append([]string(nil), parsed.Metadata.RequiresConfig...), Compatibility: parsed.Compatibility,
			AlwaysAvailable: parsed.Metadata.Always,
		},
		Installers: compileInstallers(parsed.Metadata.Install), Resources: compileResources(bundle.Files),
		Source: &capability.SourceProvenance{
			Format: "openclaw.skill.v1", Registry: bundle.Source.Registry, Publisher: bundle.Source.Publisher,
			Reference: bundle.Source.Reference, ResolvedVersion: bundle.Source.Version, Digest: digest,
			License: parsed.License, Homepage: parsed.Homepage, Trust: canonicalTrust,
		},
	}
	if strings.TrimSpace(parsed.Body) == "" {
		definition.Prompt = nil
	}

	adapterCandidate := openAPIHelperCandidate(parsed)
	if adapterCandidate {
		adapterActions, adapterErr := compileOpenAPIHelperActions(bundle, parsed, definition)
		if adapterErr != nil {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "warning", Code: NeedsActionAdapterDiagnostic, Path: "SKILL.md",
				Message: "the declared OpenAPI helper could not be compiled safely: " + adapterErr.Error(),
			})
		} else {
			definition.Actions = adapterActions
			definition.Prompt.Instructions = optimizeOpenAPIHelperPrompt(definition.Prompt.Instructions, adapterActions)
			definition.Prompt.AllowedTools = nil
			definition.Requirements.Executables = nil
			definition.Requirements.AnyExecutables = nil
			definition.Requirements.Environment = nil
			definition.Requirements.OperatingSystems = nil
			definition.Installers = nil
			definition.Version += "." + httpCompilationRevision
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "info", Code: "openapi_http.compiled", Path: "bin/run.mjs",
				Message: fmt.Sprintf("compiled %d source-declared read operation(s) as typed governed HTTP actions", len(adapterActions)),
			})
		}
	} else if parsed.CommandDispatch != nil {
		definition.Transport = capability.TransportReference{
			Kind: "tool", Endpoint: parsed.CommandDispatch.ToolName,
			Arguments: map[string]capability.TransportArgument{
				"command":     {SourceArgument: "command"},
				"commandName": {Literal: parsed.Name},
				"skillName":   {Literal: parsed.Name},
			},
		}
		credentials := []capability.CredentialRequirement{}
		if parsed.Metadata.PrimaryEnv != "" {
			credentials = append(credentials, capability.CredentialRequirement{Name: parsed.Metadata.PrimaryEnv, Kind: "environment-secret"})
		}
		definition.Actions["invoke"] = capability.Action{
			Name: "invoke", Description: parsed.Description,
			InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"command": map[string]interface{}{"type": "string"}}, "required": []interface{}{"command"}, "additionalProperties": false},
			SideEffect:  capability.SideEffectExternal, Risk: capability.RiskLevelExternal,
			Permissions: append([]string(nil), parsed.AllowedTools...), Credentials: credentials,
			Retry: capability.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: capability.IdempotencySupported,
		}
		diagnostics = append(diagnostics, Diagnostic{Severity: "info", Code: "command.compiled", Path: "SKILL.md", Message: "deterministic tool command dispatch compiled as a native action"})
	} else if len(definition.Requirements.Executables) > 0 || len(definition.Requirements.AnyExecutables) > 0 {
		processActions, err := compileProcessActions(parsed, definition)
		if err != nil {
			return nil, err
		}
		for name, action := range processActions {
			definition.Actions[name] = action
		}
		definition.Version += "." + processCompilationRevision
		diagnostics = append(diagnostics, Diagnostic{Severity: "info", Code: "process.compiled", Path: "SKILL.md", Message: fmt.Sprintf("compiled %d declared executable(s) as argument-safe governed process actions", len(processActions))})
	}
	if len(definition.Actions) == 0 {
		if definition.Prompt == nil {
			return nil, fmt.Errorf("skill contains neither instructions nor a deterministic command dispatch")
		}
		if promptRequiresGovernedAction(parsed, definition) && !hasDiagnostic(diagnostics, NeedsActionAdapterDiagnostic) {
			diagnostics = append(diagnostics, Diagnostic{
				Severity: "warning", Code: NeedsActionAdapterDiagnostic, Path: "SKILL.md",
				Message: "this Skill declares external tools or credentials but no governed action; add deterministic command dispatch, a declared executable, or a host action adapter before activation",
			})
		} else {
			diagnostics = append(diagnostics, Diagnostic{Severity: "info", Code: PromptCompiledDiagnostic, Path: "SKILL.md", Message: "local instruction skill compiled as a native prompt module"})
		}
	}
	if len(bundle.Files) > 0 {
		diagnostics = append(diagnostics, Diagnostic{Severity: "info", Code: "resources.indexed", Message: fmt.Sprintf("indexed %d supporting resources for progressive disclosure", len(bundle.Files))})
	}
	return &Compilation{Definition: definition, Parsed: parsed, Diagnostics: diagnostics, SourceDigest: digest, Artifact: cloneBundle(bundle)}, nil
}

var openAPIHelperReference = regexp.MustCompile(`(?m)\{baseDir\}/bin/run\.mjs\b`)

func openAPIHelperCandidate(parsed *skillmd.ParsedSkill) bool {
	return parsed != nil && parsed.Metadata.PrimaryEnv != "" && openAPIHelperReference.MatchString(parsed.Body)
}

type openAPIHelperManifest struct {
	BaseURL    string                   `json:"baseUrl"`
	Slug       string                   `json:"slug"`
	Operations []openAPIHelperOperation `json:"operations"`
}

type openAPIHelperOperation struct {
	Description string                   `json:"description"`
	Method      string                   `json:"method"`
	OperationID string                   `json:"operationId"`
	Parameters  []openAPIHelperParameter `json:"parameters"`
	Path        string                   `json:"path"`
	RequestBody interface{}              `json:"requestBody"`
	Summary     string                   `json:"summary"`
}

type openAPIHelperParameter struct {
	DefaultValue interface{}   `json:"defaultValue"`
	Description  string        `json:"description"`
	EnumValues   []interface{} `json:"enumValues"`
	Location     string        `json:"location"`
	Name         string        `json:"name"`
	Required     bool          `json:"required"`
	SchemaType   string        `json:"schemaType"`
}

func compileOpenAPIHelperActions(bundle Bundle, parsed *skillmd.ParsedSkill, definition *capability.Definition) (map[string]capability.Action, error) {
	manifest, err := parseOpenAPIHelperManifest(bundle)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(manifest.Slug) != parsed.CanonicalName {
		return nil, fmt.Errorf("helper manifest slug %q does not match canonical Skill identity %q", manifest.Slug, parsed.CanonicalName)
	}
	if len(manifest.Operations) == 0 || len(manifest.Operations) > 64 {
		return nil, fmt.Errorf("helper manifest must declare between 1 and 64 operations")
	}
	credentialName := strings.TrimSpace(parsed.Metadata.PrimaryEnv)
	actions := make(map[string]capability.Action, len(manifest.Operations))
	for _, operation := range manifest.Operations {
		name := strings.TrimSpace(operation.OperationID)
		if !openAPIActionName.MatchString(name) || actions[name].Name != "" {
			return nil, fmt.Errorf("helper manifest contains an invalid or duplicate operationId")
		}
		if !strings.Contains(parsed.Body, name) {
			return nil, fmt.Errorf("operation %s is not declared by SKILL.md", name)
		}
		if err := opensealhttp.ValidateEndpoint(manifest.BaseURL, operation.Method, operation.Path); err != nil {
			return nil, fmt.Errorf("operation %s: %w", name, err)
		}
		if operation.RequestBody != nil {
			return nil, fmt.Errorf("operation %s declares an unsupported request body", name)
		}
		properties := make(map[string]interface{})
		required := make([]interface{}, 0)
		contract := make([]opensealhttp.Parameter, 0, len(operation.Parameters))
		credentialParameter := ""
		seen := make(map[string]bool, len(operation.Parameters))
		for _, parameter := range operation.Parameters {
			parameter.Name = strings.TrimSpace(parameter.Name)
			parameter.Location = strings.ToLower(strings.TrimSpace(parameter.Location))
			if parameter.Name == "" || seen[parameter.Name] {
				return nil, fmt.Errorf("operation %s contains an invalid or duplicate parameter", name)
			}
			seen[parameter.Name] = true
			if parameter.Name == "token" {
				if !parameter.Required || parameter.Location != "query" || credentialParameter != "" {
					return nil, fmt.Errorf("operation %s has an unsupported credential mapping", name)
				}
				credentialParameter = parameter.Name
				continue
			}
			if parameter.Location != "query" && parameter.Location != "path" {
				return nil, fmt.Errorf("operation %s parameter %s uses unsupported location %q", name, parameter.Name, parameter.Location)
			}
			property, schemaErr := openAPIParameterSchema(parameter)
			if schemaErr != nil {
				return nil, fmt.Errorf("operation %s parameter %s: %w", name, parameter.Name, schemaErr)
			}
			properties[parameter.Name] = property
			if parameter.Required && parameter.DefaultValue == nil {
				required = append(required, parameter.Name)
			}
			contract = append(contract, opensealhttp.Parameter{
				Name: parameter.Name, Location: parameter.Location, Required: parameter.Required, Default: parameter.DefaultValue,
			})
		}
		if credentialParameter == "" {
			return nil, fmt.Errorf("operation %s does not map primary credential %s to a required token query parameter", name, credentialName)
		}
		parameterSchema := map[string]interface{}{"type": "object", "properties": properties, "additionalProperties": false}
		if len(required) > 0 {
			parameterSchema["required"] = required
		}
		description := strings.TrimSpace(operation.Description)
		if description == "" {
			description = strings.TrimSpace(operation.Summary)
		}
		if description == "" {
			description = definition.Description
		}
		actions[name] = capability.Action{
			Name: name, Description: description,
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"parameters": parameterSchema}, "required": []interface{}{"parameters"},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": true,
				"properties": map[string]interface{}{
					"statusCode": map[string]interface{}{"type": "integer"},
					"body":       map[string]interface{}{},
					"provenance": map[string]interface{}{"type": "object"},
				},
				"required": []interface{}{"statusCode", "body", "provenance"},
			},
			SideEffect: capability.SideEffectRead, Risk: capability.RiskLevelRead,
			Permissions: []string{"network.read:" + mustEndpointHost(manifest.BaseURL)},
			Credentials: []capability.CredentialRequirement{{Name: credentialName, Kind: "environment-secret"}},
			Timeout:     capability.Duration(30 * time.Second), Retry: capability.ActionRetryPolicy{MaxAttempts: 2},
			Idempotency: capability.IdempotencySupported,
			Transport: &capability.TransportReference{Kind: "tool", Endpoint: opensealhttp.TransportName, Arguments: map[string]capability.TransportArgument{
				opensealhttp.BaseURLKey:             {Literal: strings.TrimRight(strings.TrimSpace(manifest.BaseURL), "/")},
				opensealhttp.MethodKey:              {Literal: strings.ToUpper(strings.TrimSpace(operation.Method))},
				opensealhttp.PathKey:                {Literal: strings.TrimSpace(operation.Path)},
				opensealhttp.ParametersKey:          {SourceArgument: "parameters"},
				opensealhttp.ParameterContractKey:   {Literal: contract},
				opensealhttp.CredentialNameKey:      {Literal: credentialName},
				opensealhttp.CredentialParameterKey: {Literal: credentialParameter},
			}},
		}
	}
	return actions, nil
}

var openAPIActionName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,127}$`)

func parseOpenAPIHelperManifest(bundle Bundle) (*openAPIHelperManifest, error) {
	var script []byte
	for _, file := range bundle.Files {
		if filepath.ToSlash(filepath.Clean(file.Path)) == "bin/run.mjs" {
			script = file.Content
			break
		}
	}
	if len(script) == 0 {
		return nil, fmt.Errorf("referenced bin/run.mjs resource is missing")
	}
	const marker = "const manifest ="
	index := strings.Index(string(script), marker)
	if index < 0 {
		return nil, fmt.Errorf("bin/run.mjs does not contain a declarative manifest")
	}
	decoder := json.NewDecoder(strings.NewReader(string(script[index+len(marker):])))
	var manifest openAPIHelperManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("parse bin/run.mjs manifest: %w", err)
	}
	return &manifest, nil
}

func optimizeOpenAPIHelperPrompt(instructions string, actions map[string]capability.Action) string {
	lines := strings.Split(instructions, "\n")
	result := make([]string, 0, len(lines)+2)
	for _, line := range lines {
		if openAPIHelperReference.MatchString(line) {
			continue
		}
		result = append(result, line)
	}
	names := make([]string, 0, len(actions))
	for name := range actions {
		names = append(names, name)
	}
	sort.Strings(names)
	optimized := strings.TrimSpace(strings.Join(result, "\n"))
	if optimized != "" {
		optimized += "\n\n"
	}
	return optimized + "Use only the governed OpenSeal action(s) " + strings.Join(names, ", ") + " for external requests. Credentials are injected out of band; never request or include them in action input."
}

func openAPIParameterSchema(parameter openAPIHelperParameter) (map[string]interface{}, error) {
	typeName := strings.ToLower(strings.TrimSpace(parameter.SchemaType))
	switch typeName {
	case "string", "integer", "number", "boolean", "object", "array":
	case "":
		typeName = "string"
	default:
		return nil, fmt.Errorf("unsupported schema type %q", parameter.SchemaType)
	}
	result := map[string]interface{}{"type": typeName}
	if description := strings.TrimSpace(parameter.Description); description != "" {
		result["description"] = description
	}
	if len(parameter.EnumValues) > 0 {
		result["enum"] = append([]interface{}(nil), parameter.EnumValues...)
	}
	if parameter.DefaultValue != nil {
		result["default"] = parameter.DefaultValue
	}
	return result, nil
}

func mustEndpointHost(baseURL string) string {
	parsed, _ := url.Parse(baseURL)
	return strings.ToLower(parsed.Hostname())
}

func hasDiagnostic(diagnostics []Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func promptRequiresGovernedAction(parsed *skillmd.ParsedSkill, definition *capability.Definition) bool {
	if parsed == nil || definition == nil || definition.Prompt == nil || len(definition.Actions) != 0 {
		return false
	}
	return len(parsed.AllowedTools) > 0 || len(definition.Prompt.Credentials) > 0 || len(definition.Requirements.Environment) > 0
}

func compileProcessActions(parsed *skillmd.ParsedSkill, definition *capability.Definition) (map[string]capability.Action, error) {
	executables := uniqueStrings(append(append([]string(nil), definition.Requirements.Executables...), definition.Requirements.AnyExecutables...))
	result := make(map[string]capability.Action, len(executables))
	environmentNames := uniqueStrings(append(append([]string(nil), definition.Requirements.Environment...), parsed.Metadata.PrimaryEnv))
	credentials := make([]capability.CredentialRequirement, 0, len(environmentNames))
	for _, name := range environmentNames {
		credentials = append(credentials, capability.CredentialRequirement{Name: name, Kind: "environment-secret"})
	}
	for index, executable := range executables {
		executable = strings.TrimSpace(executable)
		if !opensealprocess.ValidExecutableName(executable) {
			return nil, fmt.Errorf("declared executable %q is not a portable basename", executable)
		}
		name := "execute"
		if len(executables) > 1 {
			name = fmt.Sprintf("execute_%d_%s", index+1, processActionSuffix(executable))
		}
		installers := installersForExecutable(definition.Installers, executable)
		result[name] = capability.Action{
			Name: name, Description: fmt.Sprintf("Execute %s for the %s Skill in an isolated governed process.", executable, parsed.Name),
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"arguments": map[string]interface{}{
						"type": "array", "description": "Ordered arguments passed directly to the executable without a shell.",
						"items": map[string]interface{}{"type": "string", "maxLength": opensealprocess.MaxArgumentBytes}, "maxItems": opensealprocess.MaxArguments,
					},
				},
				"required": []interface{}{"arguments"},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{
					"exitCode": map[string]interface{}{"type": "integer"}, "stdout": map[string]interface{}{"type": "string"}, "stderr": map[string]interface{}{"type": "string"},
				},
				"required": []interface{}{"exitCode", "stdout", "stderr"},
			},
			SideEffect: capability.SideEffectExternal, Risk: capability.RiskLevelExternal,
			Permissions: []string{"process.exec:" + executable}, Credentials: credentials,
			Timeout: capability.Duration(time.Duration(opensealprocess.MaxTimeoutSeconds) * time.Second),
			Retry:   capability.ActionRetryPolicy{MaxAttempts: 1}, Idempotency: capability.IdempotencySupported,
			Transport: &capability.TransportReference{
				Kind: "tool", Endpoint: opensealprocess.TransportName,
				Arguments: map[string]capability.TransportArgument{
					opensealprocess.ExecutableKey:       {Literal: executable},
					opensealprocess.ArgumentsKey:        {SourceArgument: "arguments"},
					opensealprocess.InstallersKey:       {Literal: installers},
					opensealprocess.EnvironmentNamesKey: {Literal: environmentNames},
					opensealprocess.TimeoutSecondsKey:   {Literal: opensealprocess.MaxTimeoutSeconds},
				},
			},
		}
	}
	return result, nil
}

func installersForExecutable(installers []capability.Installer, executable string) []capability.Installer {
	result := make([]capability.Installer, 0)
	for _, installer := range installers {
		for _, provided := range installer.Executables {
			if strings.TrimSpace(provided) == executable {
				result = append(result, installer)
				break
			}
		}
	}
	return result
}

func uniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			result = append(result, value)
			seen[value] = true
		}
	}
	sort.Strings(result)
	return result
}

func processActionSuffix(executable string) string {
	var builder strings.Builder
	for _, character := range executable {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	return strings.Trim(builder.String(), "_")
}

func canonicalSourceName(reference string) string {
	reference = strings.TrimSpace(strings.TrimPrefix(reference, "@"))
	if separator := strings.LastIndex(reference, "/"); separator >= 0 {
		reference = reference[separator+1:]
	}
	return reference
}

func compilePromptCredentials(primaryEnv string) []capability.CredentialRequirement {
	primaryEnv = strings.TrimSpace(primaryEnv)
	if primaryEnv == "" {
		return nil
	}
	return []capability.CredentialRequirement{{Name: primaryEnv, Kind: "environment-secret"}}
}

// ExportBundle returns the exact source artifact retained by the compiler.
// Deterministic native optimization never replaces or mutates the portable
// OpenClaw source, so supported imports can be exported byte-for-byte.
func ExportBundle(compilation *Compilation) (Bundle, error) {
	if compilation == nil || len(compilation.Artifact.SkillMD) == 0 || compilation.SourceDigest == "" {
		return Bundle{}, fmt.Errorf("compiled source artifact is required")
	}
	if digest := bundleDigest(compilation.Artifact); digest != compilation.SourceDigest {
		return Bundle{}, fmt.Errorf("compiled source artifact digest does not match provenance")
	}
	return cloneBundle(compilation.Artifact), nil
}

// BundleDigest returns the canonical content identity used by compiled source
// provenance. Source registry and trust metadata are intentionally excluded:
// identical bytes have one content digest while retention references preserve
// each independent origin.
func BundleDigest(bundle Bundle) string {
	return bundleDigest(bundle)
}

func resolvedVersion(parsed *skillmd.ParsedSkill, source Source, digest string, trust map[string]interface{}) string {
	version := strings.TrimSpace(source.Version)
	if version == "" {
		version = strings.TrimSpace(parsed.Version)
	}
	if version == "" {
		version = "0.0.0"
	}
	separator := "+source."
	if strings.Contains(version, "+") {
		separator = ".source."
	}
	version += separator + digest[:12]
	if origin := sourceOriginDigest(source); origin != "" {
		version += ".origin." + origin[:12]
	}
	if evidence := trustEvidenceDigest(trust); evidence != "" {
		version += ".trust." + evidence[:12]
	}
	return version
}

// CanonicalTrust projects registry verification into immutable definition
// evidence. Resolution hints and observation timestamps remain available in
// the retained source artifact, but cannot make identical verified bytes
// compile into conflicting definitions.
func CanonicalTrust(input map[string]interface{}) map[string]interface{} {
	canonical, _ := canonicalTrustValue(input).(map[string]interface{})
	if len(canonical) == 0 {
		return nil
	}
	return canonical
}

func canonicalTrustValue(input interface{}) interface{} {
	switch value := input.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(value))
		for key, child := range value {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "resolvedfrom", "createdat", "checkedat":
				continue
			}
			result[key] = canonicalTrustValue(child)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(value))
		for index, child := range value {
			result[index] = canonicalTrustValue(child)
		}
		return result
	default:
		return value
	}
}

func trustEvidenceDigest(trust map[string]interface{}) string {
	if len(trust) == 0 {
		return ""
	}
	encoded, err := json.Marshal(trust)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func sourceOriginDigest(source Source) string {
	identity := strings.Join([]string{
		strings.TrimSpace(source.Registry), strings.TrimSpace(source.Publisher),
		strings.TrimSpace(source.Reference), strings.TrimSpace(source.ExpectedName),
	}, "\x00")
	if strings.Trim(identity, "\x00") == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func compileInstallers(values []skillmd.InstallSpec) []capability.Installer {
	result := make([]capability.Installer, 0, len(values))
	for _, value := range values {
		result = append(result, capability.Installer{ID: value.ID, Kind: value.Kind, Label: value.Label, OperatingSystems: append([]string(nil), value.OS...), Executables: append([]string(nil), value.Bins...), Package: value.Package, Module: value.Module, Formula: value.Formula, URL: value.URL, Archive: value.Archive, Extract: value.Extract, StripComponents: value.StripComponents, TargetDirectory: value.TargetDir})
	}
	return result
}

func compileResources(files []File) []capability.Resource {
	result := make([]capability.Resource, 0, len(files))
	for _, file := range files {
		path := filepath.ToSlash(filepath.Clean(file.Path))
		if path == "." || path == "SKILL.md" || strings.HasPrefix(path, "../") || strings.HasPrefix(path, "/") {
			continue
		}
		digest := sha256.Sum256(file.Content)
		kind := capability.ResourceKindFile
		if strings.HasPrefix(path, "scripts/") {
			kind = capability.ResourceKindScript
		} else if strings.HasPrefix(path, "references/") {
			kind = capability.ResourceKindReference
		} else if strings.HasPrefix(path, "assets/") {
			kind = capability.ResourceKindAsset
		}
		result = append(result, capability.Resource{Path: path, Kind: kind, MediaType: mime.TypeByExtension(filepath.Ext(path)), Digest: hex.EncodeToString(digest[:]), Size: int64(len(file.Content))})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func bundleDigest(bundle Bundle) string {
	hash := sha256.New()
	hash.Write([]byte("SKILL.md\x00"))
	hash.Write(bundle.SkillMD)
	files := append([]File(nil), bundle.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		hash.Write([]byte("\x00" + filepath.ToSlash(file.Path) + "\x00"))
		hash.Write(file.Content)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func cloneMap(value map[string]interface{}) map[string]interface{} {
	if value == nil {
		return nil
	}
	encoded, _ := json.Marshal(value)
	var result map[string]interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}

func cloneBundle(value Bundle) Bundle {
	result := Bundle{
		SkillMD: append([]byte(nil), value.SkillMD...),
		Source: Source{
			Registry: value.Source.Registry, Publisher: value.Source.Publisher, Reference: value.Source.Reference,
			ExpectedName: value.Source.ExpectedName, Version: value.Source.Version, Trust: cloneMap(value.Source.Trust),
		},
		Files: make([]File, len(value.Files)),
	}
	for index, file := range value.Files {
		result.Files[index] = File{Path: file.Path, Content: append([]byte(nil), file.Content...)}
	}
	return result
}
