package runtime

func cloneSkillSetupRequest(r *SkillSetupRequest) *SkillSetupRequest {
	if r == nil {
		return nil
	}
	c := *r
	c.RequiredActions = append([]string(nil), r.RequiredActions...)
	return &c
}
