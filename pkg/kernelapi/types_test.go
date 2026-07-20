package kernelapi

import (
	"encoding/json"
	"strings"
	"testing"

	kernelcapability "github.com/axiom-studio/openseal/pkg/capability"
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

func TestWorkforceAuthoringVersionDeclaresDeterministicPlacementContract(t *testing.T) {
	capability := WorkforceAuthoringCapability(WorkforceAuthoringCapabilityFeatures{ChangeSets: true})
	if capability.Version != "7" || !capability.Supports(OperationCompile) || !capability.Supports(OperationPropose) || capability.Supports(OperationRefine) || capability.Supports(OperationPatch) {
		t.Fatalf("workforce authoring capability = %#v", capability)
	}
}

func TestOutreachAdvertisesReviewedDeliveryLifecycle(t *testing.T) {
	capability, ok := Capabilities().Find(OutreachCapabilityID, OutreachCapabilityVersion)
	if !ok {
		t.Fatal("outreach capability was not advertised")
	}
	for _, operation := range []string{OperationCreate, OperationGet, OperationList, OperationDeliver} {
		if !capability.Supports(operation) {
			t.Fatalf("outreach operation %q not advertised: %#v", operation, capability.Operations)
		}
	}
	for _, unsupported := range []string{OperationUpdate, OperationPost, "delete"} {
		if capability.Supports(unsupported) {
			t.Fatalf("unsupported outreach operation %q advertised", unsupported)
		}
	}
}

func TestAgentDefinitionsUseCanonicalStudioOperationVocabulary(t *testing.T) {
	capability := AgentDefinitionsCapability()
	if capability.Version != "5" || !capability.Supports(OperationGet) || !capability.Supports(OperationList) || !capability.Supports(OperationUpdate) ||
		!capability.Supports("list-compilations") || capability.Supports("list_compilations") {
		t.Fatalf("agent definition capability = %#v", capability)
	}
}

func TestSkillBindingsAdvertiseExplicitGovernedLifecycle(t *testing.T) {
	readOnly := SkillBindingsCapability(false)
	if readOnly.ID != SkillBindingsCapabilityID || readOnly.Version != "1" || !readOnly.Supports(OperationGet) || !readOnly.Supports(OperationList) || readOnly.Supports(OperationUpsert) {
		t.Fatalf("read-only binding capability = %#v", readOnly)
	}
	managed := SkillBindingsCapability(true)
	for _, operation := range []string{OperationGet, OperationList, OperationUpsert, OperationDisable} {
		if !managed.Supports(operation) {
			t.Fatalf("binding operation %q not advertised: %#v", operation, managed.Operations)
		}
	}
}

func TestActivityCapabilityIsReadOnlyAndSelectorBounded(t *testing.T) {
	capability := ActivityCapability()
	if capability.ID != ActivityCapabilityID || capability.Version != ActivityCapabilityVersion || !capability.Supports(OperationList) {
		t.Fatalf("activity capability = %#v", capability)
	}
	for _, operation := range []string{OperationCreate, OperationUpdate, OperationStream} {
		if capability.Supports(operation) {
			t.Fatalf("activity capability advertised mutation %q: %#v", operation, capability.Operations)
		}
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
	capability := ActionApprovalsCapability(ActionApprovalCapabilityFeatures{})
	if capability.ID != ActionApprovalsCapabilityID || capability.Version != ActionApprovalsCapabilityVersion {
		t.Fatalf("action approval capability identity = %#v", capability)
	}
	for _, operation := range []string{OperationGet, OperationList} {
		if !capability.Supports(operation) {
			t.Fatalf("action approval operation %q not advertised: %#v", operation, capability.Operations)
		}
	}
	if capability.Supports(OperationResolve) || capability.Supports(OperationCreate) || capability.Supports(OperationApprove) {
		t.Fatalf("unsupported action approval operation advertised: %#v", capability.Operations)
	}
	governed := ActionApprovalsCapability(ActionApprovalCapabilityFeatures{Resolution: true})
	if !governed.Supports(OperationResolve) {
		t.Fatalf("configured approval resolution not advertised: %#v", governed.Operations)
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
	for _, operation := range []string{OperationProposeAmendment, OperationListAmendments, OperationGetAmendment, OperationEvaluateAmendment, OperationResolveAmendment, OperationActivateAmendment} {
		if !amendments.Supports(operation) {
			t.Fatalf("configured Team amendment operation %q not advertised: %#v", operation, amendments.Operations)
		}
	}
}

func TestWorkforceAuthoringAdvertisesCompilationWithoutActivation(t *testing.T) {
	capability := WorkforceAuthoringCapability(WorkforceAuthoringCapabilityFeatures{})
	if !capability.Supports(OperationCompile) {
		t.Fatalf("workforce authoring operations = %#v", capability.Operations)
	}
	if capability.Supports(OperationActivate) || capability.Supports(OperationRegister) {
		t.Fatalf("unsafe authoring operation advertised: %#v", capability.Operations)
	}
}

func TestContextualApprovalEligibilityIsTypedAndNotAnOperationInference(t *testing.T) {
	capability := WorkforceAuthoringCapability(WorkforceAuthoringCapabilityFeatures{ChangeSets: true})
	for _, operation := range []string{OperationCompile, OperationPropose, OperationGet} {
		if !capability.Supports(operation) {
			t.Fatalf("change set operation %q not advertised: %#v", operation, capability.Operations)
		}
	}
	if capability.Supports(OperationApply) || capability.Supports(OperationEvaluate) || capability.Supports(OperationRetry) {
		t.Fatalf("resource-authorized operation advertised globally: %#v", capability.Operations)
	}
	capability.Context = &CapabilityContext{ChangeSetID: "change-1", Revision: 4, EligibleApprovalRequirements: []ApprovalRequirementReference{{EvaluationID: "eval-1", PolicyID: "production", Role: "operator"}}}
	if capability.Supports(OperationApprove) {
		t.Fatal("eligibility context must not silently advertise an operation")
	}
	capability.Operations = append(capability.Operations, OperationApprove)
	if !capability.Supports(OperationApprove) || capability.Context.Revision != 4 || capability.Context.EligibleApprovalRequirements[0].EvaluationID != "eval-1" {
		t.Fatalf("contextual capability = %#v", capability)
	}
}

func TestContextualCredentialBindingsExposeOnlyOpaqueOperatorChoices(t *testing.T) {
	context := CapabilityContext{
		CredentialBindings: []kernelcapability.CredentialBindingChoice{{
			Reference:   kernelcapability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"},
			DisplayName: "Development",
		}},
		BlockingRequirements: []CapabilityBlockingRequirement{{
			Code: "requires_credential", CredentialKey: "MODEL_PROVIDER", Message: "Connect a model provider.",
		}},
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	if !strings.Contains(value, `"displayName":"Development"`) || !strings.Contains(value, `"id":"cluster://7"`) || !strings.Contains(value, `"code":"requires_credential"`) || !strings.Contains(value, `"credentialKey":"MODEL_PROVIDER"`) || strings.Contains(value, "kubeconfig") || strings.Contains(value, "token") {
		t.Fatalf("credential binding context = %s", encoded)
	}
}
