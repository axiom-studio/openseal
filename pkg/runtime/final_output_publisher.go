package runtime

import (
	"context"
	"encoding/json"
)

// FinalOutputPublisher lets an embedding host add fields to turn_final_answer
// that it stores itself (the desktop's generated files). The kernel offers the
// fields, validates them through the host before accepting the final call, and
// publishes them before the Run completes. Published output never contains the
// field bodies.
type FinalOutputPublisher interface {
	// FinalAnswerProperties returns extra optional JSON Schema properties for
	// turn_final_answer on this Run, or nil. Never required properties.
	FinalAnswerProperties(run *AgentRun) map[string]interface{}
	// ValidateFinalAnswer checks only those fields. An error becomes an
	// is_error result on the final tool call (the model may correct it).
	ValidateFinalAnswer(run *AgentRun, fields map[string]json.RawMessage) error
	// PublishFinalOutput stores the fields and returns keys to merge into
	// Run.Output (e.g. artifactRefs). It must be idempotent for the same
	// (Run, StepID, ToolCallID) so crash resume can call it again.
	PublishFinalOutput(ctx context.Context, publication FinalOutputPublication) (map[string]interface{}, error)
}

// FinalOutputPublication is one publish request for the host extension fields
// of an accepted turn_final_answer call.
type FinalOutputPublication struct {
	Run        *AgentRun
	StepID     string // agent_turns.id of the step whose assistant entry holds the call
	ToolCallID string
	Fields     map[string]json.RawMessage // exactly the FinalAnswerProperties keys present in the call
}
