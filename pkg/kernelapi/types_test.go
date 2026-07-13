package kernelapi

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCapabilitiesAreExplicitAndDiscoverable(t *testing.T) {
	document := Capabilities()
	if document.APIVersion != APIVersion {
		t.Fatalf("capability API version = %q", document.APIVersion)
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"apiVersion":"agent-kernel/v1"`) || strings.Contains(string(encoded), `"version":"1","capabilities"`) {
		t.Fatalf("capability document envelope = %s", encoded)
	}
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

func TestAgentDefinitionsUseCanonicalStudioOperationVocabulary(t *testing.T) {
	capability := AgentDefinitionsCapability()
	if capability.Version != "2" || !capability.Supports("list-compilations") || capability.Supports("list_compilations") {
		t.Fatalf("agent definition capability = %#v", capability)
	}
}

func TestAgentRequestsAdvertisePortableCollaborationLifecycle(t *testing.T) {
	capability := AgentRequestsCapability()
	if capability.ID != AgentRequestsCapabilityID || capability.Version != AgentRequestsCapabilityVersion {
		t.Fatalf("agent request capability identity = %#v", capability)
	}
	for _, operation := range []string{OperationCreate, OperationGet, OperationList, OperationRespond, OperationComplete} {
		if !capability.Supports(operation) {
			t.Fatalf("agent request operation %q not advertised: %#v", operation, capability.Operations)
		}
	}
	if capability.Supports(OperationResolve) || capability.Supports("delete") {
		t.Fatalf("unsupported agent request operation advertised: %#v", capability.Operations)
	}
}

func TestActionApprovalsAdvertiseGovernedDecisionLifecycle(t *testing.T) {
	capability := ActionApprovalsCapability()
	if capability.ID != ActionApprovalsCapabilityID || capability.Version != ActionApprovalsCapabilityVersion {
		t.Fatalf("action approval capability identity = %#v", capability)
	}
	for _, operation := range []string{OperationGet, OperationList, OperationResolve} {
		if !capability.Supports(operation) {
			t.Fatalf("action approval operation %q not advertised: %#v", operation, capability.Operations)
		}
	}
	if capability.Supports(OperationCreate) || capability.Supports(OperationApprove) {
		t.Fatalf("unsupported action approval operation advertised: %#v", capability.Operations)
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

func TestChannelCapabilityAdvertisesOnlyConfiguredFeatures(t *testing.T) {
	portable := ChannelsCapability(ChannelCapabilityFeatures{Coordination: true, Changes: true})
	for _, operation := range []string{OperationCreate, OperationPost, OperationRead, OperationPresence, OperationAudit, OperationCoordinate, OperationChanges} {
		if !portable.Supports(operation) {
			t.Fatalf("portable channel operation %q not advertised: %#v", operation, portable.Operations)
		}
	}
	for _, operation := range []string{OperationReceipts, OperationCoordinateAuto, OperationStream} {
		if portable.Supports(operation) {
			t.Fatalf("unconfigured channel operation %q advertised: %#v", operation, portable.Operations)
		}
	}

	enterprise := ChannelsCapability(ChannelCapabilityFeatures{Coordination: true, AutomaticCoordination: true, Receipts: true, Streaming: true})
	for _, operation := range []string{OperationCoordinate, OperationCoordinateAuto, OperationReceipts, OperationStream} {
		if !enterprise.Supports(operation) {
			t.Fatalf("configured channel operation %q not advertised: %#v", operation, enterprise.Operations)
		}
	}
	if enterprise.Version != ChannelsCapabilityVersion {
		t.Fatalf("channel capability version = %q", enterprise.Version)
	}
	if ChannelsCapabilityVersion != "4" {
		t.Fatalf("canonical channel capability version = %q", ChannelsCapabilityVersion)
	}
}

func TestTeamDefinitionsAdvertiseOnlyImplementedLifecycle(t *testing.T) {
	capability := TeamDefinitionsCapability(TeamDefinitionCapabilityFeatures{})
	for _, operation := range []string{OperationRegister, OperationGet, OperationList, OperationDeploy, OperationUpdate, OperationActivate} {
		if !capability.Supports(operation) {
			t.Fatalf("Team definition operation %q not advertised", operation)
		}
	}
	if capability.Supports("delete") {
		t.Fatalf("unsupported Team definition operation advertised: %#v", capability.Operations)
	}
	if capability.Version != "2" || capability.Supports(OperationProposeAmendment) {
		t.Fatalf("portable Team definition capability = %#v", capability)
	}
	amendments := TeamDefinitionsCapability(TeamDefinitionCapabilityFeatures{Amendments: true})
	for _, operation := range []string{OperationProposeAmendment, OperationEvaluateAmendment, OperationResolveAmendment, OperationActivateAmendment} {
		if !amendments.Supports(operation) {
			t.Fatalf("configured Team amendment operation %q not advertised: %#v", operation, amendments.Operations)
		}
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
