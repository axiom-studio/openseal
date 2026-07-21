package resourcefs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	kernelskill "github.com/axiom-studio/openseal/pkg/skill"
)

type contentProvider map[string][]byte

func (p contentProvider) ReadResource(_ context.Context, _ kernelskill.ScopeReference, digest, path string) ([]byte, error) {
	return append([]byte(nil), p[digest+":"+path]...), nil
}

func declaredResource(path, kind string, content []byte) capability.Resource {
	digest := sha256.Sum256(content)
	return capability.Resource{Path: path, Kind: kind, Digest: hex.EncodeToString(digest[:]), Size: int64(len(content))}
}

func stageRequest(source string, resources []capability.Resource) kernelskill.ResourceStageRequest {
	return kernelskill.ResourceStageRequest{
		Scope: kernelskill.ScopeReference{Kind: "tenant", ID: "one"}, DeploymentID: "agent", BindingID: "binding",
		SkillID: "research", SkillVersion: "1.0.0", SourceDigest: source, Resources: resources,
	}
}

func TestFilesystemStagerIsContainedAtomicAndRestartStable(t *testing.T) {
	source := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	guide := []byte("preserve provenance")
	script := []byte("#!/bin/sh\nexit 0\n")
	resources := []capability.Resource{
		declaredResource("references/guide.md", "reference", guide),
		declaredResource("scripts/run.sh", "script", script),
	}
	provider := contentProvider{
		source + ":references/guide.md": guide,
		source + ":scripts/run.sh":      script,
	}
	root := filepath.Join(t.TempDir(), "sandbox")
	stager, err := New(root, provider)
	if err != nil {
		t.Fatal(err)
	}
	request := stageRequest(source, resources)
	first, err := stager.StageResources(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Adapter != AdapterID || first.Revision == "" || first.SourceDigest != source || first.ResourceCount != len(resources) || !withinRoot(root, first.Root) {
		t.Fatalf("invalid stage result: %#v", first)
	}
	if got, err := os.ReadFile(filepath.Join(first.Root, "references", "guide.md")); err != nil || string(got) != string(guide) {
		t.Fatalf("staged reference = %q, %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(first.Root, "scripts", "run.sh")); err != nil || info.Mode().Perm() != 0o500 {
		t.Fatalf("script permissions = %#v, %v", info, err)
	}

	restarted, err := New(root, provider)
	if err != nil {
		t.Fatal(err)
	}
	second, err := restarted.StageResources(context.Background(), request)
	if err != nil || second.Root != first.Root || second.Revision != first.Revision {
		t.Fatalf("restart changed staged identity: first=%#v second=%#v err=%v", first, second, err)
	}

	const workers = 8
	var wait sync.WaitGroup
	errors := make(chan error, workers)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			stage, stageErr := restarted.StageResources(context.Background(), request)
			if stageErr == nil && (stage.Root != first.Root || stage.Revision != first.Revision) {
				stageErr = os.ErrInvalid
			}
			errors <- stageErr
		}()
	}
	wait.Wait()
	close(errors)
	for stageErr := range errors {
		if stageErr != nil {
			t.Fatalf("concurrent stage failed: %v", stageErr)
		}
	}
}

func TestFilesystemStagerFailsClosedOnUnsafeOrChangedContent(t *testing.T) {
	source := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	content := []byte("trusted")
	provider := contentProvider{source + ":safe.txt": content, source + ":../escape": content}
	stager, err := New(filepath.Join(t.TempDir(), "sandbox"), provider)
	if err != nil {
		t.Fatal(err)
	}
	unsafe := stageRequest(source, []capability.Resource{declaredResource("../escape", "resource", content)})
	if _, err := stager.StageResources(context.Background(), unsafe); err == nil {
		t.Fatal("unsafe resource path was accepted")
	}

	request := stageRequest(source, []capability.Resource{declaredResource("safe.txt", "resource", content)})
	stage, err := stager.StageResources(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(stage.Root, "safe.txt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage.Root, "safe.txt"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := stager.StageResources(context.Background(), request); err == nil {
		t.Fatal("tampered resource stage was silently reused")
	}
}

func TestFilesystemStagerRejectsUnsupportedKindsAndUnexpectedEntries(t *testing.T) {
	source := "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	content := []byte("trusted")
	provider := contentProvider{source + ":safe.txt": content}
	root := filepath.Join(t.TempDir(), "sandbox")
	stager, err := New(root, provider)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := stageRequest(source, []capability.Resource{declaredResource("safe.txt", "executable-ish", content)})
	if _, err := stager.StageResources(context.Background(), unsupported); err == nil {
		t.Fatal("unsupported resource kind was accepted")
	}

	request := stageRequest(source, []capability.Resource{declaredResource("safe.txt", capability.ResourceKindFile, content)})
	stage, err := stager.StageResources(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage.Root, "injected.txt"), []byte("unexpected"), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := stager.StageResources(context.Background(), request); err == nil {
		t.Fatal("stage containing an undeclared entry was silently reused")
	}
}

func TestFilesystemStagerNormalizesSourceDigestIdentity(t *testing.T) {
	source := "DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
	content := []byte("trusted")
	provider := contentProvider{strings.ToLower(source) + ":safe.txt": content}
	stager, err := New(filepath.Join(t.TempDir(), "sandbox"), provider)
	if err != nil {
		t.Fatal(err)
	}
	stage, err := stager.StageResources(context.Background(), stageRequest(source, []capability.Resource{
		declaredResource("safe.txt", capability.ResourceKindFile, content),
	}))
	if err != nil || stage.SourceDigest != strings.ToLower(source) {
		t.Fatalf("normalized stage=%#v err=%v", stage, err)
	}
}
