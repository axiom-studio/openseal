package sourceartifact

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

const FormatOpenClawSkillV1 = "openclaw.skill.v1"

var (
	ErrNotFound          = errors.New("skill source artifact not found")
	ErrImmutable         = errors.New("skill source artifacts are immutable")
	ErrReferenceConflict = errors.New("skill source artifact reference conflict")
)

// Origin preserves registry, publisher, version, identity, and trust evidence
// independently from the content digest. It is host-only provenance and must
// never be projected into model-visible prompt catalogs.
type Origin struct {
	Registry     string                 `json:"registry,omitempty"`
	Publisher    string                 `json:"publisher,omitempty"`
	Reference    string                 `json:"reference,omitempty"`
	ExpectedName string                 `json:"expectedName,omitempty"`
	Version      string                 `json:"version,omitempty"`
	Trust        map[string]interface{} `json:"trust,omitempty"`
}

type File struct {
	Path    string `json:"path"`
	Digest  string `json:"digest"`
	Size    int64  `json:"size"`
	Content []byte `json:"content"`
}

// Artifact is one immutable, tenant-scoped source snapshot. Files retain
// exact bytes and order; normalized paths and per-file hashes make staging and
// export independently verifiable after process restart.
type Artifact struct {
	Scope      capability.ScopeReference `json:"scope"`
	Digest     string                    `json:"digest"`
	Format     string                    `json:"format"`
	EntryPoint string                    `json:"entryPoint"`
	Origin     Origin                    `json:"origin"`
	Files      []File                    `json:"files"`
	CreatedAt  time.Time                 `json:"createdAt"`
}

// Reference is an explicit retention hold. Installation, catalog, binding, or
// another host lifecycle owns the ID and releases it when the source is no
// longer needed. Optional expiry supports temporary inspection/import holds.
type Reference struct {
	Scope     capability.ScopeReference `json:"scope"`
	Digest    string                    `json:"digest"`
	ID        string                    `json:"id"`
	Kind      string                    `json:"kind"`
	CreatedAt time.Time                 `json:"createdAt"`
	ExpiresAt *time.Time                `json:"expiresAt,omitempty"`
}

type Key struct {
	Scope  capability.ScopeReference `json:"scope"`
	Digest string                    `json:"digest"`
}

type GarbageCollectionReport struct {
	ExpiredReferences int `json:"expiredReferences"`
	DeletedArtifacts  int `json:"deletedArtifacts"`
}

type Store interface {
	ImportSourceArtifact(context.Context, *Artifact, *Reference) (bool, error)
	GetSourceArtifact(context.Context, capability.ScopeReference, string) (*Artifact, error)
	DeleteSourceArtifactReference(context.Context, capability.ScopeReference, string, string) error
	PurgeExpiredSourceArtifactReferences(context.Context, time.Time, int) (int, error)
	ListUnreferencedSourceArtifacts(context.Context, time.Time, int) ([]Key, error)
	DeleteSourceArtifactIfUnreferenced(context.Context, Key, time.Time) (bool, error)
}

func ValidateArtifact(value *Artifact) error {
	if value == nil {
		return errors.New("skill source artifact is required")
	}
	if err := validateScope(value.Scope); err != nil {
		return err
	}
	if err := validateDigest(value.Digest); err != nil {
		return err
	}
	if value.Format != FormatOpenClawSkillV1 {
		return fmt.Errorf("unsupported skill source artifact format %q", value.Format)
	}
	if value.EntryPoint != "SKILL.md" || value.CreatedAt.IsZero() {
		return errors.New("skill source artifact entry point and creation time are required")
	}
	if len(value.Files) == 0 || value.Files[0].Path != value.EntryPoint {
		return errors.New("skill source artifact must retain SKILL.md as its first file")
	}
	seen := make(map[string]bool, len(value.Files))
	for index := range value.Files {
		file := &value.Files[index]
		normalized, err := normalizePath(file.Path)
		if err != nil || normalized != file.Path || seen[normalized] {
			return fmt.Errorf("skill source artifact file path %q is unsafe, non-canonical, or duplicated", file.Path)
		}
		seen[normalized] = true
		if file.Size != int64(len(file.Content)) || contentDigest(file.Content) != file.Digest {
			return fmt.Errorf("skill source artifact file %q does not match its size or digest", file.Path)
		}
	}
	bundle, err := artifactBundle(value)
	if err != nil {
		return err
	}
	if openclaw.BundleDigest(bundle) != value.Digest {
		return errors.New("skill source artifact content does not match its source digest")
	}
	return nil
}

func ValidateReference(value *Reference) error {
	if value == nil {
		return errors.New("skill source artifact reference is required")
	}
	if err := validateScope(value.Scope); err != nil {
		return err
	}
	if err := validateDigest(value.Digest); err != nil {
		return err
	}
	if strings.TrimSpace(value.ID) == "" || value.ID != strings.TrimSpace(value.ID) || strings.TrimSpace(value.Kind) == "" || value.Kind != strings.TrimSpace(value.Kind) || value.CreatedAt.IsZero() {
		return errors.New("skill source artifact reference id, kind, and creation time are required")
	}
	if value.ExpiresAt != nil && !value.ExpiresAt.After(value.CreatedAt) {
		return errors.New("skill source artifact reference expiry must follow creation")
	}
	return nil
}

func EquivalentArtifacts(left, right *Artifact) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftCopy, rightCopy := CloneArtifact(left), CloneArtifact(right)
	leftCopy.CreatedAt, rightCopy.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func EquivalentReferences(left, right *Reference) bool {
	if left == nil || right == nil {
		return left == right
	}
	leftCopy, rightCopy := CloneReference(left), CloneReference(right)
	leftCopy.CreatedAt, rightCopy.CreatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(leftCopy, rightCopy)
}

func ValidateKey(value Key) error {
	if err := validateScope(value.Scope); err != nil {
		return err
	}
	return validateDigest(value.Digest)
}

func CloneArtifact(value *Artifact) *Artifact {
	if value == nil {
		return nil
	}
	result := *value
	result.Origin.Trust = cloneMap(value.Origin.Trust)
	result.Files = make([]File, len(value.Files))
	for index, file := range value.Files {
		result.Files[index] = file
		result.Files[index].Content = bytes.Clone(file.Content)
	}
	return &result
}

func CloneReference(value *Reference) *Reference {
	if value == nil {
		return nil
	}
	result := *value
	if value.ExpiresAt != nil {
		expires := *value.ExpiresAt
		result.ExpiresAt = &expires
	}
	return &result
}

func artifactBundle(value *Artifact) (openclaw.Bundle, error) {
	if value == nil || len(value.Files) == 0 || value.Files[0].Path != "SKILL.md" {
		return openclaw.Bundle{}, errors.New("skill source artifact has no SKILL.md entry point")
	}
	bundle := openclaw.Bundle{
		SkillMD: bytes.Clone(value.Files[0].Content),
		Source: openclaw.Source{
			Registry: value.Origin.Registry, Publisher: value.Origin.Publisher, Reference: value.Origin.Reference,
			ExpectedName: value.Origin.ExpectedName, Version: value.Origin.Version, Trust: cloneMap(value.Origin.Trust),
		},
		Files: make([]openclaw.File, 0, len(value.Files)-1),
	}
	for _, file := range value.Files[1:] {
		bundle.Files = append(bundle.Files, openclaw.File{Path: file.Path, Content: bytes.Clone(file.Content)})
	}
	return bundle, nil
}

func validateScope(scope capability.ScopeReference) error {
	if strings.TrimSpace(scope.Kind) == "" || scope.Kind != strings.TrimSpace(scope.Kind) || strings.TrimSpace(scope.ID) == "" || scope.ID != strings.TrimSpace(scope.ID) {
		return errors.New("skill source artifact scope is required")
	}
	return nil
}

func validateDigest(value string) error {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != 32 || value != strings.ToLower(value) {
		return errors.New("skill source artifact requires a lowercase SHA-256 digest")
	}
	return nil
}

func normalizePath(value string) (string, error) {
	if strings.ContainsRune(value, 0) || strings.Contains(value, "\\") || strings.TrimSpace(value) != value {
		return "", errors.New("source artifact path must be normalized")
	}
	normalized := path.Clean(value)
	if normalized == "." || path.IsAbs(normalized) || normalized == ".." || strings.HasPrefix(normalized, "../") {
		return "", errors.New("source artifact path must be relative")
	}
	return normalized, nil
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
