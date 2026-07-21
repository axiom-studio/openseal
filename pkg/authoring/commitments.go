package authoring

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

const maximumExplicitCommitmentCount = 1000

// effectivePromptCommitments merges generator-declared commitments with the
// narrow facts that OpenSeal can prove from a fixed lexical grammar. Extracted
// facts win, and a generator that omitted or weakened one receives a repairable
// diagnostic. This is intentionally not a general natural-language parser.
func effectivePromptCommitments(prompt string, declared PromptCommitments) (PromptCommitments, []ValidationIssue) {
	extracted := extractExplicitPromptCommitments(prompt)
	issues := validateDeclaredCommitmentCoverage(declared, extracted)
	effective := declared
	if extracted.AgentCount != nil {
		effective.AgentCount = intPointer(*extracted.AgentCount)
	}
	if extracted.TeamCount != nil {
		effective.TeamCount = intPointer(*extracted.TeamCount)
	}
	for _, commitment := range extracted.ObjectiveCounts {
		if !objectiveCommitmentCovered(effective.ObjectiveCounts, commitment, commitmentOwnerIsSingleton(extracted, commitment.OwnerType)) {
			effective.ObjectiveCounts = upsertObjectiveCommitment(effective.ObjectiveCounts, commitment)
		}
	}
	if extracted.Activation != "" {
		effective.Activation = extracted.Activation
	}
	for _, commitment := range extracted.ApprovalRequirements {
		if !approvalCommitmentCovered(effective.ApprovalRequirements, commitment, commitmentOwnerIsSingleton(extracted, commitment.OwnerType)) {
			effective.ApprovalRequirements = upsertApprovalCommitment(effective.ApprovalRequirements, commitment)
		}
	}
	return normalizedPromptCommitments(effective), issues
}

func extractExplicitPromptCommitments(prompt string) PromptCommitments {
	tokens := commitmentTokens(prompt)
	result := PromptCommitments{}
	for index, token := range tokens {
		switch token {
		case "agent", "agents":
			if count, ok := explicitNounCount(tokens, index); ok && authoringVerbNearby(tokens, index) {
				result.AgentCount = intPointer(count)
			}
		case "team", "teams":
			if count, ok := explicitNounCount(tokens, index); ok && authoringVerbNearby(tokens, index) {
				result.TeamCount = intPointer(count)
			} else if phraseBefore(tokens, index, "no") || phraseBefore(tokens, index, "without", "a") {
				result.TeamCount = intPointer(0)
			}
		case "objective", "objectives":
			count, ok := explicitNounCount(tokens, index)
			agentOwner := nearestClauseNoun(tokens, index, "agent", "agents")
			teamOwner := nearestClauseNoun(tokens, index, "team", "teams")
			if !ok || !authoringVerbNearby(tokens, index) && !(agentOwner && result.AgentCount != nil) && !(teamOwner && result.TeamCount != nil) {
				continue
			}
			owner := CommitmentOwnerWorkforce
			if agentOwner {
				owner = CommitmentOwnerAgent
			} else if teamOwner {
				owner = CommitmentOwnerTeam
			}
			result.ObjectiveCounts = upsertObjectiveCommitment(result.ObjectiveCounts, ObjectiveCountCommitment{OwnerType: owner, Count: count})
		}
	}
	if containsTokenPhrase(tokens, "do", "not", "activate") || containsTokenPhrase(tokens, "don", "t", "activate") ||
		containsTokenPhrase(tokens, "keep", "inactive") || containsTokenPhrase(tokens, "without", "activation") {
		result.Activation = ActivationCommitmentInactive
	}
	if explicitSideEffectApproval(tokens) {
		// Publication and production mutation cross the portable write boundary
		// even when a host later assigns a higher concrete risk. Requiring at write
		// means ordinary dispatch cannot bypass the user's approval commitment.
		result.ApprovalRequirements = []ApprovalCommitment{{
			OwnerType: CommitmentOwnerAgent, RequireApprovalAt: capability.RiskLevelWrite,
		}}
	}
	return normalizedPromptCommitments(result)
}

func commitmentTokens(value string) []string {
	result := make([]string, 0, len(value)/4)
	var current []rune
	flush := func() {
		if len(current) > 0 {
			result = append(result, strings.ToLower(string(current)))
			current = current[:0]
		}
	}
	for _, value := range value {
		if unicode.IsLetter(value) || unicode.IsDigit(value) {
			current = append(current, value)
			continue
		}
		flush()
		if value == '.' || value == '!' || value == '?' || value == ';' {
			result = append(result, "|")
		}
	}
	flush()
	return result
}

func explicitNounCount(tokens []string, nounIndex int) (int, bool) {
	for index := nounIndex - 1; index >= 0 && nounIndex-index <= 4 && tokens[index] != "|"; index-- {
		if count, ok := explicitCount(tokens[index]); ok {
			return count, true
		}
		switch tokens[index] {
		case "agent", "agents", "team", "teams", "objective", "objectives", "and", "with":
			return 0, false
		}
	}
	return 0, false
}

func explicitCount(value string) (int, bool) {
	if number, err := strconv.Atoi(value); err == nil && number >= 0 && number <= maximumExplicitCommitmentCount {
		return number, true
	}
	words := map[string]int{"zero": 0, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7, "eight": 8, "nine": 9, "ten": 10}
	number, ok := words[value]
	return number, ok
}

func authoringVerbNearby(tokens []string, nounIndex int) bool {
	verbs := map[string]bool{"create": true, "build": true, "compose": true, "design": true, "make": true, "add": true, "generate": true}
	for index := nounIndex - 2; index >= 0 && nounIndex-index <= 8 && tokens[index] != "|"; index-- {
		if verbs[tokens[index]] {
			return true
		}
	}
	return false
}

func nearestClauseNoun(tokens []string, index int, nouns ...string) bool {
	wanted := make(map[string]bool, len(nouns))
	for _, noun := range nouns {
		wanted[noun] = true
	}
	for cursor := index - 2; cursor >= 0 && index-cursor <= 8 && tokens[cursor] != "|"; cursor-- {
		if wanted[tokens[cursor]] {
			return true
		}
		if tokens[cursor] == "objective" || tokens[cursor] == "objectives" {
			break
		}
	}
	return false
}

func phraseBefore(tokens []string, index int, phrase ...string) bool {
	if index < len(phrase) {
		return false
	}
	for offset, value := range phrase {
		if tokens[index-len(phrase)+offset] != value {
			return false
		}
	}
	return true
}

func containsTokenPhrase(tokens []string, phrase ...string) bool {
	for index := 0; index+len(phrase) <= len(tokens); index++ {
		matched := true
		for offset, value := range phrase {
			if tokens[index+offset] != value {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func explicitSideEffectApproval(tokens []string) bool {
	for index := 0; index+1 < len(tokens); index++ {
		if (tokens[index] != "approval" && tokens[index] != "ask" && tokens[index] != "asks") || tokens[index+1] != "before" {
			continue
		}
		for cursor := index + 2; cursor < len(tokens) && cursor-index <= 8 && tokens[cursor] != "|"; cursor++ {
			switch tokens[cursor] {
			case "publication", "publish", "publishing", "posting", "post", "external", "outbound", "production":
				return true
			}
		}
	}
	return false
}

// applyExtractedApprovalCommitments repairs only a safety threshold that the
// compiler can prove directly from the prompt. It never invents an Agent or
// widens authority; it only makes an existing Agent require approval earlier.
func applyExtractedApprovalCommitments(candidate *WorkforceCandidate, extracted PromptCommitments) {
	if candidate == nil {
		return
	}
	agents := candidateAgentsByID(candidate)
	for _, commitment := range extracted.ApprovalRequirements {
		targets := candidate.Agents
		if commitment.OwnerID != "" {
			target := agents[commitment.OwnerID]
			if target == nil {
				continue
			}
			targets = []*agent.AgentDefinition{target}
		}
		for _, definition := range targets {
			if definition == nil {
				continue
			}
			current := definition.Authority.RequireApprovalAt
			if current == "" || riskRank(current) > riskRank(commitment.RequireApprovalAt) {
				definition.Authority.RequireApprovalAt = commitment.RequireApprovalAt
			}
		}
	}
}

// applyActivationCommitment turns the reviewed commitment into candidate
// state before the candidate digest is computed. Apply consumes only this
// typed field; it never reparses prompt prose or trusts a mutable UI choice.
func applyActivationCommitment(candidate *WorkforceCandidate, commitments PromptCommitments) {
	if candidate == nil {
		return
	}
	candidate.Activation = WorkforceActivationActive
	if commitments.Activation == ActivationCommitmentInactive {
		candidate.Activation = WorkforceActivationInactive
	}
}

func validateDeclaredCommitmentCoverage(declared, extracted PromptCommitments) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	if extracted.AgentCount != nil && (declared.AgentCount == nil || *declared.AgentCount != *extracted.AgentCount) {
		issues = append(issues, issue("commitments.agentCount", "prompt_commitment_missing", fmt.Sprintf("Generator must declare the explicit Agent count %d", *extracted.AgentCount)))
	}
	if extracted.TeamCount != nil && (declared.TeamCount == nil || *declared.TeamCount != *extracted.TeamCount) {
		issues = append(issues, issue("commitments.teamCount", "prompt_commitment_missing", fmt.Sprintf("Generator must declare the explicit Team count %d", *extracted.TeamCount)))
	}
	for _, expected := range extracted.ObjectiveCounts {
		if !objectiveCommitmentCovered(declared.ObjectiveCounts, expected, commitmentOwnerIsSingleton(extracted, expected.OwnerType)) {
			issues = append(issues, issue("commitments.objectiveCounts", "prompt_commitment_missing", fmt.Sprintf("Generator must declare exactly %d %s-owned Objective(s)", expected.Count, expected.OwnerType)))
		}
	}
	if extracted.Activation != "" && declared.Activation != extracted.Activation {
		issues = append(issues, issue("commitments.activation", "prompt_commitment_missing", "Generator must declare that compilation remains inactive"))
	}
	for _, expected := range extracted.ApprovalRequirements {
		if !approvalCommitmentCovered(declared.ApprovalRequirements, expected, commitmentOwnerIsSingleton(extracted, expected.OwnerType)) {
			issues = append(issues, issue("commitments.approvalRequirements", "prompt_commitment_missing", fmt.Sprintf("Generator must declare approval at %s risk for the requested side effect", expected.RequireApprovalAt)))
		}
	}
	return issues
}

func validatePromptCommitments(commitments PromptCommitments, candidate *WorkforceCandidate) []ValidationIssue {
	issues := make([]ValidationIssue, 0)
	if commitments.AgentCount != nil {
		if *commitments.AgentCount < 0 || *commitments.AgentCount > maximumExplicitCommitmentCount {
			issues = append(issues, issue("commitments.agentCount", "invalid_prompt_commitment", "Agent count commitment must be between 0 and 1000"))
		} else if len(candidate.Agents) != *commitments.AgentCount {
			issues = append(issues, issue("agents", "prompt_count_mismatch", fmt.Sprintf("Prompt requires exactly %d Agent(s); candidate contains %d", *commitments.AgentCount, len(candidate.Agents))))
		}
	}
	if commitments.TeamCount != nil {
		actual := 0
		if candidate.Team != nil {
			actual = 1
		}
		if *commitments.TeamCount < 0 || *commitments.TeamCount > 1 {
			issues = append(issues, issue("commitments.teamCount", "invalid_prompt_commitment", "Portable workforce candidates support exactly zero or one Team"))
		} else if actual != *commitments.TeamCount {
			issues = append(issues, issue("team", "prompt_count_mismatch", fmt.Sprintf("Prompt requires exactly %d Team(s); candidate contains %d", *commitments.TeamCount, actual)))
		}
	}
	agents := candidateAgentsByID(candidate)
	seenObjectives := make(map[string]bool)
	for index, commitment := range commitments.ObjectiveCounts {
		path := fmt.Sprintf("commitments.objectiveCounts[%d]", index)
		key := string(commitment.OwnerType) + ":" + strings.TrimSpace(commitment.OwnerID)
		if seenObjectives[key] {
			issues = append(issues, issue(path, "duplicate_prompt_commitment", "Objective count commitments must have unique owners"))
			continue
		}
		seenObjectives[key] = true
		if commitment.Count < 0 || commitment.Count > maximumExplicitCommitmentCount {
			issues = append(issues, issue(path+".count", "invalid_prompt_commitment", "Objective count commitment must be between 0 and 1000"))
			continue
		}
		actual, valid := committedObjectiveCount(commitment, candidate, agents)
		if !valid {
			issues = append(issues, issue(path, "invalid_prompt_commitment", "Objective commitment owner must reference the candidate workforce"))
		} else if actual != commitment.Count {
			issues = append(issues, issue(objectiveCommitmentPath(commitment), "prompt_objective_mismatch", fmt.Sprintf("Prompt requires exactly %d Objective(s) for this owner; candidate contains %d", commitment.Count, actual)))
		}
	}
	if commitments.Activation != "" && commitments.Activation != ActivationCommitmentInactive {
		issues = append(issues, issue("commitments.activation", "invalid_prompt_commitment", "Compilation can only commit to inactive output"))
	}
	seenApprovals := make(map[string]bool)
	for index, commitment := range commitments.ApprovalRequirements {
		path := fmt.Sprintf("commitments.approvalRequirements[%d]", index)
		key := string(commitment.OwnerType) + ":" + strings.TrimSpace(commitment.OwnerID)
		if seenApprovals[key] {
			issues = append(issues, issue(path, "duplicate_prompt_commitment", "Approval commitments must have unique owners"))
			continue
		}
		seenApprovals[key] = true
		if commitment.OwnerType != CommitmentOwnerAgent || riskRank(commitment.RequireApprovalAt) < 0 {
			issues = append(issues, issue(path, "invalid_prompt_commitment", "Approval commitment must target candidate Agents at a valid risk threshold"))
			continue
		}
		targets := candidate.Agents
		if commitment.OwnerID != "" {
			target := agents[commitment.OwnerID]
			if target == nil {
				issues = append(issues, issue(path+".ownerId", "invalid_prompt_commitment", "Approval commitment Agent is not in the candidate"))
				continue
			}
			targets = []*agent.AgentDefinition{target}
		}
		for _, definition := range targets {
			if definition == nil || definition.Authority.RequireApprovalAt == "" || riskRank(definition.Authority.RequireApprovalAt) > riskRank(commitment.RequireApprovalAt) {
				ownerID := commitment.OwnerID
				if definition != nil {
					ownerID = definition.ID
				}
				issues = append(issues, issue("agents."+ownerID+".authority.requireApprovalAt", "prompt_approval_mismatch", fmt.Sprintf("Prompt requires approval at %s risk or stricter", commitment.RequireApprovalAt)))
			}
		}
	}
	sort.SliceStable(issues, func(i, j int) bool {
		if issues[i].Path == issues[j].Path {
			return issues[i].Code < issues[j].Code
		}
		return issues[i].Path < issues[j].Path
	})
	return issues
}

func candidateAgentsByID(candidate *WorkforceCandidate) map[string]*agent.AgentDefinition {
	result := make(map[string]*agent.AgentDefinition, len(candidate.Agents))
	for _, definition := range candidate.Agents {
		if definition != nil {
			result[definition.ID] = definition
		}
	}
	return result
}

func committedObjectiveCount(commitment ObjectiveCountCommitment, candidate *WorkforceCandidate, agents map[string]*agent.AgentDefinition) (int, bool) {
	switch commitment.OwnerType {
	case CommitmentOwnerWorkforce:
		if commitment.OwnerID != "" {
			return 0, false
		}
		count := 0
		for _, definition := range candidate.Agents {
			if definition != nil {
				count += len(definition.ObjectiveTemplates)
			}
		}
		if candidate.Team != nil {
			count += len(candidate.Team.ObjectiveTemplates)
		}
		return count, true
	case CommitmentOwnerAgent:
		if commitment.OwnerID != "" {
			definition := agents[commitment.OwnerID]
			if definition == nil {
				return 0, false
			}
			return len(definition.ObjectiveTemplates), true
		}
		count := 0
		for _, definition := range candidate.Agents {
			if definition != nil {
				count += len(definition.ObjectiveTemplates)
			}
		}
		return count, true
	case CommitmentOwnerTeam:
		if candidate.Team == nil || commitment.OwnerID != "" && commitment.OwnerID != candidate.Team.ID {
			return 0, false
		}
		return len(candidate.Team.ObjectiveTemplates), true
	default:
		return 0, false
	}
}

func objectiveCommitmentPath(commitment ObjectiveCountCommitment) string {
	if commitment.OwnerID != "" {
		return string(commitment.OwnerType) + "." + commitment.OwnerID + ".objectiveTemplates"
	}
	return string(commitment.OwnerType) + ".objectiveTemplates"
}

func normalizedPromptCommitments(commitments PromptCommitments) PromptCommitments {
	for index := range commitments.ObjectiveCounts {
		commitments.ObjectiveCounts[index].OwnerID = strings.TrimSpace(commitments.ObjectiveCounts[index].OwnerID)
	}
	for index := range commitments.ApprovalRequirements {
		commitments.ApprovalRequirements[index].OwnerID = strings.TrimSpace(commitments.ApprovalRequirements[index].OwnerID)
	}
	sort.Slice(commitments.ObjectiveCounts, func(i, j int) bool {
		left, right := commitments.ObjectiveCounts[i], commitments.ObjectiveCounts[j]
		if left.OwnerType == right.OwnerType {
			return left.OwnerID < right.OwnerID
		}
		return left.OwnerType < right.OwnerType
	})
	sort.Slice(commitments.ApprovalRequirements, func(i, j int) bool {
		left, right := commitments.ApprovalRequirements[i], commitments.ApprovalRequirements[j]
		if left.OwnerType == right.OwnerType {
			return left.OwnerID < right.OwnerID
		}
		return left.OwnerType < right.OwnerType
	})
	return commitments
}

func upsertObjectiveCommitment(values []ObjectiveCountCommitment, value ObjectiveCountCommitment) []ObjectiveCountCommitment {
	for index := range values {
		if values[index].OwnerType == value.OwnerType && strings.TrimSpace(values[index].OwnerID) == strings.TrimSpace(value.OwnerID) {
			values[index] = value
			return values
		}
	}
	return append(values, value)
}

func upsertApprovalCommitment(values []ApprovalCommitment, value ApprovalCommitment) []ApprovalCommitment {
	for index := range values {
		if values[index].OwnerType == value.OwnerType && strings.TrimSpace(values[index].OwnerID) == strings.TrimSpace(value.OwnerID) {
			values[index] = value
			return values
		}
	}
	return append(values, value)
}

func objectiveCommitmentCovered(values []ObjectiveCountCommitment, expected ObjectiveCountCommitment, singleton bool) bool {
	for _, value := range values {
		ownerCovered := value.OwnerID == expected.OwnerID || expected.OwnerID == "" && singleton && value.OwnerID != ""
		if value.OwnerType == expected.OwnerType && value.Count == expected.Count && ownerCovered {
			return true
		}
	}
	return false
}

func approvalCommitmentCovered(values []ApprovalCommitment, expected ApprovalCommitment, singleton bool) bool {
	for _, value := range values {
		ownerCovered := value.OwnerID == expected.OwnerID || expected.OwnerID == "" && singleton && value.OwnerID != ""
		if value.OwnerType == expected.OwnerType && ownerCovered &&
			riskRank(value.RequireApprovalAt) >= 0 && riskRank(value.RequireApprovalAt) <= riskRank(expected.RequireApprovalAt) {
			return true
		}
	}
	return false
}

func commitmentOwnerIsSingleton(commitments PromptCommitments, owner CommitmentOwnerType) bool {
	switch owner {
	case CommitmentOwnerAgent:
		return commitments.AgentCount != nil && *commitments.AgentCount == 1
	case CommitmentOwnerTeam:
		return commitments.TeamCount != nil && *commitments.TeamCount == 1
	default:
		return false
	}
}

func intPointer(value int) *int { return &value }
