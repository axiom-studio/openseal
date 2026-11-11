package runtime

import (
	"encoding/json"
	"strings"
)

func portfolioKey(scope Scope, id string) string { return scope.key() + ":" + id }

func matchesObjectiveFilter(objective *Objective, filter ObjectiveFilter) bool {
	if filter.Owner != nil && objective.Owner != *filter.Owner {
		return false
	}
	if !filter.IncludeRetired && objective.Status == ObjectiveStatusRetired {
		return false
	}
	if len(filter.Statuses) > 0 && !containsObjectiveStatus(filter.Statuses, objective.Status) {
		return false
	}
	return true
}

func matchesRunFilter(run *AgentRun, filter AgentRunFilter) bool {
	if filter.ConcurrencyKey != "" && run.ConcurrencyKey != filter.ConcurrencyKey {
		return false
	}
	if filter.ConversationID != "" && run.Context[conversationRunContextConversationID] != filter.ConversationID {
		return false
	}
	if query := strings.ToLower(strings.TrimSpace(filter.Query)); query != "" && !strings.Contains(strings.ToLower(run.Goal+" "+string(run.Status)), query) {
		return false
	}
	if filter.Kind != "" && normalizeRunKind(run.Kind) != filter.Kind {
		return false
	}
	if filter.Owner != nil && run.Owner != *filter.Owner {
		return false
	}
	if filter.ObjectiveID != "" && run.ObjectiveID != filter.ObjectiveID {
		return false
	}
	if filter.ParentRunID != "" && run.ParentRunID != filter.ParentRunID {
		return false
	}
	if filter.RootRunID != "" && run.RootRunID != filter.RootRunID {
		return false
	}
	if len(filter.RootRunIDs) > 0 {
		found := false
		for _, rootID := range filter.RootRunIDs {
			if run.RootRunID == rootID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if filter.AssignedAgentID != "" && run.AssignedAgentID != filter.AssignedAgentID {
		return false
	}
	if len(filter.Statuses) > 0 && !containsRunStatus(filter.Statuses, run.Status) {
		return false
	}
	return true
}

func containsObjectiveStatus(statuses []ObjectiveStatus, status ObjectiveStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func containsRunStatus(statuses []AgentRunStatus, status AgentRunStatus) bool {
	for _, candidate := range statuses {
		if candidate == status {
			return true
		}
	}
	return false
}

func pageObjectives(values []*Objective, offset, limit int) []*Objective {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*Objective{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}

func pageAgentRuns(values []*AgentRun, offset, limit int) []*AgentRun {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(values) {
		return []*AgentRun{}
	}
	values = values[offset:]
	if limit > 0 && limit < len(values) {
		values = values[:limit]
	}
	return values
}

func cloneObjective(in *Objective) *Objective {
	if in == nil {
		return nil
	}
	var out Objective
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	return &out
}

func cloneObjectiveExecutionPolicy(in *ObjectiveExecutionPolicy) *ObjectiveExecutionPolicy {
	if in == nil {
		return nil
	}
	out := *in
	out.ResourceCapacities = cloneResourceQuantities(in.ResourceCapacities)
	return &out
}

func cloneAgentRun(in *AgentRun) *AgentRun {
	if in == nil {
		return nil
	}
	var out AgentRun
	data, _ := json.Marshal(in)
	_ = json.Unmarshal(data, &out)
	out.Kind = normalizeRunKind(out.Kind)
	return &out
}
