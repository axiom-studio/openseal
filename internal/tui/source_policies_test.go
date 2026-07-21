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
	policies  []*source.Lifecycle
	listCalls int
}

func (f *sourcePolicyKernelClient) RegisterSourcePolicyVersion(context.Context, source.RegisterVersionRequest) (*source.PolicyVersion, error) {
	return nil, nil
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
func (f *sourcePolicyKernelClient) ActivateSourcePolicy(context.Context, string, source.ActivateRequest) (*source.LifecycleResult, error) {
	return nil, nil
}
func (f *sourcePolicyKernelClient) RevokeSourcePolicy(context.Context, string, source.RevokeRequest) (*source.LifecycleResult, error) {
	return nil, nil
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
