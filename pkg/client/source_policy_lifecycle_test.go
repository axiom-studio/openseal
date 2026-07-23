package client_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/client"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/source"
	"go.uber.org/zap"
)

func TestSourcePolicyLifecycleHTTPRoundTrip(t *testing.T) {
	store := source.NewMemoryLifecycleStore()
	service, err := source.NewLifecycleService(store)
	if err != nil {
		t.Fatal(err)
	}
	kernel := server.NewServer(runtime.NewMemoryStore(), zap.NewNop().Sugar())
	kernel.SetSourcePolicyLifecycle(service)
	httpServer := httptest.NewServer(kernel.Handler())
	defer httpServer.Close()
	api := client.NewKernelHTTPClient(httpServer.URL, httpServer.Client())
	ctx := context.Background()
	scope := capability.ScopeReference{Kind: "tenant", ID: "42"}

	document, err := api.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	contract, ok := document.Find(kernelapi.SourcePoliciesCapabilityID, kernelapi.SourcePoliciesCapabilityVersion)
	if !ok || !contract.Supports(kernelapi.OperationRegister) || !contract.Supports(kernelapi.OperationRevoke) || !contract.Supports(kernelapi.OperationListVersions) {
		t.Fatalf("source policy capability = %#v", contract)
	}

	policy := source.Policy{ID: "approved-forums", Version: "2026-07-21", Enabled: true, MaximumItems: 10, RetentionDays: 30,
		Sources:  []source.PolicySource{{Host: "www.reddit.com", PathPrefixes: []string{"/r/kubernetes"}, Methods: []string{http.MethodGet}}},
		Outreach: &source.OutreachPolicy{Enabled: true, ApprovalPolicy: "human-review", MaximumBytes: 1500}}
	registered, err := api.RegisterSourcePolicyVersion(ctx, source.RegisterVersionRequest{Scope: scope, Policy: policy, ActorType: "user", ActorID: "admin", Reason: "reviewed exact source authority"})
	if err != nil || registered.Policy.ID != policy.ID || registered.Policy.Sources[0].Methods[0] != http.MethodGet || registered.ActorID != "admin" {
		t.Fatalf("registered = %#v, %v", registered, err)
	}
	activated, err := api.ActivateSourcePolicy(ctx, policy.ID, source.ActivateRequest{Scope: scope, Version: policy.Version, ExpectedRevision: 0, ActorType: "user", ActorID: "admin", Reason: "approved"})
	if err != nil || activated.APIVersion != source.LifecycleAPIVersion || activated.Lifecycle.Revision != 1 || activated.Event.ToState != source.LifecycleActive {
		t.Fatalf("activated = %#v, %v", activated, err)
	}
	detail, err := api.GetSourcePolicy(ctx, scope, policy.ID)
	if err != nil || detail.Policy.Version != policy.Version || detail.Lifecycle.State != source.LifecycleActive {
		t.Fatalf("detail = %#v, %v", detail, err)
	}
	version, err := api.GetSourcePolicyVersion(ctx, scope, policy.ID, policy.Version)
	if err != nil || version.Policy.MaximumItems != 10 {
		t.Fatalf("version = %#v, %v", version, err)
	}
	versions, err := api.ListSourcePolicyVersions(ctx, scope, policy.ID)
	if err != nil || versions.APIVersion != source.LifecycleAPIVersion || len(versions.Items) != 1 {
		t.Fatalf("versions = %#v, %v", versions, err)
	}
	listed, err := api.ListSourcePolicies(ctx, scope)
	if err != nil || len(listed.Items) != 1 || listed.Items[0].PolicyID != policy.ID {
		t.Fatalf("listed = %#v, %v", listed, err)
	}

	if _, err := api.ActivateSourcePolicy(ctx, policy.ID, source.ActivateRequest{Scope: scope, Version: policy.Version, ExpectedRevision: 0, ActorType: "user", ActorID: "admin", Reason: "stale"}); err == nil {
		t.Fatal("stale HTTP activation was accepted")
	} else if apiErr := new(client.APIError); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		t.Fatalf("stale activation error = %T %v", err, err)
	}
	revoked, err := api.RevokeSourcePolicy(ctx, policy.ID, source.RevokeRequest{Scope: scope, ExpectedRevision: 1, ActorType: "user", ActorID: "admin", Reason: "source approval withdrawn"})
	if err != nil || revoked.Lifecycle.State != source.LifecycleRevoked || revoked.Lifecycle.Revision != 2 {
		t.Fatalf("revoked = %#v, %v", revoked, err)
	}
	events, err := api.ListSourcePolicyActivations(ctx, scope, policy.ID)
	if err != nil || events.APIVersion != source.LifecycleAPIVersion || len(events.Items) != 2 || events.Items[1].ToState != source.LifecycleRevoked {
		t.Fatalf("events = %#v, %v", events, err)
	}
}

func TestSourcePolicyClientRejectsContractDrift(t *testing.T) {
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"openseal.source-policy/v99","items":[]}`))
	}))
	defer httpServer.Close()
	api := client.NewKernelHTTPClient(httpServer.URL+"/api/v1", httpServer.Client())
	_, err := api.ListSourcePolicies(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "42"})
	if err == nil {
		t.Fatal("future source policy contract was silently accepted")
	}
}
