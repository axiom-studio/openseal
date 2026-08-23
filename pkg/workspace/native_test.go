package workspace

import "testing"

func TestOperationsFollowNativeWorkspacePolicy(t *testing.T) {
	authority := &Authority{Workspace: DefaultSpec()}
	operations := Operations(authority)
	if len(operations) != 5 || operations[len(operations)-1].Name != OperationApplyPatch {
		t.Fatalf("default operations = %#v", operations)
	}
	authority.Workspace.Policy.Filesystem = AccessReadOnly
	if got := Operations(authority); len(got) != 3 {
		t.Fatalf("read-only operations = %#v", got)
	}
	authority.Workspace.Policy.Commands = CommandPolicy{Enabled: true, Network: NetworkDenied, MaxDurationSeconds: 60, AllowedExecutables: []string{"git"}}
	if got := Operations(authority); len(got) != 4 || got[3].Name != OperationRunCommand {
		t.Fatalf("command operations = %#v", got)
	}
}
