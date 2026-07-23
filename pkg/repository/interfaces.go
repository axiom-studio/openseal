package repository

import (
	"time"
)

type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusCompleted RunStatus = "completed"
	RunStatusFailed    RunStatus = "failed"
)

type AgentRun struct {
	ID            int
	InstanceID    int
	TriggerNodeID string
	TriggeredBy   string
	TriggerData   string
	Status        RunStatus
	Error         string
	StartedAt     time.Time
	CompletedAt   *time.Time
	Ephemeral     bool
}

type AgentStepRun struct {
	ID          int
	RunID       int
	StepName    string
	StepType    string
	StepIndex   int
	Status      RunStatus
	Input       string
	Output      string
	Error       string
	StartedAt   *time.Time
	CompletedAt *time.Time
}

type AgentRunRepository interface {
	CreateRun(run *AgentRun) (*AgentRun, error)
	GetRun(id int) (*AgentRun, error)
	UpdateRunStatus(id int, status RunStatus, errMsg string) error
	SaveStepRun(step *AgentStepRun) (*AgentStepRun, error)
}

type AgentInstance struct {
	ID            int
	Name          string
	BlueprintID   int
	Bindings      string
	Enabled       bool
	Status        string
	Timeout       int
	EnvironmentId int
}

type AgentInstanceRepository interface {
	GetInstance(id int) (*AgentInstance, error)
	ListInstances(blueprintID int) ([]*AgentInstance, error)
	FindById(id int) (*AgentInstance, error)
}

type AgentSkill struct {
	ID           int
	SkillID      string
	Name         string
	Description  string
	Version      string
	Author       string
	Category     string
	Tags         []string
	ManifestPath string
	RepoURL      string
	IsEnabled    bool
}

type AgentSkillRepository interface {
	SaveSkill(skill *AgentSkill) (*AgentSkill, error)
	GetSkill(skillID string) (*AgentSkill, error)
	ListSkills() ([]*AgentSkill, error)
}
