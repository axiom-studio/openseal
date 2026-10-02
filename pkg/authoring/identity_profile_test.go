package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/team"
)

type profileFixtureGenerator struct {
	profile                    IdentityProfile
	fail                       bool
	profileCalls, plannerCalls int
	requests                   []GenerateRequest
}

func (g *profileFixtureGenerator) GenerateProfile(_ context.Context, request GenerateRequest) (IdentityProfile, error) {
	g.profileCalls++
	g.requests = append(g.requests, request)
	if g.fail {
		return IdentityProfile{}, errors.New("profile provider unavailable")
	}
	return g.profile, nil
}

func (g *profileFixtureGenerator) GenerateIntent(context.Context, GenerateRequest) (AuthoringIntent, error) {
	g.plannerCalls++
	return AuthoringIntent{SchemaVersion: AuthoringIntentSchemaVersion, Kind: AuthoringResourceAgent, Name: "Orbit", Purpose: "Help research", Agents: []AuthoringAgentIntent{{Key: "orbit", Name: "Orbit", Purpose: "Help research", Behavior: "Research clearly."}}}, nil
}

func validProfileFixture() IdentityProfile {
	return IdentityProfile{Name: "Orbit", Purpose: "Research markets with you", Behavior: "Help with the task at hand; discover useful Skills in conversation and guide connection setup when needed.", Personality: "Warm and curious", OperatingPrinciples: []string{"Ask focused questions when needed."}}
}

func profileCreateFixture(key string) CreateChangeSetRequest {
	return CreateChangeSetRequest{ProfileOnly: true, Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a friendly market researcher that uses Slack and sends a report every day at 9am.", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: key}
}

func profileService(t *testing.T, g *profileFixtureGenerator) (*ChangeSetService, *MemoryChangeSetStore) {
	t.Helper()
	c, err := NewCompiler(g)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryChangeSetStore()
	s, err := NewChangeSetService(c, store)
	if err != nil {
		t.Fatal(err)
	}
	return s, store
}

func TestIdentityProfileSkipsPlannerForSlackAndDailyRequest(t *testing.T) {
	g := &profileFixtureGenerator{profile: validProfileFixture()}
	s, _ := profileService(t, g)
	request := profileCreateFixture("profile")
	// Even unusable inventory cannot turn identity creation into Skill planning.
	request.Catalog = CapabilityCatalog{Skills: map[string]SkillCapability{"bad": {ID: "different"}}, AvailableCredentials: map[string]bool{"credential-only-in-vault": true}}
	prepared, _, err := s.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !prepared.ProfileOnly || !prepared.Generation.Request.ProfileOnly || g.profileCalls != 0 {
		t.Fatal("profile mode not durably prepared")
	}
	compiled, err := s.GeneratePrepared(t.Context(), prepared.Scope, prepared.ID, prepared.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if g.profileCalls != 1 || g.plannerCalls != 0 || compiled.Status != ChangeSetReview || !compiled.Result.Valid {
		t.Fatalf("calls=%d planner=%d state=%s issues=%#v", g.profileCalls, g.plannerCalls, compiled.Status, compiled.Result.Validation)
	}
	if err := ValidateProfileChangeSet(compiled); err != nil {
		t.Fatal(err)
	}
	if compiled.Result.Candidate.Agents[0].Runbook != nil || len(compiled.RequiredCredentials) != 0 || len(compiled.Result.MissingRequirements) != 0 {
		t.Fatal("creation generated work or setup")
	}
	for _, request := range g.requests {
		if len(request.Catalog.Skills) != 0 || len(request.Catalog.AvailableCredentials) != 0 || len(request.Form.Fields) != 0 || request.CompositionRequirements != nil {
			t.Fatal("profile provider received planner inputs")
		}
	}
}

func TestIdentityProfileModeIsStickyThroughChildrenRefinementAndRetry(t *testing.T) {
	g := &profileFixtureGenerator{profile: validProfileFixture()}
	g.profile.Clarifications = []IdentityProfileClarification{{Field: "personality", Question: "Should the voice be playful or calm?", WhyNeeded: "Your requested voices conflict."}}
	s, _ := profileService(t, g)
	request := profileCreateFixture("parent")
	parent, _, err := s.Create(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	request.ParentID, request.IdempotencyKey, request.ProfileOnly = parent.ID, "child", false
	child, _, err := s.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !child.ProfileOnly || !child.Generation.Request.ProfileOnly {
		t.Fatal("child escaped profile mode")
	}
	child, err = s.GeneratePrepared(t.Context(), child.Scope, child.ID, child.Revision)
	if err != nil {
		t.Fatal(err)
	}
	answered, _, err := s.AnswerRefinement(t.Context(), AnswerChangeSetRefinementRequest{Scope: child.Scope, ChangeSetID: child.ID, ExpectedRevision: child.Revision, QuestionID: "profile-personality", Value: RefinementAnswerValue{Text: "Calm"}, Actor: request.Actor, IdempotencyKey: "identity-answer"})
	if err != nil {
		t.Fatal(err)
	}
	if !answered.ProfileOnly || !answered.Generation.Request.ProfileOnly {
		t.Fatal("refinement escaped profile mode")
	}
	g.fail = true
	failed, err := s.GeneratePrepared(t.Context(), answered.Scope, answered.ID, answered.Revision)
	if err == nil || failed.Status != ChangeSetFailed {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	retried, _, err := s.RetryGeneration(t.Context(), RetryChangeSetGenerationRequest{Scope: failed.Scope, ChangeSetID: failed.ID, ExpectedRevision: failed.Revision, Actor: request.Actor, IdempotencyKey: "retry-profile", Reason: "Try again"})
	if err != nil {
		t.Fatal(err)
	}
	if !retried.ProfileOnly || !retried.Generation.Request.ProfileOnly {
		t.Fatal("retry escaped profile mode")
	}
	g.fail = false
	g.profile.Clarifications = nil
	retried, err = s.GeneratePrepared(t.Context(), retried.Scope, retried.ID, retried.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Status != ChangeSetReview || g.plannerCalls != 0 || retried.Result.Candidate.Agents[0].ID != parent.Result.Candidate.Agents[0].ID {
		t.Fatal("profile identity or generation path changed")
	}
}

func TestIdentityProfileFlagParticipatesInIdempotency(t *testing.T) {
	g := &profileFixtureGenerator{profile: validProfileFixture()}
	s, _ := profileService(t, g)
	request := profileCreateFixture("same-key")
	first, _, err := s.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, found, err := s.Prepare(t.Context(), request)
	if err != nil || !found || first.ID != replay.ID {
		t.Fatalf("replay=%v err=%v", found, err)
	}
	request.ProfileOnly = false
	if _, _, err = s.Prepare(t.Context(), request); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("mode substitution returned %v", err)
	}
	payload, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var restored ChangeSet
	if err := json.Unmarshal(payload, &restored); err != nil || !restored.ProfileOnly || !restored.Generation.Request.ProfileOnly {
		t.Fatalf("durable flag lost: %v", err)
	}
}

func TestIdentityProfilesApplyIndependentlyAndReplay(t *testing.T) {
	g := &profileFixtureGenerator{profile: validProfileFixture()}
	s, _ := profileService(t, g)
	ids := map[string]bool{}
	for _, key := range []string{"one-profile", "two-profile"} {
		request := profileCreateFixture(key)
		created, _, err := s.Create(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		id := created.Result.Candidate.Agents[0].ID
		if id == "profile" || ids[id] {
			t.Fatal("profile identity collided")
		}
		ids[id] = true
		ready, _, err := s.SubmitEvaluation(t.Context(), SubmitChangeSetEvaluationRequest{Scope: created.Scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest, Actor: request.Actor, Allowed: true, IdempotencyKey: "allow-" + key})
		if err != nil {
			t.Fatal(err)
		}
		apply := ApplyChangeSetRequest{Scope: ready.Scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Create Agent", Actor: request.Actor, IdempotencyKey: "apply-" + key}
		applied, _, err := s.Apply(t.Context(), apply)
		if err != nil || len(applied.ApplyReceipt.Resources) != 2 {
			t.Fatalf("apply=%#v err=%v", applied, err)
		}
		replay, found, err := s.Apply(t.Context(), apply)
		if err != nil || !found || replay.ApplyReceipt.ID != applied.ApplyReceipt.ID {
			t.Fatalf("apply replay=%v err=%v", found, err)
		}
	}
}

func TestIdentityProfileRejectsWorkflowAndAccountMutations(t *testing.T) {
	base, err := compileIdentityProfile(validProfileFixture(), GenerateRequest{Mode: ModeCreate})
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*WorkforceCandidate){
		"skill": func(c *WorkforceCandidate) {
			c.Agents[0].SkillRequirements = []agent.SkillRequirement{{SkillID: "slack"}}
		},
		"runbook": func(c *WorkforceCandidate) { c.Agents[0].Runbook = &runbook.Definition{} },
		"team":    func(c *WorkforceCandidate) { c.Team = &team.Definition{} },
		"channel": func(c *WorkforceCandidate) { c.ConversationEndpoints = []ConversationEndpointBlueprint{{}} },
	} {
		t.Run(name, func(t *testing.T) {
			payload, _ := json.Marshal(base.Candidate)
			var candidate WorkforceCandidate
			_ = json.Unmarshal(payload, &candidate)
			change(&candidate)
			if ValidateProfileCandidate(&candidate) == nil {
				t.Fatal("complex profile accepted")
			}
		})
	}
	g := &profileFixtureGenerator{profile: validProfileFixture()}
	s, _ := profileService(t, g)
	created, _, err := s.Create(t.Context(), profileCreateFixture("profile-mutation"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.UpdatePlacement(t.Context(), UpdateChangeSetPlacementRequest{Scope: created.Scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, Placement: ChangeSetPlacement{BindingConfigs: map[string]map[string]map[string]interface{}{"profile": {"slack": {"channel": "C123"}}}}, Actor: created.Actor, Reason: "Add Slack", IdempotencyKey: "no-setup"})
	if err == nil || !strings.Contains(err.Error(), "cannot configure") {
		t.Fatalf("account setup accepted: %v", err)
	}
	legacy := profileCreateFixture("advanced")
	legacy.ProfileOnly = false
	full, _, err := s.Create(t.Context(), legacy)
	if err != nil {
		t.Fatal(err)
	}
	legacy.ProfileOnly, legacy.ParentID, legacy.IdempotencyKey = true, full.ID, "convert-advanced"
	if _, _, err := s.Prepare(t.Context(), legacy); err == nil {
		t.Fatal("profile mode accepted a workflow parent")
	}
	if g.plannerCalls != 1 {
		t.Fatal("default advanced authoring changed")
	}
}
