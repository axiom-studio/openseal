package models

const (
	ObjectiveManagementSkillID = "openseal.objectives"
)

const (
	RunManagementSkillID = "openseal.runs"
	RunActionPause       = "pause"
	RunActionResume      = "resume"
	RunActionCancel      = "cancel"
)

const (
	RunbookManagementSkillID = "openseal.runbooks"
	RunbookActionCreateTask  = "create_task"
	RunbookActionList        = "list"
	RunbookActionSetStatus   = "set_status"
	RunbookActionSetScript   = "set_script"
)

const (
	SkillManagementSkillID = "openseal.skills"
)

const (
	TeamManagementSkillID      = "openseal.teams"
	TeamManagementSkillVersion = "2.2.0"
	TeamActionUpdateMember     = "update_member"
	TeamActionUpdateProfile    = "update_profile"
	TeamActionCreateTeam       = "create_team"
	TeamManagementEndpoint     = "kernel://teams"
)
