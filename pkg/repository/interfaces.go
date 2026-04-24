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

type TriggerType string

const (
	TriggerTypeWebhook  TriggerType = "webhook"
	TriggerTypeSchedule TriggerType = "schedule"
	TriggerTypeManual   TriggerType = "manual"
	TriggerTypeEvent    TriggerType = "event"
	TriggerTypeK8sWatch TriggerType = "k8s-watch"
	TriggerTypeK8sEvent TriggerType = "k8s-event"
	TriggerTypeCron     TriggerType = "cron"
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
	ID         int
	RunID      int
	StepName   string
	StepType   string
	StepIndex  int
	Status     RunStatus
	Input      string
	Output     string
	Error      string
	StartedAt  *time.Time
	CompletedAt *time.Time
}

type AgentRunRepository interface {
	CreateRun(run *AgentRun) (*AgentRun, error)
	GetRun(id int) (*AgentRun, error)
	UpdateRunStatus(id int, status RunStatus, errMsg string) error
	SaveStepRun(step *AgentStepRun) (*AgentStepRun, error)
}

type AgentInstance struct {
	ID          int
	Name        string
	BlueprintID int
	Bindings    string
	Enabled     bool
	Status      string
	Timeout     int
	EnvironmentId int
}

type AgentInstanceRepository interface {
	GetInstance(id int) (*AgentInstance, error)
	ListInstances(blueprintID int) ([]*AgentInstance, error)
	FindById(id int) (*AgentInstance, error)
}

type AgentTrigger struct {
	TableName       struct{}  `sql:"agent_trigger"`
	Id              int       `sql:"id,pk"`
	AgentInstanceId int       `sql:"agent_instance_id"`
	NodeId          string    `sql:"node_id"`
	TriggerType     string    `sql:"trigger_type"`
	Config          string    `sql:"config"`
	WebhookPath     string    `sql:"webhook_path"`
	CronExpression  string    `sql:"cron_expression"`
	InputSchema     string    `sql:"input_schema"`
	Enabled         bool      `sql:"enabled,notnull"`
	LastTriggeredAt time.Time `sql:"last_triggered_at"`
	TriggerCount    int       `sql:"trigger_count"`
	WorkflowId      *int      `sql:"workflow_id"`
}

type AgentTriggerRepository interface {
	Save(model *AgentTrigger) (*AgentTrigger, error)
	Update(model *AgentTrigger) (*AgentTrigger, error)
	FindById(id int) (*AgentTrigger, error)
	FindByInstanceId(instanceId int) ([]*AgentTrigger, error)
	FindByWorkflowId(workflowId int) ([]*AgentTrigger, error)
	FindByWebhookPath(webhookPath string) (*AgentTrigger, error)
	FindAllCronTriggers() ([]*AgentTrigger, error)
	FindAllK8sTriggers() ([]*AgentTrigger, error)
	DeleteByInstanceId(instanceId int) error
	IncrementTriggerCount(id int) error
	UpdateLastTriggered(webhookPath string) error
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
