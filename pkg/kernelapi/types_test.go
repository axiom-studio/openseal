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
