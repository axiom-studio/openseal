package clawhub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

const CompilationPreviewAPIVersion = "openseal.skill.compilation-preview/v1"

var ErrCompilationPreviewMismatch = errors.New("skill artifact no longer matches compilation preview")

// PreviewRequest identifies one immutable registry release to fetch, verify,
// and compile without installing or activating it.
type PreviewRequest struct {
	Reference SkillReference `json:"reference"`
	Version   string         `json:"version,omitempty"`
	Tag       string         `json:"tag,omitempty"`
}

// CompilationReceipt is the minimum immutable identity an installer needs to
// prove it consumed the artifact that an operator reviewed. It contains no
// source content, credential reference, or registry verification payload.
type CompilationReceipt struct {
	APIVersion        string         `json:"apiVersion"`
	SourceIdentity    string         `json:"sourceIdentity"`
	Reference         SkillReference `json:"reference"`
	Version           string         `json:"version"`
	SourceDigest      string         `json:"sourceDigest"`
	CompilationDigest string         `json:"compilationDigest"`
	ArchiveSHA256     string         `json:"archiveSha256"`
}

// CompilationPreview is a credential-free control-plane projection of a
// verified compilation. Action credentials are requirement names and kinds,
// never bound references or resolved values. Prompt instructions, source
// files, installer paths, and free-form trust evidence remain private to the
// retained compiler artifact.
type CompilationPreview struct {
	APIVersion             string                             `json:"apiVersion"`
	Receipt                CompilationReceipt                 `json:"receipt"`
	DefinitionID           string                             `json:"definitionId"`
	DefinitionVersion      string                             `json:"definitionVersion"`
	Name                   string                             `json:"name"`
	Description            string                             `json:"description,omitempty"`
	Compatible             bool                               `json:"compatible"`
	PromptAvailable        bool                               `json:"promptAvailable,omitempty"`
	Actions                map[string]capability.Action       `json:"actions"`
	CredentialRequirements []capability.CredentialRequirement `json:"credentialRequirements,omitempty"`
	Requirements           capability.Requirements            `json:"requirements,omitempty"`
	Diagnostics            []opensealclaw.Diagnostic          `json:"diagnostics,omitempty"`
}

// Preview fetches, verifies, and compiles one ClawHub artifact without
// installing, activating, or executing it. The returned receipt can be passed
// to Install to fail closed if the registry serves different bytes later.
func (m *InstallManager) Preview(ctx context.Context, request PreviewRequest) (*CompilationPreview, error) {
	return m.preview(ctx, request, nil)
}

// PreviewValidated additionally projects host action-adapter compatibility.
// A host incompatibility is returned as an explicit diagnostic rather than
// discarding an otherwise valid portable compilation.
func (m *InstallManager) PreviewValidated(ctx context.Context, request PreviewRequest, validate CompilationValidator) (*CompilationPreview, error) {
	return m.preview(ctx, request, validate)
}

func (m *PreviewManager) Preview(ctx context.Context, request PreviewRequest) (*CompilationPreview, error) {
	if m == nil {
		return nil, errors.New("ClawHub preview manager is required")
	}
	return m.compiler.preview(ctx, request, nil)
}

func (m *PreviewManager) PreviewValidated(ctx context.Context, request PreviewRequest, validate CompilationValidator) (*CompilationPreview, error) {
	if m == nil {
		return nil, errors.New("ClawHub preview manager is required")
	}
	return m.compiler.preview(ctx, request, validate)
}

func (m *InstallManager) preview(ctx context.Context, request PreviewRequest, validate CompilationValidator) (*CompilationPreview, error) {
	if m == nil || m.registry == nil {
		return nil, errors.New("ClawHub install manager is required")
	}
	reference, err := canonicalReference(request.Reference)
	if err != nil || request.Version != "" && request.Tag != "" {
		return nil, errors.New("skill reference is required and version and tag are mutually exclusive")
	}
	detail, err := m.registry.InspectSkill(ctx, reference)
	if err != nil {
		return nil, err
	}
	verification, err := m.registry.VerifySkill(ctx, reference, request.Version, request.Tag)
	if err != nil {
		return nil, err
	}
	if verification == nil || !verification.OK || verification.Decision != "pass" {
		reasons := []string(nil)
		if verification != nil {
			reasons = verification.Reasons
		}
		return nil, fmt.Errorf("%w: %s", ErrVerificationFailed, strings.Join(reasons, "; "))
	}
	if verification.Slug != reference.Slug || reference.Owner != "" && verification.PublisherHandle != reference.Owner || verification.Version == "" {
		return nil, fmt.Errorf("%w: verification identity does not match requested skill", ErrVerificationFailed)
	}
	version := strings.TrimSpace(verification.Version)
	if version == "" {
		version = strings.TrimSpace(request.Version)
	}
	if version == "" {
		version = strings.TrimSpace(detail.Version)
	}
	if version == "" {
		return nil, errors.New("registry did not resolve a skill version")
	}
	archive, err := m.registry.DownloadArchive(ctx, reference, version, "")
	if err != nil {
		return nil, err
	}

	// Reuse the exact secure archive extraction and canonical compiler used by
	// installation, but stage only in an ephemeral preview directory.
	previewRoot, err := os.MkdirTemp("", ".openseal-skill-preview-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(previewRoot)
	stage, compilation, _, err := m.stageArchiveIn(previewRoot, reference, version, archive, verification)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)

	receipt := CompilationReceipt{
		APIVersion: CompilationPreviewAPIVersion, SourceIdentity: m.identity(reference), Reference: reference,
		Version: version, SourceDigest: compilation.SourceDigest, CompilationDigest: compilationProjectionDigest(compilation), ArchiveSHA256: archive.SHA256,
	}
	definition := compilation.Definition
	diagnostics := append([]opensealclaw.Diagnostic(nil), compilation.Diagnostics...)
	compatible := true
	for _, diagnostic := range diagnostics {
		if diagnostic.Severity == "error" || diagnostic.Code == opensealclaw.NeedsActionAdapterDiagnostic {
			compatible = false
		}
	}
	if validate != nil {
		if validationErr := validate(compilation); validationErr != nil {
			compatible = false
			diagnostics = append(diagnostics, opensealclaw.Diagnostic{
				Severity: "error", Code: "host.incompatible", Message: validationErr.Error(),
			})
		}
	}
	return &CompilationPreview{
		APIVersion: CompilationPreviewAPIVersion, Receipt: receipt,
		DefinitionID: definition.ID, DefinitionVersion: definition.Version, Name: definition.Name, Description: definition.Description,
		Compatible: compatible, PromptAvailable: definition.Prompt != nil,
		Actions: clonePreviewActions(definition.Actions), CredentialRequirements: previewCredentialRequirements(definition),
		Requirements: definition.Requirements, Diagnostics: diagnostics,
	}, nil
}

func verifyCompilationReceipt(receipt *CompilationReceipt, sourceIdentity string, reference SkillReference, version string, archive *DownloadedArchive, compilation *opensealclaw.Compilation) error {
	if receipt == nil {
		return nil
	}
	receiptReference, err := canonicalReference(receipt.Reference)
	if err != nil {
		return ErrCompilationPreviewMismatch
	}
	if receipt.APIVersion != CompilationPreviewAPIVersion || receipt.SourceIdentity != sourceIdentity ||
		receiptReference != reference || receipt.Version != version || receipt.SourceDigest != compilation.SourceDigest ||
		receipt.CompilationDigest != compilationProjectionDigest(compilation) ||
		strings.TrimPrefix(strings.ToLower(receipt.ArchiveSHA256), "sha256:") != archive.SHA256 {
		return ErrCompilationPreviewMismatch
	}
	return nil
}

func compilationProjectionDigest(compilation *opensealclaw.Compilation) string {
	if compilation == nil || compilation.Definition == nil {
		return ""
	}
	definition := compilation.Definition
	projection := struct {
		DefinitionID           string                             `json:"definitionId"`
		DefinitionVersion      string                             `json:"definitionVersion"`
		Name                   string                             `json:"name"`
		Description            string                             `json:"description,omitempty"`
		Actions                map[string]capability.Action       `json:"actions"`
		CredentialRequirements []capability.CredentialRequirement `json:"credentialRequirements,omitempty"`
		Requirements           capability.Requirements            `json:"requirements,omitempty"`
		Diagnostics            []opensealclaw.Diagnostic          `json:"diagnostics,omitempty"`
	}{
		DefinitionID: definition.ID, DefinitionVersion: definition.Version, Name: definition.Name, Description: definition.Description,
		Actions: clonePreviewActions(definition.Actions), CredentialRequirements: previewCredentialRequirements(definition),
		Requirements: definition.Requirements, Diagnostics: append([]opensealclaw.Diagnostic(nil), compilation.Diagnostics...),
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func clonePreviewActions(actions map[string]capability.Action) map[string]capability.Action {
	if len(actions) == 0 {
		return map[string]capability.Action{}
	}
	encoded, _ := json.Marshal(actions)
	var result map[string]capability.Action
	_ = json.Unmarshal(encoded, &result)
	return result
}

func previewCredentialRequirements(definition *capability.Definition) []capability.CredentialRequirement {
	if definition == nil {
		return nil
	}
	values := make([]capability.CredentialRequirement, 0)
	if definition.Prompt != nil {
		values = append(values, definition.Prompt.Credentials...)
	}
	for _, action := range definition.Actions {
		values = append(values, action.Credentials...)
	}
	for _, name := range definition.Requirements.Environment {
		values = append(values, capability.CredentialRequirement{Name: name, Kind: "environment-secret"})
	}
	seen := make(map[string]bool)
	result := make([]capability.CredentialRequirement, 0, len(values))
	for _, value := range values {
		value.Name, value.Kind = strings.TrimSpace(value.Name), strings.TrimSpace(value.Kind)
		if value.Name == "" || value.Kind == "" {
			continue
		}
		key := value.Name + "\x00" + value.Kind + fmt.Sprint("\x00", value.Optional)
		if !seen[key] {
			seen[key] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Kind < result[j].Kind
		}
		return result[i].Name < result[j].Name
	})
	return result
}
