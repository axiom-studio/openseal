package skill

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// DefinitionDigest identifies the complete, validated portable Skill definition.
// It hashes the JSON encoding of NewManifest, which omits catalog diagnostics
// and includes the source, executable declaration, and governed capabilities.
// Hosts may use this identity to share an exact runtime without conflating
// definitions that happen to have the same ID and version.
func DefinitionDigest(definition *Definition) (string, error) {
	// NewManifest uses the existing permissive cloning helper. Reject values
	// it cannot serialize before cloning can silently discard their fields.
	if _, err := json.Marshal(definition); err != nil {
		return "", fmt.Errorf("encode Skill definition for digest: %w", err)
	}
	manifest, err := NewManifest(definition)
	if err != nil {
		return "", fmt.Errorf("validate Skill definition for digest: %w", err)
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("encode Skill definition for digest: %w", err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(encoded)), nil
}
