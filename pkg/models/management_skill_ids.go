package models

// The kernel's management Skills: built-in Skills whose actions the kernel
// runs itself (transport kind "kernel") to manage its own records.

// ObjectiveManagementSkillID is the Skill that manages objectives.
const (
	ObjectiveManagementSkillID = "openseal.objectives"
)

// RunManagementSkillID is the Skill that pauses, resumes and cancels Runs;
// the RunAction names are its actions.
const (
	RunManagementSkillID = "openseal.runs"
	RunActionPause       = "pause"
	RunActionResume      = "resume"
	RunActionCancel      = "cancel"
)

// RunbookManagementSkillID is the Skill that creates and changes routines
// (runbook activations); the RunbookAction names are its actions.
const (
	RunbookManagementSkillID = "openseal.runbooks"
	RunbookActionCreateTask  = "create_task"
	RunbookActionList        = "list"
	RunbookActionSetStatus   = "set_status"
	RunbookActionSetScript   = "set_script"
)

// SkillManagementSkillID is the Skill that requests Skill setup and
// credentials.
const (
	SkillManagementSkillID = "openseal.skills"
)

// TeamManagementSkillID is the Skill that creates Teams and updates their
// members and profiles; the TeamAction names are its actions.
// TeamManagementSkillVersion is the version the kernel registers and
// TeamManagementEndpoint its kernel transport endpoint.
const (
	TeamManagementSkillID      = "openseal.teams"
	TeamManagementSkillVersion = "2.2.0"
	TeamActionUpdateMember     = "update_member"
	TeamActionUpdateProfile    = "update_profile"
	TeamActionCreateTeam       = "create_team"
	TeamManagementEndpoint     = "kernel://teams"
)
