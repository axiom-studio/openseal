package authoring

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
)

type refinementGenerator struct {
	mu       sync.Mutex
	payloads [][]byte
	requests []GenerateRequest
}

func (g *refinementGenerator) Generate(_ context.Context, request GenerateRequest) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requests = append(g.requests, request)
	if len(g.payloads) == 0 {
		return nil, errors.New("no refinement fixture")
	}
	payload := g.payloads[0]
	g.payloads = g.payloads[1:]
	return payload, nil
}

func refinementPayload(t *testing.T, version string, questions ...RefinementQuestion) []byte {
	t.Helper()
	payload, err := json.Marshal(GenerationResponse{Candidate: marketingCandidate(version, capability.RiskLevelRead), UnresolvedQuestions: questions})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestChangeSetRefinementIsSequentialAuditedAndRestartSafe(t *testing.T) {
	skillQuestion := RefinementQuestion{
		ID: "select-reddit-skill", Category: RefinementCategorySkill,
		Prompt: "Which compatible source Skill should perform Reddit reads?", WhyNeeded: "The monitor requires one executable source capability.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Minimum: 1, Maximum: 1, Options: []RefinementQuestionOption{{ID: "reddit-research", Label: "Reddit research"}}},
		Priority: 100, AutoResolvable: true, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog, Reference: "skill-catalog"}},
	}
	scopeQuestion := RefinementQuestion{
		ID: "target-subreddits", Category: RefinementCategoryScope,
		Prompt: "Which subreddits should be monitored?", WhyNeeded: "Source scope cannot be inferred safely.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerStringList, Minimum: 1, Maximum: 20},
		DependsOn: []RefinementQuestionDependency{{QuestionID: skillQuestion.ID}}, Priority: 80,
		Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
	}
	scopeQuestionAfterSkill := scopeQuestion
	scopeQuestionAfterSkill.DependsOn = nil
	generator := &refinementGenerator{payloads: [][]byte{
		refinementPayload(t, "1", skillQuestion, scopeQuestion),
		refinementPayload(t, "2", scopeQuestionAfterSkill),
		refinementPayload(t, "3"),
		refinementPayload(t, "4"),
	}}
	compiler, _ := NewCompiler(generator)
	store := NewMemoryChangeSetStore()
	service, _ := NewChangeSetService(compiler, store)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	changeSet, replay, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, Prompt: "Monitor Reddit",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"search", "read"}},
		}},
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-refinement",
	})
	if err != nil || replay || changeSet.Status != ChangeSetBlocked || changeSet.Refinement.NextQuestion().ID != skillQuestion.ID {
		t.Fatalf("created=%#v replay=%t err=%v", changeSet, replay, err)
	}
	if _, _, err = service.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, QuestionID: scopeQuestion.ID,
		Value: RefinementAnswerValue{Items: []string{"openseal"}}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "out-of-order",
	}); err == nil || err.Error() != "refinement question is not currently answerable" {
		t.Fatalf("out-of-order answer error=%v", err)
	}
	answeredSkill, replay, err := service.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, QuestionID: skillQuestion.ID,
		Value: RefinementAnswerValue{SkillIDs: []string{"reddit-research"}}, Source: RefinementAnswerSourceRuntime,
		Actor: ChangeSetActor{Type: "runtime", ID: "catalog-resolver"}, IdempotencyKey: "answer-skill",
	})
	if err != nil || replay || answeredSkill.Status != ChangeSetEvaluating || len(answeredSkill.Refinement.Answers) != 1 || answeredSkill.Generation.PreviousCandidateDigest != changeSet.CandidateDigest {
		t.Fatalf("answered skill=%#v replay=%t err=%v", answeredSkill, replay, err)
	}
	if answeredSkill.Generation.Request.Refinement == nil || len(answeredSkill.Generation.Request.Refinement.Answers) != 1 {
		t.Fatalf("provider refinement context=%#v", answeredSkill.Generation.Request.Refinement)
	}
	// A new service over the same durable store simulates worker recovery after
	// process restart; the persisted generation intent remains sufficient.
	restarted, _ := NewChangeSetService(compiler, store)
	regenerated, err := restarted.GeneratePrepared(context.Background(), scope, changeSet.ID, answeredSkill.Revision)
	if err != nil || regenerated.Status != ChangeSetBlocked || regenerated.Refinement.NextQuestion().ID != scopeQuestion.ID || regenerated.Refinement.CurrentAnswer(skillQuestion.ID) == nil {
		t.Fatalf("regenerated=%#v err=%v", regenerated, err)
	}
	answerRequest := AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: changeSet.ID, ExpectedRevision: regenerated.Revision, QuestionID: scopeQuestion.ID,
		Value: RefinementAnswerValue{Items: []string{" openseal ", "golang", "openseal"}},
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "answer-scope",
	}
	answeredScope, replay, err := restarted.AnswerRefinement(context.Background(), answerRequest)
	got := answeredScope.Refinement.CurrentAnswer(scopeQuestion.ID).Value.Items
	if err != nil || replay || len(got) != 2 || got[0] != "openseal" {
		t.Fatalf("answered scope=%#v replay=%t err=%v", answeredScope, replay, err)
	}
	replayed, replay, err := restarted.AnswerRefinement(context.Background(), answerRequest)
	if err != nil || !replay || replayed.Revision != answeredScope.Revision || len(replayed.Refinement.Answers) != 2 {
		t.Fatalf("answer replay=%#v replay=%t err=%v", replayed, replay, err)
	}
	conflict := answerRequest
	conflict.Value.Items = []string{"different"}
	if _, _, err = restarted.AnswerRefinement(context.Background(), conflict); !errors.Is(err, ErrChangeSetIdempotency) {
		t.Fatalf("idempotency conflict=%v", err)
	}
	complete, err := restarted.GeneratePrepared(context.Background(), scope, changeSet.ID, answeredScope.Revision)
	if err != nil || complete.Status != ChangeSetReview || !complete.Result.Valid || complete.Refinement.NextQuestion() != nil || len(complete.Refinement.Answers) != 2 {
		t.Fatalf("complete=%#v err=%v", complete, err)
	}
	// Historical answers are revisable from the review surface and append a new
	// event rather than rewriting the audit trail.
	revised, _, err := restarted.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: complete.ID, ExpectedRevision: complete.Revision, QuestionID: scopeQuestion.ID,
		Value: RefinementAnswerValue{Items: []string{"openseal", "selfhosted"}}, Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "revise-scope",
	})
	if err != nil || revised.Status != ChangeSetEvaluating || len(revised.Refinement.Answers) != 3 || revised.Refinement.CurrentAnswer(scopeQuestion.ID).Value.Items[1] != "selfhosted" {
		t.Fatalf("revised=%#v err=%v", revised, err)
	}
}

func TestRefinementAtomicallyAddsHostVerifiedDiscoveredSkill(t *testing.T) {
	question := RefinementQuestion{
		ID: "select-analysis-skill", Category: RefinementCategorySkill,
		Prompt: "Which Skill should analyze the evidence?", WhyNeeded: "Analysis needs an executable capability.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Minimum: 1, Maximum: 1},
		Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	generator := &refinementGenerator{payloads: [][]byte{
		refinementPayload(t, "1", question),
		refinementPayload(t, "2"),
	}}
	compiler, _ := NewCompiler(generator)
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	changeSet, _, err := service.Create(context.Background(), CreateChangeSetRequest{
		Scope: scope, Prompt: "Analyze research",
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"read", "search"}, Readiness: SkillReadinessReady},
		}},
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "create-discovery-refinement",
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := SkillSearchCandidate{
		SkillCapability: SkillCapability{
			ID: "marketplace.summarize", Version: "1.2.3", SourceIdentity: "registry.example::publisher/summarize",
			Name: "Summarize", Description: "Summarizes evidence.", Actions: []string{"summarize"},
			Readiness: SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{{
				Requirement: "installation", Compatible: true, Evidence: "Verified compilation receipt.", Reference: "receipt:sha256:abc",
			}},
		},
		Origin: SkillSearchOriginCatalog, Verification: SkillSearchVerificationVerified,
		Provenance:       SkillSearchProvenance{Registry: "registry.example", Publisher: "publisher", Reference: "listing:7"},
		RequiresApproval: true,
	}
	unverified := candidate
	unverified.ID = "marketplace.unverified"
	unverified.Verification = SkillSearchVerificationRequired
	if _, _, err := service.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, QuestionID: question.ID,
		Value: RefinementAnswerValue{SkillIDs: []string{unverified.ID}}, TrustedSkill: &unverified,
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "answer-unverified-skill",
	}); err == nil || !strings.Contains(err.Error(), "must be verified") {
		t.Fatalf("unverified discovered Skill error = %v", err)
	}
	identity := SkillSearchIdentity{ID: candidate.ID, Version: candidate.Version, SourceIdentity: candidate.SourceIdentity}
	if _, _, err := service.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, QuestionID: question.ID,
		Value: RefinementAnswerValue{SkillIDs: []string{candidate.ID}}, DiscoveredSkill: &identity,
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "answer-client-only-discovery",
	}); err == nil || !strings.Contains(err.Error(), "verified by the host") {
		t.Fatalf("unverified client discovery error = %v", err)
	}
	answered, replayed, err := service.AnswerRefinement(context.Background(), AnswerChangeSetRefinementRequest{
		Scope: scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, QuestionID: question.ID,
		Value: RefinementAnswerValue{SkillIDs: []string{candidate.ID}}, TrustedSkill: &candidate,
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "answer-discovered-skill",
	})
	if err != nil || replayed {
		t.Fatalf("answer discovered Skill: replayed=%t err=%v", replayed, err)
	}
	added, exists := answered.Catalog.Skills[candidate.ID]
	if !exists || added.Version != candidate.Version || added.SourceIdentity != candidate.SourceIdentity {
		t.Fatalf("durable discovered Skill = %#v", added)
	}
	acquisitionReference := ""
	for _, compatibility := range added.Compatibility {
		if compatibility.Requirement == "installation" && !compatibility.Compatible {
			acquisitionReference = compatibility.Reference
		}
	}
	if acquisitionReference != candidate.Provenance.Reference {
		t.Fatalf("durable discovered Skill acquisition reference = %q", acquisitionReference)
	}
	if got := answered.Generation.Request.Catalog.Skills[candidate.ID]; got.Version != candidate.Version {
		t.Fatalf("generation catalog Skill = %#v", got)
	}
	if got := answered.Refinement.CurrentAnswer(question.ID); got == nil || len(got.Value.SkillIDs) != 1 || got.Value.SkillIDs[0] != candidate.ID {
		t.Fatalf("durable answer = %#v", got)
	}
	if options := answered.Refinement.Questions[0].Answer.Options; len(options) != 1 || options[0].ID != candidate.ID {
		t.Fatalf("durable options = %#v", options)
	}
}

func TestTrustedRefinementSkillRefreshesSameIdentityAcquisitionReference(t *testing.T) {
	question := RefinementQuestion{
		ID: CapabilityNeedQuestionID("analysis"), Category: RefinementCategorySkill,
		Prompt: "Which Skill should analyze the evidence?", WhyNeeded: "Analysis requires one exact Skill.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Minimum: 1, Maximum: 1},
		Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	const (
		skillID = "marketplace.summarize"
		version = "1.2.3"
		source  = "registry.example::publisher/summarize"
		listing = "listing:7"
		generic = "needs_installation"
	)
	changeSet := &ChangeSet{
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			skillID: {
				ID: skillID, Version: version, SourceIdentity: source,
				Readiness: SkillReadinessNeedsInstallation,
				Compatibility: []SkillCompatibility{{
					Requirement: "installation", Compatible: false,
					Evidence: "Installation is required.", Reference: generic,
				}, {
					Requirement: "source_digest", Compatible: true,
					Evidence: "The exact source was verified.", Reference: "sha256:verified",
				}},
			},
		}},
		Refinement: ChangeSetRefinement{Questions: []RefinementQuestion{question}},
	}
	candidate := SkillSearchCandidate{
		SkillCapability: SkillCapability{
			ID: skillID, Version: version, SourceIdentity: source, Name: "Summarize",
			Readiness: SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{{
				Requirement: "installation", Compatible: false,
				Evidence: "Installation is required.", Reference: generic,
			}, {
				Requirement: "source_digest", Compatible: true,
				Evidence: "The exact source was verified.", Reference: "sha256:verified",
			}},
		},
		Origin: SkillSearchOriginCatalog, Verification: SkillSearchVerificationVerified,
		Provenance: SkillSearchProvenance{Registry: "registry.example", Reference: listing},
	}
	if err := addTrustedRefinementSkill(
		changeSet,
		question.ID,
		RefinementAnswerValue{SkillIDs: []string{skillID}},
		candidate,
	); err != nil {
		t.Fatal(err)
	}
	if reference := plannedInstallationReference(changeSet.Catalog.Skills[skillID]); reference != listing {
		t.Fatalf("refreshed acquisition reference = %q", reference)
	}
}

func TestTrustedRefinementSkillReplacesReviewedVersionFromSameSource(t *testing.T) {
	question := RefinementQuestion{
		ID: CapabilityNeedQuestionID("team-messaging"), Category: RefinementCategorySkill,
		Prompt: "Which Skill should send messages?", WhyNeeded: "Messaging requires one exact Skill.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer: RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Minimum: 1, Maximum: 1,
			Options: []RefinementQuestionOption{{ID: "skill-slack", Label: "Slack"}}},
		Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	const source = "https://github.com/axiom-studio/skills::skill-slack"
	changeSet := &ChangeSet{
		Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{
			"skill-slack": {ID: "skill-slack", Version: "1.0.0", SourceIdentity: source, Readiness: SkillReadinessReady},
		}},
		Refinement: ChangeSetRefinement{Questions: []RefinementQuestion{question}},
	}
	candidate := SkillSearchCandidate{
		SkillCapability: SkillCapability{
			ID: "skill-slack", Version: "2.0.0", SourceIdentity: source, Name: "Slack",
			Readiness: SkillReadinessNeedsInstallation,
			Compatibility: []SkillCompatibility{
				{Requirement: "installation", Compatible: false, Evidence: "Upgrade required."},
				{Requirement: "source_digest", Compatible: true, Evidence: "Verified exact source.", Reference: "sha256:verified"},
			},
		},
		Origin: SkillSearchOriginCatalog, Verification: SkillSearchVerificationVerified,
		Provenance: SkillSearchProvenance{Registry: "https://catalog.example", Reference: "listing:42"},
	}
	if err := addTrustedRefinementSkill(changeSet, question.ID,
		RefinementAnswerValue{SkillIDs: []string{"skill-slack"}}, candidate); err != nil {
		t.Fatal(err)
	}
	got := changeSet.Catalog.Skills["skill-slack"]
	if got.Version != "2.0.0" || got.SourceIdentity != source || plannedInstallationReference(got) != "listing:42" {
		t.Fatalf("reviewed upgrade = %#v", got)
	}

	conflicting := candidate
	conflicting.SourceIdentity = "https://catalog.example::different/slack"
	if err := addTrustedRefinementSkill(changeSet, question.ID,
		RefinementAnswerValue{SkillIDs: []string{"skill-slack"}}, conflicting); err == nil ||
		!strings.Contains(err.Error(), "conflicts with the authorized catalog identity") {
		t.Fatalf("conflicting source error = %v", err)
	}
}

func TestRefinementSequenceCanGateSkillsAndScopeOnCredentialConfiguration(t *testing.T) {
	credential := RefinementQuestion{
		ID: "reddit-credential", Category: RefinementCategoryCredential, Prompt: "Which authorized Reddit credential should be used?", WhyNeeded: "Reddit access requires an authorized credential.",
		Blocking: []RefinementBlockingScope{RefinementBlocksApply}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference},
		Priority: 100, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCredential}},
	}
	skills := RefinementQuestion{
		ID: "reddit-skills", Category: RefinementCategorySkill, Prompt: "Which verified Reddit and language Skills should be used?", WhyNeeded: "The workforce needs compatible capabilities.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Minimum: 1, Maximum: 2, Options: []RefinementQuestionOption{{ID: "reddit-research", Label: "Reddit research"}, {ID: "language-analysis", Label: "Language analysis"}}},
		DependsOn: []RefinementQuestionDependency{{QuestionID: credential.ID}}, Priority: 90, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	subreddits := RefinementQuestion{
		ID: "subreddits", Category: RefinementCategoryScope, Prompt: "Which subreddits should be monitored?", WhyNeeded: "The source scope must be explicit.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerStringList, Minimum: 1, Maximum: 20},
		DependsOn: []RefinementQuestionDependency{{QuestionID: skills.ID}}, Priority: 80, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
	}
	refinement := ChangeSetRefinement{Questions: []RefinementQuestion{credential, skills, subreddits}}
	if err := validateRefinementQuestions(refinement.Questions); err != nil {
		t.Fatalf("valid sequential refinement contract: %v", err)
	}
	if next := refinement.NextQuestion(); next == nil || next.ID != credential.ID {
		t.Fatalf("first question = %#v", next)
	}
	refinement.Answers = append(refinement.Answers, RefinementAnswerEvent{QuestionID: credential.ID, Value: RefinementAnswerValue{CredentialReference: &capability.CredentialReference{Kind: "oauth", ID: "opaque-credential-reference"}}})
	if next := refinement.NextQuestion(); next == nil || next.ID != skills.ID {
		t.Fatalf("second question = %#v", next)
	}
	refinement.Answers = append(refinement.Answers, RefinementAnswerEvent{QuestionID: skills.ID, Value: RefinementAnswerValue{SkillIDs: []string{"reddit-research", "language-analysis"}}})
	if next := refinement.NextQuestion(); next == nil || next.ID != subreddits.ID {
		t.Fatalf("third question = %#v", next)
	}
}

func TestRefinementQuestionsRejectCyclesAndSecretShapedAnswers(t *testing.T) {
	first := RefinementQuestion{ID: "first", Category: RefinementCategoryScope, Prompt: "First?", WhyNeeded: "Needed", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerText}, Priority: 1, DependsOn: []RefinementQuestionDependency{{QuestionID: "second"}}, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}}}
	second := RefinementQuestion{ID: "second", Category: RefinementCategoryCredential, Prompt: "Credential?", WhyNeeded: "Needed", Blocking: []RefinementBlockingScope{RefinementBlocksApply}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference}, Priority: 2, DependsOn: []RefinementQuestionDependency{{QuestionID: "first"}}, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCredential}}}
	if err := validateRefinementQuestions([]RefinementQuestion{first, second}); err == nil {
		t.Fatal("cyclic dependencies were accepted")
	}
	if err := validateRefinementAnswer(second, RefinementAnswerValue{Text: "secret-value"}); err == nil {
		t.Fatal("credential answer accepted a text secret instead of an opaque reference")
	}
	second.DependsOn = nil
	second.Provenance[0].Reference = "credential/tenant/opaque-reference"
	if err := validateRefinementQuestions([]RefinementQuestion{second}); err == nil {
		t.Fatal("opaque credential reference was accepted in model-visible question provenance")
	}
}

func TestRefinementQuestionDiagnosticsIdentifyExactInvalidField(t *testing.T) {
	base := RefinementQuestion{
		ID: "targets", Category: RefinementCategoryScope, Prompt: "Which targets?", WhyNeeded: "The source scope must be explicit.",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerStringList},
		Priority: 1, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenancePrompt}},
	}
	tests := []struct {
		name string
		edit func(*RefinementQuestion)
		want string
	}{
		{name: "category", edit: func(question *RefinementQuestion) { question.Category = "source" }, want: "unresolvedQuestions[0].category must be one of"},
		{name: "answer kind", edit: func(question *RefinementQuestion) { question.Answer.Kind = "list" }, want: "unresolvedQuestions[0].answer.kind must be one of"},
		{name: "missing blocking", edit: func(question *RefinementQuestion) { question.Blocking = nil }, want: "unresolvedQuestions[0].blocking must contain at least one"},
		{name: "blocking value", edit: func(question *RefinementQuestion) { question.Blocking = []RefinementBlockingScope{"generation"} }, want: "unresolvedQuestions[0].blocking[0] must be"},
		{name: "credential kind", edit: func(question *RefinementQuestion) { question.Category = RefinementCategoryCredential }, want: "unresolvedQuestions[0].answer.kind must be credential_reference"},
		{name: "skill kind", edit: func(question *RefinementQuestion) { question.Category = RefinementCategorySkill }, want: "unresolvedQuestions[0].answer.kind must be skill_selection"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			question := base
			test.edit(&question)
			err := validateRefinementQuestions([]RefinementQuestion{question})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("diagnostic = %v, want %q", err, test.want)
			}
		})
	}
}

func TestProviderRefinementProjectionRedactsOpaqueCredentialReference(t *testing.T) {
	value := ChangeSetRefinement{
		Questions: []RefinementQuestion{{ID: "credential", Category: RefinementCategoryCredential, Prompt: "Configure credential", WhyNeeded: "Execution requires it", Blocking: []RefinementBlockingScope{RefinementBlocksApply}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference}, Priority: 1, AutoResolvable: true, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCredential}}}},
		Answers:   []RefinementAnswerEvent{{QuestionID: "credential", Value: RefinementAnswerValue{CredentialReference: &capability.CredentialReference{Kind: "oauth", ID: "credential/tenant/private-reference"}}, Source: RefinementAnswerSourceRuntime}},
	}
	payload, err := json.Marshal(providerRefinementContext(value))
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(payload)
	if strings.Contains(encoded, "credential/tenant/private-reference") || strings.Contains(encoded, "credentialReference") || !strings.Contains(encoded, `"credentialConfigured":true`) || !strings.Contains(encoded, `"credentialKind":"oauth"`) {
		t.Fatalf("provider projection=%s", encoded)
	}
}

func TestAnsweredRefinementQuestionsRemainResolvedAcrossProviderRetries(t *testing.T) {
	credential := RefinementQuestion{
		ID: "browser-binding", Category: RefinementCategoryCredential, Prompt: "Choose credential", WhyNeeded: "Browser login",
		Blocking: []RefinementBlockingScope{RefinementBlocksApply},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerCredentialReference}, Priority: 1,
	}
	refinement := ChangeSetRefinement{Answers: []RefinementAnswerEvent{{
		QuestionID: credential.ID,
		Value: RefinementAnswerValue{CredentialReference: &capability.CredentialReference{
			Kind: "http_basic_auth", ID: "credential://reddit-fixture.username",
		}},
	}}}
	if questions := unansweredRefinementQuestions([]RefinementQuestion{credential}, refinement); len(questions) != 0 {
		t.Fatalf("answered provider question was reopened: %#v", questions)
	}
	credential.Answer.Kind = RefinementAnswerBoolean
	if questions := unansweredRefinementQuestions([]RefinementQuestion{credential}, refinement); len(questions) != 1 {
		t.Fatalf("materially changed question was hidden: %#v", questions)
	}
}

func TestRefinementSkillOptionsMustBeTruthfulAuthorizedCatalogEntries(t *testing.T) {
	question := RefinementQuestion{ID: "skill", Category: RefinementCategorySkill, Prompt: "Choose Skill", WhyNeeded: "Capability required", Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Options: []RefinementQuestionOption{{ID: "invented", Label: "Invented"}}}, Priority: 1, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}}}
	if err := validateRefinementCatalog([]RefinementQuestion{question}, CapabilityCatalog{}); err == nil {
		t.Fatal("invented Skill option was accepted")
	}
	question.Answer.Options[0].ID = "reddit"
	if err := validateRefinementCatalog([]RefinementQuestion{question}, CapabilityCatalog{Skills: map[string]SkillCapability{"reddit": {ID: "reddit", Readiness: SkillReadinessUnavailable}}}); err == nil {
		t.Fatal("unavailable Skill option was presented")
	}
	question.Answer.Options[0].ID = "delivery"
	err := validateRefinementCatalog([]RefinementQuestion{question}, CapabilityCatalog{Skills: map[string]SkillCapability{"openseal.delivery": {ID: "openseal.delivery", Readiness: SkillReadinessNeedsBinding}}})
	if err == nil || !strings.Contains(err.Error(), `exact authorized catalog key "openseal.delivery"`) || !strings.Contains(err.Error(), "aliases are not accepted") {
		t.Fatalf("delivery alias diagnostic=%v", err)
	}
}

func TestBindingConfigurationIsAnExplicitSkillLifecycleGap(t *testing.T) {
	skill := SkillCapability{
		ID: "openseal.kubernetes", Version: "1.1.0", Readiness: SkillReadinessNeedsBinding,
		Compatibility: []SkillCompatibility{
			{Requirement: "action:list_events", Compatible: true, Evidence: "declared by the canonical Skill"},
			{Requirement: "binding_configuration", Compatible: false, Evidence: "reviewed cluster selection is required"},
		},
	}
	catalog := CapabilityCatalog{
		Skills: map[string]SkillCapability{skill.ID: skill},
		CapabilityNeeds: []CapabilityNeed{{
			ID: "kubernetes-operations", Prompt: "Which Kubernetes Skill should be used?", WhyNeeded: "Cluster work requires an authorized capability.",
			SkillIDs: []string{skill.ID}, Priority: 50,
		}},
	}
	if err := ValidateCapabilityCatalog(catalog); err != nil {
		t.Fatalf("typed binding configuration was treated as semantic incompatibility: %v", err)
	}
	question := RefinementQuestion{
		ID: "skill", Category: RefinementCategorySkill, Prompt: "Choose Skill", WhyNeeded: "Capability required",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Options: []RefinementQuestionOption{{ID: skill.ID, Label: "Kubernetes operations"}}},
		Priority: 1, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	if err := validateRefinementCatalog([]RefinementQuestion{question}, catalog); err != nil {
		t.Fatalf("typed binding configuration option was rejected: %v", err)
	}
}

func TestInstallableSkillOptionsRequireExactReceiptBackedEvidence(t *testing.T) {
	question := RefinementQuestion{
		ID: "skill", Category: RefinementCategorySkill, Prompt: "Choose Skill", WhyNeeded: "Capability required",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate}, Answer: RefinementAnswerSchema{Kind: RefinementAnswerSkillSelection, Options: []RefinementQuestionOption{{ID: "reddit", Label: "Reddit"}}},
		Priority: 1, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	skill := SkillCapability{ID: "reddit", Readiness: SkillReadinessNeedsInstallation}
	validate := func() error {
		return validateRefinementCatalog([]RefinementQuestion{question}, CapabilityCatalog{Skills: map[string]SkillCapability{"reddit": skill}})
	}
	if err := validate(); err == nil || !strings.Contains(err.Error(), "exact version and source identity") {
		t.Fatalf("missing exact identity error=%v", err)
	}
	skill.Version, skill.SourceIdentity = "1.2.3", "https://registry.example/@publisher/reddit"
	if err := validate(); err == nil || !strings.Contains(err.Error(), "referenced positive compatibility evidence") {
		t.Fatalf("missing receipt evidence error=%v", err)
	}
	skill.Compatibility = []SkillCompatibility{
		{Requirement: "installation", Compatible: false, Evidence: "verified artifact is not installed", Reference: "needs_installation"},
		{Requirement: "credential:reddit-oauth", Compatible: false, Evidence: "authorized credential still needs binding"},
		{Requirement: "source_digest", Compatible: true, Evidence: "verified compilation", Reference: "receipt:sha256:verified"},
	}
	if err := validate(); err != nil {
		t.Fatalf("receipt-backed option with truthful lifecycle gaps: %v", err)
	}
	skill.Compatibility = append(skill.Compatibility, SkillCompatibility{Requirement: "action_adapter", Compatible: false, Evidence: "governed adapter unavailable", Reference: "diagnostic:needs_action_adapter"})
	if err := validate(); err == nil || !strings.Contains(err.Error(), "incompatibility-proven") {
		t.Fatalf("incompatible option error=%v", err)
	}
}

func TestCapabilityCatalogDiagnosticsAreBoundedAndValidatedBeforeGeneration(t *testing.T) {
	catalog := CapabilityCatalog{Diagnostics: []CatalogDiagnostic{
		{Code: CatalogDiagnosticNoCompatibleCapability, Message: "No compatible capability was verified.", Reference: "reddit-research"},
		{Code: CatalogDiagnosticDiscoveryUnavailable, Message: "Authorized capability discovery is unavailable.", Reference: "language-analysis"},
		{Code: CatalogDiagnosticDiscoveryTimeout, Message: "The bounded verification deadline elapsed.", Reference: "reddit-research"},
		{Code: CatalogDiagnosticDiscoveryStale, Message: "Only stale verification evidence was found.", Reference: "language-analysis"},
	}}
	if err := ValidateCapabilityCatalog(catalog); err != nil {
		t.Fatalf("valid catalog diagnostics: %v", err)
	}

	tests := map[string]CapabilityCatalog{
		"invalid code":      {Diagnostics: []CatalogDiagnostic{{Code: "Timeout Now", Message: "Unavailable"}}},
		"empty message":     {Diagnostics: []CatalogDiagnostic{{Code: CatalogDiagnosticDiscoveryTimeout}}},
		"multiline message": {Diagnostics: []CatalogDiagnostic{{Code: CatalogDiagnosticDiscoveryTimeout, Message: "provider error\nraw detail"}}},
		"oversized message": {Diagnostics: []CatalogDiagnostic{{Code: CatalogDiagnosticDiscoveryTimeout, Message: strings.Repeat("x", 1025)}}},
		"raw URL reference": {Diagnostics: []CatalogDiagnostic{{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Unavailable", Reference: "https://registry.example/private?q=raw"}}},
		"padded reference":  {Diagnostics: []CatalogDiagnostic{{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Unavailable", Reference: " reddit-research "}}},
		"duplicate": {Diagnostics: []CatalogDiagnostic{
			{Code: CatalogDiagnosticDiscoveryTimeout, Message: "First", Reference: "reddit-research"},
			{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Second", Reference: "reddit-research"},
		}},
	}
	tooMany := CapabilityCatalog{Diagnostics: make([]CatalogDiagnostic, MaximumCatalogDiagnostics+1)}
	for index := range tooMany.Diagnostics {
		tooMany.Diagnostics[index] = CatalogDiagnostic{Code: CatalogDiagnosticDiscoveryTimeout, Message: "Unavailable", Reference: fmt.Sprintf("intent-%d", index)}
	}
	tests["too many"] = tooMany
	for name, invalid := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateCapabilityCatalog(invalid); err == nil {
				t.Fatal("invalid catalog diagnostics were accepted")
			}
		})
	}

	generator := &refinementGenerator{payloads: [][]byte{refinementPayload(t, "1")}}
	compiler, _ := NewCompiler(generator)
	if _, err := compiler.Compile(context.Background(), GenerateRequest{Mode: ModeCreate, Prompt: "Create a research Agent", Catalog: tests["invalid code"]}); err == nil {
		t.Fatal("invalid host catalog reached generation")
	}
	if len(generator.requests) != 0 {
		t.Fatalf("generator received an invalid host catalog: %#v", generator.requests)
	}
	service, _ := NewChangeSetService(compiler, NewMemoryChangeSetStore())
	if _, _, err := service.Prepare(context.Background(), CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a research Agent", Catalog: tests["invalid code"],
		Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "invalid-catalog",
	}); err == nil {
		t.Fatal("invalid host catalog was durably prepared")
	}
}

func TestRefinementSkillCategoryRequiresSkillSelectionAnswer(t *testing.T) {
	question := RefinementQuestion{
		ID: "skills", Category: RefinementCategorySkill, Prompt: "Choose Skills", WhyNeeded: "Capabilities are required",
		Blocking: []RefinementBlockingScope{RefinementBlocksCandidate},
		Answer:   RefinementAnswerSchema{Kind: RefinementAnswerMultiSelect, Options: []RefinementQuestionOption{{ID: "openseal.document", Label: "Document"}}},
		Priority: 1, Provenance: []RefinementQuestionProvenance{{Kind: RefinementProvenanceCatalog}},
	}
	err := validateRefinementQuestions([]RefinementQuestion{question})
	if err == nil || !strings.Contains(err.Error(), "unresolvedQuestions[0].answer.kind") || !strings.Contains(err.Error(), "skill_selection") {
		t.Fatalf("generic Skill question diagnostic=%v", err)
	}
}

func TestSkillReadinessBlocksCandidateUntilInstallationOrBindingCompletes(t *testing.T) {
	candidate := marketingCandidate("1", capability.RiskLevelRead)
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{
		"reddit-research": {ID: "reddit-research", Version: "1.0.0", Actions: []string{"search", "read"}, Readiness: SkillReadinessNeedsBinding},
	}}
	missing := missingRequirements(&candidate, catalog)
	if !hasMissingRequirementKind(missing, "skill_binding") {
		t.Fatalf("binding readiness=%#v", missing)
	}
	skill := catalog.Skills["reddit-research"]
	skill.Readiness = SkillReadinessNeedsInstallation
	catalog.Skills["reddit-research"] = skill
	missing = missingRequirements(&candidate, catalog)
	if !hasMissingRequirementKind(missing, "skill_installation") {
		t.Fatalf("installation readiness=%#v", missing)
	}
}

func hasMissingRequirementKind(values []MissingRequirement, kind string) bool {
	for _, value := range values {
		if value.Kind == kind {
			return true
		}
	}
	return false
}
