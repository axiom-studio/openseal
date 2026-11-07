package runtime

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

// A model that grinds one tool call at a time, such as reading a repository
// file by file, is nudged towards the chat's secure VM. The notice is derived
// only from the kernel-owned action history the turn already carries, so it
// is deterministic and bounded. It never blocks a call.
const (
	// repeatedActionThreshold is how often one tool must appear in the
	// action history before the turn is nudged.
	repeatedActionThreshold = 6
	// maximumRepeatedActionNotices bounds the tools named in one turn.
	maximumRepeatedActionNotices = 2
)

// repeatedIdenticalActionNotice names the latest call when its exact
// capability and arguments had already succeeded earlier in the history.
func repeatedIdenticalActionNotice(entries []map[string]interface{}, names []string) string {
	latest := len(entries) - 1
	digest, _ := entries[latest]["semanticDigest"].(string)
	if digest == "" {
		return ""
	}
	for index := 0; index < latest; index++ {
		if entries[index]["semanticDigest"] == digest && fmt.Sprint(entries[index]["status"]) == string(ActionCallStatusSucceeded) {
			return fmt.Sprintf(
				"You called %s again with the same arguments after it had already succeeded. You already have that result: it is in continuationCheckpoint._opensealActionHistory (actionCallId %v). Use it instead of calling again.",
				names[latest], entries[index]["actionCallId"])
		}
	}
	return ""
}

type actionUse struct {
	name  string
	count int
}

func frequentActions(names []string) []actionUse {
	counts := map[string]int{}
	for _, name := range names {
		counts[name]++
	}
	uses := make([]actionUse, 0, len(counts))
	for name, count := range counts {
		if count >= repeatedActionThreshold {
			uses = append(uses, actionUse{name: name, count: count})
		}
	}
	sort.Slice(uses, func(i, j int) bool {
		if uses[i].count != uses[j].count {
			return uses[i].count > uses[j].count
		}
		return uses[i].name < uses[j].name
	})
	if len(uses) > maximumRepeatedActionNotices {
		uses = uses[:maximumRepeatedActionNotices]
	}
	return uses
}

// historyEntryActionName is the tool name the model sees for a recorded
// call: a binding tool is matched by its fixed arguments, a definition action
// by its skill and action.
func historyEntryActionName(actions []capability.ModelAction, entry map[string]interface{}) string {
	skillID, _ := entry["skillId"].(string)
	action, _ := entry["action"].(string)
	bindingID, _ := entry["bindingId"].(string)
	arguments, _ := entry["arguments"].(map[string]interface{})
	plain := ""
	for _, candidate := range actions {
		if candidate.SkillID != skillID || candidate.Action != action || (candidate.BindingID != "" && bindingID != "" && candidate.BindingID != bindingID) {
			continue
		}
		if candidate.BindingTool == nil {
			if plain == "" {
				plain = candidate.Name
			}
			continue
		}
		if fixedArgumentsMatch(candidate.BindingTool.Arguments, arguments) {
			return candidate.Name
		}
	}
	if plain != "" {
		return plain
	}
	return strings.Trim(skillID+"."+action, ".")
}

func fixedArgumentsMatch(fixed, arguments map[string]interface{}) bool {
	for name, value := range fixed {
		actual, ok := arguments[name]
		if !ok {
			return false
		}
		want, wantErr := json.Marshal(value)
		got, gotErr := json.Marshal(actual)
		if wantErr != nil || gotErr != nil || string(want) != string(got) {
			return false
		}
	}
	return true
}
