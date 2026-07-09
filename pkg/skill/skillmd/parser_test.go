package skillmd

import (
	"strings"
	"testing"
)

func TestParseCurrentAgentSkillsAndOpenClawFrontmatter(t *testing.T) {
	content := []byte(`---
name: release-notes
description: Draft release notes and dispatch publishing when requested.
license: Apache-2.0
compatibility: Requires git and network access
allowed-tools: Bash(git:*) Read
homepage: https://example.test/release-notes
user-invocable: true
disable-model-invocation: true
command-dispatch: tool
command-tool: publish_release
metadata:
  author: example
  version: "2.3.0"
  openclaw:
    skillKey: releases
    primaryEnv: PUBLISH_TOKEN
    emoji: "🚀"
    os: [linux, darwin]
    requires:
      bins: [git]
      anyBins: [gh, glab]
      env: [PUBLISH_TOKEN]
      config: [publishing.enabled]
    install:
      - id: release-cli
        kind: download
        url: https://example.test/release.zip
        archive: zip
        extract: true
        stripComponents: 1
        targetDir: ~/.local/release
        bins: [release]
---
Use the release template in references/template.md.
`)
	parsed, err := ParseSkillMD(content)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Version != "2.3.0" || parsed.License != "Apache-2.0" || len(parsed.AllowedTools) != 2 {
		t.Fatalf("standard fields not preserved: %#v", parsed)
	}
	if parsed.Invocation.UserInvocable != true || !parsed.Invocation.DisableModelInvocation || parsed.CommandDispatch == nil || parsed.CommandDispatch.ToolName != "publish_release" {
		t.Fatalf("invocation fields not preserved: %#v", parsed)
	}
	meta := parsed.Metadata
	if meta.SkillKey != "releases" || meta.PrimaryEnv != "PUBLISH_TOKEN" || len(meta.RequiresAnyBin) != 2 || len(meta.Install) != 1 {
		t.Fatalf("OpenClaw metadata not preserved: %#v", meta)
	}
	installer := meta.Install[0]
	if installer.URL == "" || installer.Extract == nil || !*installer.Extract || installer.StripComponents == nil || *installer.StripComponents != 1 {
		t.Fatalf("installer not preserved: %#v", installer)
	}
}

func TestParseLegacyMetadataWithoutTruncatingSource(t *testing.T) {
	body := strings.Repeat("x", warningSkillSize+100)
	content := []byte("---\nname: legacy-skill\ndescription: Legacy source\nmetadata: '{openclaw: {requires_env: [TOKEN], requires_bins: [jq], primary_env: TOKEN}}'\n---\n" + body)
	result, err := ParseSkillMDWithWarnings(content)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skill.RawContent) != len(content) || len(result.Skill.Body) != len(body) {
		t.Fatal("parser truncated source content")
	}
	if len(result.Skill.Metadata.RequiresEnv) != 1 || result.Skill.Metadata.PrimaryEnv != "TOKEN" || len(result.Warnings) == 0 {
		t.Fatalf("legacy metadata/warning mismatch: %#v", result)
	}
}

func TestAgentSkillsNameConstraints(t *testing.T) {
	for _, name := range []string{"-bad", "bad-", "bad--name", strings.Repeat("a", 65)} {
		_, err := ParseSkillMD([]byte("---\nname: " + name + "\ndescription: test\n---\nbody"))
		if err == nil {
			t.Fatalf("name %q should be rejected", name)
		}
	}
}
