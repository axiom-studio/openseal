package authoring

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func validateDraftAgentName(name string, optional bool) error {
	if name == "" && optional {
		return nil
	}
	if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 80 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return errors.New("agent name must contain 1 to 80 characters without control characters")
	}
	return nil
}

func applyDraftAgentName(candidate *WorkforceCandidate, name string) {
	if name != "" && len(candidate.Agents) == 1 && candidate.Agents[0] != nil && candidate.Team == nil {
		candidate.Agents[0].DisplayName = name
	}
}

// RenameAgent changes only the display name of a standalone draft. It never
// changes resource identities, grants, bindings, or an already deployed Agent.
func (s *ChangeSetService) RenameAgent(ctx context.Context, scope skill.ScopeReference, id string, revision int64, name string, actor ChangeSetActor) (*ChangeSet, error) {
	store, ok := s.store.(AgentNameChangeSetStore)
	if !ok {
		return nil, errors.New("draft naming is unavailable")
	}
	return store.RenameChangeSetAgent(ctx, scope, id, revision, name, actor, s.now().UTC())
}

// AgentNameChangeSetStore provides an atomic name-only candidate amendment.
// Ordinary UpdateChangeSet still forbids changes to the candidate digest.
type AgentNameChangeSetStore interface {
	RenameChangeSetAgent(context.Context, skill.ScopeReference, string, int64, string, ChangeSetActor, time.Time) (*ChangeSet, error)
}

// PrepareAgentNameUpdate validates a name-only amendment under a store's CAS.
// Stores must atomically compare the source revision before persisting it.
func PrepareAgentNameUpdate(current *ChangeSet, revision int64, name string, actor ChangeSetActor, now time.Time) (*ChangeSet, error) {
	name = strings.TrimSpace(name)
	if err := validateDraftAgentName(name, false); err != nil {
		return nil, err
	}
	if actor.Type == "" || actor.ID == "" || actor != current.Actor {
		return nil, errors.New("only the draft creator can rename this agent")
	}
	if !DraftAgentNameEditable(current) {
		return nil, ErrChangeSetTransition
	}
	// A lost response may be retried without producing a second mutation.
	if current.AgentName == name {
		return current, nil
	}
	if current.Revision != revision {
		return nil, ErrChangeSetRevision
	}
	next := cloneChangeSet(current)
	next.AgentName = name
	if next.Generation != nil {
		next.Generation.Request.AgentName = name
	}
	applyDraftAgentName(&next.Result.Candidate, name)
	digest, err := digestJSON(next.Result.Candidate)
	next.CandidateDigest = digest
	if err != nil {
		return nil, err
	}
	var existing *WorkforceCandidate
	if next.Generation != nil {
		existing = next.Generation.Request.Existing
	}
	next.Result.Diff = workforceDiff(existing, &next.Result.Candidate)
	// Prior approvals apply only to their original candidate digest. Run the
	// normal policy review again for any otherwise-ready renamed candidate.
	next.Status = ChangeSetBlocked
	if next.Result.Valid {
		next.Status = ChangeSetReview
	}
	next.Revision++
	next.UpdatedAt = now.UTC()
	next.Lifecycle = append(next.Lifecycle, ChangeSetLifecycleEvent{Revision: next.Revision, From: current.Status, To: next.Status, Reason: "agent_name_updated", Actor: actor, At: next.UpdatedAt})
	return next, nil
}

// DraftAgentNameEditable excludes activation continuations, whose definition
// identity and version are already published and immutable.
func DraftAgentNameEditable(current *ChangeSet) bool {
	if current == nil || isActivationContinuation(current) || len(current.Result.Candidate.Agents) != 1 || current.Result.Candidate.Agents[0] == nil || current.Result.Candidate.Team != nil {
		return false
	}
	switch current.Status {
	case ChangeSetBlocked, ChangeSetReview, ChangeSetReady, ChangeSetAwaitingApproval:
		return true
	}
	return false
}

func validateSuggestedAgentNames(candidate *WorkforceCandidate, request GenerateRequest) []ValidationIssue {
	if request.AgentName != "" {
		return nil
	}
	occupied := make(map[string]bool, len(request.ExistingAgentNames))
	for _, name := range request.ExistingAgentNames {
		occupied[strings.ToLower(strings.Join(strings.Fields(name), " "))] = true
	}
	var issues []ValidationIssue
	for i, proposed := range candidate.Agents {
		if proposed == nil {
			continue
		}
		preserved := false
		if request.Existing != nil {
			for _, previous := range request.Existing.Agents {
				if previous != nil && previous.ID == proposed.ID && previous.DisplayName == proposed.DisplayName {
					preserved = true
				}
			}
		}
		key := strings.ToLower(strings.Join(strings.Fields(proposed.DisplayName), " "))
		if occupied[key] && !preserved {
			issues = append(issues, issue(fmt.Sprintf("candidate.agents.%d.displayName", i), "agent_name_in_use", "Choose a different, purpose-inspired name that is absent from existingAgentNames and distinct from the other agents in this proposal."))
		}
		occupied[key] = true
	}
	return issues
}
