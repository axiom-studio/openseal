package runtime

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/computer"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// A machine operation repeats on a person's computer as it does on a chat's
// secure VM: the same delete, write or command again is never suppressed as
// a duplicate.
func TestMachineOperationsRepeatAlikeOnTheSecureVMAndAComputer(t *testing.T) {
	definition := computer.SkillDefinition()
	for _, op := range workspace.MachineOps {
		action := definition.Actions[op]
		vm := repeatEffectOf(LoopToolHost, action.SideEffect, false, workspace.OperationPrefix+op)
		onComputer := repeatEffectOf(LoopToolGoverned, action.SideEffect, action.Repeatable, repeatGovernedOperation(computer.SkillID, op))
		if vm != onComputer {
			t.Errorf("%s repeats as %s on the secure VM but %s on a computer", op, vm, onComputer)
		}
	}
}
