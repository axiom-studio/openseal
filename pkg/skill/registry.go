package skill

import (
	"encoding/json"
	"time"
)

type Skill struct {
	ID             int       `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	Category       string    `json:"category"`
	AgentLibraryID int       `json:"agentLibraryId"`
	InputSchema    []byte    `json:"inputSchema"`
	OutputSchema   []byte    `json:"outputSchema"`
	Implementation []byte    `json:"implementation"`
	Embedding      []float32 `json:"embedding"`
	CreatedAt      time.Time `json:"createdAt"`
}

type SkillRegistry interface {
	Register(skillID string, nodeTypes []NodeDefinition) error
	GetNodeTypes(skillID string) ([]NodeDefinition, error)
	ListSkills() []string
	Unregister(skillID string) error
}

type NodeDefinition struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema string `json:"inputSchema"`
	OutputSchema string `json:"outputSchema"`
}

func (s *Skill) GetInputSchema() (map[string]interface{}, error) {
	if len(s.InputSchema) == 0 {
		return nil, nil
	}
	var schema map[string]interface{}
	err := json.Unmarshal(s.InputSchema, &schema)
	return schema, err
}

func (s *Skill) GetOutputSchema() (map[string]interface{}, error) {
	if len(s.OutputSchema) == 0 {
		return nil, nil
	}
	var schema map[string]interface{}
	err := json.Unmarshal(s.OutputSchema, &schema)
	return schema, err
}

func (s *Skill) GetImplementation() (map[string]interface{}, error) {
	if len(s.Implementation) == 0 {
		return nil, nil
	}
	var impl map[string]interface{}
	err := json.Unmarshal(s.Implementation, &impl)
	return impl, err
}

func (s *Skill) SetInputSchema(schema map[string]interface{}) error {
	data, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	s.InputSchema = data
	return nil
}

func (s *Skill) SetOutputSchema(schema map[string]interface{}) error {
	data, err := json.Marshal(schema)
	if err != nil {
		return err
	}
	s.OutputSchema = data
	return nil
}

func (s *Skill) SetImplementation(impl map[string]interface{}) error {
	data, err := json.Marshal(impl)
	if err != nil {
		return err
	}
	s.Implementation = data
	return nil
}
