package openseal

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	skillopenclaw "github.com/axiom-studio/openseal/pkg/skill/openclaw"
)

func TestClawHubHostAdmissionRejectsInstallBeforePublishingFiles(t *testing.T) {
	workspace := t.TempDir()
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t)}
	denied := errors.New("serving reader cannot decode candidate")
	calls := 0
	engine, err := New(WithClawHubRegistry("https://registry.test", registry, workspace),
		WithClawHubCompilationValidator(func(compilation *skillopenclaw.Compilation) error {
			calls++
			if compilation == nil || compilation.Definition == nil {
				t.Fatal("host admission received no compiled definition")
			}
			return denied
		}))
	if err != nil {
		t.Fatal(err)
	}
	before := clawHubAdmissionFiles(t, workspace)
	_, err = engine.InstallClawHubSkill(context.Background(), ClawHubInstallRequest{
		Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"},
	})
	if !errors.Is(err, denied) || calls != 1 {
		t.Fatalf("install error=%v, admission calls=%d", err, calls)
	}
	if after := clawHubAdmissionFiles(t, workspace); !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected install changed workspace files: before=%v, after=%v", before, after)
	}
	states, err := engine.ListInstalledClawHubSkillStates()
	if err != nil || len(states) != 0 {
		t.Fatalf("rejected install became installed: states=%#v, error=%v", states, err)
	}
}

func TestClawHubHostAdmissionPreservesWorkingUpdateAndAllowsRestore(t *testing.T) {
	workspace := t.TempDir()
	registry := &facadeClawHubRegistry{archive: facadeSkillZip(t), version: "1.0.0"}
	denied := errors.New("reader disappeared before update")
	reject := false
	validator := func(*skillopenclaw.Compilation) error {
		if reject {
			return denied
		}
		return nil
	}
	engine, err := New(WithClawHubRegistry("https://registry.test", registry, workspace), WithClawHubCompilationValidator(validator))
	if err != nil {
		t.Fatal(err)
	}
	installed, err := engine.InstallClawHubSkill(context.Background(), ClawHubInstallRequest{
		Reference: ClawHubSkillReference{Owner: "acme", Slug: "research"},
	})
	if err != nil {
		t.Fatal(err)
	}
	before := clawHubAdmissionFiles(t, workspace)
	reject = true
	registry.version = "2.0.0"
	_, err = engine.UpdateClawHubSkill(context.Background(), installed.Reference.String())
	if !errors.Is(err, denied) {
		t.Fatalf("update error=%v, want admission rejection", err)
	}
	if after := clawHubAdmissionFiles(t, workspace); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected update changed the installed skill or lockfile")
	}
	restored, err := New(WithClawHubRegistry("https://registry.test", registry, workspace),
		WithClawHubCompilationValidator(func(*skillopenclaw.Compilation) error {
			t.Fatal("restoring the published skill must not invoke candidate admission")
			return denied
		}))
	if err != nil {
		t.Fatalf("restore published skill: %v", err)
	}
	states, err := restored.ListInstalledClawHubSkillStates()
	if err != nil || len(states) != 1 || states[0].Version != "1.0.0" || !states[0].Verified {
		t.Fatalf("working installed state=%#v, error=%v", states, err)
	}
	definition, err := restored.GetSkillDefinition(context.Background(), installed.Compilation.Definition.ID, installed.Compilation.Definition.Version)
	if err != nil || definition == nil {
		t.Fatalf("published definition was not restored: %#v, %v", definition, err)
	}
}

func TestClawHubHostAdmissionCannotBypassCanonicalValidation(t *testing.T) {
	engine, err := New(WithClawHubCompilationValidator(func(*skillopenclaw.Compilation) error {
		t.Fatal("invalid canonical compilation reached host admission")
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.validateClawHubCandidateCompilation(&skillopenclaw.Compilation{}); err == nil {
		t.Fatal("missing definition was accepted")
	}
}

func clawHubAdmissionFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[relative] = string(data)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return files
}
