package kernelapi

const SkillSetupRequestsCapabilityID = "skill-setup-requests"

func SkillSetupRequestsCapability(management bool) Capability {
	result := Capability{ID: SkillSetupRequestsCapabilityID, Version: "1", Available: true, Operations: []string{OperationList}}
	if management {
		result.Operations = append(result.Operations, OperationUpdate)
	}
	return result
}
