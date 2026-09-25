package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
)

type sequenceChangeSetGenerator struct{ payloads [][]byte }

func (g *sequenceChangeSetGenerator) Generate(context.Context, GenerateRequest) ([]byte, error) {
	if len(g.payloads) == 0 {
		return nil, errors.New("no fixture payload")
	}
	payload := g.payloads[0]
	g.payloads = g.payloads[1:]
	return payload, nil
}

type recoverExistingChangeSetGenerator struct{ initial []byte }

func (g recoverExistingChangeSetGenerator) Generate(_ context.Context, request GenerateRequest) ([]byte, error) {
	if request.Mode == ModeAmend && request.Existing != nil {
		payload, err := json.Marshal(request.Existing)
		if err != nil {
			return nil, err
		}
		var candidate WorkforceCandidate
		if err = json.Unmarshal(payload, &candidate); err != nil {
			return nil, err
		}
		for _, definition := range candidate.Agents {
			definition.Version = "2"
		}
		if candidate.Team != nil {
			candidate.Team.Version = "2"
		}
		return json.Marshal(GenerationResponse{Candidate: candidate})
	}
	return append([]byte(nil), g.initial...), nil
}

type staticChangeSetReadinessValidator struct {
	issues []ValidationIssue
	calls  int
}

func (v *staticChangeSetReadinessValidator) ValidateChangeSetReadiness(context.Context, *ChangeSet) ([]ValidationIssue, error) {
	v.calls++
	return append([]ValidationIssue(nil), v.issues...), nil
}

func TestChangeSetReadinessErrorClassifiesOnlyAgentDeploymentIdentityConflicts(t *testing.T) {
	conflict := &ChangeSetReadinessError{Issues: []ValidationIssue{{
		Code:    "agent_deployment_identity_conflict",
		Message: "Agent deployment identity already exists",
	}}}
	if !errors.Is(conflict, ErrChangeSetPlacementConflict) {
		t.Fatalf("identity conflict classification = %v", conflict)
	}
	ordinary := &ChangeSetReadinessError{Issues: []ValidationIssue{{
		Code:    "skill_binding_definition_unavailable",
		Message: "Skill is unavailable",
	}}}
	if errors.Is(ordinary, ErrChangeSetPlacementConflict) {
		t.Fatalf("ordinary readiness failure classified as placement conflict: %v", ordinary)
	}
}

func TestApplyReportsMissingTeamRequirementWithRepair(t *testing.T) {
	value := &ChangeSet{Result: CompileResult{
		Valid:               false,
		MissingRequirements: []MissingRequirement{{Kind: "skill_binding", ID: "delivery", RequiredBy: "team:outbound/role:sender"}},
	}}
	err := validateApplyPlacement(value)
	var readiness *ChangeSetReadinessError
	if !errors.As(err, &readiness) || len(readiness.Issues) != 1 ||
		!strings.Contains(err.Error(), "Skill delivery required by team:outbound/role:sender") ||
		!strings.Contains(err.Error(), "select and configure its binding") {
		t.Fatalf("team requirement error = %#v", err)
	}
}

func TestPreparePersistsGenerationBeforeModelWorkAndReplays(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	generator := &sequenceChangeSetGenerator{payloads: [][]byte{payload}}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	request := CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "create",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
		Actor:   ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "async-create",
	}
	prepared, replayed, err := service.Prepare(context.Background(), request)
	if err != nil || replayed || prepared.Status != ChangeSetEvaluating || prepared.Revision != 1 || prepared.Generation == nil || len(generator.payloads) != 1 {
		t.Fatalf("prepared=%#v replayed=%t payloads=%d err=%v", prepared, replayed, len(generator.payloads), err)
	}
	replay, replayed, err := service.Prepare(context.Background(), request)
	if err != nil || !replayed || replay.ID != prepared.ID || len(generator.payloads) != 1 {
		t.Fatalf("replay=%#v replayed=%t payloads=%d err=%v", replay, replayed, len(generator.payloads), err)
	}
	completed, err := service.GeneratePrepared(context.Background(), prepared.Scope, prepared.ID, prepared.Revision)
	if err != nil || completed.Status != ChangeSetReview || completed.Revision != 2 || completed.CandidateDigest == "" || completed.Generation.CompletedAt == nil || len(generator.payloads) != 0 {
		t.Fatalf("completed=%#v payloads=%d err=%v", completed, len(generator.payloads), err)
	}
	if completed.Placement.Environment != "default" || len(completed.Placement.TeamDeploymentID) != 21 || strings.Contains(completed.Placement.TeamDeploymentID, "/") ||
		len(completed.Placement.AgentDeploymentIDs[completed.Result.Candidate.Agents[0].ID]) != 21 || strings.Contains(completed.Placement.AgentDeploymentIDs[completed.Result.Candidate.Agents[0].ID], "/") {
		t.Fatalf("default placement=%#v", completed.Placement)
	}
	if _, err := service.GeneratePrepared(context.Background(), prepared.Scope, prepared.ID, prepared.Revision); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("duplicate generation error=%v", err)
	}
}

func TestGeneratePreparedAtomicallyPlacesExactConversationSkillIdentity(t *testing.T) {
	candidate := directChatbotCandidate("slack", capability.ConversationEndpointChannel, ConversationReplyThread)
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	catalog := slackChatbotCatalog()
	skillCapability := catalog.Skills["slack"]
	skillCapability.SourceIdentity = "registry.example::communications/slack"
	exact := capability.NewSkillIdentity(
		"skill-slack",
		"2.0.0",
		skillCapability.SourceIdentity,
	)
	skillCapability.RuntimeIdentity = &exact
	skillCapability.Readiness = SkillReadinessNeedsBinding
	catalog.Skills["slack"] = skillCapability

	prepared, replayed, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: slackChatbotPrompt,
		Catalog: catalog, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "exact-conversation-placement",
	})
	if err != nil || replayed {
		t.Fatalf("prepare replayed=%t err=%v", replayed, err)
	}
	completed, err := service.GeneratePrepared(context.Background(), prepared.Scope, prepared.ID, prepared.Revision)
	if err != nil {
		t.Fatal(err)
	}
	ownerID := completed.Result.Candidate.ConversationEndpoints[0].Owner.ID
	if got := completed.Placement.SkillSourceIdentities[ownerID]["slack"]; got != exact.SourceIdentity {
		t.Fatalf("source identity = %q", got)
	}
	if got := completed.Placement.SkillSourceVersions[ownerID]["slack"]; got != exact.Version {
		t.Fatalf("source version = %q", got)
	}
	if got := completed.Placement.SkillRuntimeIdentities[ownerID]["slack"]; !got.Equal(exact) {
		t.Fatalf("runtime identity = %#v", got)
	}
	persisted, err := store.GetChangeSet(context.Background(), completed.Scope, completed.ID)
	if err != nil || !persisted.Placement.SkillRuntimeIdentities[ownerID]["slack"].Equal(exact) {
		t.Fatalf("persisted placement=%#v err=%v", persisted.Placement, err)
	}
}

func TestSeedExactCatalogSkillPlacementCoversAgentAndTeamWithoutOverwritingReview(t *testing.T) {
	candidate := marketingCandidate("1.0.0", capability.RiskLevelRead)
	candidate.ConversationEndpoints = []ConversationEndpointBlueprint{{
		ID: "team-chat", Owner: ConversationEndpointOwner{Type: ConversationEndpointOwnerTeam, ID: candidate.Team.ID},
		SkillID: "team-chat", SkillVersion: "2.0.0",
	}}
	candidate.Agents[0].SkillRequirements = append(candidate.Agents[0].SkillRequirements,
		agent.SkillRequirement{SkillID: "native"},
		agent.SkillRequirement{SkillID: "install-later"},
		agent.SkillRequirement{SkillID: "unavailable"},
	)
	exactResearch := capability.NewSkillIdentity("compiled-research", "1.2.3+source.abc", "registry.example::research")
	exactChat := capability.NewSkillIdentity("compiled-chat", "2.0.0+source.def", "registry.example::chat")
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"reddit-research": {ID: "reddit-research", Version: "1.2.3", SourceIdentity: exactResearch.SourceIdentity, RuntimeIdentity: &exactResearch, Readiness: SkillReadinessNeedsBinding},
		"team-chat":       {ID: "team-chat", Version: "2.0.0", SourceIdentity: exactChat.SourceIdentity, RuntimeIdentity: &exactChat, Readiness: SkillReadinessReady},
		"native":          {ID: "native-runtime", Version: "3.0.0", Readiness: SkillReadinessReady},
		"install-later":   {ID: "install-runtime", Version: "1.0.0", Readiness: SkillReadinessNeedsInstallation},
		"unavailable":     {ID: "unavailable-runtime", Version: "1.0.0", Readiness: SkillReadinessUnavailable},
	}}
	reviewed := capability.NewSkillIdentity("reviewed-runtime", "9.0.0", "reviewed::source")
	placement := ChangeSetPlacement{
		SkillSourceIdentities:  map[string]map[string]string{candidate.Agents[0].ID: {"reddit-research": reviewed.SourceIdentity}},
		SkillSourceVersions:    map[string]map[string]string{candidate.Agents[0].ID: {"reddit-research": reviewed.Version}},
		SkillRuntimeIdentities: map[string]map[string]capability.SkillIdentity{candidate.Agents[0].ID: {"reddit-research": reviewed}},
	}

	seedExactCatalogSkillPlacement(&candidate, catalog, &placement)
	if got := placement.SkillRuntimeIdentities[candidate.Agents[0].ID]["reddit-research"]; !got.Equal(reviewed) {
		t.Fatalf("reviewed identity overwritten: %#v", got)
	}
	if got := placement.SkillRuntimeIdentities[candidate.Team.ID]["team-chat"]; !got.Equal(exactChat) {
		t.Fatalf("Team endpoint identity = %#v", got)
	}
	if got := placement.SkillRuntimeIdentities[candidate.Agents[0].ID]["native"]; !got.Equal(capability.NewSkillIdentity("native-runtime", "3.0.0", "")) {
		t.Fatalf("native identity = %#v", got)
	}
	if _, exists := placement.SkillRuntimeIdentities[candidate.Agents[0].ID]["install-later"]; exists {
		t.Fatal("needs-installation Skill was falsely placed")
	}
	if _, exists := placement.SkillRuntimeIdentities[candidate.Agents[0].ID]["unavailable"]; exists {
		t.Fatal("unavailable Skill was falsely placed")
	}
}

func TestSensitivePromptIsRejectedBeforePersistenceOrModelWork(t *testing.T) {
	generator := &sequenceChangeSetGenerator{payloads: [][]byte{[]byte(`{"must":"remain unused"}`)}}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	request := CreateChangeSetRequest{
		Scope:  capability.ScopeReference{Kind: "tenant", ID: "one"},
		Prompt: "Create an Agent with password: never-persist-this",
		Actor:  ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "sensitive-create",
	}
	if _, _, err := service.Prepare(context.Background(), request); !errors.Is(err, ErrSensitiveAuthoringInput) {
		t.Fatalf("prepare error = %v", err)
	}
	if len(store.changeSets) != 0 || len(store.idempotency) != 0 || len(generator.payloads) != 1 {
		t.Fatalf("sensitive prepare mutated state: changes=%d idempotency=%d provider payloads=%d", len(store.changeSets), len(store.idempotency), len(generator.payloads))
	}
	if _, _, err := service.Create(context.Background(), request); !errors.Is(err, ErrSensitiveAuthoringInput) {
		t.Fatalf("create error = %v", err)
	}
	if len(store.changeSets) != 0 || len(generator.payloads) != 1 {
		t.Fatalf("sensitive create mutated state: changes=%d provider payloads=%d", len(store.changeSets), len(generator.payloads))
	}
	if _, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: request.Prompt}); !errors.Is(err, ErrSensitiveAuthoringInput) {
		t.Fatalf("compiler error = %v", err)
	}
	if len(generator.payloads) != 1 {
		t.Fatal("compiler invoked the provider with a sensitive prompt")
	}
}

func TestSensitiveProviderCandidateIsRejectedBeforePersistence(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Agents[0].SystemPrompt = "Authenticate with password: provider-must-not-persist"
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	generator := &sequenceChangeSetGenerator{payloads: [][]byte{payload}}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	request := CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a market research Agent",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
		Actor:   ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "sensitive-provider-output",
	}
	if _, _, err := service.Create(context.Background(), request); !errors.Is(err, ErrSensitiveAuthoringInput) {
		t.Fatalf("create error = %v", err)
	}
	if len(store.changeSets) != 0 || len(store.idempotency) != 0 {
		t.Fatalf("sensitive provider output persisted: changes=%d idempotency=%d", len(store.changeSets), len(store.idempotency))
	}
}

func TestPreparedCatalogRefreshIsCASBoundAndAuditable(t *testing.T) {
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	prepared, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "create",
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "catalog-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	catalog := CapabilityCatalog{AvailableCredentials: map[string]bool{"managed-secret": true}}
	refreshed, err := service.RefreshPreparedCatalog(context.Background(), prepared.Scope, prepared.ID, prepared.Revision, catalog)
	if err != nil || refreshed.Revision != prepared.Revision+1 || !refreshed.Catalog.AvailableCredentials["managed-secret"] ||
		!refreshed.Generation.Request.Catalog.AvailableCredentials["managed-secret"] ||
		refreshed.Lifecycle[len(refreshed.Lifecycle)-1].Reason != "capability_catalog_resolved" {
		t.Fatalf("refreshed=%#v err=%v", refreshed, err)
	}
	unchanged, err := service.RefreshPreparedCatalog(context.Background(), prepared.Scope, prepared.ID, refreshed.Revision, catalog)
	if err != nil || unchanged.Revision != refreshed.Revision {
		t.Fatalf("unchanged=%#v err=%v", unchanged, err)
	}
	if _, err := service.RefreshPreparedCatalog(context.Background(), prepared.Scope, prepared.ID, prepared.Revision, catalog); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale refresh error=%v", err)
	}
}

func TestPreparedCatalogRefreshRetainsExactAnsweredDiscoveredSkill(t *testing.T) {
	question := RefinementQuestion{
		ID: CapabilityNeedQuestionID("kubernetes-audit"), Category: RefinementCategorySkill,
		Prompt: "Which verified Skill should audit Kubernetes RBAC?", WhyNeeded: "The audit needs one exact capability.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer: RefinementAnswerSchema{
			Kind: RefinementAnswerSkillSelection, Minimum: 1, Maximum: 1,
			Options: []RefinementQuestionOption{{ID: "openseal.kubernetes", Label: "Kubernetes operations"}},
		},
		Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	initialPayload, err := json.Marshal(GenerationResponse{
		Candidate: marketingCandidate("1", capability.RiskLevelRead), UnresolvedQuestions: []RefinementQuestion{question},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{initialPayload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	changeSet, _, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, Prompt: "Audit Kubernetes RBAC",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"openseal.kubernetes": {
				ID: "openseal.kubernetes", Version: "1.1.0", Readiness: SkillReadinessReady,
			},
		}},
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-exact-skill-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	discovered := SkillSearchCandidate{
		SkillCapability: SkillCapability{
			ID: "auditing-k8s-rbac", Version: "1.0.0", SourceIdentity: "https://clawhub.ai::auditing-k8s-rbac",
			Name: "Kubernetes RBAC audit", PromptAvailable: true, Readiness: SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{{
				Requirement: "installation", Compatible: true, Evidence: "Verified compilation receipt.",
				Reference: "listing:13946960",
			}},
		},
		Origin: SkillSearchOriginCatalog, Verification: SkillSearchVerificationVerified,
		Provenance: SkillSearchProvenance{
			Registry: "https://clawhub.ai", Reference: "listing:13946960",
		},
	}
	answered, _, err := service.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, QuestionID: question.ID,
		Value: RefinementAnswerValue{SkillIDs: []string{discovered.ID}}, TrustedSkill: &discovered,
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "choose-exact-discovered-skill",
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshedCatalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"openseal.kubernetes": {
			ID: "openseal.kubernetes", Version: "1.1.0", Readiness: SkillReadinessReady,
		},
		discovered.ID: {
			ID: discovered.ID, Version: discovered.Version, SourceIdentity: discovered.SourceIdentity,
			Readiness: SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{{
				Requirement: "installation", Compatible: false,
				Evidence: "Installation is still required.", Reference: "needs_installation",
			}, {
				Requirement: "source_digest", Compatible: true,
				Evidence: "The current registry snapshot remains verified.", Reference: "sha256:current",
			}},
		},
	}}
	refreshed, err := service.RefreshPreparedCatalog(
		context.Background(), scope, changeSet.ID, answered.Revision, refreshedCatalog,
	)
	if err != nil {
		t.Fatal(err)
	}
	for surface, skill := range map[string]SkillCapability{
		"change set":         refreshed.Catalog.Skills[discovered.ID],
		"generation request": refreshed.Generation.Request.Catalog.Skills[discovered.ID],
	} {
		if skill.ID != discovered.ID || skill.Version != discovered.Version ||
			skill.SourceIdentity != discovered.SourceIdentity || skill.Readiness != SkillReadinessNeedsInstallation {
			t.Fatalf("%s exact selected Skill = %#v", surface, skill)
		}
		if reference := plannedInstallationReference(skill); reference != discovered.Provenance.Reference {
			t.Fatalf("%s exact selected Skill acquisition reference = %q", surface, reference)
		}
	}
}

func TestPreparedCatalogRefreshRetainsReviewedUpgradeOverInstalledVersion(t *testing.T) {
	const (
		skillID = "skill-slack"
		source  = "https://github.com/axiom-studio/skills::skill-slack"
	)
	question := RefinementQuestion{
		ID: CapabilityNeedQuestionID("team-messaging"), Category: RefinementCategorySkill,
		Answer: RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection},
	}
	reviewed := SkillCapability{
		ID: skillID, Version: "2.0.0", SourceIdentity: source, Readiness: SkillReadinessNeedsInstallation,
		Compatibility: []SkillCompatibility{
			{Requirement: "installation", Compatible: false, Evidence: "Upgrade required.", Reference: "listing:42"},
			{Requirement: "source_digest", Compatible: true, Evidence: "Verified exact source.", Reference: "sha256:v2"},
		},
	}
	changeSet := &ChangeSet{
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{skillID: reviewed}},
		Refinement: ChangeSetRefinement{
			Questions: []RefinementQuestion{question},
			Answers:   []RefinementAnswerEvent{{QuestionID: question.ID, Value: RefinementAnswerValue{SkillIDs: []string{skillID}}}},
		},
	}
	fresh := CapabilityCatalog{Skills: map[string]SkillCapability{skillID: {
		ID: skillID, Version: "1.0.0", SourceIdentity: source, Readiness: SkillReadinessReady,
	}}}
	if err := retainAnsweredSkillSelections(changeSet, &fresh); err != nil {
		t.Fatal(err)
	}
	got := fresh.Skills[skillID]
	if got.Version != reviewed.Version || got.Readiness != SkillReadinessNeedsInstallation || plannedInstallationReference(got) != "listing:42" {
		t.Fatalf("retained reviewed upgrade = %#v", got)
	}

	conflicting := CapabilityCatalog{Skills: map[string]SkillCapability{skillID: {
		ID: skillID, Version: "1.0.0", SourceIdentity: "https://catalog.example::different/slack", Readiness: SkillReadinessReady,
	}}}
	if err := retainAnsweredSkillSelections(changeSet, &conflicting); err == nil || !strings.Contains(err.Error(), "changed immutable source") {
		t.Fatalf("source substitution error = %v", err)
	}
}

func TestPreparedCatalogFailureTerminatesGenerationIntent(t *testing.T) {
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	prepared, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "create",
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "catalog-failure",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.FailPreparedGeneration(context.Background(), prepared.Scope, prepared.ID, prepared.Revision, "capability_discovery_failed", "Capability discovery failed")
	if err != nil || failed.Status != ChangeSetFailed || failed.Generation.FailureCode != "capability_discovery_failed" ||
		failed.Generation.LastError != "Capability discovery failed" || failed.Generation.Attempt != 1 {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
}

func TestPreparedGenerationFailureIsDurable(t *testing.T) {
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	prepared, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "create",
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "failed-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.GeneratePrepared(context.Background(), prepared.Scope, prepared.ID, prepared.Revision)
	if err == nil || failed == nil || failed.Status != ChangeSetFailed || failed.Generation.LastError == "" || failed.Revision != 2 {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	restored, getErr := service.Get(context.Background(), prepared.Scope, prepared.ID)
	if getErr != nil || restored.Status != ChangeSetFailed || restored.Generation.LastError == "" {
		t.Fatalf("restored=%#v err=%v", restored, getErr)
	}
}

func TestPreparedInvalidRunbookGenerationNeverPersistsCandidate(t *testing.T) {
	_, invalid := deterministicRunbookPayloads(t)
	generator := &repairingGenerator{
		generated: invalid, repairSequence: [][]byte{invalid, invalid},
	}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	prepared, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope:  capability.ScopeReference{Kind: "tenant", ID: "one"},
		Prompt: "Create a deterministic report publisher.",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"openseal.document": {
				ID: "openseal.document", Version: "1.0.2", Actions: []string{"render_pdf"},
				MaximumRisk: capability.RiskLevelWrite,
			},
		}},
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "failed-runbook-shape",
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := service.GeneratePrepared(context.Background(), prepared.Scope, prepared.ID, prepared.Revision)
	if err == nil || failed == nil || failed.Status != ChangeSetFailed ||
		failed.Generation.FailureCode != "schema_failed" || len(failed.Result.Candidate.Agents) != 0 ||
		failed.CandidateDigest != "" {
		t.Fatalf("failed=%#v err=%v", failed, err)
	}
	restored, getErr := service.Get(context.Background(), prepared.Scope, prepared.ID)
	if getErr != nil || len(restored.Result.Candidate.Agents) != 0 || restored.CandidateDigest != "" {
		t.Fatalf("restored invalid candidate=%#v err=%v", restored.Result.Candidate, getErr)
	}
}

func TestSchemaGenerationFailureClassificationIsActionable(t *testing.T) {
	code, message := classifyGenerationFailure(&SchemaGenerationError{RepairAttempts: 2, Diagnostic: "field candidate.team.roles expects []team.RoleSlot but received string"})
	if code != "schema_failed" || message != "We couldn't finish this proposal automatically. Your request and answers are saved; try again." || strings.Contains(message, "candidate.team.roles") {
		t.Fatalf("schema failure classification = %q / %q", code, message)
	}
}

func TestContractGenerationFailureClassificationIsActionable(t *testing.T) {
	code, message := classifyGenerationFailure(&ContractGenerationError{RepairAttempts: 1, Diagnostic: "invalid_refinement_question: answer kind is required"})
	if code != "contract_failed" || message != "We couldn't finish this proposal automatically. Your request and answers are saved; try again." || strings.Contains(message, "invalid_refinement_question") {
		t.Fatalf("contract failure classification = %q / %q", code, message)
	}
}

func TestProviderGenerationFailureClassificationIsActionableAndSecretFree(t *testing.T) {
	const secret = "provider-secret-must-not-persist"
	tests := []struct {
		name    string
		kind    ProviderFailureKind
		code    string
		message string
	}{
		{name: "not configured", kind: ProviderFailureNotConfigured, code: "provider_not_configured", message: "Choose a default LLM credential in Vault before generating a proposal."},
		{name: "invalid configuration", kind: ProviderFailureConfigurationInvalid, code: "provider_configuration_invalid", message: "The default LLM credential is incomplete. Verify its endpoint, model, and API key, then try again."},
		{name: "credentials rejected", kind: ProviderFailureCredentialsRejected, code: "provider_credentials_rejected", message: "The configured model provider rejected its credentials. Update or replace the default LLM credential, then try again."},
		{name: "request rejected", kind: ProviderFailureRequestRejected, code: "provider_request_rejected", message: "The model provider rejected the authoring request. Verify that the endpoint and model support OpenAI-compatible chat completions."},
		{name: "rate limited", kind: ProviderFailureRateLimited, code: "provider_rate_limited", message: "The model provider is rate limited. Wait briefly, then try again."},
		{name: "unavailable", kind: ProviderFailureUnavailable, code: "provider_unavailable", message: "The model provider is currently unavailable. Try again when the provider has recovered."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			code, message := classifyGenerationFailure(NewProviderFailure(test.kind, errors.New("provider failed with "+secret)))
			if code != test.code || message != test.message || strings.Contains(message, secret) {
				t.Fatalf("provider failure classification = %q / %q", code, message)
			}
		})
	}
}

func TestProviderHTTPFailureClassification(t *testing.T) {
	tests := map[int]string{400: "provider_request_rejected", 401: "provider_credentials_rejected", 403: "provider_credentials_rejected", 429: "provider_rate_limited", 503: "provider_unavailable"}
	for status, expected := range tests {
		code, _ := classifyGenerationFailure(NewProviderHTTPFailure(status))
		if code != expected {
			t.Fatalf("HTTP %d classified as %q", status, code)
		}
	}
}

func TestChangeSetRepairsGenericSkillQuestionAndPersistsCanonicalSelection(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	question := RefinementQuestion{
		ID: "report-skills", Category: RefinementCategorySkill, Prompt: "Which reporting Skills should be configured?",
		WhyNeeded: "The report requires generation and delivery capabilities.", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer: RefinementAnswerSchema{Kind: RefinementAnswerMultiSelect, Minimum: 1, Maximum: 2, Options: []RefinementQuestionOption{
			{ID: "document", Label: "Document"}, {ID: "delivery", Label: "Delivery"},
		}}, Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	repairedQuestion := question
	repairedQuestion.Answer.Kind = RefinementAnswerSkillSelection
	repairedQuestion.Answer.Options = []RefinementQuestionOption{
		{ID: "openseal.document", Label: "Document", Description: "Ready and compatible with PDF generation."},
		{ID: "openseal.delivery", Label: "Delivery", Description: "Requires a configured delivery binding."},
	}
	generated, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{question}})
	repaired, _ := json.Marshal(GenerationResponse{Candidate: candidate, UnresolvedQuestions: []RefinementQuestion{repairedQuestion}})
	generator := &repairingGenerator{generated: generated, repaired: repaired}
	compiler, _ := NewCompiler(generator)
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"reddit-research":   {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}, Readiness: SkillReadinessReady},
		"openseal.document": {ID: "openseal.document", Version: "1.0.0", Actions: []string{"render_pdf"}, Readiness: SkillReadinessReady, Compatibility: []SkillCompatibility{{Requirement: "pdf", Compatible: true, Evidence: "native renderer"}}},
		"openseal.delivery": {ID: "openseal.delivery", Version: "1.0.0", Actions: []string{"send_email"}, Readiness: SkillReadinessNeedsBinding, Compatibility: []SkillCompatibility{{Requirement: "email", Compatible: true, Evidence: "host adapter"}}},
	}}

	created, replay, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a research Team that delivers a PDF report.", Catalog: catalog,
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "canonical-skill-question",
	})
	if err != nil || replay || generator.repairs != 1 || created.Status != ChangeSetBlocked || len(created.Result.Validation) != 0 || len(created.Refinement.Questions) != 1 {
		t.Fatalf("created=%#v replay=%t repairs=%d err=%v", created, replay, generator.repairs, err)
	}
	if got := generator.lastError.Error(); !strings.Contains(got, "unresolvedQuestions[0].answer.kind") || !strings.Contains(got, "skill_selection") {
		t.Fatalf("repair diagnostic=%s", got)
	}
	persisted := created.Refinement.Questions[0]
	if persisted.Answer.Kind != RefinementAnswerSkillSelection || persisted.Answer.Options[0].ID != "openseal.document" || persisted.Answer.Options[1].ID != "openseal.delivery" {
		t.Fatalf("persisted Skill question=%#v", persisted)
	}
}

func TestGenerationRetryIsConcurrentIdempotent(t *testing.T) {
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	prepared, _, _ := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "create",
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "failed-concurrent",
	})
	failed, _ := service.GeneratePrepared(context.Background(), prepared.Scope, prepared.ID, prepared.Revision)
	request := RetryChangeSetGenerationRequest{
		Scope: failed.Scope, ChangeSetID: failed.ID, ExpectedRevision: failed.Revision, Reason: "provider recovered",
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "retry-concurrent-1",
	}
	results := make(chan *ChangeSet, 2)
	errorsFound := make(chan error, 2)
	replays := make(chan bool, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			result, replayed, err := service.RetryGeneration(context.Background(), request)
			results <- result
			replays <- replayed
			errorsFound <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errorsFound)
	close(replays)
	for err := range errorsFound {
		if err != nil {
			t.Fatalf("concurrent retry = %v", err)
		}
	}
	for result := range results {
		if result == nil || result.Status != ChangeSetEvaluating || result.Revision != failed.Revision+1 || len(result.Generation.Retries) != 1 {
			t.Fatalf("concurrent retry result = %#v", result)
		}
	}
	replayCount := 0
	for replayed := range replays {
		if replayed {
			replayCount++
		}
	}
	if replayCount != 1 {
		t.Fatalf("concurrent retry replay count = %d", replayCount)
	}
	changed := request
	changed.Reason = "different"
	if _, _, err := service.RetryGeneration(context.Background(), changed); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("changed retry key = %v", err)
	}
	changed = request
	changed.ExpectedRevision++
	if _, _, err := service.RetryGeneration(context.Background(), changed); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("changed retry revision = %v", err)
	}
}

func TestGenerationRetryRepairsValidationOnlyBlockedDraft(t *testing.T) {
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	prepared, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "create",
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "validation-retry-create",
	})
	if err != nil {
		t.Fatal(err)
	}
	blocked := cloneChangeSet(prepared)
	blocked.Status = ChangeSetBlocked
	blocked.Revision++
	blocked.Result.Validation = []ValidationIssue{issue("conversationEndpoints", "reactive_conversation_endpoint_missing", "old validator")}
	blocked, err = store.UpdateChangeSet(context.Background(), blocked, prepared.Revision)
	if err != nil {
		t.Fatal(err)
	}
	request := RetryChangeSetGenerationRequest{Scope: blocked.Scope, ChangeSetID: blocked.ID,
		ExpectedRevision: blocked.Revision, Reason: "validator corrected", Actor: blocked.Actor,
		IdempotencyKey: "validation-retry"}
	retried, replayed, err := service.RetryGeneration(context.Background(), request)
	if err != nil || replayed || retried.Status != ChangeSetEvaluating || retried.Revision != blocked.Revision+1 {
		t.Fatalf("validation retry = %#v, replayed=%t, err=%v", retried, replayed, err)
	}
	if len(retried.Lifecycle) == 0 || retried.Lifecycle[len(retried.Lifecycle)-1].From != ChangeSetBlocked {
		t.Fatalf("validation retry lifecycle = %#v", retried.Lifecycle)
	}

	questions := cloneChangeSet(blocked)
	questions.ID = "blocked-with-question"
	questions.Result.UnresolvedQuestions = []RefinementQuestion{{ID: "admission"}}
	if _, _, err := store.CreateChangeSet(context.Background(), questions, "question-create", "question-digest"); err != nil {
		t.Fatal(err)
	}
	request.ChangeSetID, request.ExpectedRevision, request.IdempotencyKey = questions.ID, questions.Revision, "question-retry"
	if _, _, err := service.RetryGeneration(context.Background(), request); !errors.Is(err, ErrChangeSetTransition) {
		t.Fatalf("unanswered question retry = %v", err)
	}
}

func TestAtomicMemoryApplyIsIdempotentAndConcurrent(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}}, Placement: ChangeSetPlacement{TeamDeploymentID: "marketing-live", AgentDeploymentIDs: map[string]string{"community-researcher": "researcher-live"}, Environment: "production"}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	ready, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow"})
	if err != nil || ready.Status != ChangeSetReady {
		t.Fatalf("ready=%#v err=%v", ready, err)
	}
	req := ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Activate approved workforce", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply"}
	var wg sync.WaitGroup
	wg.Add(2)
	results := make(chan *ChangeSet, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			value, _, err := service.Apply(context.Background(), req)
			results <- value
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var receipt string
	for value := range results {
		if value.Status != ChangeSetApplied || value.ApplyReceipt == nil || value.ApplyReceipt.Activation != WorkforceActivationActive || len(value.ApplyReceipt.Resources) < 4 {
			t.Fatalf("applied=%#v", value)
		}
		if value.ApplyReceipt.Reason != req.Reason || value.Lifecycle[len(value.Lifecycle)-1].Reason != req.Reason {
			t.Fatalf("apply audit=%#v", value)
		}
		if receipt != "" && receipt != value.ApplyReceipt.ID {
			t.Fatalf("receipts differ")
		}
		receipt = value.ApplyReceipt.ID
	}
	changedReason := req
	changedReason.Reason = "Different activation reason"
	if _, _, err := service.Apply(context.Background(), changedReason); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("changed reason replay=%v", err)
	}
	if len(store.definitions) != 2 || len(store.deployments) != 2 {
		t.Fatalf("definitions=%d deployments=%d", len(store.definitions), len(store.deployments))
	}
	if _, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: req.Reason, Actor: req.Actor, IdempotencyKey: "other"}); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("different retry=%v", err)
	}
}

func TestAtomicMemoryApplyCarriesInactiveCommitmentIntoReceipt(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{
		Candidate:   marketingCandidate("1", capability.RiskLevelRead),
		Commitments: PromptCommitments{Activation: ActivationCommitmentInactive},
	})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, Prompt: "Create this workforce and do not activate it.",
		Catalog:   CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
		Placement: ChangeSetPlacement{TeamDeploymentID: "marketing-live", AgentDeploymentIDs: map[string]string{"community-researcher": "researcher-live"}, Environment: "production"},
		Actor:     ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-inactive",
	})
	if err != nil || created.Result.Candidate.Activation != WorkforceActivationInactive {
		t.Fatalf("inactive candidate=%#v err=%v", created, err)
	}
	ready, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow-inactive"})
	if err != nil {
		t.Fatal(err)
	}
	applied, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Create inactive for review", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply-inactive"})
	if err != nil || applied.ApplyReceipt == nil || applied.ApplyReceipt.Activation != WorkforceActivationInactive || applied.ApplyReceipt.CandidateDigest != created.CandidateDigest {
		t.Fatalf("inactive receipt=%#v err=%v", applied, err)
	}
}

func TestPrepareActivationReusesAppliedResourcesAndGovernedApply(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{
		Candidate:   marketingCandidate("1", capability.RiskLevelRead),
		Commitments: PromptCommitments{Activation: ActivationCommitmentInactive},
	})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	catalog := CapabilityCatalog{
		Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}},
		AgentCredentialRequirements: []AgentCredentialRequirement{{
			BindingKey: "MODEL_PROVIDER", DisplayName: "Model provider",
			Prompt: "Choose the model provider this Agent may use.", RequiredForActivation: true,
		}},
	}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, Prompt: "Create this workforce and keep it inactive.", Catalog: catalog,
		Placement: ChangeSetPlacement{
			TeamDeploymentID: "marketing-live", AgentDeploymentIDs: map[string]string{"community-researcher": "researcher-live"},
			Environment: "production",
		},
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-inactive-for-activation",
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{
		Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest,
		Allowed: true, Actor: ChangeSetActor{Type: "policy_evaluator", ID: "policy"}, IdempotencyKey: "allow-inactive-for-activation",
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{
		Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest,
		Reason: "Create reviewed resources", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply-inactive-for-activation",
	})
	if err != nil || applied.ApplyReceipt.Activation != WorkforceActivationInactive {
		t.Fatalf("inactive apply=%#v err=%v", applied, err)
	}

	activation, replayed, err := service.PrepareActivation(context.Background(), PrepareChangeSetActivationRequest{
		Scope: scope, ChangeSetID: applied.ID, ExpectedRevision: applied.Revision, CandidateDigest: applied.CandidateDigest,
		Catalog: catalog, Reason: "Start the reviewed workforce", Actor: ChangeSetActor{Type: "user", ID: "7"},
		IdempotencyKey: "prepare-activation",
	})
	if err != nil || replayed || activation.ParentID != applied.ID || activation.Result.Candidate.Activation != WorkforceActivationActive ||
		activation.Status != ChangeSetReview || activation.Placement.TeamExpectedRevision < 1 {
		t.Fatalf("activation=%#v replayed=%v err=%v", activation, replayed, err)
	}
	// Assert the agent count before indexing. Reading Agents[0] inside the
	// compound condition above would panic on an empty slice rather than fail,
	// taking the rest of the package's tests down with it — the exact shape
	// that made the equivalent assertion in internal/server panic instead of
	// reporting a stale key.
	if len(created.Result.Candidate.Agents) != 1 {
		t.Fatalf("created candidate agents = %d, want 1", len(created.Result.Candidate.Agents))
	}
	agentID := created.Result.Candidate.Agents[0].ID
	if credentials := activation.RequiredCredentials[agentID]; len(credentials) != 1 || credentials[0] != "MODEL_PROVIDER" {
		t.Fatalf("required credentials for Agent %q = %v, want [MODEL_PROVIDER]; all = %v",
			agentID, credentials, activation.RequiredCredentials)
	}
	if revision := activation.Placement.AgentExpectedRevisions[agentID]; revision < 1 {
		t.Fatalf("expected revision for Agent %q = %d, want >= 1", agentID, revision)
	}
	if replay, wasReplayed, replayErr := service.PrepareActivation(context.Background(), PrepareChangeSetActivationRequest{
		Scope: scope, ChangeSetID: applied.ID, ExpectedRevision: applied.Revision, CandidateDigest: applied.CandidateDigest,
		Catalog: catalog, Reason: "Start the reviewed workforce", Actor: ChangeSetActor{Type: "user", ID: "7"},
		IdempotencyKey: "prepare-activation",
	}); replayErr != nil || !wasReplayed || replay.ID != activation.ID {
		t.Fatalf("activation replay=%#v replayed=%v err=%v", replay, wasReplayed, replayErr)
	}

	evaluated, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{
		Scope: scope, ChangeSetID: activation.ID, ExpectedRevision: activation.Revision, CandidateDigest: activation.CandidateDigest,
		Allowed: true, Actor: ChangeSetActor{Type: "policy_evaluator", ID: "policy"}, IdempotencyKey: "allow-activation",
	})
	if err != nil || evaluated.Status != ChangeSetReady {
		t.Fatalf("activation evaluation=%#v err=%v", evaluated, err)
	}
	if _, _, err = service.Apply(context.Background(), ApplyChangeSetRequest{
		Scope: scope, ChangeSetID: evaluated.ID, ExpectedRevision: evaluated.Revision, CandidateDigest: evaluated.CandidateDigest,
		Reason: "Activate reviewed resources", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply-without-provider",
	}); err == nil || !strings.Contains(err.Error(), "MODEL_PROVIDER") {
		t.Fatalf("missing provider apply err=%v", err)
	}

	placed := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		created.Result.Candidate.Agents[0].ID: {
			"MODEL_PROVIDER": {Kind: "managed-secret", ID: "29"},
		},
	}}
	updated, _, err := service.UpdatePlacement(context.Background(), UpdateChangeSetPlacementRequest{
		Scope: scope, ChangeSetID: evaluated.ID, ExpectedRevision: evaluated.Revision, Placement: placed,
		Reason: "Select authorized model provider", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "place-provider",
	})
	if err != nil || updated.Status != ChangeSetReview ||
		updated.Placement.AgentDeploymentIDs[created.Result.Candidate.Agents[0].ID] != activation.Placement.AgentDeploymentIDs[created.Result.Candidate.Agents[0].ID] ||
		updated.Placement.AgentExpectedRevisions[created.Result.Candidate.Agents[0].ID] != activation.Placement.AgentExpectedRevisions[created.Result.Candidate.Agents[0].ID] ||
		updated.Placement.TeamDeploymentID != activation.Placement.TeamDeploymentID ||
		updated.Placement.TeamExpectedRevision != activation.Placement.TeamExpectedRevision {
		t.Fatalf("placement=%#v err=%v", updated, err)
	}
	retargeted := clonePlacement(updated.Placement)
	retargeted.AgentDeploymentIDs[created.Result.Candidate.Agents[0].ID] = "different-agent"
	if _, _, err = service.UpdatePlacement(context.Background(), UpdateChangeSetPlacementRequest{
		Scope: scope, ChangeSetID: updated.ID, ExpectedRevision: updated.Revision, Placement: retargeted,
		Reason: "Retarget activation", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "retarget-activation",
	}); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("retargeted activation err=%v", err)
	}
	reviewed, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{
		Scope: scope, ChangeSetID: updated.ID, ExpectedRevision: updated.Revision, CandidateDigest: updated.CandidateDigest,
		Allowed: true, Actor: ChangeSetActor{Type: "policy_evaluator", ID: "policy"}, IdempotencyKey: "allow-placed-activation",
	})
	if err != nil {
		t.Fatal(err)
	}
	activated, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{
		Scope: scope, ChangeSetID: reviewed.ID, ExpectedRevision: reviewed.Revision, CandidateDigest: reviewed.CandidateDigest,
		Reason: "Activate reviewed resources", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply-activation",
	})
	if err != nil || activated.ApplyReceipt.Activation != WorkforceActivationActive {
		t.Fatalf("activated=%#v err=%v", activated, err)
	}
}

func TestActivationConversationRoutingIsTypedAndResourceIdentityStaysImmutable(t *testing.T) {
	candidate := WorkforceCandidate{
		Activation: WorkforceActivationActive,
		ConversationEndpoints: []ConversationEndpointBlueprint{{
			ID: "support", Name: "Support channel", Address: "C-support",
		}},
	}
	current := ChangeSetPlacement{ConversationEndpoints: map[string]ConversationEndpointPlacement{
		"support": {ID: "conversation-endpoint:support", ExpectedRevision: 3},
	}}
	issues := conversationRoutingValidation(&candidate, current)
	if len(issues) != 1 || issues[0].Code != "conversation_routing_installation_required" {
		t.Fatalf("routing issues = %#v", issues)
	}
	service := &ChangeSetService{}
	readiness, err := service.validateReadiness(context.Background(), &ChangeSet{
		Result: CompileResult{Candidate: candidate}, Placement: current,
	}, true)
	if err != nil || len(readiness) != 1 || readiness[0].Code != "conversation_routing_installation_required" {
		t.Fatalf("final readiness = %#v err=%v", readiness, err)
	}

	requested := ChangeSetPlacement{ConversationEndpoints: map[string]ConversationEndpointPlacement{
		"support": {
			ID: "conversation-endpoint:support", ExpectedRevision: 3,
			InstallationID: "T-support", ApplicationID: "A-support", Address: "C-support",
			Configuration: map[string]interface{}{"threading": "thread"},
		},
	}}
	updated, err := activationPlacementUpdate(current, requested)
	if err != nil {
		t.Fatal(err)
	}
	placed := updated.ConversationEndpoints["support"]
	if placed.ID != "conversation-endpoint:support" || placed.ExpectedRevision != 3 ||
		placed.InstallationID != "T-support" || placed.ApplicationID != "A-support" || placed.Address != "C-support" {
		t.Fatalf("updated routing placement = %#v", placed)
	}
	if issues = conversationRoutingValidation(&candidate, updated); len(issues) != 0 {
		t.Fatalf("configured routing issues = %#v", issues)
	}

	retargeted := clonePlacement(requested)
	value := retargeted.ConversationEndpoints["support"]
	value.ID = "conversation-endpoint:other"
	retargeted.ConversationEndpoints["support"] = value
	if _, err = activationPlacementUpdate(current, retargeted); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("retargeted endpoint err = %v", err)
	}
}

func TestSlackConversationRoutingUsesInstallationMembershipWithoutAddress(t *testing.T) {
	candidate := WorkforceCandidate{
		Activation: WorkforceActivationActive,
		ConversationEndpoints: []ConversationEndpointBlueprint{{
			ID: "slack", Name: "Slack conversations", SkillID: "skill-slack",
			Purposes: []ConversationEndpointPurpose{ConversationEndpointPurposeConversation},
		}},
	}
	placement := ChangeSetPlacement{ConversationEndpoints: map[string]ConversationEndpointPlacement{
		"slack": {ID: "conversation-endpoint:slack", InstallationID: "T-workspace"},
	}}
	if issues := conversationRoutingValidation(&candidate, placement); len(issues) != 0 {
		t.Fatalf("installation-wide Slack routing issues = %#v", issues)
	}

	candidate.ConversationEndpoints[0].Purposes = append(
		candidate.ConversationEndpoints[0].Purposes,
		ConversationEndpointPurposeApprovals,
	)
	issues := conversationRoutingValidation(&candidate, placement)
	if len(issues) != 1 || issues[0].Code != "conversation_routing_address_required" {
		t.Fatalf("approval destination issues = %#v", issues)
	}
}

func TestEffectiveChangeSetActivationIntentMigratesTypedCommitmentWithoutPromptParsing(t *testing.T) {
	legacy := &ChangeSet{Result: CompileResult{Commitments: PromptCommitments{Activation: ActivationCommitmentInactive}}}
	if intent, err := EffectiveChangeSetActivationIntent(legacy); err != nil || intent != WorkforceActivationInactive {
		t.Fatalf("legacy inactive intent=%q err=%v", intent, err)
	}
	legacy.Result.Commitments.Activation = ""
	if intent, err := EffectiveChangeSetActivationIntent(legacy); err != nil || intent != WorkforceActivationActive {
		t.Fatalf("legacy active intent=%q err=%v", intent, err)
	}
	legacy.Result.Commitments.Activation = ActivationCommitmentInactive
	legacy.Result.Candidate.Activation = WorkforceActivationActive
	if _, err := EffectiveChangeSetActivationIntent(legacy); err == nil {
		t.Fatal("conflicting candidate and commitment activation was accepted")
	}
}

func TestPlacementAwareMissingRequirementsResolvesConfiguredSkillBinding(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "tenant/one/operator",
		SkillRequirements: []agent.SkillRequirement{{
			SkillID: "openseal.kubernetes",
		}},
	}}}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"openseal.kubernetes": {
			ID:        "openseal.kubernetes",
			Readiness: SkillReadinessNeedsBinding,
			BindingConfigSchema: map[string]interface{}{
				"type":     "object",
				"required": []interface{}{"clusterId"},
				"properties": map[string]interface{}{
					"clusterId": map[string]interface{}{"type": "integer", "minimum": 1},
				},
			},
		},
	}}
	if missing := placementAwareMissingRequirements(&candidate, catalog, ChangeSetPlacement{}); len(missing) != 1 || missing[0].Kind != "skill_binding" {
		t.Fatalf("unconfigured missing requirements = %#v", missing)
	}
	invalidPlacement := ChangeSetPlacement{BindingConfigs: map[string]map[string]map[string]interface{}{
		"tenant/one/operator": {
			"openseal.kubernetes": {"clusterId": 0},
		},
	}}
	if missing := placementAwareMissingRequirements(&candidate, catalog, invalidPlacement); len(missing) != 1 || missing[0].Kind != "skill_binding" {
		t.Fatalf("invalid configuration resolved requirements = %#v", missing)
	}
	placement := ChangeSetPlacement{BindingConfigs: map[string]map[string]map[string]interface{}{
		"tenant/one/operator": {
			"openseal.kubernetes": {"clusterId": 1},
		},
	}}
	if missing := placementAwareMissingRequirements(&candidate, catalog, placement); len(missing) != 0 {
		t.Fatalf("configured missing requirements = %#v", missing)
	}
}

func TestPlacementAwareMissingRequirementsRequiresExactNamedSkillCredential(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "tenant/one/security-reviewer",
		SkillRequirements: []agent.SkillRequirement{{
			SkillID: "security-scorecard", RequiredActions: []string{"score"},
		}},
	}}}
	catalog := CapabilityCatalog{
		Skills: map[string]SkillCapability{
			"security-scorecard": {
				ID:        "security-scorecard",
				Readiness: SkillReadinessNeedsBinding,
				Actions:   []string{"score"},
				Credentials: []SkillCredential{{
					Name: "TOOLWEB_API_KEY", Kind: "environment-secret", Actions: []string{"score"},
				}},
			},
		},
		AvailableCredentials: map[string]bool{"TOOLWEB_API_KEY": true},
	}
	if missing := placementAwareMissingRequirements(&candidate, catalog, ChangeSetPlacement{}); len(missing) != 1 ||
		missing[0].Kind != "skill_binding" {
		t.Fatalf("unplaced exact credential missing requirements = %#v", missing)
	}
	wrongPlacement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"tenant/one/security-reviewer": {
			"environment-secret": {Kind: "environment-secret", ID: "credential://toolweb"},
		},
	}}
	if missing := placementAwareMissingRequirements(&candidate, catalog, wrongPlacement); len(missing) != 1 ||
		missing[0].Kind != "skill_binding" {
		t.Fatalf("kind-only placement resolved exact credential = %#v", missing)
	}
	exactPlacement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		"tenant/one/security-reviewer": {
			"TOOLWEB_API_KEY": {Kind: "environment-secret", ID: "credential://toolweb"},
		},
	}}
	if missing := placementAwareMissingRequirements(&candidate, catalog, exactPlacement); len(missing) != 0 {
		t.Fatalf("exact credential placement missing requirements = %#v", missing)
	}
}

func TestPlacementAwareQuestionsDropSatisfiedCredentialPrompt(t *testing.T) {
	credential := RefinementQuestion{
		ID: "skill-browser-binding", Category: RefinementCategoryCredential,
		Prompt: "Select an authorized Browser credential.", WhyNeeded: "Login requires it.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate, RefinementBlocksApply},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference},
	}
	policy := RefinementQuestion{
		ID: "write-policy", Category: RefinementCategoryApproval,
		Prompt: "Who approves writes?", WhyNeeded: "Writes are governed.",
		Blocking: []RefinementBlockingScope{RefinementBlocksApply},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerText},
	}

	questions := placementAwareUnresolvedQuestions([]RefinementQuestion{credential, policy}, nil)
	if len(questions) != 1 || questions[0].ID != policy.ID {
		t.Fatalf("satisfied placement questions = %#v", questions)
	}
	questions = placementAwareUnresolvedQuestions([]RefinementQuestion{credential, policy}, []MissingRequirement{{
		Kind: "skill_binding", ID: "skill-browser", RequiredBy: "agent:researcher",
	}})
	if len(questions) != 2 {
		t.Fatalf("missing placement questions = %#v", questions)
	}
}

func TestSourceMonitorUsesAssignedAgentSkillBindingPlacement(t *testing.T) {
	const agentID = "tenant/one/researcher"
	candidate := WorkforceCandidate{
		Agents: []*agent.AgentDefinition{{
			ID: agentID,
			SkillRequirements: []agent.SkillRequirement{{
				SkillID: "skill-browser", RequiredActions: []string{"browser-open", "browser-fill-secret"},
			}},
		}},
		Project: &ProjectBlueprint{
			ID: "research",
			SourceMonitors: []ProjectSourceMonitorBlueprint{{
				ID: "reddit", AssignedAgentDefinitionID: agentID, SkillID: "skill-browser",
			}},
		},
	}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"skill-browser": {
			ID: "skill-browser", Readiness: SkillReadinessNeedsBinding,
			Actions: []string{"browser-open", "browser-fill-secret"},
			Credentials: []SkillCredential{
				{Name: "username", Kind: "http_basic_auth", Actions: []string{"browser-fill-secret"}},
				{Name: "password", Kind: "http_basic_auth", Actions: []string{"browser-fill-secret"}},
			},
		},
	}}
	requirement := MissingRequirement{
		Kind: "skill_binding", ID: "skill-browser", RequiredBy: "project:research/monitor:reddit",
	}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{
		agentID: {
			"username": {Kind: "http_basic_auth", ID: "credential://reddit-fixture.username"},
			"password": {Kind: "http_basic_auth", ID: "credential://reddit-fixture.password"},
		},
	}}
	if !skillBindingPlacementPresent(&candidate, requirement, catalog, placement) {
		t.Fatal("source monitor ignored its assigned Agent credential placement")
	}
	delete(placement.CredentialReferences[agentID], "password")
	if skillBindingPlacementPresent(&candidate, requirement, catalog, placement) {
		t.Fatal("source monitor accepted an incomplete assigned Agent credential placement")
	}
}

func TestPlacementAwareMissingRequirementsResolvesOnlyExactPlannedSkillInstallation(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "tenant/one/researcher",
		SkillRequirements: []agent.SkillRequirement{{
			SkillID: "community.research",
		}},
	}}}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"community.research": {
			ID: "community.research", Version: "2.1.0",
			SourceIdentity: "registry.example::community/research",
			Readiness:      SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{{
				Requirement: "installation", Compatible: false,
				Evidence: "Verified immutable build is available.", Reference: "listing:42",
			}, {
				Requirement: "source_digest", Compatible: true,
				Evidence: "Pinned source artifact digest.", Reference: "sha256:research",
			}},
		},
	}}
	if missing := placementAwareMissingRequirements(&candidate, catalog, ChangeSetPlacement{}); len(missing) != 1 || missing[0].Kind != "skill_installation" {
		t.Fatalf("unplanned requirements = %#v", missing)
	}
	placement := ChangeSetPlacement{PlannedSkillInstallations: []SkillInstallationIntent{{
		SkillID: "community.research", Version: "2.1.0",
		SourceIdentity: "registry.example::community/research", SourceDigest: "sha256:research", Reference: "listing:42",
	}}}
	if err := validatePlannedSkillInstallations(placement, catalog); err != nil {
		t.Fatalf("validate exact installation plan: %v", err)
	}
	if missing := placementAwareMissingRequirements(&candidate, catalog, placement); len(missing) != 1 || missing[0].Kind != "skill_installation" {
		t.Fatalf("installation without consuming Agent placement resolved requirements = %#v", missing)
	}
	placement.SkillSourceIdentities = map[string]map[string]string{
		"tenant/one/researcher": {"community.research": "registry.example::community/research"},
	}
	placement.SkillSourceVersions = map[string]map[string]string{
		"tenant/one/researcher": {"community.research": "2.1.0"},
	}
	placement.SkillRuntimeIdentities = map[string]map[string]capability.SkillIdentity{
		"tenant/one/researcher": {
			"community.research": capability.NewSkillIdentity(
				"community-research-runtime", "2.1.0", "registry.example::community/research",
			),
		},
	}
	if missing := placementAwareMissingRequirements(&candidate, catalog, placement); len(missing) != 0 {
		t.Fatalf("reviewed installation remained missing = %#v", missing)
	}
	compiledVersion := "2.1.0+source.0123456789ab.origin.abcdef012345.trust.9876543210ab"
	placement.SkillSourceVersions["tenant/one/researcher"]["community.research"] = compiledVersion
	placement.SkillRuntimeIdentities["tenant/one/researcher"]["community.research"] = capability.NewSkillIdentity(
		"community-research-runtime", compiledVersion, "registry.example::community/research",
	)
	if missing := placementAwareMissingRequirements(&candidate, catalog, placement); len(missing) != 0 {
		t.Fatalf("reviewed compiled installation remained missing = %#v", missing)
	}
	placement.SkillSourceVersions["tenant/one/researcher"]["community.research"] = "2.2.0+source.0123456789ab"
	placement.SkillRuntimeIdentities["tenant/one/researcher"]["community.research"] = capability.NewSkillIdentity(
		"community-research-runtime", "2.2.0+source.0123456789ab", "registry.example::community/research",
	)
	if missing := placementAwareMissingRequirements(&candidate, catalog, placement); len(missing) != 1 || missing[0].Kind != "skill_installation" {
		t.Fatalf("unreviewed compiled version resolved requirements = %#v", missing)
	}
	placement.SkillSourceVersions["tenant/one/researcher"]["community.research"] = compiledVersion
	placement.SkillRuntimeIdentities["tenant/one/researcher"]["community.research"] = capability.NewSkillIdentity(
		"community-research-runtime", compiledVersion, "registry.example::community/research",
	)
	placement.PlannedSkillInstallations[0].Reference = "listing:forged"
	if err := validatePlannedSkillInstallations(placement, catalog); err == nil {
		t.Fatal("forged installation reference was accepted")
	}
	if missing := placementAwareMissingRequirements(&candidate, catalog, placement); len(missing) != 1 || missing[0].Kind != "skill_installation" {
		t.Fatalf("forged installation resolved requirements = %#v", missing)
	}
	placement.PlannedSkillInstallations[0].Reference = "listing:42"
	placement.PlannedSkillInstallations[0].SourceDigest = "sha256:forged"
	if err := validatePlannedSkillInstallations(placement, catalog); err == nil {
		t.Fatal("forged source digest was accepted")
	}
}

func TestPlacementAwareMissingRequirementsResolvesPromptDeliveredByPlannedSkillInstallation(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "tenant/one/auditor",
		SkillRequirements: []agent.SkillRequirement{{
			SkillID: "community.audit", VersionConstraint: "1.0.0", PromptRequired: true,
		}},
	}}}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"community.audit": {
			ID: "community.audit", Version: "1.0.0",
			SourceIdentity: "registry.example::community/audit",
			Readiness:      SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{{
				Requirement: "installation", Compatible: false,
				Evidence: "Verified immutable build is available.", Reference: "listing:84",
			}, {
				Requirement: "source_digest", Compatible: true,
				Evidence: "Pinned source artifact digest.", Reference: "sha256:audit",
			}},
		},
	}}
	placement := ChangeSetPlacement{
		SkillSourceIdentities: map[string]map[string]string{
			"tenant/one/auditor": {"community.audit": "registry.example::community/audit"},
		},
		SkillSourceVersions: map[string]map[string]string{
			"tenant/one/auditor": {"community.audit": "1.0.0"},
		},
		SkillRuntimeIdentities: map[string]map[string]capability.SkillIdentity{
			"tenant/one/auditor": {
				"community.audit": capability.NewSkillIdentity(
					"community-audit-runtime", "1.0.0", "registry.example::community/audit",
				),
			},
		},
		PlannedSkillInstallations: []SkillInstallationIntent{{
			SkillID: "community.audit", Version: "1.0.0",
			SourceIdentity: "registry.example::community/audit", SourceDigest: "sha256:audit", Reference: "listing:84",
		}},
	}
	if missing := missingRequirements(&candidate, catalog); len(missing) != 2 {
		t.Fatalf("unplaced prompt Skill requirements = %#v", missing)
	}
	if missing := placementAwareMissingRequirements(&candidate, catalog, placement); len(missing) != 0 {
		t.Fatalf("planned prompt Skill installation remained missing = %#v", missing)
	}
}

func TestAtomicMemoryApplyUsesSafeDefaultPlacement(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, _ := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create"})
	ready, _, _ := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: 1, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow"})
	applied, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: 2, CandidateDigest: ready.CandidateDigest, Reason: "Activate approved workforce", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply"})
	if err != nil || applied.Placement.Environment != "default" || len(store.definitions) != 2 || len(store.deployments) != 2 {
		t.Fatalf("applied=%#v err=%v definitions=%d deployments=%d", applied, err, len(store.definitions), len(store.deployments))
	}
}

func TestAtomicMemoryApplyReturnsPermanentResourceConflictWithoutRecursing(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	candidate := marketingCandidate("2", capability.RiskLevelRead)
	agentID := candidate.Agents[0].ID
	value := &ChangeSet{
		ID: "conflicting", Scope: scope, Mode: ModeCreate, Prompt: "create", PromptDigest: "prompt", CandidateDigest: "candidate",
		Result:    CompileResult{Candidate: candidate, Valid: true},
		Placement: ChangeSetPlacement{TeamDeploymentID: "team-live", AgentDeploymentIDs: map[string]string{agentID: "agent-live"}, Environment: "development"},
		Status:    ChangeSetReady, Actor: ChangeSetActor{Type: "user", ID: "7"}, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create-conflict", "digest-conflict"); err != nil {
		t.Fatal(err)
	}
	store.deployments[changeSetKey(scope, "agent-live")] = AppliedResourceReference{Kind: "agent_deployment", ID: "agent-live", Revision: 1}
	service := &ChangeSetService{store: store, now: time.Now}
	started := time.Now()
	_, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{
		Scope: scope, ChangeSetID: value.ID, ExpectedRevision: value.Revision, CandidateDigest: value.CandidateDigest,
		Reason: "Activate", Actor: value.Actor, IdempotencyKey: "apply-conflict",
	})
	if !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("conflict=%v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("permanent conflict took %s", elapsed)
	}
}

func TestAtomicMemoryApplyCreatesResourcesForRecoveredUnappliedAmendment(t *testing.T) {
	candidate := GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)}
	payload, _ := json.Marshal(candidate)
	compiler, _ := NewCompiler(recoverExistingChangeSetGenerator{initial: payload})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}},
	}}
	placement := ChangeSetPlacement{TeamDeploymentID: "marketing-live", AgentDeploymentIDs: map[string]string{"community-researcher": "researcher-live"}, Environment: "development"}
	parent, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "create", Catalog: catalog, Placement: placement, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create"})
	if err != nil {
		t.Fatal(err)
	}
	evaluated, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{
		Scope: scope, ChangeSetID: parent.ID, ExpectedRevision: parent.Revision, CandidateDigest: parent.CandidateDigest, Allowed: true,
		ApprovalRequirements: []ChangeSetApprovalRequirement{{PolicyID: "activation", Role: "tenant:admin", Count: 1}},
		Actor:                ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "evaluate-parent",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejected, _, err := service.ResolveApproval(context.Background(), ResolveChangeSetApprovalRequest{
		Scope: scope, ChangeSetID: parent.ID, ExpectedRevision: evaluated.Revision, EvaluationID: evaluated.Evaluations[0].ID,
		PolicyID: "activation", Role: "tenant:admin", Approved: false, Reason: "revise", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "reject",
	})
	if err != nil || rejected.Status != ChangeSetRejected {
		t.Fatalf("rejected=%#v err=%v", rejected, err)
	}
	recovered, _, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, ParentID: rejected.ID, Prompt: "recover unchanged", Catalog: catalog, Placement: rejected.Placement,
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "recover",
	})
	if err != nil || recovered.Status != ChangeSetReview || recovered.Mode != ModeAmend || len(recovered.Placement.AgentExpectedRevisions) != 0 || recovered.Placement.TeamExpectedRevision != 0 || recovered.Placement.ProjectExpectedRevision != 0 {
		t.Fatalf("recovered=%#v err=%v", recovered, err)
	}
	for key, objective := range recovered.Placement.Objectives {
		if objective.ExpectedRevision != 0 {
			t.Fatalf("unapplied parent objective %q inherited revision %d", key, objective.ExpectedRevision)
		}
	}
	ready, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{
		Scope: scope, ChangeSetID: recovered.ID, ExpectedRevision: recovered.Revision, CandidateDigest: recovered.CandidateDigest,
		Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "evaluate-recovery",
	})
	if err != nil {
		t.Fatal(err)
	}
	applied, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{
		Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest,
		Reason: "Activate recovered workforce", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply-recovery",
	})
	if err != nil || applied.Status != ChangeSetApplied || len(store.deployments) != 2 {
		t.Fatalf("applied=%#v deployments=%#v err=%v", applied, store.deployments, err)
	}
	for _, deployment := range store.deployments {
		if deployment.Revision != 1 {
			t.Fatalf("recovered deployment=%#v", deployment)
		}
	}
}

func TestPreparedCatalogRefreshInheritsAuditedAnswersOnlyForUnchangedPrompt(t *testing.T) {
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	prompt := "Monitor the reviewed community"
	question := RefinementQuestion{
		ID: CapabilitySourceScopeQuestionID("community"), Category: RefinementCategoryScope,
		Prompt: "Which community?", WhyNeeded: "Scope must remain explicit.", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer: RefinementAnswerSchema{Kind: RefinementAnswerStringList, Minimum: 1, Maximum: 1}, Priority: 10,
		Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	answer := RefinementAnswerEvent{
		ID: "answer-1", QuestionID: question.ID, QuestionRevision: 3, Value: RefinementAnswerValue{Items: []string{"LocalLLaMA"}},
		Source: RefinementAnswerSourceUser, Actor: ChangeSetActor{Type: "user", ID: "7"}, AnsweredAt: time.Now().UTC(),
	}
	parent := &ChangeSet{
		ID: "parent", Scope: scope, Prompt: prompt, PromptDigest: digestString(prompt), Status: ChangeSetBlocked, Revision: 3,
		Actor: ChangeSetActor{Type: "user", ID: "7"}, Result: CompileResult{Candidate: capabilityNeedCandidate()},
		Refinement: ChangeSetRefinement{Questions: []RefinementQuestion{question}, Answers: []RefinementAnswerEvent{answer}},
	}
	store := NewMemoryChangeSetStore()
	if _, _, err := store.CreateChangeSet(context.Background(), parent, "parent-key", "parent-digest"); err != nil {
		t.Fatal(err)
	}
	compiler, _ := NewCompiler(staticGenerator{payload: capabilityNeedPayload(t)})
	service, _ := NewChangeSetService(compiler, store)
	catalog := capabilityNeedCatalog(false, "openseal.source")

	refreshed, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: scope, ParentID: parent.ID, Prompt: prompt, Catalog: catalog,
		Actor: parent.Actor, IdempotencyKey: "refresh-same-prompt",
	})
	if err != nil || refreshed.Generation.Request.Refinement == nil || len(refreshed.Generation.Request.Refinement.Answers) != 1 ||
		len(refreshed.Refinement.Answers) != 1 || refreshed.Refinement.Answers[0].Value.Items[0] != "LocalLLaMA" {
		t.Fatalf("same-prompt refresh = %#v, err = %v", refreshed, err)
	}
	changed, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: scope, ParentID: parent.ID, Prompt: "Monitor a different community", Catalog: catalog,
		Actor: parent.Actor, IdempotencyKey: "refresh-changed-prompt",
	})
	if err != nil || changed.Generation.Request.Refinement != nil || len(changed.Refinement.Answers) != 0 {
		t.Fatalf("changed-prompt refresh inherited stale answers: %#v, err = %v", changed, err)
	}
}

func TestAmendmentPlacementRevisionsComeOnlyFromAppliedReceipt(t *testing.T) {
	const (
		agentDefinitionID = "tenant/one/researcher"
	)
	objectiveKey := WorkforceObjectiveKey("agent", agentDefinitionID, "monitor")
	parent := &ChangeSet{Placement: ChangeSetPlacement{
		AgentDeploymentIDs: map[string]string{agentDefinitionID: "agent-live"},
		TeamDeploymentID:   "team-live",
		ProjectID:          "project-live",
		Environment:        "production",
		Objectives:         map[string]ObjectivePlacement{objectiveKey: {ID: "objective-live"}},
	}}

	unapplied := ChangeSetPlacement{}
	inheritParentPlacement(&unapplied, parent)
	if unapplied.AgentDeploymentIDs[agentDefinitionID] != "agent-live" || unapplied.TeamDeploymentID != "team-live" || unapplied.ProjectID != "project-live" || unapplied.Environment != "production" {
		t.Fatalf("stable placement was not inherited: %#v", unapplied)
	}
	if len(unapplied.AgentExpectedRevisions) != 0 || unapplied.TeamExpectedRevision != 0 || unapplied.ProjectExpectedRevision != 0 || unapplied.Objectives[objectiveKey].ExpectedRevision != 0 {
		t.Fatalf("unapplied candidate invented persistence revisions: %#v", unapplied)
	}

	parent.ApplyReceipt = &ChangeSetApplyReceipt{Resources: []AppliedResourceReference{
		{Kind: "agent_deployment", ID: "agent-live", Revision: 3},
		{Kind: "team_deployment", ID: "team-live", Revision: 4},
		{Kind: "objective", ID: "objective-live", Revision: 5},
		{Kind: "project", ID: "project-live", Revision: 6},
	}}
	// Positive values submitted by a client are not concurrency truth. Stable
	// resource identities inherit the exact authoritative receipt revisions.
	applied := ChangeSetPlacement{
		AgentDeploymentIDs:      map[string]string{agentDefinitionID: "agent-live"},
		AgentExpectedRevisions:  map[string]int64{agentDefinitionID: 1},
		TeamDeploymentID:        "team-live",
		TeamExpectedRevision:    1,
		ProjectID:               "project-live",
		ProjectExpectedRevision: 1,
		Objectives:              map[string]ObjectivePlacement{objectiveKey: {ID: "objective-live", ExpectedRevision: 1}},
	}
	inheritParentPlacement(&applied, parent)
	if applied.AgentExpectedRevisions[agentDefinitionID] != 3 || applied.TeamExpectedRevision != 4 || applied.Objectives[objectiveKey].ExpectedRevision != 5 || applied.ProjectExpectedRevision != 6 {
		t.Fatalf("applied receipt revisions were not inherited exactly: %#v", applied)
	}
	retargeted := ChangeSetPlacement{
		AgentDeploymentIDs: map[string]string{agentDefinitionID: "agent-new"},
		TeamDeploymentID:   "team-new",
		ProjectID:          "project-new",
		Objectives:         map[string]ObjectivePlacement{objectiveKey: {ID: "objective-new"}},
	}
	inheritParentPlacement(&retargeted, parent)
	if retargeted.AgentExpectedRevisions[agentDefinitionID] != 0 || retargeted.TeamExpectedRevision != 0 || retargeted.Objectives[objectiveKey].ExpectedRevision != 0 || retargeted.ProjectExpectedRevision != 0 {
		t.Fatalf("retargeted resources inherited old resource revisions: %#v", retargeted)
	}

	refined := &ChangeSet{Placement: applied}
	grandchild := ChangeSetPlacement{}
	inheritParentPlacement(&grandchild, refined)
	if grandchild.AgentExpectedRevisions[agentDefinitionID] != 3 || grandchild.TeamExpectedRevision != 4 || grandchild.Objectives[objectiveKey].ExpectedRevision != 5 || grandchild.ProjectExpectedRevision != 6 {
		t.Fatalf("applied lineage revisions were not preserved: %#v", grandchild)
	}
}

func TestAtomicMemoryApplyComposesProjectWithPortableDefaultPlacement(t *testing.T) {
	payload, _ := json.Marshal(GenerationResponse{Candidate: researchProjectCandidate()})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"community-source": {ID: "community-source", Version: "1.2.3", Actions: []string{"observe"}},
	}, SourcePolicies: map[string]SourcePolicyCapability{
		"approved-communities": {Reference: "approved-communities", Sources: []SourcePolicySourceCapability{{Host: "community.example"}}, MaximumItems: 5},
	}}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "Create a continuing research Project that runs every hour", Catalog: catalog, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-project"})
	if err != nil || !created.Result.Valid || created.Result.Candidate.Project == nil {
		t.Fatalf("created Project ChangeSet=%#v err=%v", created, err)
	}
	ready, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow"})
	if err != nil {
		t.Fatal(err)
	}
	applied, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Activate Project", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply"})
	if err != nil || len(store.projects) != 1 || strings.Contains(applied.Placement.ProjectID, "/") {
		t.Fatalf("applied Project=%#v stored=%#v err=%v", applied, store.projects, err)
	}
	found := false
	for _, resource := range applied.ApplyReceipt.Resources {
		found = found || resource.Kind == "project" && resource.ID == applied.Placement.ProjectID && resource.Revision == 1
	}
	if !found {
		t.Fatalf("Project receipt=%#v", applied.ApplyReceipt.Resources)
	}
}

func TestChangeSetCanonicalizesDefinitionIdentityPerScopeBeforeApproval(t *testing.T) {
	response := GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)}
	payload, _ := json.Marshal(response)
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload, payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	create := func(scopeID, key string) *ChangeSet {
		scope := capability.ScopeReference{Kind: "tenant", ID: scopeID}
		value, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "create", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}}, Placement: ChangeSetPlacement{TeamDeploymentID: "team-live", AgentDeploymentIDs: map[string]string{"community-researcher": "agent-live"}, Environment: "test"}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	one, two := create("one", "one"), create("two", "two")
	if one.Result.Candidate.Agents[0].ID == two.Result.Candidate.Agents[0].ID || strings.Contains(one.Result.Candidate.Agents[0].ID, "community-researcher") || one.CandidateDigest == two.CandidateDigest {
		t.Fatalf("one=%s two=%s", one.Result.Candidate.Agents[0].ID, two.Result.Candidate.Agents[0].ID)
	}
	if one.Placement.AgentDeploymentIDs[one.Result.Candidate.Agents[0].ID] != "agent-live" {
		t.Fatalf("placement=%#v", one.Placement)
	}
}

func TestChangeSetCanonicalizesProjectSymbolicReferencesWithDefinitions(t *testing.T) {
	candidate := researchProjectCandidate()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	canonicalizeCandidateScope(&candidate, scope)
	canonicalizeCandidateScope(&candidate, scope)

	project := candidate.Project
	agentID, teamID := "tenant/one/community-researcher", "tenant/one/gtm-research"
	monitorRef := WorkforceObjectiveKey(ProjectOwnerTeam, teamID, "monitor")
	if candidate.Agents[0].ID != agentID || candidate.Team.ID != teamID || project.Owner.DefinitionID != teamID ||
		project.ObjectiveRefs[0] != WorkforceObjectiveKey(ProjectOwnerAgent, agentID, "collect") ||
		project.ObjectiveRefs[1] != monitorRef || project.Milestones[0].ObjectiveRefs[0] != monitorRef ||
		project.SourceMonitors[0].ObjectiveRef != monitorRef || project.SourceMonitors[0].AssignedAgentDefinitionID != agentID ||
		project.Deliverables[0].ObjectiveRefs[0] != WorkforceObjectiveKey(ProjectOwnerTeam, teamID, "report") {
		t.Fatalf("canonical Project candidate = %#v", candidate)
	}
	trigger := candidate.Agents[0].Runbook.Triggers["hourly"]
	if trigger.ObjectiveID != monitorRef {
		t.Fatalf("canonical monitor Runbook Objective = %q", trigger.ObjectiveID)
	}
	placement := ChangeSetPlacement{}
	canonicalizePlacement(&placement, scope, &candidate)
	firstProjectID := placement.ProjectID
	canonicalizePlacement(&placement, scope, &candidate)
	if placement.ProjectID == "" || placement.ProjectID != firstProjectID || strings.Contains(placement.ProjectID, "/") {
		t.Fatalf("portable deterministic Project placement = %#v", placement)
	}
	for key, objective := range placement.Objectives {
		if objective.ID == "" || strings.Contains(objective.ID, "/") {
			t.Fatalf("portable deterministic Objective placement %s = %#v", key, objective)
		}
	}
}

func TestAtomicMemoryApplySupportsAgentWithoutTeam(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	candidate.Team = nil
	candidate.Assignments = nil
	payload, _ := json.Marshal(GenerationResponse{Candidate: candidate})
	compiler, _ := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{Scope: scope, Prompt: "agent only", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}}, Placement: ChangeSetPlacement{AgentDeploymentIDs: map[string]string{"community-researcher": "agent-live"}, Environment: "test"}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create"})
	if err != nil || !created.Result.Valid {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	ready, _, _ := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: 1, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "evaluator", ID: "policy"}, IdempotencyKey: "allow"})
	applied, _, err := service.Apply(context.Background(), ApplyChangeSetRequest{Scope: scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Activate approved workforce", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "apply"})
	if err != nil || applied.Status != ChangeSetApplied {
		t.Fatalf("applied=%#v err=%v", applied, err)
	}
	for _, resource := range applied.ApplyReceipt.Resources {
		if resource.Kind == "team_definition" || resource.Kind == "team_deployment" {
			t.Fatalf("unexpected Team resource %#v", resource)
		}
	}
}

func TestChangeSetServicePersistsIdempotentImmutableCreateAndRefineLineage(t *testing.T) {
	create := GenerationResponse{
		Candidate:           marketingCandidate("1", capability.RiskLevelRead),
		UnresolvedQuestions: []RefinementQuestion{testRefinementQuestion("authorized-sources", "Which sources are authorized?")},
	}
	amend := GenerationResponse{Candidate: marketingCandidate("2", capability.RiskLevelExternal)}
	createPayload, _ := jsonMarshal(create)
	amendPayload, _ := jsonMarshal(amend)
	generator := &sequenceChangeSetGenerator{payloads: [][]byte{createPayload, amendPayload}}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, err := NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	request := CreateChangeSetRequest{
		Scope: scope, Prompt: "Create a marketing research Team", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
		Placement: ChangeSetPlacement{TeamDeploymentID: "marketing", AgentDeploymentIDs: map[string]string{"researcher": "researcher-live"}},
		Actor:     ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-marketing",
	}
	created, replayed, err := service.Create(context.Background(), request)
	if err != nil || replayed || created.Status != ChangeSetBlocked || created.Mode != ModeCreate || created.Revision != 1 || created.CandidateDigest == "" {
		t.Fatalf("created = %#v, replayed = %t, err = %v", created, replayed, err)
	}
	replay, replayed, err := service.Create(context.Background(), request)
	if err != nil || !replayed || replay.ID != created.ID || replay.CandidateDigest != created.CandidateDigest {
		t.Fatalf("replay = %#v, replayed = %t, err = %v", replay, replayed, err)
	}
	request.Prompt = "Different intent"
	if _, _, err := service.Create(context.Background(), request); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}

	refined, replayed, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, ParentID: created.ID, Prompt: "Allow reviewed outreach", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
		Placement: created.Placement, Actor: request.Actor, IdempotencyKey: "refine-marketing",
	})
	if err != nil || replayed || refined.Mode != ModeAmend || refined.ParentID != created.ID || refined.Status != ChangeSetReview || len(refined.Result.RiskChanges) == 0 {
		t.Fatalf("refined = %#v, replayed = %t, err = %v", refined, replayed, err)
	}
	refined.Result.Candidate.Team.DisplayName = "mutated caller copy"
	restored, err := service.Get(context.Background(), scope, refined.ID)
	if err != nil || restored.Result.Candidate.Team.DisplayName == "mutated caller copy" {
		t.Fatalf("stored candidate was mutable: %#v, err = %v", restored, err)
	}
	if _, err := service.Get(context.Background(), capability.ScopeReference{Kind: "tenant", ID: "two"}, refined.ID); !errors.Is(err, ErrChangeSetNotFound) {
		t.Fatalf("cross-scope lookup = %v", err)
	}
}

func TestChangeSetEvaluationIsScopedIdempotentAuditableAndPolicyDerived(t *testing.T) {
	payload, err := json.Marshal(GenerationResponse{Candidate: marketingCandidate("1", capability.RiskLevelRead)})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := NewCompiler(&sequenceChangeSetGenerator{payloads: [][]byte{payload}})
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryChangeSetStore()
	service, err := NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create team",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}}}},
		Actor:   ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create",
	})
	if err != nil || created.Status != ChangeSetReview {
		t.Fatalf("created = %#v, err = %v", created, err)
	}
	request := SubmitChangeSetEvaluationRequest{Scope: created.Scope, ChangeSetID: created.ID, ExpectedRevision: 1,
		CandidateDigest: created.CandidateDigest, Allowed: true, Actor: ChangeSetActor{Type: "policy_evaluator", ID: "enterprise-policy"},
		IdempotencyKey: "evaluation-1", Findings: []ChangeSetPolicyFinding{{PolicyID: "production", Code: "review", Message: "Human review required"}},
		ApprovalRequirements: []ChangeSetApprovalRequirement{{PolicyID: "production", Role: "workforce_admin", Count: 1}}}
	evaluated, replay, err := service.SubmitEvaluation(context.Background(), request)
	if err != nil || replay || evaluated.Status != ChangeSetAwaitingApproval || evaluated.Revision != 2 {
		t.Fatalf("evaluated = %#v replay=%t err=%v", evaluated, replay, err)
	}
	if evaluated.CandidateDigest != created.CandidateDigest || len(evaluated.Evaluations) != 1 || len(evaluated.Lifecycle) != 2 || evaluated.Lifecycle[1].Reason != "policy_requires_approval" {
		t.Fatalf("evaluation audit = %#v", evaluated)
	}
	replayed, replay, err := service.SubmitEvaluation(context.Background(), request)
	if err != nil || !replay || replayed.Revision != 2 {
		t.Fatalf("replay = %#v replay=%t err=%v", replayed, replay, err)
	}
	conflict := request
	conflict.Allowed = false
	if _, _, err := service.SubmitEvaluation(context.Background(), conflict); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	other := request
	other.Scope.ID = "two"
	if _, _, err := service.SubmitEvaluation(context.Background(), other); !errors.Is(err, ErrChangeSetNotFound) {
		t.Fatalf("cross scope = %v", err)
	}
	stale := request
	stale.IdempotencyKey = "evaluation-2"
	if _, _, err := service.SubmitEvaluation(context.Background(), stale); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale revision = %v", err)
	}
}

func TestChangeSetEvaluationCanMakeCandidateReadyOrRejectIt(t *testing.T) {
	for _, test := range []struct {
		name    string
		allowed bool
		want    ChangeSetStatus
		reason  string
	}{{"ready", true, ChangeSetReady, "policy_allowed"}, {"rejected", false, ChangeSetRejected, "policy_denied"}} {
		t.Run(test.name, func(t *testing.T) {
			store := NewMemoryChangeSetStore()
			now := time.Now().UTC()
			value := &ChangeSet{ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, CandidateDigest: "candidate", Status: ChangeSetReview, Revision: 1, CreatedAt: now, UpdatedAt: now}
			if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
				t.Fatal(err)
			}
			service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
			updated, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 1, CandidateDigest: "candidate", Allowed: test.allowed, Actor: ChangeSetActor{Type: "evaluator", ID: "one"}, IdempotencyKey: "eval"})
			if err != nil || updated.Status != test.want || updated.Lifecycle[0].Reason != test.reason {
				t.Fatalf("updated = %#v, err = %v", updated, err)
			}
		})
	}
}

func TestHostReadinessValidatorKeepsReviewableCandidateBlocked(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	value := &ChangeSet{
		ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"},
		CandidateDigest: "candidate", Status: ChangeSetReview, Revision: 1,
		CreatedAt: now, UpdatedAt: now,
	}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	validator := &staticChangeSetReadinessValidator{issues: []ValidationIssue{{
		Path: "placement.environment", Code: "execution_target_unavailable",
		Message: "Choose where this workforce can run before applying it.",
	}}}
	service := &ChangeSetService{
		store: store, readinessValidators: []ChangeSetReadinessValidator{validator},
		now: func() time.Time { return now.Add(time.Minute) },
	}
	updated, _, err := service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{
		Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 1,
		CandidateDigest: "candidate", Allowed: true,
		Actor: ChangeSetActor{Type: "evaluator", ID: "one"}, IdempotencyKey: "eval",
	})
	if err != nil || updated.Status != ChangeSetBlocked || !strings.Contains(updated.Result.Validation[0].Message, "Choose where") || validator.calls != 1 {
		t.Fatalf("updated=%#v calls=%d err=%v", updated, validator.calls, err)
	}
}

func TestPendingChangeSetEvaluationsAreDurableScopedAndLeaveAfterDecision(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	for _, value := range []*ChangeSet{
		{ID: "review", Scope: scope, CandidateDigest: "candidate", Status: ChangeSetReview, Revision: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "blocked", Scope: scope, CandidateDigest: "blocked", Status: ChangeSetBlocked, Revision: 1, CreatedAt: now, UpdatedAt: now},
		{ID: "other", Scope: capability.ScopeReference{Kind: "tenant", ID: "two"}, CandidateDigest: "other", Status: ChangeSetReview, Revision: 1, CreatedAt: now, UpdatedAt: now},
	} {
		if _, _, err := store.CreateChangeSet(context.Background(), value, "create-"+value.ID, "digest-"+value.ID); err != nil {
			t.Fatal(err)
		}
	}
	service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
	pending, err := service.ListPendingEvaluations(context.Background(), scope, 10)
	if err != nil || len(pending) != 1 || pending[0].ID != "review" {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	if _, _, err = service.SubmitEvaluation(context.Background(), SubmitChangeSetEvaluationRequest{
		Scope: scope, ChangeSetID: "review", ExpectedRevision: 1, CandidateDigest: "candidate", Allowed: true,
		Actor: ChangeSetActor{Type: "workload", ID: "policy-host"}, IdempotencyKey: "evaluation",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err = service.ListPendingEvaluations(context.Background(), scope, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after decision=%#v err=%v", pending, err)
	}
}

func TestChangeSetApprovalsRequireDistinctPrincipalsAndAreAuditable(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	evaluation := ChangeSetEvaluation{ID: "evaluation", CandidateDigest: "candidate", Allowed: true,
		ApprovalRequirements: []ChangeSetApprovalRequirement{{PolicyID: "production", Role: "workforce_admin", Count: 2}}}
	value := &ChangeSet{ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, CandidateDigest: "candidate",
		Status: ChangeSetAwaitingApproval, Evaluations: []ChangeSetEvaluation{evaluation}, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
	firstRequest := ResolveChangeSetApprovalRequest{Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 2,
		EvaluationID: evaluation.ID, PolicyID: "production", Role: "workforce_admin", Approved: true,
		Actor: ChangeSetActor{Type: "user", ID: "alice"}, IdempotencyKey: "alice-approves"}
	first, replay, err := service.ResolveApproval(context.Background(), firstRequest)
	if err != nil || replay || first.Status != ChangeSetAwaitingApproval || first.Revision != 3 || len(first.ApprovalDecisions) != 1 || first.Lifecycle[0].Reason != "approval_recorded" {
		t.Fatalf("first approval = %#v replay=%t err=%v", first, replay, err)
	}
	replayed, replay, err := service.ResolveApproval(context.Background(), firstRequest)
	if err != nil || !replay || replayed.Revision != 3 {
		t.Fatalf("approval replay = %#v replay=%t err=%v", replayed, replay, err)
	}
	duplicatePrincipal := firstRequest
	duplicatePrincipal.ExpectedRevision = 3
	duplicatePrincipal.IdempotencyKey = "alice-again"
	if _, _, err := service.ResolveApproval(context.Background(), duplicatePrincipal); !errors.Is(err, ErrChangeSetTransition) {
		t.Fatalf("duplicate principal = %v", err)
	}
	secondRequest := firstRequest
	secondRequest.ExpectedRevision = 3
	secondRequest.Actor.ID = "bob"
	secondRequest.IdempotencyKey = "bob-approves"
	ready, replay, err := service.ResolveApproval(context.Background(), secondRequest)
	if err != nil || replay || ready.Status != ChangeSetReady || ready.Revision != 4 || len(ready.ApprovalDecisions) != 2 || ready.Lifecycle[1].Reason != "approvals_satisfied" {
		t.Fatalf("ready = %#v replay=%t err=%v", ready, replay, err)
	}
	stale := secondRequest
	stale.Actor.ID = "carol"
	stale.IdempotencyKey = "carol-stale"
	if _, _, err := service.ResolveApproval(context.Background(), stale); !errors.Is(err, ErrChangeSetRevision) {
		t.Fatalf("stale approval = %v", err)
	}
}

func TestChangeSetApprovalRejectionFailsClosed(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	evaluation := ChangeSetEvaluation{ID: "evaluation", CandidateDigest: "candidate", Allowed: true,
		ApprovalRequirements: []ChangeSetApprovalRequirement{{PolicyID: "production", Role: "workforce_admin", Count: 1}}}
	value := &ChangeSet{ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, CandidateDigest: "candidate",
		Status: ChangeSetAwaitingApproval, Evaluations: []ChangeSetEvaluation{evaluation}, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
	rejected, _, err := service.ResolveApproval(context.Background(), ResolveChangeSetApprovalRequest{Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 2,
		EvaluationID: evaluation.ID, PolicyID: "production", Role: "workforce_admin", Approved: false, Reason: "insufficient controls",
		Actor: ChangeSetActor{Type: "user", ID: "alice"}, IdempotencyKey: "reject"})
	if err != nil || rejected.Status != ChangeSetRejected || rejected.Lifecycle[0].Reason != "approval_rejected" || rejected.ApprovalDecisions[0].Reason != "insufficient controls" {
		t.Fatalf("rejected = %#v err=%v", rejected, err)
	}
}

func TestChangeSetApprovalSeparationGroupRequiresDifferentPrincipals(t *testing.T) {
	store := NewMemoryChangeSetStore()
	now := time.Now().UTC()
	evaluation := ChangeSetEvaluation{ID: "evaluation", CandidateDigest: "candidate", Allowed: true,
		ApprovalRequirements: []ChangeSetApprovalRequirement{
			{PolicyID: "production", Role: "tenant:admin", Count: 1, SeparationGroup: "production-release"},
			{PolicyID: "production", Role: "role-group:security-reviewer", Count: 1, SeparationGroup: "production-release"},
		}}
	value := &ChangeSet{ID: "change", Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, CandidateDigest: "candidate",
		Status: ChangeSetAwaitingApproval, Evaluations: []ChangeSetEvaluation{evaluation}, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if _, _, err := store.CreateChangeSet(context.Background(), value, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	service := &ChangeSetService{store: store, now: func() time.Time { return now.Add(time.Minute) }}
	first, _, err := service.ResolveApproval(context.Background(), ResolveChangeSetApprovalRequest{
		Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 2, EvaluationID: evaluation.ID,
		PolicyID: "production", Role: "tenant:admin", Approved: true,
		Actor: ChangeSetActor{Type: "user", ID: "alice"}, IdempotencyKey: "alice-admin",
	})
	if err != nil || first.Status != ChangeSetAwaitingApproval {
		t.Fatalf("first approval = %#v err=%v", first, err)
	}
	samePrincipal := ResolveChangeSetApprovalRequest{
		Scope: value.Scope, ChangeSetID: value.ID, ExpectedRevision: 3, EvaluationID: evaluation.ID,
		PolicyID: "production", Role: "role-group:security-reviewer", Approved: true,
		Actor: ChangeSetActor{Type: "user", ID: "alice"}, IdempotencyKey: "alice-security",
	}
	if _, _, err = service.ResolveApproval(context.Background(), samePrincipal); !errors.Is(err, ErrChangeSetTransition) {
		t.Fatalf("same principal separation approval = %v", err)
	}
	samePrincipal.Actor.ID = "bob"
	samePrincipal.IdempotencyKey = "bob-security"
	ready, _, err := service.ResolveApproval(context.Background(), samePrincipal)
	if err != nil || ready.Status != ChangeSetReady || len(ready.ApprovalDecisions) != 2 {
		t.Fatalf("separated approval = %#v err=%v", ready, err)
	}
}

func TestReconcilePlacementToCandidatePrunesStaleInheritedOwnersAndSkills(t *testing.T) {
	candidate := WorkforceCandidate{Agents: []*agent.AgentDefinition{{
		ID: "tenant/one/researcher", SkillRequirements: []agent.SkillRequirement{{SkillID: "browser"}},
	}}}
	placement := ChangeSetPlacement{
		TeamDeploymentID: "team:stale", TeamExpectedRevision: 4,
		AgentDeploymentIDs:     map[string]string{"tenant/one/researcher": "agent:current", "tenant/one/": "agent:stale"},
		AgentExpectedRevisions: map[string]int64{"tenant/one/researcher": 2, "tenant/one/": 1},
		CredentialReferences: map[string]map[string]capability.CredentialReference{
			"tenant/one/researcher": {"username": {Kind: "basic", ID: "credential://browser.username"}},
			"tenant/one/":           {"password": {Kind: "basic", ID: "credential://stale.password"}},
		},
		SkillSourceIdentities: map[string]map[string]string{
			"tenant/one/researcher": {"browser": "registry::browser", "removed": "registry::removed"},
			"tenant/one/":           {"browser": "registry::browser"},
		},
		SkillSourceVersions: map[string]map[string]string{
			"tenant/one/researcher": {"browser": "1.0.0", "removed": "1.0.0"},
		},
		SkillRuntimeIdentities: map[string]map[string]capability.SkillIdentity{
			"tenant/one/researcher": {
				"browser": capability.NewSkillIdentity("browser", "1.0.0", "registry::browser"),
				"removed": capability.NewSkillIdentity("removed", "1.0.0", "registry::removed"),
			},
		},
		PlannedSkillInstallations: []SkillInstallationIntent{{SkillID: "browser"}, {SkillID: "removed"}},
	}
	reconcilePlacementToCandidate(&placement, &candidate)
	if placement.TeamDeploymentID != "" || placement.TeamExpectedRevision != 0 || placement.AgentDeploymentIDs["tenant/one/"] != "" || placement.AgentExpectedRevisions["tenant/one/"] != 0 {
		t.Fatalf("stale owners retained: %#v", placement)
	}
	if _, exists := placement.SkillSourceIdentities["tenant/one/researcher"]["removed"]; exists || len(placement.PlannedSkillInstallations) != 1 || placement.PlannedSkillInstallations[0].SkillID != "browser" {
		t.Fatalf("stale Skills retained: %#v", placement)
	}
	if placement.SkillSourceIdentities["tenant/one/researcher"]["browser"] != "registry::browser" || placement.CredentialReferences["tenant/one/researcher"]["username"].ID == "" {
		t.Fatalf("active setup was not preserved: %#v", placement)
	}
}

func jsonMarshal(value interface{}) ([]byte, error) {
	return json.Marshal(value)
}
