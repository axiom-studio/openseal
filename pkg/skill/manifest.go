package skill

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"sigs.k8s.io/yaml"
)

const (
	ManifestAPIVersion = "openseal.dev/v1alpha1"
	ManifestKind       = "SkillDefinition"
)

// Manifest is the canonical portable envelope for a complete Skill
// definition. Installers and resources are part of the immutable definition;
// registries may add catalog metadata around this envelope but must not alter
// or partially project it.
type Manifest struct {
	APIVersion  string                `json:"apiVersion"`
	Kind        string                `json:"kind"`
	Definition  capability.Definition `json:"definition"`
	Diagnostics []ManifestDiagnostic  `json:"diagnostics,omitempty"`
}

type ManifestDiagnostic struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

func NewManifest(definition *Definition) (*Manifest, error) {
	if err := validateDefinition(definition); err != nil {
		return nil, err
	}
	return &Manifest{
		APIVersion: ManifestAPIVersion,
		Kind:       ManifestKind,
		Definition: capability.Definition(*cloneDefinition(definition)),
	}, nil
}

// ClaimsManifest reports whether an untyped document explicitly identifies
// itself as the canonical OpenSeal Skill envelope.
func ClaimsManifest(value map[string]interface{}) bool {
	if value == nil {
		return false
	}
	apiVersion, _ := value["apiVersion"].(string)
	kind, _ := value["kind"].(string)
	return apiVersion == ManifestAPIVersion || kind == ManifestKind
}

// DecodeManifest validates one untyped canonical envelope without accepting
// unknown fields or legacy executor/node documents.
func DecodeManifest(value map[string]interface{}) (*Manifest, error) {
	if value == nil {
		return nil, errors.New("skill manifest is required")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode canonical Skill manifest: %w", err)
	}
	return decodeManifestJSON(encoded)
}

// DecodeManifestYAML accepts YAML or JSON while retaining strict duplicate and
// unknown-field rejection.
func DecodeManifestYAML(data []byte) (*Manifest, error) {
	encoded, err := yaml.YAMLToJSONStrict(data)
	if err != nil {
		return nil, fmt.Errorf("decode canonical Skill YAML: %w", err)
	}
	return decodeManifestJSON(encoded)
}

func decodeManifestJSON(encoded []byte) (*Manifest, error) {
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, fmt.Errorf("decode canonical Skill manifest: %w", err)
	}
	if manifest.APIVersion != ManifestAPIVersion || manifest.Kind != ManifestKind {
		return nil, fmt.Errorf("unsupported Skill manifest %q %q", manifest.APIVersion, manifest.Kind)
	}
	definition := Definition(manifest.Definition)
	if err := validateDefinition(&definition); err != nil {
		return nil, fmt.Errorf("invalid canonical Skill definition: %w", err)
	}
	for index, diagnostic := range manifest.Diagnostics {
		if strings.TrimSpace(diagnostic.Severity) == "" || strings.TrimSpace(diagnostic.Code) == "" || strings.TrimSpace(diagnostic.Message) == "" {
			return nil, fmt.Errorf("canonical Skill manifest diagnostic %d is incomplete", index)
		}
	}
	return &manifest, nil
}

func EncodeManifestYAML(manifest *Manifest) ([]byte, error) {
	if manifest == nil {
		return nil, errors.New("skill manifest is required")
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("encode canonical Skill manifest: %w", err)
	}
	if _, err := decodeManifestJSON(encoded); err != nil {
		return nil, err
	}
	data, err := yaml.JSONToYAML(encoded)
	if err != nil {
		return nil, fmt.Errorf("encode canonical Skill YAML: %w", err)
	}
	return data, nil
}
