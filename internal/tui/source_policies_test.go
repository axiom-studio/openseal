package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/source"
)

type sourcePolicyKernelClient struct {
	*fakeKernelClient
	policies   []*source.Lifecycle
	listCalls  int
	registered *source.RegisterVersionRequest
	activated  *source.ActivateRequest
	revoked    *source.RevokeRequest
}

func (f *sourcePolicyKernelClient) RegisterSourcePolicyVersion(_ context.Context, request source.RegisterVersionRequest) (*source.PolicyVersion, error) {
	f.registered = &request
	return &source.PolicyVersion{APIVersion: source.LifecycleAPIVersion, Scope: request.Scope, Policy: &request.Policy}, nil
}
func (f *sourcePolicyKernelClient) GetSourcePolicy(context.Context, capability.ScopeReference, string) (*source.LifecycleDetail, error) {
	return nil, nil
}
func (f *sourcePolicyKernelClient) ListSourcePolicies(context.Context, capability.ScopeReference) (*source.LifecycleList, error) {
	f.listCalls++
	return &source.LifecycleList{APIVersion: source.LifecycleAPIVersion, Items: f.policies}, nil
}
func (f *sourcePolicyKernelClient) GetSourcePolicyVersion(context.Context, capability.ScopeReference, string, string) (*source.PolicyVersion, error) {
	return nil, nil
}
func (f *sourcePolicyKernelClient) ListSourcePolicyVersions(context.Context, capability.ScopeReference, string) (*source.PolicyVersionList, error) {
	return nil, nil
}
func (f *sourcePolicyKernelClient) ActivateSourcePolicy(_ context.Context, _ string, request source.ActivateRequest) (*source.LifecycleResult, error) {
	f.activated = &request
	return &source.LifecycleResult{APIVersion: source.LifecycleAPIVersion, Lifecycle: &source.Lifecycle{PolicyID: request.PolicyID, ActiveVersion: request.Version, State: source.LifecycleActive, Revision: request.ExpectedRevision + 1}}, nil
}
func (f *sourcePolicyKernelClient) RevokeSourcePolicy(_ context.Context, _ string, request source.RevokeRequest) (*source.LifecycleResult, error) {
	f.revoked = &request
	return &source.LifecycleResult{APIVersion: source.LifecycleAPIVersion, Lifecycle: &source.Lifecycle{PolicyID: request.PolicyID, ActiveVersion: "1", State: source.LifecycleRevoked, Revision: request.ExpectedRevision + 1}}, nil
}

func TestTUISourcePolicyMutationsUseAdvertisedPortableLifecycle(t *testing.T) {
	fake := &sourcePolicyKernelClient{fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.SourcePoliciesCapability())}}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())

	model.editor.SetValue("id: approved-forums\nversion: 2\nsources: forums.example|/feeds|GET,HEAD\nmax-items: 10\nretention-days: 30\napproval: operator-review\nreason: reviewed public feeds")
	applyCommand(t, model, model.submitSourcePolicyRegister())
	if fake.registered == nil || fake.registered.Policy.ID != "approved-forums" || len(fake.registered.Policy.Sources) != 1 || fake.registered.ActorID != model.config.Actor.ID {
		t.Fatalf("registered request = %#v", fake.registered)
	}
	if len(model.sourcePolicies) != 0 {
		t.Fatal("registration must not invent active authority")
	}

	model.editor.SetValue("policy: approved-forums\nversion: 2\nrevision: 0\nreason: approved after review")
	applyCommand(t, model, model.submitSourcePolicyActivate())
	if fake.activated == nil || fake.activated.ExpectedRevision != 0 || fake.activated.Version != "2" {
		t.Fatalf("activation request = %#v", fake.activated)
	}

	model.editor.SetValue("policy: approved-forums\nrevision: 1\nreason: access no longer required")
	applyCommand(t, model, model.submitSourcePolicyRevoke())
	if fake.revoked == nil || fake.revoked.ExpectedRevision != 1 {
		t.Fatalf("revocation request = %#v", fake.revoked)
	}
}
func (f *sourcePolicyKernelClient) ListSourcePolicyActivations(context.Context, capability.ScopeReference, string) (*source.LifecycleEventList, error) {
	return nil, nil
}

func TestTUIDiscoversSourcePolicyLifecycleWithoutInventingControls(t *testing.T) {
	fake := &sourcePolicyKernelClient{fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(kernelapi.SourcePoliciesCapability())}, policies: []*source.Lifecycle{
		{APIVersion: source.LifecycleAPIVersion, Scope: capability.ScopeReference{Kind: "local", ID: "default"}, PolicyID: "approved-forums", ActiveVersion: "2", State: source.LifecycleActive, Revision: 4},
		{APIVersion: source.LifecycleAPIVersion, Scope: capability.ScopeReference{Kind: "local", ID: "default"}, PolicyID: "retired-feed", ActiveVersion: "1", State: source.LifecycleRevoked, Revision: 2},
	}}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	applyCommand(t, model, model.loadSourcePolicies())
	view := model.View()
	for _, expected := range []string{"s Skills", "Governed source access", "active", "approved-forums@2", "revoked", "retired-feed@1"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("source policy TUI missing %q:\n%s", expected, view)
		}
	}
	if fake.listCalls != 2 {
		// Capability loading schedules one list and the explicit refresh above
		// schedules another; both use the public client rather than local state.
		t.Fatalf("source policy list calls = %d", fake.listCalls)
	}
	for _, forbidden := range []string{"activate policy", "revoke policy", "edit policy"} {
		if strings.Contains(strings.ToLower(view), forbidden) {
			t.Fatalf("TUI invented unimplemented control %q:\n%s", forbidden, view)
		}
	}
}

func TestTUISourcePoliciesFailClosedOnCapabilityVersionDrift(t *testing.T) {
	drifted := kernelapi.SourcePoliciesCapability()
	drifted.Version = "openseal.source-policy/v99"
	fake := &sourcePolicyKernelClient{fakeKernelClient: &fakeKernelClient{document: kernelapi.NewCapabilityDocument(drifted)}}
	model := newModelWithClient(t, fake)
	applyCommand(t, model, model.loadCapabilities())
	if model.sourcePolicyCapability.Available || fake.listCalls != 0 || strings.Contains(model.View(), "Governed source access") {
		t.Fatalf("future source policy capability leaked into TUI:\n%s", model.View())
	}
}
