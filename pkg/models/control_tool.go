package models

import "encoding/json"

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
