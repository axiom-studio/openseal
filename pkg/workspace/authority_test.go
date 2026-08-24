package workspace

import "testing"

func TestSelectDefaultWorkspaceAuthority(t *testing.T) {
	spec := DefaultSpec()
	selected, err := Select(DefaultID, []Spec{spec})
	if err != nil || selected == nil || selected.ID != DefaultID {
		t.Fatalf("selected=%#v error=%v", selected, err)
	}
	authority := Authority{Workspace: *selected}
	if err := authority.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSelectWorkspaceRejectsMissingDefault(t *testing.T) {
	if _, err := Select("missing", []Spec{DefaultSpec()}); err == nil {
		t.Fatal("expected missing default Workspace to fail")
	}
}

func TestWorkspacePolicyRejectsImplicitCommandAuthority(t *testing.T) {
	spec := DefaultSpec()
	spec.Policy.Commands.AllowedExecutables = []string{"git"}
	if err := spec.Validate(); err == nil {
		t.Fatal("disabled commands accepted executable authority")
	}
	spec.Policy.Commands = CommandPolicy{Enabled: true, Network: NetworkEgress, MaxDurationSeconds: 300, AllowedExecutables: []string{"git", "go"}}
	if err := spec.Validate(); err != nil {
		t.Fatal(err)
	}
}
