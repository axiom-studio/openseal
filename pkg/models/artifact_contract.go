package models

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var (
	ErrUnsafeSharedContext = errors.New("shared context cannot contain credentials or secrets")
	ErrInvalidArtifact     = errors.New("invalid artifact contract")
)

type ArtifactRequirement struct {
	Name        string                 `json:"name"`
	Type        string                 `json:"type,omitempty"`
	Description string                 `json:"description,omitempty"`
	Schema      map[string]interface{} `json:"schema,omitempty"`
	Required    bool                   `json:"required"`
}

// ArtifactReference is a portable, immutable link to output stored by a host
// artifact service. ContentRef is deliberately opaque: OpenSeal persists and
// audits identity and provenance without exposing signed URLs or credentials.
type ArtifactReference struct {
	ID              string                 `json:"id"`
	Version         int64                  `json:"version"`
	RequirementName string                 `json:"requirementName,omitempty"`
	Name            string                 `json:"name"`
	Type            string                 `json:"type,omitempty"`
	MediaType       string                 `json:"mediaType,omitempty"`
	ContentRef      string                 `json:"contentRef"`
	Digest          string                 `json:"digest,omitempty"`
	SizeBytes       int64                  `json:"sizeBytes,omitempty"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
	EvidenceRefs    []string               `json:"evidenceRefs,omitempty"`
}

// ValidateCredentialFreeContext rejects resolved credentials and secret-shaped
// values before portable work state can reach persistence or model context.
// Opaque credential references belong in governed skill bindings instead.
func ValidateCredentialFreeContext(value interface{}) error {
	var walk func(interface{}) error
	walk = func(candidate interface{}) error {
		switch typed := candidate.(type) {
		case map[string]interface{}:
			for key, child := range typed {
				normalized := strings.NewReplacer("-", "", "_", "", ".", "").Replace(strings.ToLower(key))
				for _, forbidden := range []string{"credential", "credentials", "secret", "secrets", "password", "passphrase", "apikey", "apitoken", "privatekey", "accesstoken", "authtoken", "refreshtoken", "bearertoken"} {
					if normalized == forbidden || strings.HasSuffix(normalized, forbidden) {
						return ErrUnsafeSharedContext
					}
				}
				if err := walk(child); err != nil {
					return err
				}
			}
		case []interface{}:
			for _, child := range typed {
				if err := walk(child); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(value)
}

func ValidateArtifactRequirements(requirements []ArtifactRequirement) error {
	seen := make(map[string]struct{}, len(requirements))
	for index, requirement := range requirements {
		name := strings.TrimSpace(requirement.Name)
		if name == "" {
			return fmt.Errorf("artifact requirement %d name is required", index)
		}
		if _, exists := seen[name]; exists {
			return fmt.Errorf("artifact requirement %q is duplicated", name)
		}
		seen[name] = struct{}{}
		if requirement.Schema != nil {
			if _, err := compileArtifactSchema(name, requirement.Schema); err != nil {
				return fmt.Errorf("artifact requirement %q schema: %w", name, err)
			}
		}
	}
	return nil
}

func ValidateArtifactReferences(requirements []ArtifactRequirement, artifacts []ArtifactReference) error {
	requirementsByName := make(map[string]ArtifactRequirement, len(requirements))
	for _, requirement := range requirements {
		requirementsByName[strings.TrimSpace(requirement.Name)] = requirement
	}
	seenIDs := make(map[string]struct{}, len(artifacts))
	provided := make(map[string]struct{}, len(artifacts))
	for index, artifact := range artifacts {
		artifact.ID = strings.TrimSpace(artifact.ID)
		artifact.Name = strings.TrimSpace(artifact.Name)
		artifact.ContentRef = strings.TrimSpace(artifact.ContentRef)
		if artifact.ID == "" || artifact.Version <= 0 || artifact.Name == "" || artifact.ContentRef == "" {
			return fmt.Errorf("artifact %d id, version, name, and content reference are required", index)
		}
		if _, exists := seenIDs[artifact.ID]; exists {
			return fmt.Errorf("artifact id %q is duplicated", artifact.ID)
		}
		seenIDs[artifact.ID] = struct{}{}
		if err := validateOpaqueArtifactRef(artifact.ContentRef); err != nil {
			return fmt.Errorf("artifact %q content reference: %w", artifact.ID, err)
		}
		if artifact.SizeBytes < 0 {
			return fmt.Errorf("artifact %q size cannot be negative", artifact.ID)
		}
		if artifact.Digest != "" {
			parts := strings.SplitN(strings.ToLower(strings.TrimSpace(artifact.Digest)), ":", 2)
			if len(parts) != 2 || parts[0] != "sha256" || len(parts[1]) != 64 {
				return fmt.Errorf("artifact %q digest must be sha256:<64 hex characters>", artifact.ID)
			}
			if _, err := hex.DecodeString(parts[1]); err != nil {
				return fmt.Errorf("artifact %q digest is invalid", artifact.ID)
			}
		}
		if err := ValidateCredentialFreeContext(artifact.Metadata); err != nil {
			return fmt.Errorf("artifact %q metadata: %w", artifact.ID, err)
		}
		for _, evidenceRef := range artifact.EvidenceRefs {
			if err := validateOpaqueArtifactRef(evidenceRef); err != nil {
				return fmt.Errorf("artifact %q evidence reference: %w", artifact.ID, err)
			}
		}
		requirementName := strings.TrimSpace(artifact.RequirementName)
		if requirementName == "" {
			continue
		}
		requirement, exists := requirementsByName[requirementName]
		if !exists {
			return fmt.Errorf("artifact %q references unknown requirement %q", artifact.ID, requirementName)
		}
		if requirement.Type != "" && strings.TrimSpace(artifact.Type) != strings.TrimSpace(requirement.Type) {
			return fmt.Errorf("artifact %q type %q does not satisfy requirement %q type %q", artifact.ID, artifact.Type, requirementName, requirement.Type)
		}
		if requirement.Schema != nil {
			compiled, err := compileArtifactSchema(requirementName, requirement.Schema)
			if err != nil {
				return err
			}
			if err := validateArtifactSchema(compiled, artifact.Metadata); err != nil {
				return fmt.Errorf("artifact %q metadata: %w", artifact.ID, err)
			}
		}
		provided[requirementName] = struct{}{}
	}
	for _, requirement := range requirements {
		if requirement.Required {
			if _, exists := provided[strings.TrimSpace(requirement.Name)]; !exists {
				return fmt.Errorf("required artifact %q was not provided", requirement.Name)
			}
		}
	}
	return nil
}

func validateOpaqueArtifactRef(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 1024 {
		return errors.New("opaque reference must be between 1 and 1024 characters")
	}
	if strings.ContainsAny(value, "\r\n?#@") || strings.Contains(value, "://") {
		return errors.New("opaque reference cannot contain a URL, query, fragment, user info, or line break")
	}
	return nil
}

func compileArtifactSchema(name string, schema map[string]interface{}) (*jsonschema.Schema, error) {
	if err := validateLocalSchemaReferences(schema); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	resource := "artifact-" + strings.TrimSpace(name) + ".json"
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

func validateArtifactSchema(schema *jsonschema.Schema, value interface{}) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	if err := schema.Validate(document); err != nil {
		return fmt.Errorf("JSON schema validation failed: %w", err)
	}
	return nil
}

func validateLocalSchemaReferences(value interface{}) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, child := range typed {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, "#") {
					return errors.New("external JSON Schema references are not allowed")
				}
			}
			if err := validateLocalSchemaReferences(child); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, child := range typed {
			if err := validateLocalSchemaReferences(child); err != nil {
				return err
			}
		}
	}
	return nil
}

func CloneArtifactRequirements(in []ArtifactRequirement) []ArtifactRequirement {
	result := make([]ArtifactRequirement, len(in))
	for index, requirement := range in {
		result[index] = requirement
		result[index].Schema = maps.Clone(requirement.Schema)
	}
	return result
}

func CloneArtifactReferences(in []ArtifactReference) []ArtifactReference {
	result := make([]ArtifactReference, len(in))
	for index, artifact := range in {
		result[index] = artifact
		result[index].Metadata = maps.Clone(artifact.Metadata)
		result[index].EvidenceRefs = append([]string(nil), artifact.EvidenceRefs...)
	}
	return result
}
