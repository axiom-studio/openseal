package modelstep

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/axiom-studio/openseal/pkg/models"
)

// canonicalJSONValue round-trips value through JSON with UseNumber and
// normalizes the result (nulls dropped from objects, numbers canonical).
func CanonicalJSONValue(value interface{}) (interface{}, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonical arguments: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded interface{}
	if err := decoder.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("canonical arguments: %w", err)
	}
	return normalizeCanonicalJSON(decoded)
}

func normalizeCanonicalJSON(value interface{}) (interface{}, error) {
	switch typed := value.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, member := range typed {
			if member == nil {
				continue
			}
			normalized, err := normalizeCanonicalJSON(member)
			if err != nil {
				return nil, err
			}
			out[key] = normalized
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(typed))
		for i, item := range typed {
			normalized, err := normalizeCanonicalJSON(item)
			if err != nil {
				return nil, err
			}
			out[i] = normalized
		}
		return out, nil
	case json.Number:
		return canonicalJSONNumber(typed)
	}
	return value, nil
}

func canonicalJSONNumber(number json.Number) (json.Number, error) {
	literal := number.String()
	if integer, ok := new(big.Int).SetString(literal, 10); ok {
		return json.Number(integer.String()), nil
	}
	value, err := strconv.ParseFloat(literal, 64)
	if err != nil {
		return "", fmt.Errorf("canonical arguments: number %q: %w", literal, err)
	}
	exact := new(big.Float).SetFloat64(value)
	if exact.IsInt() {
		integer, _ := exact.Int(nil)
		return json.Number(integer.String()), nil
	}
	return json.Number(strconv.FormatFloat(value, 'g', -1, 64)), nil
}

// encodeCanonicalJSON encodes a normalized value: encoding/json sorts map keys;
// HTML escaping is off so strings stay as the model sent them.
func EncodeCanonicalJSON(value interface{}) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("canonical arguments: %w", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// toolResultDigest = hex(sha256(canonical JSON {"status": Status, "content":
// Content})) over the stored ToolResult. For governed calls the stored content
// carries the action error code, so "the same error" is "the same result".
// The per-call action reference (a json part's top-level "evidenceRef", an
// image part's actionCallId) is left out: every governed call has its own
// action_calls row, so two identical results would otherwise never match.
// It returns "" for a nil result or content that cannot be encoded.
func ToolResultDigest(result *models.ToolResult) string {
	if result == nil {
		return ""
	}
	content := make([]models.ContentPart, len(result.Content))
	copy(content, result.Content)
	for i := range content {
		content[i].ActionCallID = ""
	}
	normalized, err := CanonicalJSONValue(map[string]interface{}{"status": result.Status, "content": content})
	if err != nil {
		return ""
	}
	if parts, ok := normalized.(map[string]interface{})["content"].([]interface{}); ok {
		for _, part := range parts {
			if typed, ok := part.(map[string]interface{}); ok && typed["type"] == models.ContentPartJSON {
				if value, ok := typed["value"].(map[string]interface{}); ok {
					delete(value, "evidenceRef")
				}
			}
		}
	}
	encoded, err := EncodeCanonicalJSON(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
