package kernelapi

import "testing"

func TestCapabilitiesAreExplicitAndDiscoverable(t *testing.T) {
	document := Capabilities()
	capability, ok := document.Find(AgentRunsCapabilityID, AgentRunsCapabilityVersion)
	if !ok {
		t.Fatal("agent run capability was not advertised")
	}
	for _, operation := range []string{OperationCreate, OperationList, OperationPause, OperationResume, OperationCancel, OperationIntervene} {
		if !capability.Supports(operation) {
			t.Fatalf("operation %q was not advertised", operation)
		}
	}
	if capability.Supports("delete") {
		t.Fatal("unsupported operation was advertised")
	}
}

func TestArtifactCapabilityDoesNotAdvertiseUnconfiguredContentResolution(t *testing.T) {
	capability := ArtifactCapability()
	if !capability.Supports(OperationRegister) || !capability.Supports(OperationGet) || !capability.Supports(OperationList) {
		t.Fatalf("artifact operations = %#v", capability.Operations)
	}
	if capability.Supports("upload") || capability.Supports("resolve") || capability.Supports("download") {
		t.Fatalf("unconfigured content operation advertised: %#v", capability.Operations)
	}
}

func TestTeamDefinitionsAdvertiseOnlyImplementedLifecycle(t *testing.T) {
	capability := TeamDefinitionsCapability()
	for _, operation := range []string{OperationRegister, OperationGet, OperationList, OperationDeploy, OperationUpdate, OperationActivate} {
		if !capability.Supports(operation) {
			t.Fatalf("Team definition operation %q not advertised", operation)
		}
	}
	if capability.Supports("delete") {
		t.Fatalf("unsupported Team definition operation advertised: %#v", capability.Operations)
	}
}

func TestWorkforceAuthoringAdvertisesCompilationWithoutActivation(t *testing.T) {
	capability := WorkforceAuthoringCapability()
	if !capability.Supports(OperationCompile) {
		t.Fatalf("workforce authoring operations = %#v", capability.Operations)
	}
	if capability.Supports(OperationActivate) || capability.Supports(OperationRegister) {
		t.Fatalf("unsafe authoring operation advertised: %#v", capability.Operations)
	}
}

func TestContextualApprovalEligibilityIsTypedAndNotAnOperationInference(t *testing.T) {
	capability := WorkforceAuthoringCapability(true)
	capability.Context = &CapabilityContext{ChangeSetID: "change-1", Revision: 4, EligibleApprovalRequirements: []ApprovalRequirementReference{{EvaluationID: "eval-1", PolicyID: "production", Role: "operator"}}}
	if capability.Supports(OperationApprove) {
		t.Fatal("eligibility context must not silently advertise an operation")
	}
	capability.Operations = append(capability.Operations, OperationApprove)
	if !capability.Supports(OperationApprove) || capability.Context.Revision != 4 || capability.Context.EligibleApprovalRequirements[0].EvaluationID != "eval-1" {
		t.Fatalf("contextual capability = %#v", capability)
	}
}
