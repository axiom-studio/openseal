package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

func TestToolActionDispatcherExecutesCompiledOpenClawEnvelope(t *testing.T) {
	compilation, err := skillopenclaw.Compile(skillopenclaw.Bundle{SkillMD: []byte(`---
name: publisher
description: Publish a release.
command-dispatch: tool
command-tool: release_publish
command-arg-mode: raw
---
Publish the requested release.
`)})
	if err != nil {
		t.Fatal(err)
	}
	definition := compilation.Definition
	bound := &skill.BoundAction{
		Definition: definition, Action: definition.Actions["invoke"],
		Binding: &skill.Binding{
			ID: "publisher", Scope: skill.ScopeReference{Kind: "test", ID: "one"}, DeploymentID: "agent",
			Config: map[string]interface{}{"provider": "openai-compatible", "base_url": "https://llm.example/v1", "model": "reasoner"},
		},
	}
	var invokedName string
	var invokedArguments map[string]interface{}
	var invokedCredentials map[string]string
	var invocation ToolInvocation
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(_ context.Context, value ToolInvocation) (map[string]interface{}, error) {
		invocation = value
		invokedName, invokedArguments, invokedCredentials = value.Name, value.Arguments, value.Credentials
		return map[string]interface{}{"published": true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	output, err := dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Call: &ActionCall{ID: "call", RunID: "run", PreparedRuntime: &skill.PreparedRuntime{
			PreparationID: "sha256:" + strings.Repeat("a", 64), RuntimeID: "oci://runtime.test/publisher@sha256:" + strings.Repeat("b", 64),
			Revision: "sha256:" + strings.Repeat("c", 64), Adapter: "oci-builder/v1", OperatingSystem: "linux", Architecture: "amd64", Executables: []string{"publisher"},
		}},
		Bound: bound, Arguments: map[string]interface{}{"command": "release 1.2.3"}, Credentials: map[string]string{"token": "resolved-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if invokedName != "release_publish" || invokedArguments["command"] != "release 1.2.3" || invokedArguments["commandName"] != "publisher" || invokedArguments["skillName"] != "publisher" ||
		len(invokedArguments) != 3 {
		t.Fatalf("tool invocation = name %q args %#v", invokedName, invokedArguments)
	}
	if invocation.BindingConfig["provider"] != "openai-compatible" || invocation.BindingConfig["base_url"] != "https://llm.example/v1" || invocation.BindingConfig["model"] != "reasoner" {
		t.Fatalf("trusted binding config = %#v", invocation.BindingConfig)
	}
	if invokedCredentials["token"] != "resolved-secret" || output["published"] != true {
		t.Fatalf("credentials/output = %#v %#v", invokedCredentials, output)
	}
	if invocation.Scope != bound.Binding.Scope || invocation.DeploymentID != "agent" || invocation.ExecutionDeploymentID != "agent" || invocation.SkillID != "publisher" || invocation.SkillVersion != definition.Version || invocation.Action != "invoke" {
		t.Fatalf("tool routing metadata = %#v", invocation)
	}
	if invocation.PreparedRuntime == nil || invocation.PreparedRuntime.RuntimeID != "oci://runtime.test/publisher@sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("prepared runtime was not dispatched: %#v", invocation.PreparedRuntime)
	}
}

func TestToolActionDispatcherCarriesOpaqueCredentialLeaseWithoutPlaintext(t *testing.T) {
	now, call, run, fields := actionCredentialLeaseFixture()
	lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-transport-00000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := SignActionCredentialLease(t.Context(), *lease, testCredentialLeaseSigner{key: []byte("lease-signing-key")})
	if err != nil {
		t.Fatal(err)
	}
	definition := &skill.Definition{
		ID: call.SkillID, Version: call.SkillVersion, Name: "Reddit research", Transport: skill.TransportReference{Kind: "tool", Endpoint: "reddit_search"},
		Actions: map[string]skill.Action{call.Action: {Name: call.Action, Description: "Search", InputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}},
	}
	bound := &skill.BoundAction{Definition: definition, Action: definition.Actions[call.Action], Binding: &skill.Binding{
		ID: call.BindingID, Revision: call.BindingRevision, Scope: skill.ScopeReference{Kind: call.Scope.Kind, ID: call.Scope.ID}, DeploymentID: call.DeploymentID,
	}}
	var invocation ToolInvocation
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(_ context.Context, value ToolInvocation) (map[string]interface{}, error) {
		invocation = value
		return map[string]interface{}{"ok": true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.DispatchAction(t.Context(), ActionDispatchInput{
		Call: call, Run: run, Bound: bound, Arguments: map[string]interface{}{},
		CredentialLease: envelope, CredentialReferences: cloneCredentialReferences(call.CredentialRefs),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(invocation.Credentials) != 0 || invocation.CredentialLease == nil || invocation.CredentialLease.Lease.ActionCallID != call.ID || invocation.CredentialReferences["reddit"] != call.CredentialRefs["reddit"] {
		t.Fatalf("opaque credential transport = %#v", invocation)
	}
	invocation.CredentialLease.Signature.Value[0] ^= 0xff
	if envelope.Signature.Value[0] == invocation.CredentialLease.Signature.Value[0] {
		t.Fatal("tool invocation aliases the caller's signed envelope")
	}

	_, err = dispatcher.DispatchAction(t.Context(), ActionDispatchInput{
		Call: call, Run: run, Bound: bound, Arguments: map[string]interface{}{}, Credentials: map[string]string{"reddit": "plaintext"},
		CredentialLease: envelope, CredentialReferences: cloneCredentialReferences(call.CredentialRefs),
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("mixed plaintext and opaque authority was accepted: %v", err)
	}

	_, err = dispatcher.DispatchAction(t.Context(), ActionDispatchInput{
		Call: call, Run: run, Bound: bound, Arguments: map[string]interface{}{}, CredentialLease: envelope,
		CredentialReferences: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "opaque://tenant-7/other"}},
	})
	if err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("substituted opaque reference was accepted: %v", err)
	}

	otherBound := *bound
	otherDefinition := *definition
	otherDefinition.Transport.Endpoint = "other_tool"
	otherBound.Definition = &otherDefinition
	_, err = dispatcher.DispatchAction(t.Context(), ActionDispatchInput{
		Call: call, Run: run, Bound: &otherBound, Arguments: map[string]interface{}{}, CredentialLease: envelope,
		CredentialReferences: cloneCredentialReferences(call.CredentialRefs),
	})
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("credential lease was paired with another tool endpoint: %v", err)
	}
}

func TestToolActionDispatcherSeparatesTeamAuthorityFromAgentExecution(t *testing.T) {
	definition := &skill.Definition{
		ID: "fetch", Version: "1", Name: "Fetch", Transport: skill.TransportReference{Kind: "tool", Endpoint: "fetch"},
		Actions: map[string]skill.Action{"run": {Name: "run", Description: "Fetch", InputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}},
	}
	binding := &skill.Binding{ID: "team-fetch", Scope: skill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "research-team"}
	var invocation ToolInvocation
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(_ context.Context, value ToolInvocation) (map[string]interface{}, error) {
		invocation = value
		return map[string]interface{}{"ok": true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Run:   &AgentRun{Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "research-team"}, AssignedAgentID: "researcher-agent"},
		Bound: &skill.BoundAction{Definition: definition, Action: definition.Actions["run"], Binding: binding}, Arguments: map[string]interface{}{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.DeploymentID != "research-team" || invocation.ExecutionDeploymentID != "researcher-agent" {
		t.Fatalf("Team authority and execution placement were conflated: %#v", invocation)
	}

	_, err = dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Run:   &AgentRun{Scope: Scope{Kind: "tenant", ID: "one"}, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "other-team"}, AssignedAgentID: "researcher-agent"},
		Bound: &skill.BoundAction{Definition: definition, Action: definition.Actions["run"], Binding: binding}, Arguments: map[string]interface{}{},
	})
	if err == nil || !strings.Contains(err.Error(), "owned outside") {
		t.Fatalf("foreign Team binding reached execution: %v", err)
	}
}

func TestToolActionDispatcherSeparatesBindingConfigFromModelArguments(t *testing.T) {
	definition := &skill.Definition{
		ID: "fetch", Version: "1", Name: "Fetch", Transport: skill.TransportReference{Kind: "tool", Endpoint: "fetch"},
		Actions: map[string]skill.Action{"run": {Name: "run", Description: "Fetch", InputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}},
	}
	var invocation ToolInvocation
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(_ context.Context, value ToolInvocation) (map[string]interface{}, error) {
		invocation = value
		return map[string]interface{}{"ok": true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	binding := &skill.Binding{Config: map[string]interface{}{"url": "fixed"}}
	_, err = dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Bound:     &skill.BoundAction{Definition: definition, Action: definition.Actions["run"], Binding: binding},
		Arguments: map[string]interface{}{"url": "model-controlled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if invocation.Arguments["url"] != "model-controlled" || invocation.BindingConfig["url"] != "fixed" {
		t.Fatalf("invocation did not preserve authority boundary: %#v", invocation)
	}
	invocation.BindingConfig["url"] = "mutated"
	if binding.Config["url"] != "fixed" {
		t.Fatal("tool host mutated the durable binding config")
	}
}

func TestBoundActionToolTransportUsesActionOverrideAndRejectsNonTool(t *testing.T) {
	override := skill.TransportReference{Kind: "tool", Endpoint: "action_tool"}
	bound := &skill.BoundAction{
		Definition: &skill.Definition{Transport: skill.TransportReference{Kind: "tool", Endpoint: "definition_tool"}},
		Action:     skill.Action{Transport: &override},
	}
	endpoint, err := boundActionToolTransport(bound)
	if err != nil || endpoint != "action_tool" {
		t.Fatalf("action transport override = %q, %v", endpoint, err)
	}
	bound.Action.Transport = nil
	bound.Definition.Transport = skill.TransportReference{Kind: "http", Endpoint: "https://example.invalid"}
	if _, err := boundActionToolTransport(bound); err == nil {
		t.Fatal("non-tool delegated transport was accepted")
	}
	bound.Definition.Transport = skill.TransportReference{Kind: "tool"}
	if _, err := boundActionToolTransport(bound); err == nil {
		t.Fatal("empty delegated tool endpoint was accepted")
	}
}
