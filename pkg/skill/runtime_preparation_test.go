package skill

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	opensealprocess "github.com/axiom-studio/openseal/pkg/process"
)

type recordingRuntimePreparer struct {
	state  RuntimePreparationState
	mutate func(*PreparedRuntime)
	err    error
	seen   []RuntimePreparationRequest
}

func (p *recordingRuntimePreparer) PrepareRuntime(_ context.Context, request RuntimePreparationRequest) (*RuntimePreparationResult, error) {
	p.seen = append(p.seen, request)
	if p.err != nil {
		return nil, p.err
	}
	result := &RuntimePreparationResult{State: p.state}
	if p.state == RuntimePreparationReady {
		result.Runtime = &PreparedRuntime{
			PreparationID: request.PreparationID, RuntimeID: "oci://runtime.test/summarize@sha256:" + strings.Repeat("b", 64),
			Revision: "sha256:" + strings.Repeat("c", 64), Adapter: "oci-builder/v1",
			OperatingSystem: request.OperatingSystem, Architecture: request.Architecture, Executables: append([]string(nil), request.Executables...),
		}
		if p.mutate != nil {
			p.mutate(result.Runtime)
		}
	}
	return result, nil
}

func TestActivationPinsPreparedRuntimeIdentity(t *testing.T) {
	catalog, scope, definition := preparedRuntimeFixture(t)
	preparer := &recordingRuntimePreparer{state: RuntimePreparationReady}
	host := HostCapabilityState{
		OperatingSystem: "linux", Architecture: "amd64", RuntimePreparer: preparer, Revision: "host-runtime-1",
		Adapters: map[string]AdapterCapability{
			AdapterInstaller:       {State: AdapterStateAvailable, Features: []string{"brew"}},
			AdapterPreparedRuntime: {State: AdapterStateAvailable, Version: "oci-builder/v1"},
		},
	}
	first, err := catalog.Activate(context.Background(), scope, "agent", host)
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.Activate(context.Background(), scope, "agent", host)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Skills) != 1 || len(first.Unavailable) != 0 || first.SnapshotID == "" || first.SnapshotID != second.SnapshotID || len(preparer.seen) != 2 {
		t.Fatalf("prepared activation is not stable: first=%#v second=%#v seen=%#v", first, second, preparer.seen)
	}
	runtime := first.Skills[0].PreparedRuntime
	request := preparer.seen[0]
	if runtime == nil || runtime.PreparationID != request.PreparationID || runtime.Adapter != "oci-builder/v1" ||
		request.SkillID != definition.ID || request.SourceDigest != definition.Source.Digest || request.SourceTrustDigest == "" || request.BuilderRevision != "oci-builder/v1" || request.OperatingSystem != "linux" ||
		request.Architecture != "amd64" || len(request.Executables) != 1 || request.Executables[0] != "summarize" || len(request.Installers) != 1 {
		t.Fatalf("prepared runtime lost immutable identity: runtime=%#v request=%#v", runtime, request)
	}
	if err := ValidateRuntimePreparationRequest(request); err != nil {
		t.Fatalf("activation emitted invalid preparation request: %v", err)
	}
	encoded, err := json.Marshal(first)
	if err != nil || strings.Contains(string(encoded), "secret") {
		t.Fatalf("prepared runtime snapshot is not secret-free: %s, %v", encoded, err)
	}
}

func TestPreparedRuntimeReferenceRequiresCanonicalImmutableIdentity(t *testing.T) {
	runtime := &PreparedRuntime{
		PreparationID: "sha256:" + strings.Repeat("a", 64), RuntimeID: "oci://runtime.test/summarize@sha256:" + strings.Repeat("b", 64),
		Revision: "sha256:" + strings.Repeat("c", 64), Adapter: "oci-builder/v1", OperatingSystem: "linux", Architecture: "amd64",
		Executables: []string{"helper", "summarize"},
	}
	if err := ValidatePreparedRuntimeReference(runtime); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*PreparedRuntime){
		"preparation": func(value *PreparedRuntime) { value.PreparationID = "sha256:not-a-digest" },
		"platform":    func(value *PreparedRuntime) { value.Architecture = "AMD64" },
		"artifact":    func(value *PreparedRuntime) { value.RuntimeID = "artifact\nforged" },
		"executables": func(value *PreparedRuntime) { value.Executables = []string{"summarize", "helper"} },
	} {
		t.Run(name, func(t *testing.T) {
			copy := *runtime
			copy.Executables = append([]string(nil), runtime.Executables...)
			mutate(&copy)
			if ValidatePreparedRuntimeReference(&copy) == nil {
				t.Fatalf("invalid prepared runtime was accepted: %#v", copy)
			}
		})
	}
}

func TestActivationFailsClosedWhileRuntimeIsPreparingOrInvalid(t *testing.T) {
	for name, test := range map[string]struct {
		preparer RuntimePreparer
		code     string
	}{
		"preparing": {preparer: &recordingRuntimePreparer{state: RuntimePreparationPreparing}, code: "runtime_preparing"},
		"failed":    {preparer: &recordingRuntimePreparer{err: errors.New("secret host detail")}, code: "runtime_preparation_failed"},
		"missing":   {preparer: nil, code: "runtime_preparation_unavailable"},
		"invalid": {
			preparer: &recordingRuntimePreparer{state: RuntimePreparationReady, mutate: func(runtime *PreparedRuntime) { runtime.PreparationID = "sha256:" + strings.Repeat("0", 64) }},
			code:     "runtime_preparation_invalid",
		},
	} {
		t.Run(name, func(t *testing.T) {
			catalog, scope, _ := preparedRuntimeFixture(t)
			snapshot, err := catalog.Activate(context.Background(), scope, "agent", HostCapabilityState{
				OperatingSystem: "linux", Architecture: "amd64", RuntimePreparer: test.preparer,
				Adapters: map[string]AdapterCapability{
					AdapterInstaller: {State: AdapterStateAvailable, Features: []string{"brew"}}, AdapterPreparedRuntime: {State: AdapterStateAvailable, Version: "oci-builder/v1"},
				},
			})
			if err != nil || len(snapshot.Skills) != 0 || len(snapshot.Unavailable) != 1 || !hasRuntimeAvailabilityReason(snapshot.Unavailable[0].Reasons, test.code) {
				t.Fatalf("runtime state was not fail-closed: snapshot=%#v err=%v", snapshot, err)
			}
			encoded, _ := json.Marshal(snapshot)
			if strings.Contains(string(encoded), "secret host detail") {
				t.Fatalf("host failure detail escaped into activation: %s", encoded)
			}
		})
	}
}

func TestRuntimePreparationIdentityNormalizesOwnershipAndOrdering(t *testing.T) {
	base := RuntimePreparationRequest{
		Scope: ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent-one", BindingID: "binding-one",
		SkillID: "summarize", SkillVersion: "1.0.0", SourceDigest: strings.Repeat("a", 64), OperatingSystem: "linux", Architecture: "amd64",
		SourceTrustDigest: strings.Repeat("d", 64), BuilderRevision: "builder@sha256:" + strings.Repeat("e", 64),
		Executables: []string{"summarize", "summarize"}, Installers: []capability.Installer{
			{ID: "brew", Kind: "brew", Formula: "owner/tap/summarize", Executables: []string{"summarize"}, OperatingSystems: []string{"linux"}},
		},
	}
	first, err := RuntimePreparationID(base)
	if err != nil {
		t.Fatal(err)
	}
	otherOwner := base
	otherOwner.Scope.ID = "two"
	otherOwner.DeploymentID = "agent-two"
	otherOwner.BindingID = "binding-two"
	second, err := RuntimePreparationID(otherOwner)
	if err != nil || second != first {
		t.Fatalf("ownership changed immutable content identity: first=%s second=%s err=%v", first, second, err)
	}
	otherPlatform := base
	otherPlatform.Architecture = "arm64"
	third, err := RuntimePreparationID(otherPlatform)
	if err != nil || third == first {
		t.Fatalf("platform did not change runtime identity: first=%s third=%s err=%v", first, third, err)
	}
	otherTrust := base
	otherTrust.SourceTrustDigest = strings.Repeat("f", 64)
	fourth, err := RuntimePreparationID(otherTrust)
	if err != nil || fourth == first {
		t.Fatalf("trust did not change runtime identity: first=%s fourth=%s err=%v", first, fourth, err)
	}
	otherBuilder := base
	otherBuilder.BuilderRevision = "builder@sha256:" + strings.Repeat("1", 64)
	fifth, err := RuntimePreparationID(otherBuilder)
	if err != nil || fifth == first {
		t.Fatalf("builder did not change runtime identity: first=%s fifth=%s err=%v", first, fifth, err)
	}
	otherInstaller := base
	otherInstaller.Installers = append([]capability.Installer(nil), base.Installers...)
	otherInstaller.Installers[0].Formula = "owner/tap/summarize@2"
	sixth, err := RuntimePreparationID(otherInstaller)
	if err != nil || sixth == first {
		t.Fatalf("installer did not change runtime identity: first=%s sixth=%s err=%v", first, sixth, err)
	}
}

func preparedRuntimeFixture(t *testing.T) (*Catalog, ScopeReference, *Definition) {
	t.Helper()
	installer := capability.Installer{ID: "brew", Kind: "brew", Formula: "owner/tap/summarize", Executables: []string{"summarize"}, OperatingSystems: []string{"linux"}}
	definition := &Definition{
		ID: "summarize", Version: "1.0.0.process.1", Name: "Summarize", Description: "Summarize a URL.",
		Source:       &SourceProvenance{Format: "openclaw.skill.v1", Digest: strings.Repeat("a", 64)},
		Requirements: Requirements{OperatingSystems: []string{"linux"}, Executables: []string{"summarize"}}, Installers: []capability.Installer{installer},
		Prompt: &PromptModule{Instructions: "Use the governed summarize action.", UserInvocable: true},
		Actions: map[string]Action{"execute": {
			Name: "execute", Description: "Execute summarize", Risk: RiskLevelExternal, SideEffect: SideEffectExternal,
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"properties": map[string]interface{}{"arguments": map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}}},
				"required":   []interface{}{"arguments"},
			}, Retry: ActionRetryPolicy{MaxAttempts: 1}, Idempotency: IdempotencySupported,
			Transport: &TransportReference{Kind: "tool", Endpoint: opensealprocess.TransportName, Arguments: map[string]TransportArgument{
				opensealprocess.ExecutableKey: {Literal: "summarize"}, opensealprocess.ArgumentsKey: {SourceArgument: "arguments"},
				opensealprocess.InstallersKey: {Literal: []capability.Installer{installer}}, opensealprocess.TimeoutSecondsKey: {Literal: 900},
			}},
		}},
	}
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &Binding{
		ID: "summarize", Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
		AllowedActions: []string{"execute"}, EnablePrompt: true, MaximumRisk: RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	return catalog, scope, definition
}

func hasRuntimeAvailabilityReason(reasons []AvailabilityReason, code string) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}
