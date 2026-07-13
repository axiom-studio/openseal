package openclaw

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

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

func TestCompilePromptOnlyPrimaryEnvAsOpaqueCredential(t *testing.T) {
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
		len(unavailable.Unavailable[0].Reasons) != 1 || unavailable.Unavailable[0].Reasons[0].Code != "credential_missing" {
		t.Fatalf("missing prompt credential activation = %#v, %v", unavailable, err)
	}

	bind("publisher-a", map[string]skill.CredentialReference{"PUBLISH_TOKEN": {Kind: "environment-secret", ID: "opaque-a"}})
	bind("publisher-b", map[string]skill.CredentialReference{"PUBLISH_TOKEN": {Kind: "environment-secret", ID: "opaque-b"}})
	for _, deployment := range []string{"publisher-a", "publisher-b"} {
		snapshot, err := catalog.Activate(context.Background(), scope, deployment, skill.HostCapabilityState{})
		if err != nil || len(snapshot.Skills) != 1 || len(snapshot.Unavailable) != 0 || snapshot.Skills[0].Prompt == nil {
			t.Fatalf("bound prompt activation for %s = %#v, %v", deployment, snapshot, err)
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
