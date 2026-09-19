package authoring

import (
	"fmt"

	gonanoid "github.com/matoous/go-nanoid/v2"
)

func newResourceID() (string, error) {
	id, err := gonanoid.New()
	if err != nil {
		return "", fmt.Errorf("allocate workforce identity: %w", err)
	}
	return id, nil
}

// assignCandidateIdentities is the trusted boundary between model form keys and
// persisted identity. Existing resources retain their IDs; every new definition
// receives an independently generated NanoID. No display name enters allocation.
func assignCandidateIdentities(candidate, existing *WorkforceCandidate) (map[string]string, error) {
	preserved := map[string]bool{}
	existingByKey := existingAuthoringAgentsByKey(existing)
	if existing != nil {
		for _, definition := range existing.Agents {
			if definition != nil {
				preserved[definition.ID] = true
			}
		}
	}
	ids := map[string]string{}
	for _, definition := range candidate.Agents {
		if definition == nil {
			continue
		}
		old := definition.ID
		if definition.AuthoringKey == "" {
			definition.AuthoringKey = authoringPortableKey(old)
		}
		id := old
		if current := existingByKey[definition.AuthoringKey]; !preserved[old] && current != nil {
			id = current.ID
		} else if !preserved[old] {
			var err error
			id, err = newResourceID()
			if err != nil {
				return nil, err
			}
		}
		ids[old] = id
	}
	teamID := ""
	if candidate.Team != nil {
		if candidate.Team.AuthoringKey == "" {
			candidate.Team.AuthoringKey = authoringPortableKey(candidate.Team.ID)
		}
		if existing != nil && existing.Team != nil && (existing.Team.ID == candidate.Team.ID || existing.Team.AuthoringKey == candidate.Team.AuthoringKey) {
			teamID = existing.Team.ID
		} else {
			var err error
			teamID, err = newResourceID()
			if err != nil {
				return nil, err
			}
		}
	}
	canonicalizeCandidateReferences(candidate, func(id string, team bool) string {
		if team {
			return teamID
		}
		return ids[id]
	})
	for key, id := range candidateAgentIdentityMap(candidate) {
		ids[key] = id
	}
	return ids, nil
}

// Accept the reviewed ID or its stable form key when patching placement. Unknown
// references remain unchanged so validation rejects them rather than retargeting.
func candidateAgentIdentityMap(candidate *WorkforceCandidate) map[string]string {
	ids := map[string]string{}
	for _, definition := range candidate.Agents {
		if definition != nil {
			ids[definition.ID] = definition.ID
			if definition.AuthoringKey != "" {
				ids[definition.AuthoringKey] = definition.ID
			}
		}
	}
	return ids
}

// Placement edits inherit only omitted resource identities, keeping retries
// deterministic without inheriting unrelated credentials or configuration.
func inheritDeploymentIdentities(placement *ChangeSetPlacement, current *ChangeSet) {
	if placement.TeamDeploymentID == "" {
		placement.TeamDeploymentID = current.Placement.TeamDeploymentID
	}
	if placement.AgentDeploymentIDs == nil {
		placement.AgentDeploymentIDs = map[string]string{}
	}
	known := candidateAgentIdentityMap(&current.Result.Candidate)
	requested := map[string]bool{}
	for key := range placement.AgentDeploymentIDs {
		requested[known[key]] = true
	}
	for id, deploymentID := range current.Placement.AgentDeploymentIDs {
		if !requested[id] {
			placement.AgentDeploymentIDs[id] = deploymentID
		}
	}
}
