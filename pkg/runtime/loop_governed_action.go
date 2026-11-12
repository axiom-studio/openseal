package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// governedToolPrefix starts every governed tool name, so actions never collide
// with kernel or host tool names.
const governedToolPrefix = "action_"

// LoopGovernedToolNames maps each action name to its provider tool name:
// "action_" plus the name with "." as "_". A name that does not fit the
// provider pattern, or that would repeat an earlier action's tool name, gets
// a bounded form with a hash of the action name. It depends only on the
// actions and their order, so the tool executor resolves the same names.
func LoopGovernedToolNames(actions []capability.ModelAction) map[string]string {
	names := make(map[string]string, len(actions))
	taken := map[string]bool{}
	for _, action := range actions {
		if _, done := names[action.Name]; done || action.Name == "" {
			continue
		}
		name := governedToolPrefix + strings.ReplaceAll(action.Name, ".", "_")
		if !models.ValidModelStepToolName(name) || taken[name] {
			name = hashedGovernedToolName(action.Name)
		}
		names[action.Name] = name
		taken[name] = true
	}
	return names
}

func hashedGovernedToolName(actionName string) string {
	sum := sha256.Sum256([]byte(actionName))
	var b strings.Builder
	for _, r := range actionName {
		if b.Len() >= 48 {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return governedToolPrefix + b.String() + "_" + hex.EncodeToString(sum[:4])
}

// LoopGovernedAction resolves a provider tool name back to its action.
func LoopGovernedAction(actions []capability.ModelAction, toolName string) (capability.ModelAction, bool) {
	names := LoopGovernedToolNames(actions)
	for _, action := range actions {
		if name, ok := names[action.Name]; ok && name == toolName {
			return action, true
		}
	}
	return capability.ModelAction{}, false
}

// selectGovernedAction resolves a governed tool call to the binding's model
// action with the exact-name selector (section 03 §3.1). nil means the
// binding offers no such action. A name exposed twice and a directive whose
// pinned Skill version the binding no longer authorizes
// (ErrRunSkillDependencyUnavailable) are Go errors, as on the selector.
func selectGovernedAction(binding *agentrun.TurnRunnerBinding, name string, directive *agentrun.ToolCallDirective) (*capability.ModelAction, error) {
	if binding == nil {
		return nil, nil
	}
	// The model calls the provider tool name the preparer offered
	// (LoopGovernedToolNames); the selector matches action names.
	if action, ok := LoopGovernedAction(binding.ModelActions, name); ok {
		name = action.Name
	}
	request := activity.TurnAction{Capability: name}
	if directive != nil {
		request.BindingID, request.BindingRevision = directive.BindingID, directive.BindingRevision
		request.ExpectedSkillVersion = directive.ExpectedSkillVersion
	}
	return action.SelectTurnModelAction(binding.ModelActions, request)
}
