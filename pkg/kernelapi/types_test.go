package kernelapi

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	kernelcapability "github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runtime"
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
	for _, operation := range []string{OperationCreate, OperationList, OperationPause, OperationResume, OperationCancel, OperationIntervene, OperationInspectAdmission} {
		if !capability.Supports(operation) {
			t.Fatalf("operation %q was not advertised", operation)
		}
	}
	if capability.Supports("delete") {
		t.Fatal("unsupported operation was advertised")
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
	if capability.Version != "8" || !capability.Supports(OperationGet) || !capability.Supports(OperationList) || !capability.Supports(OperationUpdate) ||
		!capability.Supports("list-compilations") || capability.Supports("list_compilations") {
		t.Fatalf("agent definition capability = %#v", capability)
	}
	if capability.Supports(OperationActivate) || capability.Supports(OperationProposeAmendment) {
		t.Fatalf("unconfigured lifecycle advertised: %#v", capability.Operations)
	}
	governed := AgentDefinitionsCapability(AgentDefinitionCapabilityFeatures{Lifecycle: true, Amendments: true})
	for _, operation := range []string{OperationActivate, OperationRollback, OperationListActivations, OperationProposeAmendment,
		OperationListAmendments, OperationGetAmendment, OperationEvaluateAmendment, OperationResolveAmendment, OperationActivateAmendment} {
		if !governed.Supports(operation) {
			t.Fatalf("governed Agent definition operation %q not advertised: %#v", operation, governed.Operations)
		}
	}
	portable := AgentDefinitionsCapability(AgentDefinitionCapabilityFeatures{PortableInstallation: true})
	if !portable.Supports(OperationDeploy) {
		t.Fatalf("portable Agent installation was not advertised: %#v", portable.Operations)
	}
}

func TestSourcePoliciesAdvertiseExactGovernedLifecycle(t *testing.T) {
	capability := SourcePoliciesCapability()
	if capability.Version != SourcePoliciesCapabilityVersion {
		t.Fatalf("source policy capability version = %q", capability.Version)
	}
	for _, operation := range []string{OperationRegister, OperationGet, OperationList, OperationListVersions, OperationActivate, OperationRevoke, OperationListActivations} {
		if !capability.Supports(operation) {
			t.Fatalf("source policy operation %q not advertised: %#v", operation, capability.Operations)
		}
	}
	if capability.Supports(OperationUpdate) || capability.Supports(OperationApply) {
		t.Fatalf("source policy capability invented mutable version authority: %#v", capability.Operations)
	}
}

func TestSkillBindingsAdvertiseExplicitGovernedLifecycle(t *testing.T) {
	readOnly := SkillBindingsCapability(false)
	if readOnly.ID != SkillBindingsCapabilityID || readOnly.Version != "3" || !readOnly.Supports(OperationGet) || !readOnly.Supports(OperationList) || readOnly.Supports(OperationUpsert) {
		t.Fatalf("read-only binding capability = %#v", readOnly)
	}
	managed := SkillBindingsCapability(true)
	for _, operation := range []string{OperationGet, OperationList, OperationUpsert, OperationDisable, OperationPlanUpgrade, OperationApplyUpgrade} {
		if !managed.Supports(operation) {
			t.Fatalf("binding operation %q not advertised: %#v", operation, managed.Operations)
		}
	}
}

func TestAgentEmbedsCapabilitySeparatesReadAndManagementAuthority(t *testing.T) {
	readOnly := AgentEmbedsCapability(false)
	if readOnly.ID != AgentEmbedsCapabilityID || readOnly.Version != "1" || !readOnly.Supports(OperationGet) || !readOnly.Supports(OperationList) || readOnly.Supports(OperationCreate) {
		t.Fatalf("read-only Agent embed capability = %#v", readOnly)
	}
	managed := AgentEmbedsCapability(true)
	if !managed.Supports(OperationCreate) || !managed.Supports(OperationUpdate) {
		t.Fatalf("managed Agent embed capability = %#v", managed)
	}
}

func TestConversationGatewaysAdvertiseOnlyLifecycleCommands(t *testing.T) {
	readOnly := ConversationGatewaysCapability(false)
	if readOnly.ID != ConversationGatewaysCapabilityID || readOnly.Version != "2" ||
		!readOnly.Supports(OperationGet) || !readOnly.Supports(OperationList) ||
		readOnly.Supports(OperationCreate) || readOnly.Supports(OperationUpdate) {
		t.Fatalf("read-only gateway capability = %#v", readOnly)
	}
	managed := ConversationGatewaysCapability(true)
	for _, operation := range []string{OperationCreate, OperationGet, OperationList, OperationUpdate} {
		if !managed.Supports(operation) {
			t.Fatalf("gateway operation %q not advertised: %#v", operation, managed.Operations)
		}
	}
	if managed.Supports(OperationPost) {
		t.Fatalf("provider ingress was exposed as an operator command: %#v", managed.Operations)
	}
}

func TestCallbacksAdvertiseOnlyLifecycleCommands(t *testing.T) {
	readOnly := CallbacksCapability(false)
	if readOnly.ID != CallbacksCapabilityID || readOnly.Version != "1" ||
		!readOnly.Supports(OperationGet) || !readOnly.Supports(OperationList) ||
		readOnly.Supports(OperationCreate) || readOnly.Supports(OperationUpdate) {
		t.Fatalf("read-only callback capability = %#v", readOnly)
	}
	choice := CallbackAdapterChoice{ID: "choice", DeploymentID: "agent:one", Provider: "slack", EventTypes: []string{"approval.decided"}}
	managed := CallbacksCapability(true, []CallbackAdapterChoice{choice})
	for _, operation := range []string{OperationCreate, OperationGet, OperationList, OperationUpdate} {
		if !managed.Supports(operation) {
			t.Fatalf("callback operation %q not advertised: %#v", operation, managed.Operations)
		}
	}
	if managed.Context == nil || len(managed.Context.CallbackAdapters) != 1 || managed.Context.CallbackAdapters[0].ID != choice.ID {
		t.Fatalf("callback choices were not advertised: %#v", managed.Context)
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

func TestActionCallsExposeOnlyDurableReadOperations(t *testing.T) {
	capability, ok := Capabilities().Find(ActionCallsCapabilityID, ActionCallsCapabilityVersion)
	if !ok || !capability.Supports(OperationGet) || !capability.Supports(OperationList) {
		t.Fatalf("action call capability = %#v", capability)
	}
	for _, operation := range []string{OperationCreate, OperationUpdate, OperationResolve, OperationRetry} {
		if capability.Supports(operation) {
			t.Fatalf("action call capability advertised mutation %q: %#v", operation, capability.Operations)
		}
	}
}

func TestAgentTurnsExposeOnlyDurableReadOperations(t *testing.T) {
	if AgentTurnsCapabilityVersion != "2" {
		t.Fatalf("agent turn capability version = %q, want 2", AgentTurnsCapabilityVersion)
	}
	if _, ok := Capabilities().Find(AgentTurnsCapabilityID, "1"); ok {
		t.Fatal("agent turn capability still advertises v1")
	}
	capability, ok := Capabilities().Find(AgentTurnsCapabilityID, AgentTurnsCapabilityVersion)
	if !ok || !capability.Supports(OperationGet) || !capability.Supports(OperationList) {
		t.Fatalf("agent turn capability = %#v", capability)
	}
	for _, operation := range []string{OperationCreate, OperationUpdate, OperationResolve, OperationRetry} {
		if capability.Supports(operation) {
			t.Fatalf("agent turn capability advertised mutation %q: %#v", operation, capability.Operations)
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
	for _, operation := range []string{OperationCreate, OperationUpdate, OperationPost, OperationRead, OperationPresence, OperationAudit, OperationCoordinate, OperationChanges} {
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
	if capability.Version != "4" || capability.Supports(OperationProposeAmendment) || capability.Supports(OperationRecoverParticipation) {
		t.Fatalf("portable Team definition capability = %#v", capability)
	}
	amendments := TeamDefinitionsCapability(TeamDefinitionCapabilityFeatures{Amendments: true})
	for _, operation := range []string{OperationProposeAmendment, OperationListAmendments, OperationGetAmendment, OperationEvaluateAmendment, OperationResolveAmendment, OperationActivateAmendment} {
		if !amendments.Supports(operation) {
			t.Fatalf("configured Team amendment operation %q not advertised: %#v", operation, amendments.Operations)
		}
	}
	recovery := TeamDefinitionsCapability(TeamDefinitionCapabilityFeatures{ParticipationRecovery: true})
	if !recovery.Supports(OperationRecoverParticipation) {
		t.Fatalf("configured Team recovery operation not advertised: %#v", recovery.Operations)
	}
}

func TestWorkforceExecutionTargetsCapabilityReflectsManagement(t *testing.T) {
	readOnly := WorkforceExecutionTargetsCapability(false)
	if readOnly.Supports(OperationCreate) || !readOnly.Supports(OperationList) {
		t.Fatalf("read-only execution targets = %#v", readOnly)
	}
	managed := WorkforceExecutionTargetsCapability(true)
	if !managed.Supports(OperationCreate) || !managed.Supports(OperationUpdate) {
		t.Fatalf("managed execution targets = %#v", managed)
	}
}

func TestContextualCredentialBindingsExposeOnlyOpaqueOperatorChoices(t *testing.T) {
	context := CapabilityContext{
		CredentialBindings: []kernelcapability.CredentialBindingChoice{{
			Reference:   kernelcapability.CredentialReference{Kind: "kubernetes-cluster", ID: "cluster://7"},
			DisplayName: "Development",
			ExternalIdentity: &kernelcapability.CredentialExternalIdentity{
				InstallationID: "workspace-7", ApplicationID: "application-2", DisplayName: "Development workspace",
			},
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
	if !strings.Contains(value, `"displayName":"Development"`) || !strings.Contains(value, `"id":"cluster://7"`) || !strings.Contains(value, `"installationId":"workspace-7"`) || !strings.Contains(value, `"applicationId":"application-2"`) || !strings.Contains(value, `"code":"requires_credential"`) || !strings.Contains(value, `"credentialKey":"MODEL_PROVIDER"`) || strings.Contains(value, "kubeconfig") || strings.Contains(value, "token") {
		t.Fatalf("credential binding context = %s", encoded)
	}
}

func TestContextualBindingConfigurationFieldsExposeTypedAuthorizedChoices(t *testing.T) {
	clusterID := int64(7)
	context := CapabilityContext{BindingConfigurationFields: []kernelcapability.BindingConfigurationFieldChoice{{
		CatalogSkillID: "openseal.kubernetes",
		Skill:          kernelcapability.NewSkillIdentity("openseal.kubernetes", "1.1.0", "builtin:openseal.kubernetes"),
		Key:            "clusterId", Type: "integer", Required: true, Prompt: "Which Kubernetes cluster should this Agent operate?",
		Options: []kernelcapability.BindingConfigurationOption{{Label: "Development", Value: kernelcapability.BindingConfigurationValue{Integer: &clusterID}}},
	}}}
	if err := kernelcapability.ValidateBindingConfigurationFields(context.BindingConfigurationFields); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(context)
	if err != nil {
		t.Fatal(err)
	}
	value := string(encoded)
	if !strings.Contains(value, `"bindingConfigurationFields"`) || !strings.Contains(value, `"clusterId"`) || !strings.Contains(value, `"integer":7`) || strings.Contains(strings.ToLower(value), "credential") {
		t.Fatalf("binding configuration context = %s", encoded)
	}
}

func TestWorkforceBundlesCapabilityAdvertisesOnlyWiredMutation(t *testing.T) {
	readOnly := WorkforceBundlesCapability(false)
	if readOnly.ID != WorkforceBundlesCapabilityID || readOnly.Version != WorkforceBundlesCapabilityVersion || !readOnly.Supports(OperationValidate) || !readOnly.Supports(OperationInspect) || !readOnly.Supports(OperationCompare) || !readOnly.Supports(OperationPreviewInstallation) || !readOnly.Supports(OperationPlanUpgrade) || readOnly.Supports(OperationInstall) {
		t.Fatalf("unexpected read-only workforce bundle capability: %#v", readOnly)
	}
	mutable := WorkforceBundlesCapability(true)
	if !mutable.Supports(OperationInstall) {
		t.Fatalf("wired installation capability omitted install: %#v", mutable)
	}
}

func TestProjectAgentTurnV2EmitsOnlyPublicStepFields(t *testing.T) {
	started := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	completed := started.Add(2 * time.Second)
	leaseExpires := started.Add(time.Minute)
	turn := &runtime.AgentTurn{
		ID: "turn-1", Scope: runtime.Scope{Kind: "local", ID: "default"}, RunID: "run-1", SessionID: "ses-3",
		Sequence: 2, Status: runtime.AgentTurnStatusFailed, Producer: runtime.StepProducerModel,
		AssistantEntryID: "private-assistant-entry", DefinitionID: "reviewer", DefinitionVersion: "4",
		ModelProvider: "openai-compatible", Model: "deepseek-v4-flash",
		InputContextRefs: []string{"private-context-ref"},
		OutputSummary:    "Asking the reviewer to check the evidence.", ToolCallCount: 1,
		StopReason: string(runtime.ModelStopToolCalls), Final: true,
		Usage:           runtime.TurnUsage{InputTokens: 1840, OutputTokens: 64, ProviderDurationMS: 910},
		SkillSelections: []runtime.HostedSkillSelection{{}},
		FailureCode:     "host_unavailable", Error: "provider unreachable",
		LeaseOwner: "private-worker", LeaseExpiresAt: &leaseExpires,
		Revision: 3, CreatedAt: started, UpdatedAt: completed, StartedAt: started, CompletedAt: &completed,
	}
	record := ProjectAgentTurn(turn)
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(wire))
	for key := range wire {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{
		"completedAt", "createdAt", "definitionId", "definitionVersion", "error", "failureCode", "final",
		"id", "model", "modelProvider", "outputSummary", "producer", "revision", "runId", "scope",
		"sequence", "sessionId", "startedAt", "status", "stopReason", "toolCallCount", "updatedAt", "usage",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("agent turn v2 keys = %v, want %v", keys, want)
	}
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "planRevision") {
		t.Fatalf("agent turn v2 leaked a private field: %s", encoded)
	}
	if record.Producer != runtime.StepProducerModel || record.FailureCode != "host_unavailable" || !record.Final ||
		record.ToolCallCount != 1 || record.StopReason != "tool_calls" || record.SessionID != "ses-3" {
		t.Fatalf("agent turn v2 record = %#v", record)
	}
	if record.CompletedAt == turn.CompletedAt {
		t.Fatal("completedAt aliases the stored turn")
	}

	minimal, err := json.Marshal(ProjectAgentTurn(&runtime.AgentTurn{ID: "turn-2", RunID: "run-1", Sequence: 1, Status: runtime.AgentTurnStatusRunning}))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"toolCallCount":0`, `"final":false`} {
		if !strings.Contains(string(minimal), key) {
			t.Fatalf("agent turn v2 omits %s: %s", key, minimal)
		}
	}
	if ProjectAgentTurn(nil) != nil {
		t.Fatal("nil turn projected to a record")
	}
}

func TestAgentBuilderCapabilityContextCarriesOnlyTheBuilder(t *testing.T) {
	capability := AgentBuilderCapability("openseal-assistant")
	if !capability.Supports(OperationCreate) || !capability.Supports(OperationList) {
		t.Fatalf("agent builder operations = %#v", capability.Operations)
	}
	encoded, err := json.Marshal(capability.Context)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"builderAgentId":"openseal-assistant"}` {
		t.Fatalf("agent builder context = %s", encoded)
	}
}
