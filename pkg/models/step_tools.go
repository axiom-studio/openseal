package models

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
)

const (
	CapabilityActionPrefix = "action:"
)

// SurfacedTool is the frozen definition of one tool a load made callable.
type SurfacedTool struct {
	// Capability is the canonical capability ID (action:<name> or
	// workspace:<operation>).
	Capability  string                 `json:"capability"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

func CloneSurfacedTools(in []SurfacedTool) []SurfacedTool {
	if in == nil {
		return nil
	}
	out := make([]SurfacedTool, len(in))
	for i, tool := range in {
		tool.InputSchema = CloneJSONObject(tool.InputSchema)
		out[i] = tool
	}
	return out
}

// CloneJSONObject deep-copies a JSON object as its encoding would decode.
func CloneJSONObject(in map[string]interface{}) map[string]interface{} {
	if in == nil {
		return nil
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		return maps.Clone(in)
	}
	var out map[string]interface{}
	if json.Unmarshal(encoded, &out) != nil {
		return maps.Clone(in)
	}
	return out
}

// ModelStepTool is the loaded-group declaration of a frozen tool.
func (t SurfacedTool) ModelStepTool(surface string) ModelStepTool {
	tool := ModelStepTool{
		Name: t.Name, Description: t.Description, InputSchema: CloneJSONObject(t.InputSchema),
		Group: ModelStepToolLoaded, Surface: surface,
	}
	if strings.HasPrefix(t.Capability, CapabilityActionPrefix) {
		tool.Capability = strings.TrimPrefix(t.Capability, CapabilityActionPrefix)
	}
	return tool
}

type ModelStepToolGroup string

const (
	ModelStepToolHarness ModelStepToolGroup = "harness" // final answer, wait, capability search/load, progress, workspace, child work
	ModelStepToolCore    ModelStepToolGroup = "core"    // actions fixed at session start
	ModelStepToolLoaded  ModelStepToolGroup = "loaded"  // capabilities loaded during the session: carried, then surfaced in path order
)

func (g ModelStepToolGroup) Rank() int {
	switch g {
	case ModelStepToolHarness:
		return 1
	case ModelStepToolCore:
		return 2
	case ModelStepToolLoaded:
		return 3
	}
	return 0
}

type ModelStepTool struct {
	Name        string                 `json:"name"` // ^[a-zA-Z0-9_-]{1,64}$, unique
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"` // JSON Schema object, type "object"
	Group       ModelStepToolGroup     `json:"group"`
	// Capability is the action name of a governed tool. The host uses it
	// only to pick browser guidance and metrics.
	Capability string `json:"capability,omitempty"`
	// Surface is the id of the tool entry (a load_capabilities result in
	// Entries) after which the tool became callable; that entry lists the
	// tool's name in SessionEntryView.Surfaces. Only loaded tools may set it.
	// "" means the tool is callable from the first entry: every harness and
	// core tool, and loaded tools carried over a compaction. Surface is
	// neutral data: a host may declare a surfaced tool from the start
	// (append-only tool list) or reveal it at its surfacing entry.
	Surface string `json:"surface,omitempty"`
}

// ModelStepProducerStore is what the producer uses: the session path it
// renders, the governed calls whose screenshots it resolves, and the append
// of the step's capabilities note. KernelStore satisfies it.
type ModelStepProducerStore interface {
	LoadAgentSessionPath(ctx context.Context, scope Scope, sessionID string) (*AgentSessionPath, error)
	GetActionCall(ctx context.Context, scope Scope, actionID string) (*ActionCall, error)
	AppendAgentSessionEntries(ctx context.Context, req AppendAgentSessionEntriesRequest) (*AppendAgentSessionEntriesResult, error)
}

type HostedSkillDisposition string

const (
	HostedSkillApplied    HostedSkillDisposition = "applied"
	HostedSkillNotApplied HostedSkillDisposition = "not_applied"
)

// HostedSkillSelection is the operator-facing disposition of one prompt Skill
// offered to a step. It records capability choice without storing hidden
// model reasoning.
type HostedSkillSelection struct {
	SkillRef    string                 `json:"skillRef"`
	Disposition HostedSkillDisposition `json:"disposition"`
	Summary     string                 `json:"summary"`
}
