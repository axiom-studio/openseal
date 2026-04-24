package skill

import (
	"context"
	"time"
)

type SkillConnection struct {
	Id      string                 `json:"id"`
	SkillId string                 `json:"skillId"`
	Scope   string                 `json:"scope"`
	ScopeId string                 `json:"scopeId"`
	Name    string                 `json:"name"`
	Values  map[string]interface{} `json:"values"`
	CreatedBy string               `json:"createdBy"`
	CreatedAt time.Time            `json:"createdAt"`
	UpdatedAt time.Time            `json:"updatedAt"`
}

type SkillConnectionAssignment struct {
	Id           int       `json:"id"`
	ConnectionId string    `json:"connectionId"`
	PersonaId    string    `json:"personaId"`
}

type AgentInstanceSkill struct {
	ID               int                    `json:"id"`
	AgentInstanceID  int                    `json:"agentInstanceID"`
	SkillID          string                 `json:"skillId"`
	Name             string                 `json:"name"`
	SelectedNodeTypes []string              `json:"selectedNodeTypes"`
	FieldOverrides   map[string]interface{} `json:"fieldOverrides"`
	IsEnabled        bool                   `json:"isEnabled"`
	CreatedAt        time.Time             `json:"createdAt"`
}

type AgentInstanceSkillRepository interface {
	ListSkillsByAgent(ctx context.Context, agentInstanceID int) ([]*AgentInstanceSkill, error)
}

type SkillConnectionRepository interface {
	CreateConnection(ctx context.Context, connection *SkillConnection) (*SkillConnection, error)
	GetConnection(ctx context.Context, id string) (*SkillConnection, error)
	ListConnectionsByScope(ctx context.Context, scope, scopeId string) ([]*SkillConnection, error)
	ListConnectionsByPersona(ctx context.Context, personaId string) ([]*SkillConnection, error)
	ListAllConnections(ctx context.Context) ([]*SkillConnection, error)
	UpdateConnection(ctx context.Context, connection *SkillConnection) error
	DeleteConnection(ctx context.Context, id string) error
	AssignConnectionToPersona(ctx context.Context, connectionId, personaId string) error
}
