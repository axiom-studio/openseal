package runtime

func skillUpgradeDrainIndexStatements(actions, runs string, postgres bool) []string {
	prefix := "idx_"
	if postgres {
		prefix = ""
	}
	return []string{
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_runtime_active ON " + actions + " (scope_kind,scope_id,skill_id,skill_version,status) WHERE status IN (" + unfinishedSkillActionSQL + ")",
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_binding_active ON " + actions + " (scope_kind,scope_id,deployment_id,binding_id,skill_id,skill_version,status) WHERE status IN (" + unfinishedSkillActionSQL + ")",
		"CREATE INDEX IF NOT EXISTS " + prefix + "action_calls_skill_run_receipt ON " + actions + " (scope_kind,scope_id,run_id,skill_id,skill_version,deployment_id,binding_id) WHERE status='succeeded'",
		"CREATE INDEX IF NOT EXISTS " + prefix + "agent_runs_skill_runtime_active ON " + runs + " (scope_kind,scope_id,status,id) WHERE status IN (" + unfinishedSkillRunSQL + ")",
	}
}
