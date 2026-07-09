package openclaw

import (
	"context"
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
	if definition.Version != "1.4.0" || definition.Prompt == nil || len(definition.Actions) != 1 || len(definition.Resources) != 2 {
		t.Fatalf("incomplete compilation: %#v", definition)
	}
	action := definition.Actions["invoke"]
	if action.Transport != nil || definition.Transport.Kind != "tool" || definition.Transport.Endpoint != "release_publish" || len(action.Credentials) != 1 {
		t.Fatalf("command governance not compiled: %#v", action)
	}
	if definition.Source == nil || definition.Source.Digest == "" || definition.Source.License != "MIT-0" || len(definition.Installers) != 1 {
		t.Fatalf("source/install provenance not preserved: %#v", definition)
	}
	catalog := skill.NewCatalog()
	if err := catalog.Register(context.Background(), definition); err != nil {
		t.Fatalf("compiled definition is not canonical: %v", err)
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
	_, err = Compile(Bundle{SkillMD: []byte("---\nname: actual\ndescription: mismatch\n---\nbody"), Source: Source{Reference: "owner/other"}})
	if err == nil {
		t.Fatal("source identity mismatch should fail")
	}
}
