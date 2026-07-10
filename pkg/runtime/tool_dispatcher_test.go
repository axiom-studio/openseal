package runtime

import (
	"context"
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
		Binding: &skill.Binding{ID: "publisher", Scope: skill.ScopeReference{Kind: "test", ID: "one"}, DeploymentID: "agent"},
	}
	var invokedName string
	var invokedArguments map[string]interface{}
	var invokedCredentials map[string]string
	dispatcher, err := NewToolActionDispatcher(ToolInvokerFunc(func(_ context.Context, name string, arguments map[string]interface{}, credentials map[string]string) (map[string]interface{}, error) {
		invokedName, invokedArguments, invokedCredentials = name, arguments, credentials
		return map[string]interface{}{"published": true}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	output, err := dispatcher.DispatchAction(context.Background(), ActionDispatchInput{
		Bound: bound, Arguments: map[string]interface{}{"command": "release 1.2.3"}, Credentials: map[string]string{"token": "resolved-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if invokedName != "release_publish" || invokedArguments["command"] != "release 1.2.3" || invokedArguments["commandName"] != "publisher" || invokedArguments["skillName"] != "publisher" || len(invokedArguments) != 3 {
		t.Fatalf("tool invocation = name %q args %#v", invokedName, invokedArguments)
	}
	if invokedCredentials["token"] != "resolved-secret" || output["published"] != true {
		t.Fatalf("credentials/output = %#v %#v", invokedCredentials, output)
	}
}
