package types

import "encoding/json"

// AgentLibraryVersionExport is the portable YAML export format for agent library versions.
// It includes library metadata along with the version specification for easy import.
type AgentLibraryVersionExport struct {
	APIVersion string                            `yaml:"apiVersion" json:"apiVersion"`
	Kind       string                            `yaml:"kind" json:"kind"`
	Metadata   AgentLibraryVersionExportMetadata `yaml:"metadata" json:"metadata"`
	Spec       AgentLibraryVersionExportSpec     `yaml:"spec" json:"spec"`
}

// AgentLibraryVersionExportMetadata contains library and version metadata
type AgentLibraryVersionExportMetadata struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Icon        string `yaml:"icon,omitempty" json:"icon,omitempty"`
	Category    string `yaml:"category,omitempty" json:"category,omitempty"`
	Version     string `yaml:"version" json:"version"`
	// Marketplace-specific metadata (optional)
	Author      string   `yaml:"author,omitempty" json:"author,omitempty"`
	AuthorEmail string   `yaml:"authorEmail,omitempty" json:"authorEmail,omitempty"`
	Tags        []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	License     string   `yaml:"license,omitempty" json:"license,omitempty"`
	Homepage    string   `yaml:"homepage,omitempty" json:"homepage,omitempty"`
}

// AgentWorkflowExport is the export format for a workflow
type AgentWorkflowExport struct {
	Id          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	IsDefault   bool   `yaml:"isDefault" json:"isDefault"`
}

// AgentLibraryVersionExportSpec contains the agent version specification
type AgentLibraryVersionExportSpec struct {
	Nodes        []AgentNodeExport       `yaml:"nodes" json:"nodes"`
	Connections  []AgentConnectionExport `yaml:"connections,omitempty" json:"connections,omitempty"`
	Workflows    []AgentWorkflowExport   `yaml:"workflows,omitempty" json:"workflows,omitempty"`
	Persona      map[string]interface{}  `yaml:"persona,omitempty" json:"persona,omitempty"`
	InputSchema  map[string]InputField   `yaml:"inputSchema,omitempty" json:"inputSchema,omitempty"`
	OutputSchema map[string]OutputField  `yaml:"outputSchema,omitempty" json:"outputSchema,omitempty"`
	// Dependencies for marketplace templates (optional)
	Dependencies *AgentDependencies `yaml:"dependencies,omitempty" json:"dependencies,omitempty"`
}

// AgentNodeExport is the export format for an agent workflow node
type AgentNodeExport struct {
	Id          string                 `yaml:"id" json:"id"`
	Name        string                 `yaml:"name" json:"name"`
	Description string                 `yaml:"description,omitempty" json:"description,omitempty"`
	Type        string                 `yaml:"type" json:"type"`
	Position    PositionFloatExport    `yaml:"position" json:"position"`
	Config      map[string]interface{} `yaml:"config,omitempty" json:"config,omitempty"`
	WorkflowId  string                 `yaml:"workflowId,omitempty" json:"workflowId,omitempty"`
}

// PositionFloatExport represents node position with float precision
type PositionFloatExport struct {
	X float64 `yaml:"x" json:"x"`
	Y float64 `yaml:"y" json:"y"`
}

// AgentConnectionExport is the export format for connections between agent nodes
type AgentConnectionExport struct {
	Id           string `yaml:"id" json:"id"`
	SourceNodeId string `yaml:"sourceNodeId" json:"sourceNodeId"`
	TargetNodeId string `yaml:"targetNodeId" json:"targetNodeId"`
	SourceHandle string `yaml:"sourceHandle,omitempty" json:"sourceHandle,omitempty"`
	TargetHandle string `yaml:"targetHandle,omitempty" json:"targetHandle,omitempty"`
	Label        string `yaml:"label,omitempty" json:"label,omitempty"`
	WorkflowId   string `yaml:"workflowId,omitempty" json:"workflowId,omitempty"`
}

// Export format constants
const (
	AgentExportAPIVersion = "axiom.studio/v1"
	AgentExportKind       = "AgentLibraryVersion"
)

// AgentDependencies specifies the dependencies required by an agent template
type AgentDependencies struct {
	// Skills required by the agent (e.g., core, k8s). Accepts both ["core"] and [{"id":"core"}].
	Skills []SkillDependency `yaml:"skills,omitempty" json:"skills,omitempty"`
	// Node types required by the agent (e.g., webhook, http, code)
	NodeTypes []string `yaml:"nodeTypes,omitempty" json:"nodeTypes,omitempty"`
	// Permissions required by the agent (e.g., http:outbound, k8s:read)
	Permissions []string `yaml:"permissions,omitempty" json:"permissions,omitempty"`
}

func (d *AgentDependencies) UnmarshalJSON(data []byte) error {
	type raw AgentDependencies
	var r raw
	if err := json.Unmarshal(data, &r); err != nil {
		// Try parsing skills as an array of strings (backward compat)
		var soft map[string]json.RawMessage
		if err2 := json.Unmarshal(data, &soft); err2 != nil {
			return err
		}
		r.NodeTypes = unmarshalStringArray(soft["nodeTypes"])
		r.Permissions = unmarshalStringArray(soft["permissions"])
		r.Skills = unmarshalSkillArray(soft["skills"])
	}
	*d = AgentDependencies(r)
	return nil
}

// SkillDependency represents a dependency on a skill
type SkillDependency struct {
	// ID is the skill identifier (e.g., "core", "k8s")
	ID string `yaml:"id" json:"id"`
	// Version constraint (e.g., ">=1.0.0", "1.0.0-2.0.0")
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

func (s *SkillDependency) UnmarshalJSON(data []byte) error {
	var id string
	if err := json.Unmarshal(data, &id); err == nil {
		s.ID = id
		return nil
	}
	type alias SkillDependency
	return json.Unmarshal(data, (*alias)(s))
}

func unmarshalStringArray(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var arr []string
	_ = json.Unmarshal(raw, &arr)
	if arr != nil {
		return arr
	}
	// Try array of objects and extract "id" or "name"
	var objs []map[string]string
	if err := json.Unmarshal(raw, &objs); err == nil {
		for _, o := range objs {
			for _, v := range o {
				arr = append(arr, v)
				break
			}
		}
	}
	return arr
}

func unmarshalSkillArray(raw json.RawMessage) []SkillDependency {
	if len(raw) == 0 {
		return nil
	}
	var skills []SkillDependency
	if err := json.Unmarshal(raw, &skills); err == nil {
		return skills
	}
	var ids []string
	if err := json.Unmarshal(raw, &ids); err == nil {
		for _, id := range ids {
			skills = append(skills, SkillDependency{ID: id})
		}
	}
	return skills
}
