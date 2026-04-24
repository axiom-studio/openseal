package skill

import "context"

type SkillDiscovery interface {
	Search(ctx context.Context, query string, limit int) ([]*Skill, error)
	SearchByCategory(ctx context.Context, category, query string, limit int) ([]*Skill, error)
	GetRelatedSkills(ctx context.Context, skillID int, limit int) ([]*Skill, error)
	AutoCreateSkillFromWorkflow(ctx context.Context, workflowID int, workflowDef map[string]interface{}) (*Skill, error)
}
