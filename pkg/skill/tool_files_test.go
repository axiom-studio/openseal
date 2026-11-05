package skill

import (
	"errors"
	"reflect"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/toolfile"
)

// A Skill action whose schema takes base64 content takes a chat file as
// {"$file": path} there, and only there; a result whose declared base64
// field a host replaced with a file reference still validates.
func TestFileReferencesInSkillActions(t *testing.T) {
	catalog := NewCatalog()
	def := testSkillDefinition()
	deploy := def.Actions["deploy"]
	deploy.InputSchema["properties"].(map[string]interface{})["bundle"] = map[string]interface{}{"type": "string", "contentEncoding": "base64"}
	deploy.OutputSchema = map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"log"},
		"properties": map[string]interface{}{"log": map[string]interface{}{"type": "string", "contentMediaType": "application/gzip"}}}
	deploy.Transport = &TransportReference{Kind: "local", Endpoint: "internal://release", Arguments: map[string]TransportArgument{
		"env": {SourceArgument: "environment"}, "rev": {SourceArgument: "revision"}, "payload": {SourceArgument: "bundle"},
	}}
	def.Actions["deploy"] = deploy
	if err := catalog.Register(t.Context(), def); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	binding, err := catalog.UpsertBinding(t.Context(), UpsertBindingRequest{Binding: &Binding{ID: "release", Scope: scope, DeploymentID: "agent", SkillID: def.ID, SkillVersion: def.Version,
		MaximumRisk: RiskLevelProduction, AllowedActions: []string{"deploy"}, Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential://test-only"}}},
		Actor: BindingActor{Type: "user", ID: "one"}, Reason: "Release"})
	if err != nil {
		t.Fatal(err)
	}
	actions, err := catalog.ListModelActions(t.Context(), scope, "agent")
	if err != nil || len(actions) != 1 {
		t.Fatalf("actions: %#v %v", actions, err)
	}
	bundle := actions[0].InputSchema["properties"].(map[string]interface{})["bundle"].(map[string]interface{})
	if branches, _ := bundle["anyOf"].([]interface{}); len(branches) != 2 {
		t.Fatalf("model sees bundle as %#v", bundle)
	}
	bound, err := catalog.Resolve(t.Context(), scope, "agent", def.ID, def.Version, "deploy", BindingReference{ID: binding.ID, Revision: binding.Revision})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]interface{}{"environment": "prod", "revision": "r1", "bundle": toolfile.FileValue("tool-output/c1/build.tgz")}
	if err := catalog.ValidateInput(t.Context(), bound, input); err != nil {
		t.Fatalf("file reference rejected: %v", err)
	}
	references, err := FileReferences(bound, input)
	if err != nil || !reflect.DeepEqual(references, []toolfile.Reference{{Pointer: "/payload", Path: "tool-output/c1/build.tgz", Encoding: toolfile.Base64}}) {
		t.Fatalf("references = %#v, %v", references, err)
	}
	refused := map[string]interface{}{"environment": toolfile.FileValue("a.txt"), "revision": "r1"}
	var noFiles *toolfile.ErrNoFiles
	if err := catalog.ValidateInput(t.Context(), bound, refused); !errors.As(err, &noFiles) || noFiles.Field != "environment" {
		t.Fatalf("file in a text field: %v", err)
	}

	ref := toolfile.Ref{Type: toolfile.RefType, Path: "tool-output/c1/log.gz", MimeType: "application/gzip", SizeBytes: 12}.Value()
	if err := catalog.ValidateOutput(t.Context(), bound, map[string]interface{}{"log": ref}); err != nil {
		t.Fatalf("kept file rejected: %v", err)
	}
	if err := catalog.ValidateOutput(t.Context(), bound, map[string]interface{}{"log": map[string]interface{}{"other": true}}); err == nil {
		t.Fatal("an object that is not a file reference passed a string field")
	}
}

// A binding tool (an MCP server's tool) takes files where its own input
// schema takes base64 content; the pointer leads into the arguments object.
func TestFileReferencesInBindingTools(t *testing.T) {
	definition := &Definition{
		ID: "remote", Version: "1.0.0", Name: "Remote", Description: "Remote tools",
		Transport: TransportReference{Kind: "tool", Endpoint: "remote"},
		BindingTools: &capability.BindingToolsSpec{ModelPrefix: "remote", ReadAction: "read", WriteAction: "call", InputArgument: "arguments", FixedArguments: []string{"tool"}},
		Actions: map[string]Action{"read": {
			Name: "read", Description: "Read", SideEffect: SideEffectRead, Risk: RiskLevelRead, Idempotency: IdempotencySupported,
			InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"tool", "arguments"},
				"properties": map[string]interface{}{"tool": map[string]interface{}{"type": "string"}, "arguments": map[string]interface{}{"type": "object"}}},
		}},
	}
	call := definition.Actions["read"]
	call.Name, call.SideEffect, call.Risk, call.Idempotency = "call", SideEffectWrite, RiskLevelWrite, IdempotencyRequired
	definition.Actions["call"] = call
	catalog := NewCatalog()
	if err := catalog.Register(t.Context(), definition); err != nil {
		t.Fatal(err)
	}
	scope := ScopeReference{Kind: "tenant", ID: "one"}
	toolSchema := map[string]interface{}{"type": "object", "additionalProperties": false, "required": []interface{}{"image"}, "properties": map[string]interface{}{
		"image": map[string]interface{}{"type": "string", "contentEncoding": "base64", "minLength": 4}, "note": map[string]interface{}{"type": "string"},
	}}
	binding := &Binding{ID: "remote-binding", Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version, Revision: 1,
		AllowedActions: []string{"read"}, MaximumRisk: RiskLevelRead, Slug: "remote",
		Tools: []BindingTool{{Name: "describe", Description: "Describe an image", InputSchema: toolSchema, Effect: capability.BindingToolEffectRead, Arguments: map[string]interface{}{"tool": "describe"}}}}
	if err := catalog.Bind(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	bound, err := catalog.Resolve(t.Context(), scope, "agent", definition.ID, definition.Version, "read", BindingReference{ID: binding.ID, Revision: 1})
	if err != nil {
		t.Fatal(err)
	}
	input := map[string]interface{}{"tool": "describe", "arguments": map[string]interface{}{"image": toolfile.FileValue("tool-output/c/shot.png"), "note": "x"}}
	if err := catalog.ValidateInput(t.Context(), bound, input); err != nil {
		t.Fatalf("file reference rejected: %v", err)
	}
	references, err := FileReferences(bound, input)
	if err != nil || !reflect.DeepEqual(references, []toolfile.Reference{{Pointer: "/arguments/image", Path: "tool-output/c/shot.png", Encoding: toolfile.Base64}}) {
		t.Fatalf("references = %#v, %v", references, err)
	}
	refused := map[string]interface{}{"tool": "describe", "arguments": map[string]interface{}{"image": "AAAA", "note": toolfile.FileValue("a.png")}}
	var noFiles *toolfile.ErrNoFiles
	if err := catalog.ValidateInput(t.Context(), bound, refused); !errors.As(err, &noFiles) || noFiles.Field != "arguments.note" {
		t.Fatalf("file in a text field: %v", err)
	}
}
