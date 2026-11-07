package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// RepeatEffect is the D9 class of one tool call (section 08 §5.1, SPEC §5.4).
// It is stored as a string in ToolEntry.RepeatEffect.
type RepeatEffect string

const (
	RepeatEffectRead     RepeatEffect = "read"     // streak rule
	RepeatEffectWrite    RepeatEffect = "write"    // duplicate rule once it succeeded in this Run; streak rule before
	RepeatEffectStateful RepeatEffect = "stateful" // streak rule; never suppressed; its success resets other keys
	RepeatEffectControl  RepeatEffect = "control"  // never counted
)

func (e RepeatEffect) Valid() bool {
	switch e {
	case RepeatEffectRead, RepeatEffectWrite, RepeatEffectStateful, RepeatEffectControl:
		return true
	}
	return false
}

// LoopToolKind says which executor resolves a tool call of the loop.
type LoopToolKind string

const (
	LoopToolGoverned LoopToolKind = "governed" // Skill/MCP action through ActionCoordinator
	LoopToolHost     LoopToolKind = "host"     // host tool (workspace.*) dispatched to the step host
	LoopToolKernel   LoopToolKind = "kernel"   // kernel control tool (final, wait, fork, ...)
)

// Kernel tools by D9 class (SPEC §5.4 table, A11). A kernel tool not listed
// here is treated as a write: identical to an earlier success it is suppressed.
var (
	repeatKernelWriteTools = map[string]bool{
		"control_fork":            true,
		"agent_delegate":          true,
		"agent_delegate_reply":    true,
		"run_operation":           true,
		"conversation_start_task": true,
		"ask_user":                true,
		"react":                   true,
	}
	repeatKernelReadTools = map[string]bool{
		"search_capabilities": true,
	}
	repeatKernelControlTools = map[string]bool{
		"turn_final_answer":      true,
		"agent_request_decision": true,
		"agent_request_review":   true,
		"turn_report_progress":   true,
		"wait":                   true,
		"load_capabilities":      true,
	}
	// Host reads; every other host operation (writes, copy_from_chat,
	// run_command) is stateful.
	repeatHostReadOperations = map[string]bool{
		workspace.OperationListDirectory: true,
		workspace.OperationReadFile:      true,
		workspace.OperationSearchFiles:   true,
		workspace.OperationBatchRead:     true,
		workspace.OperationListChats:     true,
	}
)

// repeatGovernedOperation is the operation name repeatEffectOf takes for a
// governed call: skillID + "." + action.
func repeatGovernedOperation(skillID, action string) string {
	return skillID + "." + action
}

// repeatGovernedCardOperation reports whether a governed call is an in-chat
// card (setup, credential, connection request): those are control.
func repeatGovernedCardOperation(operation string) bool {
	prefix := SkillManagementSkillID + "."
	if !strings.HasPrefix(operation, prefix) {
		return false
	}
	return isInteractionRequestAction(strings.TrimPrefix(operation, prefix))
}

// repeatEffectOf classifies one call. toolKind/sideEffect/repeatable come from
// the executor's tool resolution. operation is the host operation name for host
// tools (workspace.read_file), the kernel tool name for kernel tools, and
// repeatGovernedOperation(skillID, action) for governed calls.
func repeatEffectOf(toolKind LoopToolKind, sideEffect capability.SideEffect, repeatable bool, operation string) RepeatEffect {
	switch toolKind {
	case LoopToolKernel:
		switch {
		case repeatKernelControlTools[operation]:
			return RepeatEffectControl
		case repeatKernelReadTools[operation]:
			return RepeatEffectRead
		default:
			return RepeatEffectWrite
		}
	case LoopToolHost:
		if repeatHostReadOperations[operation] {
			return RepeatEffectRead
		}
		return RepeatEffectStateful
	case LoopToolGoverned:
		if repeatGovernedCardOperation(operation) {
			return RepeatEffectControl
		}
		switch sideEffect {
		case capability.SideEffectNone, capability.SideEffectRead:
			return RepeatEffectRead
		}
		// write / external / destructive, and an unknown side effect (fail closed).
		if repeatable {
			return RepeatEffectStateful
		}
		return RepeatEffectWrite
	}
	return RepeatEffectWrite
}

// RepeatSubject is what the D9 key of one call is computed from.
type RepeatSubject struct {
	// Identity is stable across steps (not the provider-facing tool name):
	//   governed: repeatGovernedIdentity(skillID, action, bindingID)
	//   host:     the operation name, e.g. "workspace.read_file"
	//   kernel:   the kernel tool name
	Identity string
	// Arguments are the EXECUTED arguments: for governed calls the inner action
	// arguments handed to ActionCoordinator (review envelope removed); for host
	// tools the tool args.
	Arguments         map[string]interface{}
	ExternalOperation *ExternalOperationIdentity
	Effect            RepeatEffect
}

// repeatGovernedIdentity = skillID + "." + action + "@" + bindingID.
func repeatGovernedIdentity(skillID, action, bindingID string) string {
	return skillID + "." + action + "@" + bindingID
}

// Top-level argument fields that never change what a call does.
var repeatControlArgumentFields = []string{"_reviewSummary", "reviewContext", "evidenceRefs", "idempotencyKey"}

// canonicalToolArguments returns the canonical JSON of a call's arguments:
// keys sorted recursively, object members whose value is null removed
// recursively, numbers re-encoded as an integer when integral and else with
// strconv.FormatFloat(v, 'g', -1, 64), strings, booleans and array order
// untouched, and the top-level control fields removed.
func canonicalToolArguments(arguments map[string]interface{}) ([]byte, error) {
	normalized, err := canonicalJSONValue(arguments)
	if err != nil {
		return nil, err
	}
	object, _ := normalized.(map[string]interface{})
	if object == nil {
		object = map[string]interface{}{}
	}
	for _, field := range repeatControlArgumentFields {
		delete(object, field)
	}
	return encodeCanonicalJSON(object)
}

// canonicalJSONValue round-trips value through JSON with UseNumber and
// normalizes the result (nulls dropped from objects, numbers canonical).
func canonicalJSONValue(value interface{}) (interface{}, error) {
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
func encodeCanonicalJSON(value interface{}) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, fmt.Errorf("canonical arguments: %w", err)
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// repeatKey = hex(sha256(Identity + "\x00" + canonicalArgs + "\x00" + extOp)),
// where extOp = canonicalExternalOperationResource(Resource) + "\x00" +
// Operation, or "" without an external operation. Stored in
// ToolEntry.ArgumentsDigest.
func repeatKey(subject RepeatSubject) (string, error) {
	if strings.TrimSpace(subject.Identity) == "" {
		return "", errors.New("repeat key: identity is required")
	}
	arguments, err := canonicalToolArguments(subject.Arguments)
	if err != nil {
		return "", err
	}
	externalOperation := ""
	if subject.ExternalOperation != nil {
		if err := subject.ExternalOperation.Validate(); err != nil {
			return "", fmt.Errorf("repeat key: %w", err)
		}
		resource, err := canonicalExternalOperationResource(subject.ExternalOperation.Resource)
		if err != nil {
			return "", fmt.Errorf("repeat key: %w", err)
		}
		externalOperation = resource + "\x00" + subject.ExternalOperation.Operation
	}
	hash := sha256.New()
	hash.Write([]byte(subject.Identity))
	hash.Write([]byte{0})
	hash.Write(arguments)
	hash.Write([]byte{0})
	hash.Write([]byte(externalOperation))
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// toolResultDigest = hex(sha256(canonical JSON {"status": Status, "content":
// Content})) over the stored ToolResult. For governed calls the stored content
// carries the action error code, so "the same error" is "the same result".
// It returns "" for a nil result or content that cannot be encoded.
func toolResultDigest(result *ToolResult) string {
	if result == nil {
		return ""
	}
	content := result.Content
	if content == nil {
		content = []ContentPart{}
	}
	normalized, err := canonicalJSONValue(map[string]interface{}{"status": result.Status, "content": content})
	if err != nil {
		return ""
	}
	encoded, err := encodeCanonicalJSON(normalized)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
