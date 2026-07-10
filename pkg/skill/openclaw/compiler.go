package openclaw

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"mime"
	"path/filepath"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill/skillmd"
)

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
}

func Compile(bundle Bundle) (*Compilation, error) {
	parsedResult, err := skillmd.ParseSkillMDWithWarnings(bundle.SkillMD)
	if err != nil {
		return nil, fmt.Errorf("parse SKILL.md: %w", err)
	}
	parsed := parsedResult.Skill
	digest := bundleDigest(bundle)
	diagnostics := make([]Diagnostic, 0, len(parsedResult.Warnings)+2)
	for _, warning := range parsedResult.Warnings {
		diagnostics = append(diagnostics, Diagnostic{Severity: "warning", Code: "source.warning", Path: "SKILL.md", Message: warning})
	}
	if expected := strings.TrimSpace(bundle.Source.ExpectedName); expected != "" {
		if expected != parsed.Name {
			return nil, fmt.Errorf("expected skill name %q does not match SKILL.md name %q", expected, parsed.Name)
		}
	}

	definition := &capability.Definition{
		ID: parsed.Name, Version: resolvedVersion(parsed, bundle.Source.Version, digest), Name: parsed.Name,
		Description: parsed.Description, Actions: map[string]capability.Action{},
		Prompt: &capability.PromptModule{
			Instructions: parsed.Body, AlwaysActive: parsed.Metadata.Always,
			UserInvocable: parsed.Invocation.UserInvocable, DisableModelInvocation: parsed.Invocation.DisableModelInvocation,
			AllowedTools: append([]string(nil), parsed.AllowedTools...),
		},
		Requirements: capability.Requirements{
			OperatingSystems: append([]string(nil), parsed.Metadata.OS...), Executables: append([]string(nil), parsed.Metadata.RequiresBins...),
			AnyExecutables: append([]string(nil), parsed.Metadata.RequiresAnyBin...), Environment: append([]string(nil), parsed.Metadata.RequiresEnv...),
			Configuration: append([]string(nil), parsed.Metadata.RequiresConfig...), Compatibility: parsed.Compatibility,
		},
		Installers: compileInstallers(parsed.Metadata.Install), Resources: compileResources(bundle.Files),
		Source: &capability.SourceProvenance{
			Format: "openclaw.skill.v1", Registry: bundle.Source.Registry, Publisher: bundle.Source.Publisher,
			Reference: bundle.Source.Reference, ResolvedVersion: bundle.Source.Version, Digest: digest,
			License: parsed.License, Homepage: parsed.Homepage, Trust: cloneMap(bundle.Source.Trust),
		},
	}
	if strings.TrimSpace(parsed.Body) == "" {
		definition.Prompt = nil
	}

	dispatchKind, _ := parsed.Frontmatter["command-dispatch"].(string)
	if dispatchKind != "" && dispatchKind != "tool" {
		return nil, fmt.Errorf("unsupported command-dispatch %q", dispatchKind)
	}
	if dispatchKind == "tool" && parsed.CommandDispatch == nil {
		return nil, fmt.Errorf("command-dispatch tool requires command-tool")
	}
	if parsed.CommandDispatch != nil {
		definition.Transport = capability.TransportReference{Kind: "tool", Endpoint: parsed.CommandDispatch.ToolName}
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
	}
	if len(definition.Actions) == 0 {
		if definition.Prompt == nil {
			return nil, fmt.Errorf("skill contains neither instructions nor a deterministic command dispatch")
		}
		diagnostics = append(diagnostics, Diagnostic{Severity: "info", Code: "prompt.compiled", Path: "SKILL.md", Message: "instruction skill compiled as a native prompt module"})
	}
	if len(bundle.Files) > 0 {
		diagnostics = append(diagnostics, Diagnostic{Severity: "info", Code: "resources.indexed", Message: fmt.Sprintf("indexed %d supporting resources for progressive disclosure", len(bundle.Files))})
	}
	return &Compilation{Definition: definition, Parsed: parsed, Diagnostics: diagnostics, SourceDigest: digest}, nil
}

func resolvedVersion(parsed *skillmd.ParsedSkill, sourceVersion, digest string) string {
	if strings.TrimSpace(sourceVersion) != "" {
		return strings.TrimSpace(sourceVersion)
	}
	if strings.TrimSpace(parsed.Version) != "" {
		return strings.TrimSpace(parsed.Version)
	}
	return "0.0.0+source." + digest[:12]
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
		kind := "resource"
		if strings.HasPrefix(path, "scripts/") {
			kind = "script"
		} else if strings.HasPrefix(path, "references/") {
			kind = "reference"
		} else if strings.HasPrefix(path, "assets/") {
			kind = "asset"
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
	result := make(map[string]interface{}, len(value))
	for key, child := range value {
		result[key] = child
	}
	return result
}
