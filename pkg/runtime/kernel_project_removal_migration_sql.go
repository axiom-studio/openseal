package runtime

// retiredKernelProjectTables are dropped by migration 77, dependents first.
var retiredKernelProjectTables = []string{
	"project_skill_references", "outreach_threads", "source_monitor_checkpoints", "source_observations", "projects",
	"source_policy_lifecycle_events", "source_policy_lifecycles", "source_policy_versions",
}
