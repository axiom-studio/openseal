package runtime

import (
	"context"
	"strings"
	"testing"

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
		invokedArguments["provider"] != "openai-compatible" || invokedArguments["base_url"] != "https://llm.example/v1" || invokedArguments["model"] != "reasoner" || len(invokedArguments) != 6 {
		t.Fatalf("tool invocation = name %q args %#v", invokedName, invokedArguments)
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

func TestToolActionDispatcherRejectsBindingConfigCollision(t *testing.T) {
	definition := &skill.Definition{
		ID: "fetch", Version: "1", Name: "Fetch", Transport: skill.TransportReference{Kind: "tool", Endpoint: "fetch"},
		Actions: map[string]skill.Action{"run": {Name: "run", Description: "Fetch", InputSchema: map[string]interface{}{"type": "object"}, Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}},
	}
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(context.Context, ToolInvocation) (map[string]interface{}, error) {
		t.Fatal("colliding invocation reached tool host")
		return nil, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Bound:     &skill.BoundAction{Definition: definition, Action: definition.Actions["run"], Binding: &skill.Binding{Config: map[string]interface{}{"url": "fixed"}}},
		Arguments: map[string]interface{}{"url": "model-controlled"},
	})
	if err == nil {
		t.Fatal("binding config collision was accepted")
	}
}
