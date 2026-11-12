package models

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const (
	MinimumAskUserOptions                = 2
	MaximumAskUserOptions                = 6
	MaximumAskUserOptionLabelBytes       = 80
	MaximumAskUserOptionDescriptionBytes = 240
)

type ControlToolName string

const (
	ControlToolFinalAnswer      ControlToolName = "turn_final_answer"
	ControlToolRequestDecision  ControlToolName = "agent_request_decision"
	ControlToolCompletionReview ControlToolName = "agent_request_review"
	ControlToolReportProgress   ControlToolName = "turn_report_progress"
	ControlToolWait             ControlToolName = "wait"
	ControlToolAskUser          ControlToolName = "ask_user"
	ControlToolReact            ControlToolName = "react"
	ControlToolStartTask        ControlToolName = "conversation_start_task"
	ControlToolFork             ControlToolName = "control_fork"
	ControlToolRunOperation     ControlToolName = "run_operation"
	ControlToolDelegate         ControlToolName = "agent_delegate"
	ControlToolDelegateReply    ControlToolName = "agent_delegate_reply"
	// Kernel-only calls of the runbook StepProducer (§12); never offered.
	ControlToolRunbookFork     ControlToolName = "runbook_fork"
	ControlToolRunbookDelegate ControlToolName = "runbook_delegate"
)

// ControlJSONResult is a control tool result: one json part holding value,
// compact. value["status"] defaults to the result status.
func ControlJSONResult(status ToolResultStatus, value map[string]interface{}) *ToolResult {
	if value == nil {
		value = map[string]interface{}{}
	}
	if _, ok := value["status"]; !ok {
		value["status"] = string(status)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded, _ = json.Marshal(map[string]interface{}{"status": string(ToolResultFailed), "error": "result_unencodable", "message": "The tool result could not be encoded."})
		status = ToolResultFailed
	}
	return &ToolResult{
		Status: status, IsError: status != ToolResultSucceeded,
		Content: []ContentPart{{Type: ContentPartJSON, Value: encoded}},
	}
}

// decodeControlArguments decodes raw strictly: unknown fields are refused and
// raw must hold exactly one JSON value. Empty input decodes as {}.
func DecodeControlArguments(raw json.RawMessage, target interface{}) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("arguments must be one JSON object")
	}
	return nil
}

// decodeControlInto is the decode of a handler whose arguments are T.
func DecodeControlInto[T any](raw json.RawMessage) (interface{}, error) {
	var value T
	if err := DecodeControlArguments(raw, &value); err != nil {
		return nil, err
	}
	return &value, nil
}
