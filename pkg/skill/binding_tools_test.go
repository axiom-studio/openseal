package skill

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

var bindingToolsScope = ScopeReference{Kind: "tenant", ID: "one"}

func bindingToolsDefinition() *Definition {
	input := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"tool":      map[string]interface{}{"type": "string", "minLength": 1},
			"hash":      map[string]interface{}{"type": "string", "pattern": "^sha256:"},
			"arguments": map[string]interface{}{"type": "object"},
		},
		"required": []interface{}{"tool", "hash", "arguments"},
	}
	return &Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Description: "Call a connected service",
		Transport:    TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &BindingToolsSpec{ModelPrefix: "connector", ReadAction: "read", WriteAction: "call", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]Action{
			"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input, Risk: RiskLevelRead, SideEffect: SideEffectRead, Idempotency: IdempotencySupported},
			"call": {Name: "call", Description: "Call a tool", InputSchema: input, Risk: RiskLevelWrite, SideEffect: SideEffectWrite, Idempotency: IdempotencySupported},
			"refresh": {Name: "refresh", Description: "Refresh the tool list", Risk: RiskLevelRead, SideEffect: SideEffectRead, Idempotency: IdempotencySupported,
				InputSchema: map[string]interface{}{"type": "object", "additionalProperties": false}},
		},
	}
}

func bindingToolsTool(name string, effect BindingToolEffect) BindingTool {
	return BindingTool{
		Name: name, Description: "The " + name + " tool", Effect: effect,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"query": map[string]interface{}{"type": "string", "minLength": 1}},
			"required":   []interface{}{"query"},
		},
		Arguments: map[string]interface{}{"tool": name, "hash": "sha256:" + name},
	}
}

func bindingToolsBinding(id, slug string, tools ...BindingTool) *Binding {
	return &Binding{
		ID: id, Scope: bindingToolsScope, DeploymentID: "agent", SkillID: "openseal.connector", SkillVersion: "1.0.0",
		AllowedActions: []string{"read", "call", "refresh"}, MaximumRisk: RiskLevelWrite, Slug: slug, Tools: tools, Revision: 1,
	}
}

func bindingToolsCatalog(t testing.TB, bindings ...*Binding) *Catalog {
	t.Helper()
	catalog := NewCatalog()
	if err := catalog.Register(context.Background(), bindingToolsDefinition()); err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if err := catalog.Bind(context.Background(), binding); err != nil {
			t.Fatal(err)
		}
	}
	return catalog
}

func activatedActions(snapshot *ActivationSnapshot) map[string]ModelAction {
	result := make(map[string]ModelAction)
	for _, activated := range snapshot.Skills {
		for _, action := range activated.Actions {
			result[action.Name] = action
		}
	}
	return result
}

func TestBindingToolsProjectOneRoutedModelActionPerTool(t *testing.T) {
	catalog := bindingToolsCatalog(t, bindingToolsBinding("github-binding", "github",
		bindingToolsTool("search", BindingToolEffectRead), bindingToolsTool("create_issue", BindingToolEffectWrite)))
	snapshot, err := catalog.Activate(context.Background(), bindingToolsScope, "agent", HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	actions := activatedActions(snapshot)
	names := make([]string, 0, len(actions))
	for name := range actions {
		names = append(names, name)
	}
	if snapshot.Skills[0].Actions[0].Name != "connector.github.create_issue" || len(actions) != 3 ||
		actions["connector.github.search"].Name == "" || actions["openseal.connector.refresh"].Name == "" {
		t.Fatalf("model actions = %v", names)
	}
	search, create := actions["connector.github.search"], actions["connector.github.create_issue"]
	if search.Action != "read" || search.Risk != RiskLevelRead || search.SideEffect != SideEffectRead ||
		create.Action != "call" || create.Risk != RiskLevelWrite || create.SideEffect != SideEffectWrite {
		t.Fatalf("effect did not select the base action: %#v %#v", search, create)
	}
	if !valuesEqual(search.InputSchema, bindingToolsTool("search", BindingToolEffectRead).InputSchema) || search.Description != "The search tool" {
		t.Fatalf("tool schema = %#v", search.InputSchema)
	}
	input := search.ActionInput(map[string]interface{}{"query": "openseal"})
	want := map[string]interface{}{"tool": "search", "hash": "sha256:search", "arguments": map[string]interface{}{"query": "openseal"}}
	if !reflect.DeepEqual(input, want) {
		t.Fatalf("compiled input = %#v", input)
	}
	bound, err := catalog.Resolve(context.Background(), bindingToolsScope, "agent", search.SkillID, search.Version, search.Action, BindingReference{ID: search.BindingID, Revision: search.BindingRevision})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.ValidateInput(context.Background(), bound, input); err != nil {
		t.Fatal(err)
	}
	listed, err := catalog.ListModelActions(context.Background(), bindingToolsScope, "agent")
	if err != nil || len(listed) != 3 || listed[0].Name != "connector.github.create_issue" || listed[0].DeploymentID != "agent" {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
}

func TestBindingToolsEnforceDeclaredToolsAtCallTime(t *testing.T) {
	catalog := bindingToolsCatalog(t, bindingToolsBinding("github-binding", "github",
		bindingToolsTool("search", BindingToolEffectRead), bindingToolsTool("create_issue", BindingToolEffectWrite)))
	ctx := context.Background()
	read, err := catalog.Resolve(ctx, bindingToolsScope, "agent", "openseal.connector", "1.0.0", "read")
	if err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]map[string]interface{}{
		"undeclared tool": {"tool": "delete_repo", "hash": "sha256:delete_repo", "arguments": map[string]interface{}{"query": "x"}},
		"stale hash":      {"tool": "search", "hash": "sha256:other", "arguments": map[string]interface{}{"query": "x"}},
		"write via read":  {"tool": "create_issue", "hash": "sha256:create_issue", "arguments": map[string]interface{}{"query": "x"}},
		"tool schema":     {"tool": "search", "hash": "sha256:search", "arguments": map[string]interface{}{"query": ""}},
	} {
		if err := catalog.ValidateInput(ctx, read, input); err == nil {
			t.Fatalf("%s: input was accepted", name)
		}
	}
	if err := catalog.ValidateInput(ctx, read, map[string]interface{}{"tool": "search", "hash": "sha256:search", "arguments": map[string]interface{}{"query": "x"}}); err != nil {
		t.Fatal(err)
	}
}

// Regression: two bindings of one Skill used to expose identical model action
// names, and hosted turn compilation routed both to the first binding.
func TestBindingToolsTwoBindingsOfOneSkillRouteExactly(t *testing.T) {
	catalog := bindingToolsCatalog(t,
		bindingToolsBinding("github-binding", "github", bindingToolsTool("search", BindingToolEffectRead)),
		bindingToolsBinding("linear-binding", "linear", bindingToolsTool("search", BindingToolEffectRead)))
	ctx := context.Background()
	snapshot, err := catalog.Activate(ctx, bindingToolsScope, "agent", HostCapabilityState{})
	if err != nil {
		t.Fatal(err)
	}
	actions := activatedActions(snapshot)
	github, linear := actions["connector.github.search"], actions["connector.linear.search"]
	if github.BindingID != "github-binding" || linear.BindingID != "linear-binding" {
		t.Fatalf("actions = %#v", actions)
	}
	// Both bindings still expose the plain refresh action under one name.
	// The skill catalog leaves that to the host projection, which refuses it.
	if actions["openseal.connector.refresh"].Name == "" {
		t.Fatal("refresh action missing")
	}
	bound, err := catalog.Resolve(ctx, bindingToolsScope, "agent", linear.SkillID, linear.Version, linear.Action, BindingReference{ID: linear.BindingID, Revision: linear.BindingRevision})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Binding.ID != "linear-binding" {
		t.Fatalf("resolved %s", bound.Binding.ID)
	}
}

func TestBindingToolsRefuseDuplicateSlugsAndNameCollisions(t *testing.T) {
	ctx := context.Background()
	catalog := bindingToolsCatalog(t, bindingToolsBinding("github-binding", "github", bindingToolsTool("search", BindingToolEffectRead)))
	duplicate := bindingToolsBinding("other-binding", "github", bindingToolsTool("list", BindingToolEffectRead))
	if err := catalog.Bind(ctx, duplicate); !errors.Is(err, ErrBindingInvalid) {
		t.Fatalf("duplicate slug: %v", err)
	}
	derived := bindingToolsBinding("Team_GitHub.Prod", "", bindingToolsTool("list", BindingToolEffectRead))
	if err := catalog.Bind(ctx, derived); err != nil {
		t.Fatal(err)
	}
	if slug := BindingSlug(derived); slug != "team-github-prod" {
		t.Fatalf("derived slug = %q", slug)
	}
	store := newBindingManagementStore()
	durable := NewCatalogWithStore(store)
	if err := durable.Register(ctx, bindingToolsDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := durable.Bind(ctx, bindingToolsBinding("github-binding", "github", bindingToolsTool("search", BindingToolEffectRead))); err != nil {
		t.Fatal(err)
	}
	if err := durable.Bind(ctx, duplicate); !errors.Is(err, ErrBindingInvalid) {
		t.Fatalf("durable duplicate slug: %v", err)
	}
	// A definition whose own action name equals a binding tool name is a hard
	// activation error, never a first-match choice.
	colliding := &Definition{
		ID: "connector.github", Version: "1.0.0", Name: "Colliding", Transport: TransportReference{Kind: "local", Endpoint: "internal://colliding"},
		Actions: map[string]Action{"search": {Name: "search", Description: "Search", Risk: RiskLevelRead, SideEffect: SideEffectRead, Idempotency: IdempotencySupported,
			InputSchema: map[string]interface{}{"type": "object"}}},
	}
	if err := catalog.Register(ctx, colliding); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Bind(ctx, &Binding{ID: "colliding", Scope: bindingToolsScope, DeploymentID: "agent", SkillID: colliding.ID, SkillVersion: "1.0.0",
		AllowedActions: []string{"search"}, MaximumRisk: RiskLevelRead, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.Activate(ctx, bindingToolsScope, "agent", HostCapabilityState{}); !errors.Is(err, ErrBindingAmbiguous) {
		t.Fatalf("activation collision: %v", err)
	}
	if _, err := catalog.ListModelActions(ctx, bindingToolsScope, "agent"); !errors.Is(err, ErrBindingAmbiguous) {
		t.Fatalf("listing collision: %v", err)
	}
}

func TestBindingToolsValidation(t *testing.T) {
	valid := func() *Binding {
		return bindingToolsBinding("github-binding", "github", bindingToolsTool("search", BindingToolEffectRead), bindingToolsTool("create", BindingToolEffectWrite))
	}
	many := make([]BindingTool, MaxBindingTools+1)
	for index := range many {
		many[index] = bindingToolsTool(fmt.Sprintf("tool_%03d", index), BindingToolEffectRead)
	}
	cases := map[string]func(*Binding){
		"duplicate name":      func(b *Binding) { b.Tools[1].Name = "search"; b.Tools[1].Arguments["tool"] = "search" },
		"too many tools":      func(b *Binding) { b.Tools = many },
		"invalid name":        func(b *Binding) { b.Tools[0].Name = "has space" },
		"missing description": func(b *Binding) { b.Tools[0].Description = " " },
		"long description":    func(b *Binding) { b.Tools[0].Description = strings.Repeat("x", MaxBindingToolDescriptionBytes+1) },
		"unknown effect":      func(b *Binding) { b.Tools[0].Effect = "admin" },
		"write not allowed":   func(b *Binding) { b.AllowedActions = []string{"read"}; b.MaximumRisk = RiskLevelRead },
		"schema does not compile": func(b *Binding) {
			b.Tools[0].InputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": 5}}}
		},
		"external reference": func(b *Binding) {
			b.Tools[0].InputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"$ref": "https://example.test/schema"}}}
		},
		"not an object schema": func(b *Binding) { b.Tools[0].InputSchema = map[string]interface{}{"type": "string"} },
		"schema too large": func(b *Binding) {
			b.Tools[0].InputSchema = map[string]interface{}{"type": "object", "description": strings.Repeat("x", MaxBindingToolSchemaBytes)}
		},
		"reserved extension": func(b *Binding) {
			b.Tools[0].InputSchema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string", SchemaExtensionKernelResolved: true}}}
		},
		"missing fixed argument": func(b *Binding) { delete(b.Tools[0].Arguments, "hash") },
		"extra fixed argument":   func(b *Binding) { b.Tools[0].Arguments["arguments"] = map[string]interface{}{} },
		"fixed argument schema":  func(b *Binding) { b.Tools[0].Arguments["hash"] = "md5:search" },
		"identical fixed arguments": func(b *Binding) {
			b.Tools[1].Effect = BindingToolEffectRead
			b.Tools[1].Arguments = map[string]interface{}{"tool": "search", "hash": "sha256:search"}
		},
		"invalid slug": func(b *Binding) { b.Slug = "GitHub!" },
		"restricted base action": func(b *Binding) {
			b.ArgumentRestrictions = map[string]map[string]ArgumentRule{"read": {"tool": {Const: "search"}}}
		},
		"bound input argument": func(b *Binding) {
			b.ArgumentBindings = map[string]map[string]BindingArgumentValue{"read": {"arguments": {Source: BindingArgumentLiteral, Literal: map[string]interface{}{}}}}
		},
		"bound fixed argument": func(b *Binding) {
			b.ArgumentBindings = map[string]map[string]BindingArgumentValue{"read": {"hash": {Source: BindingArgumentLiteral, Literal: "sha256:x"}}}
		},
	}
	definition := bindingToolsDefinition()
	if err := validateBindingAgainstDefinition(valid(), definition); err != nil {
		t.Fatalf("valid binding: %v", err)
	}
	for name, change := range cases {
		binding := valid()
		change(binding)
		if err := validateBindingAgainstDefinition(binding, definition); err == nil {
			t.Errorf("%s: binding was accepted", name)
		}
	}
	plain := testSkillDefinition()
	toolsOnPlain := &Binding{ID: "plain", Scope: bindingToolsScope, DeploymentID: "agent", SkillID: plain.ID, SkillVersion: plain.Version,
		AllowedActions: []string{"deploy"}, MaximumRisk: RiskLevelProduction, Tools: []BindingTool{bindingToolsTool("search", BindingToolEffectRead)},
		Credentials: map[string]CredentialReference{"git": {Kind: "git-token", ID: "credential://git"}}, Revision: 1}
	if err := validateBindingAgainstDefinition(toolsOnPlain, plain); err == nil {
		t.Error("tools on a Skill without binding tools were accepted")
	}
}

func TestBindingToolsSpecValidation(t *testing.T) {
	cases := map[string]func(*Definition){
		"no routed action":       func(d *Definition) { d.BindingTools.ReadAction, d.BindingTools.WriteAction = "", "" },
		"missing action":         func(d *Definition) { d.BindingTools.WriteAction = "missing" },
		"read action writes":     func(d *Definition) { d.BindingTools.ReadAction = "call" },
		"missing input argument": func(d *Definition) { d.BindingTools.InputArgument = "payload" },
		"missing fixed argument": func(d *Definition) { d.BindingTools.FixedArguments = []string{"tool", "version"} },
		"no fixed arguments":     func(d *Definition) { d.BindingTools.FixedArguments = nil },
		"fixed input argument":   func(d *Definition) { d.BindingTools.FixedArguments = []string{"arguments"} },
		"invalid prefix":         func(d *Definition) { d.BindingTools.ModelPrefix = "has space" },
		"optional fixed argument": func(d *Definition) {
			action := d.Actions["read"]
			schema := cloneMap(action.InputSchema)
			schema["required"] = []interface{}{"arguments"}
			action.InputSchema = schema
			d.Actions["read"] = action
		},
	}
	if err := validateDefinition(bindingToolsDefinition()); err != nil {
		t.Fatal(err)
	}
	for name, change := range cases {
		definition := bindingToolsDefinition()
		change(definition)
		if err := validateDefinition(definition); err == nil {
			t.Errorf("%s: definition was accepted", name)
		}
	}
}
