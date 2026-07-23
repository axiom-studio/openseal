package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/httpaction"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestCompileProducesFirstClassGovernedSkill(t *testing.T) {
	compilation, err := Compile(Bundle{
		SkillMD: []byte(`---
name: publish-release
description: Publish a prepared release.
license: MIT-0
allowed-tools: release_publish Read
command-dispatch: tool
command-tool: release_publish
metadata:
  openclaw:
    primaryEnv: RELEASE_TOKEN
    skillKey: release-config
    emoji: "🚀"
    requires:
      bins: [release]
      env: [RELEASE_TOKEN]
    install:
      - kind: go
        module: example.test/release@v1.2.0
        bins: [release]
---
Read references/policy.md before publishing.
`),
		Files:  []File{{Path: "references/policy.md", Content: []byte("Policy")}, {Path: "scripts/check.sh", Content: []byte("#!/bin/sh")}},
		Source: Source{Registry: "https://registry.test", Publisher: "example", Reference: "example/publish-release", Version: "1.4.0", Trust: map[string]interface{}{"verified": true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := compilation.Definition
	if !strings.HasPrefix(definition.Version, "1.4.0+source."+compilation.SourceDigest[:12]+".origin.") || definition.Source.ResolvedVersion != "1.4.0" || definition.Prompt == nil || len(definition.Actions) != 1 || len(definition.Resources) != 2 {
		t.Fatalf("incomplete compilation: %#v", definition)
	}
	if definition.ConfigurationKey != "release-config" || definition.Icon != "🚀" {
		t.Fatalf("configuration/presentation metadata not compiled: %#v", definition)
	}
	action := definition.Actions["invoke"]
	if action.Transport != nil || definition.Transport.Kind != "tool" || definition.Transport.Endpoint != "release_publish" || len(action.Credentials) != 1 {
		t.Fatalf("command governance not compiled: %#v", action)
	}
	if definition.Transport.Arguments["command"].SourceArgument != "command" ||
		definition.Transport.Arguments["commandName"].Literal != "publish-release" ||
		definition.Transport.Arguments["skillName"].Literal != "publish-release" {
		t.Fatalf("OpenClaw raw tool envelope not compiled: %#v", definition.Transport.Arguments)
	}
	if definition.Source == nil || definition.Source.Digest == "" || definition.Source.License != "MIT-0" || len(definition.Installers) != 1 {
		t.Fatalf("source/install provenance not preserved: %#v", definition)
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatalf("compiled definition is not canonical: %v", err)
	}
}

func TestCompilePreservesByteExactExportArtifact(t *testing.T) {
	skillMD := []byte("---\nname: portable\ndescription: Portable source.\n---\nUse {baseDir}/references/guide.md.\n")
	resource := []byte("Preserve exact bytes.\n")
	compilation, err := Compile(Bundle{
		SkillMD: skillMD,
		Files:   []File{{Path: "references/guide.md", Content: resource}},
		Source:  Source{Registry: "https://registry.test", Reference: "publisher/portable", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	exported, err := ExportBundle(compilation)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exported.SkillMD, skillMD) || len(exported.Files) != 1 || !bytes.Equal(exported.Files[0].Content, resource) || exported.Source.Reference != "publisher/portable" {
		t.Fatalf("export changed source artifact: %#v", exported)
	}
	exported.SkillMD[0] = 'x'
	exported.Files[0].Content[0] = 'x'
	again, err := ExportBundle(compilation)
	if err != nil || !bytes.Equal(again.SkillMD, skillMD) || !bytes.Equal(again.Files[0].Content, resource) {
		t.Fatalf("export aliases compiler state: %#v, %v", again, err)
	}
}

func TestCompilePromptOnlySkillAndRejectsSemanticLoss(t *testing.T) {
	prompt, err := Compile(Bundle{SkillMD: []byte("---\nname: reviewer\ndescription: Review changes\n---\nReview changes carefully.")})
	if err != nil || prompt.Definition.Prompt == nil || len(prompt.Definition.Actions) != 0 {
		t.Fatalf("prompt compilation = %#v, %v", prompt, err)
	}
	if err := skill.NewCatalog().Register(context.Background(), prompt.Definition); err != nil {
		t.Fatalf("prompt-only definition rejected: %v", err)
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), prompt.Definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "team", ID: "marketing"}
	if err := catalog.Bind(context.Background(), &skill.Binding{ID: "review", Scope: scope, DeploymentID: "team-deployment", SkillID: "reviewer", SkillVersion: prompt.Definition.Version, EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1}); err != nil {
		t.Fatalf("prompt-only definition cannot bind to a first-class owner: %v", err)
	}
	prompts, err := catalog.ListModelPrompts(context.Background(), scope, "team-deployment")
	if err != nil || len(prompts) != 1 || prompts[0].SkillID != "reviewer" {
		t.Fatalf("bound prompt projection = %#v, %v", prompts, err)
	}
	resolved, err := catalog.ResolvePrompt(context.Background(), scope, "team-deployment", "reviewer", prompt.Definition.Version)
	if err != nil || resolved.Instructions == "" {
		t.Fatalf("resolve prompt = %#v, %v", resolved, err)
	}
	_, err = Compile(Bundle{SkillMD: []byte("---\nname: bad\ndescription: Bad dispatch\ncommand-dispatch: magic\n---\nbody")})
	if err == nil {
		t.Fatal("unsupported dispatch should fail explicitly")
	}
	_, err = Compile(Bundle{SkillMD: []byte("---\nname: actual\ndescription: mismatch\n---\nbody"), Source: Source{Reference: "owner/registry-slug", ExpectedName: "other"}})
	if err == nil {
		t.Fatal("explicit source identity mismatch should fail")
	}
	aliased, err := Compile(Bundle{SkillMD: []byte("---\nname: actual\ndescription: registry alias\n---\nbody"), Source: Source{Reference: "owner/globally-unique-registry-slug"}})
	if err != nil || aliased.Definition.Source.Reference != "owner/globally-unique-registry-slug" {
		t.Fatalf("registry reference alias should remain provenance, got %#v, %v", aliased, err)
	}
}

func TestCompileDeclaredExecutableAsGovernedProcessAction(t *testing.T) {
	compilation, err := Compile(Bundle{SkillMD: []byte(`---
name: summarize
description: Summarize a URL with a declared CLI.
metadata:
  openclaw:
    requires:
      bins: [summarize]
      env: [OPENAI_API_KEY]
    install:
      - id: brew
        kind: brew
        formula: owner/tap/summarize
        bins: [summarize]
---
Use the summarize CLI for URL summaries.
`)})
	if err != nil {
		t.Fatal(err)
	}
	definition := compilation.Definition
	if !strings.HasSuffix(definition.Version, "."+processCompilationRevision) || len(definition.Actions) != 1 {
		t.Fatalf("process compilation identity = %#v", definition)
	}
	action := definition.Actions["execute"]
	if action.Transport == nil || action.Transport.Endpoint != "openseal.process.exec" || action.Risk != skill.RiskLevelExternal ||
		action.Transport.Arguments["executable"].Literal != "summarize" || action.Transport.Arguments["arguments"].SourceArgument != "arguments" ||
		len(action.Credentials) != 1 || action.Credentials[0].Name != "OPENAI_API_KEY" {
		t.Fatalf("governed process action = %#v", action)
	}
	encoded, err := json.Marshal(action.Transport.Arguments["installers"].Literal)
	if err != nil || !strings.Contains(string(encoded), "owner/tap/summarize") {
		t.Fatalf("installer provenance = %s, %v", encoded, err)
	}
	if err := skill.NewCatalog().Register(context.Background(), definition); err != nil {
		t.Fatalf("compiled process definition is not canonical: %v", err)
	}

	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "summarize", Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
		EnablePrompt: true, AllowedActions: []string{"execute"}, MaximumRisk: skill.RiskLevelExternal,
		Credentials: map[string]skill.CredentialReference{"OPENAI_API_KEY": {Kind: "environment-secret", ID: "opaque"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	unavailable, err := catalog.Activate(context.Background(), scope, "agent", skill.HostCapabilityState{OperatingSystem: "linux"})
	if err != nil || len(unavailable.Skills) != 0 || !hasAvailabilityReason(unavailable.Unavailable[0].Reasons, "executable_missing") {
		t.Fatalf("missing installer adapter activation = %#v, %v", unavailable, err)
	}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "summarize-unbound", Scope: scope, DeploymentID: "prompt-only", SkillID: definition.ID, SkillVersion: definition.Version,
		EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	promptOnly, err := catalog.Activate(context.Background(), scope, "prompt-only", skill.HostCapabilityState{
		OperatingSystem: "linux", Environment: map[string]bool{"OPENAI_API_KEY": true},
		Adapters: map[string]skill.AdapterCapability{skill.AdapterInstaller: {State: skill.AdapterStateAvailable, Features: []string{"brew"}}},
	})
	if err != nil || len(promptOnly.Unavailable) != 1 || promptOnly.Unavailable[0].Reasons[0].Code != "process_action_unbound" {
		t.Fatalf("prompt-only process binding activation = %#v, %v", promptOnly, err)
	}
	available, err := catalog.Activate(context.Background(), scope, "agent", skill.HostCapabilityState{
		OperatingSystem: "linux", Adapters: map[string]skill.AdapterCapability{
			skill.AdapterInstaller: {State: skill.AdapterStateAvailable, Version: "sandbox-job/v1", Features: []string{"brew"}},
		},
	})
	if err != nil || len(available.Skills) != 1 || len(available.Skills[0].Actions) != 1 || len(available.Unavailable) != 0 {
		t.Fatalf("governed installer activation = %#v, %v", available, err)
	}
}

func TestCompileAnyExecutableProjectsOnlyHostEligibleAction(t *testing.T) {
	compilation, err := Compile(Bundle{SkillMD: []byte(`---
name: search-text
description: Search text with an available CLI.
metadata:
  openclaw:
    requires:
      anyBins: [rg, grep]
---
Use the available search executable.
`)})
	if err != nil {
		t.Fatal(err)
	}
	definition := compilation.Definition
	if len(definition.Actions) != 2 || definition.Actions["execute_1_grep"].Name == "" || definition.Actions["execute_2_rg"].Name == "" {
		t.Fatalf("alternative process actions = %#v", definition.Actions)
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "search", Scope: scope, DeploymentID: "agent", SkillID: definition.ID, SkillVersion: definition.Version,
		EnablePrompt: true, AllowedActions: []string{"execute_1_grep", "execute_2_rg"}, MaximumRisk: skill.RiskLevelExternal, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := catalog.Activate(context.Background(), scope, "agent", skill.HostCapabilityState{
		OperatingSystem: "linux", Executables: map[string]bool{"grep": true},
	})
	if err != nil || len(snapshot.Skills) != 1 || len(snapshot.Skills[0].Actions) != 1 || snapshot.Skills[0].Actions[0].Action != "execute_1_grep" {
		t.Fatalf("host-eligible alternative action = %#v, %v", snapshot, err)
	}
}

func hasAvailabilityReason(reasons []skill.AvailabilityReason, code string) bool {
	for _, reason := range reasons {
		if reason.Code == code {
			return true
		}
	}
	return false
}

func TestCompileCredentialBackedPromptRequiresGovernedActionAdapter(t *testing.T) {
	compilation, err := Compile(Bundle{SkillMD: []byte(`---
name: prompt-publisher
description: Draft authenticated publishing guidance.
metadata:
  openclaw:
    primaryEnv: PUBLISH_TOKEN
---
Prepare publishing guidance using the authenticated account policy.
`)})
	if err != nil {
		t.Fatal(err)
	}
	definition := compilation.Definition
	if definition.Prompt == nil || len(definition.Actions) != 0 || len(definition.Prompt.Credentials) != 1 ||
		definition.Prompt.Credentials[0] != (skill.CredentialRequirement{Name: "PUBLISH_TOKEN", Kind: "environment-secret"}) ||
		len(definition.Requirements.Environment) != 0 {
		t.Fatalf("prompt credential compilation = %#v", definition)
	}
	if !skill.NeedsActionAdapter(definition) || !hasCompilationDiagnostic(compilation.Diagnostics, NeedsActionAdapterDiagnostic) {
		t.Fatalf("credential-backed prompt was not marked as requiring an action adapter: %#v", compilation.Diagnostics)
	}

	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	bind := func(deployment string, credentials map[string]skill.CredentialReference) {
		t.Helper()
		if err := catalog.Bind(context.Background(), &skill.Binding{
			ID: "publisher", Scope: scope, DeploymentID: deployment, SkillID: definition.ID, SkillVersion: definition.Version,
			EnablePrompt: true, MaximumRisk: skill.RiskLevelRead, Credentials: credentials, Revision: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	bind("missing", nil)
	if prompts, err := catalog.ListModelPrompts(context.Background(), scope, "missing"); err != nil || len(prompts) != 0 {
		t.Fatalf("unbound prompt entered model catalog: %#v, %v", prompts, err)
	}
	if _, err := catalog.ResolvePrompt(context.Background(), scope, "missing", definition.ID, definition.Version); err == nil {
		t.Fatal("unbound prompt resolved into model instructions")
	}
	unavailable, err := catalog.Activate(context.Background(), scope, "missing", skill.HostCapabilityState{})
	if err != nil || len(unavailable.Skills) != 0 || len(unavailable.Unavailable) != 1 ||
		len(unavailable.Unavailable[0].Reasons) != 1 || unavailable.Unavailable[0].Reasons[0].Code != NeedsActionAdapterDiagnostic {
		t.Fatalf("missing prompt credential activation = %#v, %v", unavailable, err)
	}

	bind("publisher-a", map[string]skill.CredentialReference{"PUBLISH_TOKEN": {Kind: "environment-secret", ID: "opaque-a"}})
	bind("publisher-b", map[string]skill.CredentialReference{"PUBLISH_TOKEN": {Kind: "environment-secret", ID: "opaque-b"}})
	for _, deployment := range []string{"publisher-a", "publisher-b"} {
		snapshot, err := catalog.Activate(context.Background(), scope, deployment, skill.HostCapabilityState{})
		if err != nil || len(snapshot.Skills) != 0 || len(snapshot.Unavailable) != 1 ||
			!hasAvailabilityReason(snapshot.Unavailable[0].Reasons, NeedsActionAdapterDiagnostic) {
			t.Fatalf("bound prompt activation for %s = %#v, %v", deployment, snapshot, err)
		}
		if prompts, err := catalog.ListModelPrompts(context.Background(), scope, deployment); err != nil || len(prompts) != 0 {
			t.Fatalf("unadapted external prompt entered model catalog for %s: %#v, %v", deployment, prompts, err)
		}
		if _, err := catalog.ResolvePrompt(context.Background(), scope, deployment, definition.ID, definition.Version); err == nil {
			t.Fatalf("unadapted external prompt resolved for %s", deployment)
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"opaque-a", "opaque-b", "PUBLISH_TOKEN"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("activation exposed credential metadata %q: %s", forbidden, encoded)
			}
		}
	}
}

func TestCompileDeclarativeOpenAPIHelperAsTypedGovernedReadAction(t *testing.T) {
	skillMD := []byte(`---
name: Reddit Keyword Search API
description: Search Reddit through a documented API.
metadata:
  openclaw:
    primaryEnv: JUST_ONE_API_TOKEN
    requires:
      bins: [node]
      env: [JUST_ONE_API_TOKEN]
---
Use the source-declared helper:

~~~bash
node {baseDir}/bin/run.mjs --operation "searchRedditV1" --token "$JUST_ONE_API_TOKEN" --params-json '{"keyword":"<keyword>"}'
~~~
`)
	runner := []byte(`const manifest = {
  "baseUrl":"https://api.justoneapi.com",
  "slug":"justoneapi-reddit-search",
  "operations":[{
    "description":"Search public Reddit posts by keyword.",
    "method":"GET",
    "operationId":"searchRedditV1",
    "path":"/api/reddit/search/v1",
    "parameters":[
      {"name":"token","location":"query","required":true,"schemaType":"string"},
      {"name":"keyword","location":"query","required":true,"schemaType":"string","description":"Search keywords."},
      {"name":"after","location":"query","required":false,"schemaType":"string","defaultValue":""}
    ]
  }]
};
// The retained source helper is not executed by the compiled HTTP adapter.
`)
	compilation, err := Compile(Bundle{
		SkillMD: skillMD, Files: []File{{Path: "bin/run.mjs", Content: runner}},
		Source: Source{Reference: "@justoneapi/justoneapi-reddit-search", Version: "1.0.0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := compilation.Definition
	action, ok := definition.Actions["searchRedditV1"]
	if !ok || !strings.HasSuffix(definition.Version, "."+httpCompilationRevision) || action.Transport == nil ||
		action.Transport.Endpoint != httpaction.TransportName || action.Risk != skill.RiskLevelRead || action.SideEffect != skill.SideEffectRead {
		t.Fatalf("compiled HTTP action = %#v", definition)
	}
	if len(definition.Requirements.Executables) != 0 || len(definition.Requirements.Environment) != 0 || len(definition.Installers) != 0 {
		t.Fatalf("source helper runtime was not optimized away: %#v", definition.Requirements)
	}
	if strings.Contains(definition.Prompt.Instructions, "{baseDir}") || strings.Contains(definition.Prompt.Instructions, "--token") ||
		!strings.Contains(definition.Prompt.Instructions, "governed OpenSeal action") {
		t.Fatalf("compiled prompt still instructs direct helper execution: %q", definition.Prompt.Instructions)
	}
	properties := action.InputSchema["properties"].(map[string]interface{})["parameters"].(map[string]interface{})["properties"].(map[string]interface{})
	if _, leaked := properties["token"]; leaked || properties["keyword"].(map[string]interface{})["type"] != "string" || len(action.Credentials) != 1 || action.Credentials[0].Name != "JUST_ONE_API_TOKEN" {
		t.Fatalf("typed input or credential boundary = %#v %#v", properties, action.Credentials)
	}
	bound := &skill.BoundAction{Definition: definition, Action: action, Binding: &skill.Binding{}}
	envelope, err := skill.MaterializeTransportArguments(bound, map[string]interface{}{"parameters": map[string]interface{}{"keyword": "OpenClaw", "after": "cursor"}})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := httpaction.DecodeInvocation(envelope)
	if err != nil || invocation.Parameters["keyword"] != "OpenClaw" || invocation.CredentialName != "JUST_ONE_API_TOKEN" || invocation.CredentialParameter != "token" {
		t.Fatalf("materialized HTTP invocation = %#v, %v", invocation, err)
	}
	encoded, _ := json.Marshal(envelope)
	if strings.Contains(string(encoded), "JUST_ONE_API_TOKEN\":\"") || strings.Contains(string(encoded), "resolved-secret") {
		t.Fatalf("transport envelope contains a credential value: %s", encoded)
	}
	if skill.NeedsActionAdapter(definition) || !hasCompilationDiagnostic(compilation.Diagnostics, "openapi_http.compiled") || hasCompilationDiagnostic(compilation.Diagnostics, NeedsActionAdapterDiagnostic) {
		t.Fatalf("adapter readiness diagnostics = %#v", compilation.Diagnostics)
	}
	if err := skill.NewCatalog().Register(context.Background(), definition); err != nil {
		t.Fatalf("compiled definition is not canonical: %v", err)
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatal(err)
	}
	scope := skill.ScopeReference{Kind: "tenant", ID: "one"}
	if err := catalog.Bind(context.Background(), &skill.Binding{
		ID: "reddit", Scope: scope, DeploymentID: "researcher", SkillID: definition.ID, SkillVersion: definition.Version,
		EnablePrompt: true, AllowedActions: []string{"searchRedditV1"}, MaximumRisk: skill.RiskLevelRead,
		Credentials: map[string]skill.CredentialReference{"JUST_ONE_API_TOKEN": {Kind: "environment-secret", ID: "opaque-credential-reference"}}, Revision: 1,
	}); err != nil {
		t.Fatal(err)
	}
	withoutAdapter, err := catalog.Activate(context.Background(), scope, "researcher", skill.HostCapabilityState{})
	if err != nil || len(withoutAdapter.Skills) != 0 || len(withoutAdapter.Unavailable) != 1 ||
		!hasAvailabilityReason(withoutAdapter.Unavailable[0].Reasons, "action_adapter_unavailable") {
		t.Fatalf("missing HTTP adapter activation = %#v, %v", withoutAdapter, err)
	}
	available, err := catalog.Activate(context.Background(), scope, "researcher", skill.HostCapabilityState{Adapters: map[string]skill.AdapterCapability{
		skill.AdapterHTTPAction: {State: skill.AdapterStateAvailable, Version: "egress-policy/v1"},
	}})
	if err != nil || len(available.Skills) != 1 || len(available.Skills[0].Actions) != 1 || len(available.Unavailable) != 0 {
		t.Fatalf("governed HTTP adapter activation = %#v, %v", available, err)
	}
}

func TestCompileOpenAPIHelperFailsClosedWhenSemanticsCannotBePreserved(t *testing.T) {
	base := Bundle{
		SkillMD: []byte(`---
name: unsafe-helper
description: Write through an imported helper.
metadata:
  openclaw:
    primaryEnv: API_TOKEN
    requires:
      bins: [node]
---
node {baseDir}/bin/run.mjs --operation "publish" --token "$API_TOKEN" --params-json '{}'
`),
		Files: []File{{Path: "bin/run.mjs", Content: []byte(`const manifest = {
  "baseUrl":"https://api.example.test",
  "slug":"unsafe-helper",
  "operations":[{"method":"POST","operationId":"publish","path":"/v1/posts","parameters":[{"name":"token","location":"query","required":true,"schemaType":"string"}]}]
};`)}},
	}
	compilation, err := Compile(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(compilation.Definition.Actions) != 0 || !skill.NeedsActionAdapter(compilation.Definition) || !hasCompilationDiagnostic(compilation.Diagnostics, NeedsActionAdapterDiagnostic) {
		t.Fatalf("unsupported write helper did not fail closed: %#v", compilation)
	}
	for _, diagnostic := range compilation.Diagnostics {
		if diagnostic.Code == NeedsActionAdapterDiagnostic && !strings.Contains(diagnostic.Message, "GET and HEAD") {
			t.Fatalf("diagnostic is not actionable: %#v", diagnostic)
		}
	}
}

func TestCompileRegistryDisplayNameWithCanonicalSourceIdentity(t *testing.T) {
	source := []byte(`---
name: Reddit Keyword Search API
description: Search Reddit with an authenticated registry skill.
metadata:
  openclaw:
    primaryEnv: REDDIT_TOKEN
---
Search Reddit for the requested topic.
`)
	compilation, err := Compile(Bundle{SkillMD: source, Source: Source{Reference: "@justoneapi/justoneapi-reddit-search", Version: "1.0.0"}})
	if err != nil {
		t.Fatal(err)
	}
	if compilation.Definition.ID != "justoneapi-reddit-search" || compilation.Definition.Name != "Reddit Keyword Search API" || compilation.Parsed.Name != "Reddit Keyword Search API" || compilation.Parsed.CanonicalName != "justoneapi-reddit-search" {
		t.Fatalf("normalized identity = %#v", compilation.Definition)
	}
	if len(compilation.Diagnostics) == 0 || compilation.Diagnostics[0].Code != "identity.normalized" {
		t.Fatalf("normalization diagnostics = %#v", compilation.Diagnostics)
	}
	if !hasCompilationDiagnostic(compilation.Diagnostics, NeedsActionAdapterDiagnostic) || !skill.NeedsActionAdapter(compilation.Definition) {
		t.Fatalf("Reddit API Skill was advertised without a governed action: %#v", compilation.Diagnostics)
	}
	exported, err := ExportBundle(compilation)
	if err != nil || string(exported.SkillMD) != string(source) {
		t.Fatalf("source artifact was not preserved: %v", err)
	}
	if _, err := Compile(Bundle{SkillMD: source}); err == nil {
		t.Fatal("standalone invalid identity should remain rejected")
	}
	if _, err := Compile(Bundle{SkillMD: source, Source: Source{Reference: "@owner/Invalid Slug"}}); err == nil {
		t.Fatal("invalid canonical registry identity should fail closed")
	}
}

func hasCompilationDiagnostic(diagnostics []Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func TestCompileCanonicalizesVolatileTrustAndVersionsMeaningfulEvidence(t *testing.T) {
	skillMD := []byte("---\nname: summarize\ndescription: Summarize evidence.\n---\nSummarize carefully.\n")
	base := Bundle{SkillMD: skillMD, Source: Source{
		Registry: "https://clawhub.ai", Publisher: "seanford", Reference: "@seanford/summarize", Version: "1.0.0",
		Trust: map[string]interface{}{
			"schema": "clawhub.skill.verify.v1", "decision": "pass", "resolvedFrom": "latest", "createdAt": float64(10),
			"security": map[string]interface{}{"status": "clean", "checkedAt": float64(20)},
		},
	}}
	first, err := Compile(base)
	if err != nil {
		t.Fatal(err)
	}
	replayed := base
	replayed.Source.Trust = map[string]interface{}{
		"schema": "clawhub.skill.verify.v1", "decision": "pass", "resolvedFrom": "version", "createdAt": float64(999),
		"security": map[string]interface{}{"status": "clean", "checkedAt": float64(1000)},
	}
	second, err := Compile(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if first.Definition.Version != second.Definition.Version || !reflect.DeepEqual(first.Definition, second.Definition) {
		t.Fatalf("volatile verification changed immutable definition:\nfirst=%#v\nsecond=%#v", first.Definition, second.Definition)
	}
	if !strings.Contains(first.Definition.Version, ".trust.") || first.Definition.Source.Trust["resolvedFrom"] != nil || first.Artifact.Source.Trust["resolvedFrom"] != "latest" {
		t.Fatalf("canonical definition or retained artifact trust = definition=%#v artifact=%#v", first.Definition.Source.Trust, first.Artifact.Source.Trust)
	}
	changed := base
	changed.Source.Trust = map[string]interface{}{"schema": "clawhub.skill.verify.v1", "decision": "pass", "security": map[string]interface{}{"status": "review"}}
	third, err := Compile(changed)
	if err != nil {
		t.Fatal(err)
	}
	if third.Definition.Version == first.Definition.Version {
		t.Fatal("meaningful trust evidence change reused an immutable definition version")
	}
}
