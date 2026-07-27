// Package artifactoutput defines the portable boundary between a Skill that
// generates bytes and a host that promotes those bytes into its durable
// Artifact content store. Transient bytes must never cross into model-visible
// action output, checkpoints, or activity.
package artifactoutput

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"slices"
	"strings"
)

const (
	EncodedBytesField = "_transientArtifactBase64"
	MaximumBytes      = 32 << 20
)

type Envelope struct {
	Bytes           []byte
	Filename        string
	RequirementName string
	ArtifactType    string
	MediaType       string
	Digest          string
}

// Consume validates and removes the private transport fields. The returned
// public output is safe to enrich with durable artifactRefs before validation
// against the action's declared output schema.
func Consume(output map[string]interface{}, allowedTypes []string) (*Envelope, map[string]interface{}, error) {
	if output == nil {
		return nil, nil, nil
	}
	encoded, present := output[EncodedBytesField]
	if !present {
		return nil, clone(output), nil
	}
	if len(allowedTypes) == 0 {
		return nil, nil, errors.New("action returned generated artifact bytes without declaring emittedArtifactTypes")
	}
	text, ok := encoded.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, nil, errors.New("generated artifact bytes are missing")
	}
	bytes, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(bytes) == 0 || len(bytes) > MaximumBytes {
		return nil, nil, fmt.Errorf("generated artifact bytes must be valid base64 between 1 byte and %d bytes", MaximumBytes)
	}
	filename, _ := output["filename"].(string)
	requirement, _ := output["requirementName"].(string)
	artifactType, _ := output["artifactType"].(string)
	mediaType, _ := output["mediaType"].(string)
	digest, _ := output["digest"].(string)
	filename = strings.TrimSpace(filename)
	requirement = strings.TrimSpace(requirement)
	artifactType = strings.TrimSpace(artifactType)
	mediaType = strings.TrimSpace(mediaType)
	if filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, `/\\`) {
		return nil, nil, errors.New("generated artifact filename is invalid")
	}
	if requirement == "" || artifactType == "" || !slices.Contains(allowedTypes, artifactType) {
		return nil, nil, errors.New("generated artifact requirement or declared type is invalid")
	}
	if parsed, _, parseErr := mime.ParseMediaType(mediaType); parseErr != nil || parsed == "" {
		return nil, nil, errors.New("generated artifact media type is invalid")
	}
	actualDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(bytes))
	if digest != actualDigest {
		return nil, nil, errors.New("generated artifact digest does not match its bytes")
	}
	if size, ok := numericInt64(output["sizeBytes"]); !ok || size != int64(len(bytes)) {
		return nil, nil, errors.New("generated artifact size does not match its bytes")
	}
	public := clone(output)
	for _, field := range []string{EncodedBytesField, "filename", "requirementName", "artifactType", "mediaType", "digest", "sizeBytes"} {
		delete(public, field)
	}
	return &Envelope{Bytes: bytes, Filename: filename, RequirementName: requirement, ArtifactType: artifactType, MediaType: mediaType, Digest: actualDigest}, public, nil
}

func clone(value map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func numericInt64(value interface{}) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int64:
		return typed, true
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed), true
		}
	}
	return 0, false
}
