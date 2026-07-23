package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
	"go.uber.org/zap"
)

type authoringFixtureGenerator struct{}

func (authoringFixtureGenerator) Generate(context.Context, authoring.GenerateRequest) ([]byte, error) {
	return []byte(`{"candidate":{"agents":[],"assignments":[]},"unresolvedQuestions":[{"id":"team-responsibilities","category":"other","prompt":"Which responsibilities should this Team own?","whyNeeded":"The Team needs an explicit purpose.","blocking":["candidate"],"answer":{"kind":"text"},"provenance":[{"kind":"prompt"}],"priority":1}]}`), nil
}

type failingAuthoringGenerator struct{}

func (failingAuthoringGenerator) Generate(context.Context, authoring.GenerateRequest) ([]byte, error) {
	return nil, errors.New("provider temporarily unavailable")
}

func TestStandaloneAuthoringFailureIsRetryableAndReplaySafe(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(failingAuthoringGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	api.SetWorkforceLifecycleAuthorizer(StandaloneRetryAuthorizer{ActorID: "operator-one"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := api.StartWorkforceAuthoringWorker(ctx, runtime.Scope{Kind: "local", ID: "research"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := api.StartWorkforceAuthoringWorker(ctx, runtime.Scope{Kind: "local", ID: "research"}, ""); err == nil {
		t.Fatal("duplicate worker start succeeded")
	}
	defer api.Shutdown(context.Background())
	body := `{"scope":{"kind":"local","id":"research"},"prompt":"Create a Team","catalog":{},"actor":{"type":"user","id":"forged"}}`
	created := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets", body, "create-failing")
	if created.Code != http.StatusAccepted {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	var changeSet authoring.ChangeSet
	if err := json.NewDecoder(created.Body).Decode(&changeSet); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		loaded, _ := api.authoringChanges.Get(context.Background(), changeSet.Scope, changeSet.ID)
		if loaded.Status == authoring.ChangeSetFailed {
			changeSet = *loaded
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if changeSet.Status != authoring.ChangeSetFailed {
		t.Fatalf("status = %s", changeSet.Status)
	}
	capabilityPath := "/api/v1/capabilities?scopeKind=local&scopeId=research&changeSetId=" + changeSet.ID
	if response := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", ""); !strings.Contains(response.Body.String(), `"retry"`) {
		t.Fatalf("retry capability = %s", response.Body.String())
	}
	retry := authoring.RetryChangeSetGenerationRequest{Scope: changeSet.Scope, ChangeSetID: changeSet.ID, ExpectedRevision: changeSet.Revision, Reason: "provider recovered", Actor: authoring.ChangeSetActor{Type: "forged", ID: "browser"}}
	first := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+changeSet.ID+"/retry", mustJSON(t, retry), "retry-stable")
	if first.Code != http.StatusAccepted || !strings.Contains(first.Body.String(), `"id":"operator-one"`) {
		t.Fatalf("retry = %d %s", first.Code, first.Body.String())
	}
	replay := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+changeSet.ID+"/retry", mustJSON(t, retry), "retry-stable")
	if replay.Code != http.StatusOK {
		t.Fatalf("replay = %d %s", replay.Code, replay.Body.String())
	}
}

type governedAuthoringGenerator struct{}

func (governedAuthoringGenerator) Generate(context.Context, authoring.GenerateRequest) ([]byte, error) {
	agent := &kernelagent.AgentDefinition{ID: "researcher", Version: "1", DisplayName: "Researcher", Purpose: "Collect evidence", SystemPrompt: "Collect attributed evidence.", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}}
	team := &kernelteam.Definition{ID: "research", Version: "1", DisplayName: "Research", Purpose: "Synthesize evidence",
		Roles:        []kernelteam.RoleSlot{{ID: "researcher", DisplayName: "Researcher", Purpose: "Collect evidence", MinimumMembers: 1, RequiredDefinitionIDs: []string{agent.ID}, ChannelParticipation: kernelteam.RoleChannelActive}},
		Coordination: kernelteam.CoordinationPolicy{Mode: kernelteam.CoordinationDynamic, MaximumSpeakersPerRound: 1, QuietByDefault: true, RequireRoleRelevance: true, SuppressDuplicateContent: true},
		Delegation:   kernelteam.DelegationPolicy{MaximumDepth: 1, MaximumConcurrent: 1, RequireAcceptance: true}, Approvals: kernelteam.ApprovalPolicy{MaximumRisk: capability.RiskLevelRead}}
	return json.Marshal(authoring.GenerationResponse{Candidate: authoring.WorkforceCandidate{Agents: []*kernelagent.AgentDefinition{agent}, Team: team,
		Assignments: []authoring.Assignment{{ID: "researcher", RoleID: "researcher", AgentDefinitionID: agent.ID, DisplayName: agent.DisplayName}}}})
}

type governedFixtureAuthority struct {
	role          string
	actor         string
	bindingFields []capability.BindingConfigurationFieldChoice
}

func (a governedFixtureAuthority) AuthorizeWorkforceLifecycle(_ context.Context, operation string, changeSet *authoring.ChangeSet) (WorkforceLifecycleAuthorization, error) {
	switch operation {
	case kernelapi.OperationEvaluate:
		return WorkforceLifecycleAuthorization{Actor: authoring.ChangeSetActor{Type: "policy_evaluator", ID: "configured-policy"}}, nil
	case kernelapi.OperationApprove:
		result := WorkforceLifecycleAuthorization{Actor: authoring.ChangeSetActor{Type: "user", ID: a.actor}}
		if len(changeSet.Evaluations) > 0 {
			evaluation := changeSet.Evaluations[len(changeSet.Evaluations)-1]
			result.EligibleApprovalRequirements = []kernelapi.ApprovalRequirementReference{{EvaluationID: evaluation.ID, PolicyID: "production", Role: a.role}}
		}
		return result, nil
	case kernelapi.OperationApply:
		return WorkforceLifecycleAuthorization{Actor: authoring.ChangeSetActor{Type: "user", ID: a.actor}}, nil
	case kernelapi.OperationPatch:
		return WorkforceLifecycleAuthorization{Actor: authoring.ChangeSetActor{Type: "user", ID: a.actor}, BindingConfigurationFields: a.bindingFields}, nil
	case kernelapi.OperationRefine:
		return WorkforceLifecycleAuthorization{Actor: authoring.ChangeSetActor{Type: "user", ID: a.actor}}, nil
	default:
		return WorkforceLifecycleAuthorization{}, errors.New("unsupported operation")
	}
}

type bindingConfigAuthoringGenerator struct{}

func (bindingConfigAuthoringGenerator) Generate(ctx context.Context, request authoring.GenerateRequest) ([]byte, error) {
	encoded, err := (governedAuthoringGenerator{}).Generate(ctx, request)
	if err != nil {
		return nil, err
	}
	var response authoring.GenerationResponse
	if err := json.Unmarshal(encoded, &response); err != nil {
		return nil, err
	}
	response.Candidate.Agents[0].SkillRequirements = []kernelagent.SkillRequirement{{SkillID: "openseal.kubernetes", VersionConstraint: "1.1.0", RequiredActions: []string{"list_events"}}}
	return json.Marshal(response)
}

func TestWorkforcePlacementPatchIsContextualAuthorizedAndIdempotent(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "placement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(governedAuthoringGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	api.SetWorkforceLifecycleAuthorizer(governedFixtureAuthority{actor: "configured-operator"})
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, err := api.authoringChanges.Create(context.Background(), authoring.CreateChangeSetRequest{
		Scope: scope, Prompt: "Create a governed research Team", IdempotencyKey: "create-placement",
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "research-live", AgentDeploymentIDs: map[string]string{"researcher": "researcher-live"}, Environment: "development"},
		Actor:     authoring.ChangeSetActor{Type: "user", ID: "requester"},
	})
	if err != nil {
		t.Fatal(err)
	}
	capabilityPath := "/api/v1/capabilities?scopeKind=tenant&scopeId=one&changeSetId=" + created.ID
	contextual := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if !strings.Contains(contextual.Body.String(), `"patch"`) || !strings.Contains(contextual.Body.String(), `"revision":1`) {
		t.Fatalf("placement capability = %s", contextual.Body.String())
	}
	request := authoring.UpdateChangeSetPlacementRequest{
		Scope: scope, ChangeSetID: created.ID, ExpectedRevision: created.Revision, Reason: "Use production placement",
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "research-live", AgentDeploymentIDs: map[string]string{"researcher": "researcher-live"}, Environment: "production"},
		Actor:     authoring.ChangeSetActor{Type: "forged", ID: "browser"},
	}
	path := "/api/v1/authoring/workforce/change-sets/" + created.ID + "/placement"
	updatedResponse := performAgentRunRequest(t, api.Handler(), http.MethodPatch, path, mustJSON(t, request), "placement-stable")
	var updated authoring.ChangeSet
	if updatedResponse.Code != http.StatusCreated || json.NewDecoder(updatedResponse.Body).Decode(&updated) != nil || updated.Revision != 2 || updated.Placement.Environment != "production" || updated.PlacementUpdates[0].Actor.ID != "configured-operator" {
		t.Fatalf("updated placement = %d %s", updatedResponse.Code, updatedResponse.Body.String())
	}
	replay := performAgentRunRequest(t, api.Handler(), http.MethodPatch, path, mustJSON(t, request), "placement-stable")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), updated.PlacementUpdates[0].ID) {
		t.Fatalf("placement replay = %d %s", replay.Code, replay.Body.String())
	}
}

func TestWorkforceCapabilityAdvertisesOnlySchemaMatchedBindingConfigurationFields(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "binding-config-capability.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(bindingConfigAuthoringGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	development := int64(7)
	field := capability.BindingConfigurationFieldChoice{
		CatalogSkillID: "openseal.kubernetes",
		Skill:          capability.NewSkillIdentity("openseal.kubernetes", "1.1.0", "builtin:openseal.kubernetes"),
		Key:            "clusterId", Type: "integer", Required: true, Prompt: "Which Kubernetes cluster should this Agent operate?",
		Options: []capability.BindingConfigurationOption{{Label: "Development", Value: capability.BindingConfigurationValue{Integer: &development}}},
	}
	api.SetWorkforceLifecycleAuthorizer(governedFixtureAuthority{actor: "configured-operator", bindingFields: []capability.BindingConfigurationFieldChoice{field}})
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	created, _, err := api.authoringChanges.Create(context.Background(), authoring.CreateChangeSetRequest{
		Scope: scope, Prompt: "Create an SRE Team", IdempotencyKey: "create-binding-config-capability",
		Catalog: authoring.CapabilityCatalog{Skills: map[string]authoring.SkillCapability{"openseal.kubernetes": {
			ID: "openseal.kubernetes", Version: "1.1.0", SourceIdentity: "builtin:openseal.kubernetes", Readiness: authoring.SkillReadinessReady,
			Actions: []string{"list_events"}, BindingConfigSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"clusterId"},
				"properties": map[string]interface{}{"clusterId": map[string]interface{}{"type": "integer", "minimum": 1}},
			},
		}}},
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "research-live", AgentDeploymentIDs: map[string]string{"researcher": "researcher-live"}, Environment: "development"},
		Actor:     authoring.ChangeSetActor{Type: "user", ID: "requester"},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/capabilities?scopeKind=tenant&scopeId=one&changeSetId=" + created.ID
	response := performAgentRunRequest(t, api.Handler(), http.MethodGet, path, "", "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"bindingConfigurationFields"`) || !strings.Contains(response.Body.String(), `"integer":7`) || strings.Contains(response.Body.String(), `"credentialBindings"`) {
		t.Fatalf("binding configuration capability = %d %s", response.Code, response.Body.String())
	}

	invalid := field
	invalid.Key = "apiToken"
	api.SetWorkforceLifecycleAuthorizer(governedFixtureAuthority{actor: "configured-operator", bindingFields: []capability.BindingConfigurationFieldChoice{invalid}})
	response = performAgentRunRequest(t, api.Handler(), http.MethodGet, path, "", "")
	if strings.Contains(response.Body.String(), `"bindingConfigurationFields"`) || !strings.Contains(response.Body.String(), `"patch"`) {
		t.Fatalf("invalid binding configuration capability was not filtered = %s", response.Body.String())
	}
}

func TestWorkforceChangeSetAPIIsDurableScopedAndIdempotent(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "kernel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(authoringFixtureGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := api.StartWorkforceAuthoringWorker(ctx, runtime.Scope{Kind: "tenant", ID: "one"}, "api-test"); err != nil {
		t.Fatal(err)
	}
	defer api.Shutdown(context.Background())
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	if !strings.Contains(capabilities.Body.String(), `"operations":["compile","propose","get"]`) {
		t.Fatalf("change set capability = %s", capabilities.Body.String())
	}
	body := `{"scope":{"kind":"tenant","id":"one"},"prompt":"Create a research Team","catalog":{},"placement":{"teamDeploymentId":"research-live","agentDeploymentIds":{}},"actor":{"type":"user","id":"7"}}`
	created := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets", body, "intent-one")
	if created.Code != http.StatusAccepted {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var changeSet authoring.ChangeSet
	if err := json.NewDecoder(created.Body).Decode(&changeSet); err != nil || changeSet.ID == "" ||
		changeSet.Status != authoring.ChangeSetEvaluating && changeSet.Status != authoring.ChangeSetBlocked {
		t.Fatalf("change set = %#v, err = %v", changeSet, err)
	}
	replay := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets", body, "intent-one")
	if replay.Code != http.StatusOK || !strings.Contains(replay.Body.String(), changeSet.ID) {
		t.Fatalf("replay status = %d, body = %s", replay.Code, replay.Body.String())
	}
	var loaded *httptest.ResponseRecorder
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		loaded = performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/authoring/workforce/change-sets/"+changeSet.ID+"?scopeKind=tenant&scopeId=one", "", "")
		if strings.Contains(loaded.Body.String(), `"status":"blocked"`) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if loaded == nil || loaded.Code != http.StatusOK || !strings.Contains(loaded.Body.String(), `"status":"blocked"`) {
		t.Fatalf("load status = %d, body = %s", loaded.Code, loaded.Body.String())
	}
	var blocked authoring.ChangeSet
	if err := json.NewDecoder(strings.NewReader(loaded.Body.String())).Decode(&blocked); err != nil || blocked.Refinement.NextQuestion() == nil {
		t.Fatalf("blocked refinement = %#v, err = %v", blocked.Refinement, err)
	}
	api.SetWorkforceLifecycleAuthorizer(governedFixtureAuthority{actor: "configured-operator"})
	contextual := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities?scopeKind=tenant&scopeId=one&changeSetId="+url.QueryEscape(blocked.ID), "", "")
	if !strings.Contains(contextual.Body.String(), `"refine"`) {
		t.Fatalf("contextual refinement capability = %s", contextual.Body.String())
	}
	question := blocked.Refinement.NextQuestion()
	answer := authoring.AnswerChangeSetRefinementRequest{
		Scope: blocked.Scope, ChangeSetID: blocked.ID, ExpectedRevision: blocked.Revision, QuestionID: question.ID,
		Value: authoring.RefinementAnswerValue{Text: "Own evidence-backed product research"}, Source: authoring.RefinementAnswerSourceRuntime,
		Actor: authoring.ChangeSetActor{Type: "user", ID: "forged"}, IdempotencyKey: "body-key-ignored",
	}
	refined := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+blocked.ID+"/refinements", mustJSON(t, answer), "answer-refinement")
	if refined.Code != http.StatusAccepted || !strings.Contains(refined.Body.String(), `"status":"evaluating"`) || !strings.Contains(refined.Body.String(), `"configured-operator"`) || !strings.Contains(refined.Body.String(), `"source":"user"`) {
		t.Fatalf("refined = %d %s", refined.Code, refined.Body.String())
	}
	replayedAnswer := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+blocked.ID+"/refinements", mustJSON(t, answer), "answer-refinement")
	if replayedAnswer.Code != http.StatusOK || !strings.Contains(replayedAnswer.Body.String(), `"answer-refinement"`) {
		t.Fatalf("refinement replay = %d %s", replayedAnswer.Code, replayedAnswer.Body.String())
	}
	foreign := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/authoring/workforce/change-sets/"+changeSet.ID+"?scopeKind=tenant&scopeId=two", "", "")
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status = %d, body = %s", foreign.Code, foreign.Body.String())
	}
}

func TestWorkforceAuthoringAPIIsTruthfulAndNonActivating(t *testing.T) {
	api := NewServer(runtime.NewMemoryStore(10), zap.NewNop().Sugar())
	unconfigured := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/compile", `{"mode":"create","prompt":"Create a Team","catalog":{}}`, "")
	if unconfigured.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured status = %d, body = %s", unconfigured.Code, unconfigured.Body.String())
	}

	compiler, err := authoring.NewCompiler(authoringFixtureGenerator{})
	if err != nil {
		t.Fatal(err)
	}
	api.SetWorkforceAuthoringCompiler(compiler)
	capabilities := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.NewDecoder(capabilities.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.WorkforceAuthoringCapabilityID, kernelapi.WorkforceAuthoringCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationCompile) || capability.Supports(kernelapi.OperationActivate) {
		t.Fatalf("authoring capability = %#v", capability)
	}

	compiled := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/compile", `{"mode":"create","prompt":"Create a Team","catalog":{}}`, "")
	if compiled.Code != http.StatusOK {
		t.Fatalf("compile status = %d, body = %s", compiled.Code, compiled.Body.String())
	}
	var result authoring.CompileResult
	if err := json.NewDecoder(compiled.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Valid || len(result.UnresolvedQuestions) != 1 || len(result.Validation) == 0 {
		t.Fatalf("compile result = %#v", result)
	}
}

func TestGovernedWorkforceLifecycleIsContextualExactAndAtomic(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "governed.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(governedAuthoringGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	createRequest := authoring.CreateChangeSetRequest{Scope: scope, Prompt: "Create a governed research Team",
		Placement: authoring.ChangeSetPlacement{TeamDeploymentID: "research-live", AgentDeploymentIDs: map[string]string{"researcher": "researcher-live"}, Environment: "production"},
		Actor:     authoring.ChangeSetActor{Type: "user", ID: "requester"}}
	createRequest.IdempotencyKey = "create-governed"
	createdPointer, _, createErr := api.authoringChanges.Create(context.Background(), createRequest)
	if createErr != nil || createdPointer.Status != authoring.ChangeSetReview {
		t.Fatalf("created = %#v err=%v", createdPointer, createErr)
	}
	created := *createdPointer
	capabilityPath := "/api/v1/capabilities?scopeKind=tenant&scopeId=one&changeSetId=" + created.ID
	unconfigured := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if strings.Contains(unconfigured.Body.String(), `"evaluate"`) || strings.Contains(unconfigured.Body.String(), `"approve"`) || strings.Contains(unconfigured.Body.String(), `"apply"`) || !strings.Contains(unconfigured.Body.String(), `"revision":1`) {
		t.Fatalf("unconfigured capability = %s", unconfigured.Body.String())
	}
	bodyOnly := authoring.SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: 1, CandidateDigest: created.CandidateDigest, Allowed: true, Actor: authoring.ChangeSetActor{Type: "forged", ID: "browser"}, IdempotencyKey: "body-only"}
	bodyOnlyResponse := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/evaluations", mustJSON(t, bodyOnly), "")
	if bodyOnlyResponse.Code != http.StatusBadRequest {
		t.Fatalf("body-only idempotency = %d %s", bodyOnlyResponse.Code, bodyOnlyResponse.Body.String())
	}
	unconfiguredMutation := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/evaluations", mustJSON(t, bodyOnly), "evaluation-unconfigured")
	if unconfiguredMutation.Code != http.StatusForbidden {
		t.Fatalf("unconfigured lifecycle authority = %d %s", unconfiguredMutation.Code, unconfiguredMutation.Body.String())
	}

	api.SetWorkforceLifecycleAuthorizer(governedFixtureAuthority{role: "operator", actor: "configured-operator"})
	reviewCapability := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if !strings.Contains(reviewCapability.Body.String(), `"evaluate"`) || strings.Contains(reviewCapability.Body.String(), `"approve"`) || strings.Contains(reviewCapability.Body.String(), `"apply"`) {
		t.Fatalf("review capability = %s", reviewCapability.Body.String())
	}
	evaluationRequest := bodyOnly
	evaluationRequest.ApprovalRequirements = []authoring.ChangeSetApprovalRequirement{{PolicyID: "production", Role: "operator", Count: 1}, {PolicyID: "production", Role: "security", Count: 1}}
	evaluationResponse := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/evaluations", mustJSON(t, evaluationRequest), "evaluation-header")
	var evaluated authoring.ChangeSet
	if evaluationResponse.Code != http.StatusCreated || json.NewDecoder(evaluationResponse.Body).Decode(&evaluated) != nil || evaluated.Status != authoring.ChangeSetAwaitingApproval || evaluated.Evaluations[0].Actor.ID != "configured-policy" || evaluated.Evaluations[0].IdempotencyKey != "evaluation-header" {
		t.Fatalf("evaluated = %d %s", evaluationResponse.Code, evaluationResponse.Body.String())
	}
	approvalCapability := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if strings.Contains(approvalCapability.Body.String(), `"evaluate"`) || !strings.Contains(approvalCapability.Body.String(), `"approve"`) || strings.Contains(approvalCapability.Body.String(), `"role":"security"`) || !strings.Contains(approvalCapability.Body.String(), `"revision":2`) {
		t.Fatalf("approval capability = %s", approvalCapability.Body.String())
	}
	approval := authoring.ResolveChangeSetApprovalRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: evaluated.Revision, EvaluationID: evaluated.Evaluations[0].ID, PolicyID: "production", Role: "security", Approved: true, Actor: authoring.ChangeSetActor{Type: "forged", ID: "browser"}}
	forgedApproval := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/approvals", mustJSON(t, approval), "forged-approval")
	if forgedApproval.Code != http.StatusForbidden {
		t.Fatalf("forged approval = %d %s", forgedApproval.Code, forgedApproval.Body.String())
	}
	approval.Role = "operator"
	approvedResponse := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/approvals", mustJSON(t, approval), "approval-header")
	var approved authoring.ChangeSet
	if approvedResponse.Code != http.StatusCreated || json.NewDecoder(approvedResponse.Body).Decode(&approved) != nil || approved.Status != authoring.ChangeSetAwaitingApproval || approved.ApprovalDecisions[0].Actor.ID != "configured-operator" {
		t.Fatalf("approved = %d %s", approvedResponse.Code, approvedResponse.Body.String())
	}
	// The remaining security requirement is intentionally not granted to this
	// principal, so Apply must remain absent and the exact decided requirement
	// must disappear from its contextual capability.
	afterDecision := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if strings.Contains(afterDecision.Body.String(), `"approve"`) || strings.Contains(afterDecision.Body.String(), `"apply"`) || !strings.Contains(afterDecision.Body.String(), `"revision":3`) {
		t.Fatalf("post-decision capability = %s", afterDecision.Body.String())
	}
	api.SetWorkforceLifecycleAuthorizer(governedFixtureAuthority{role: "security", actor: "configured-security"})
	securityCapability := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if !strings.Contains(securityCapability.Body.String(), `"approve"`) || !strings.Contains(securityCapability.Body.String(), `"role":"security"`) || strings.Contains(securityCapability.Body.String(), `"role":"operator"`) {
		t.Fatalf("security capability = %s", securityCapability.Body.String())
	}
	approval.ExpectedRevision = approved.Revision
	approval.Role = "security"
	securityApproval := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/approvals", mustJSON(t, approval), "security-approval")
	var ready authoring.ChangeSet
	if securityApproval.Code != http.StatusCreated || json.NewDecoder(securityApproval.Body).Decode(&ready) != nil || ready.Status != authoring.ChangeSetReady || ready.Revision != 4 {
		t.Fatalf("security approval = %d %s", securityApproval.Code, securityApproval.Body.String())
	}
	readyCapability := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if !strings.Contains(readyCapability.Body.String(), `"apply"`) || strings.Contains(readyCapability.Body.String(), `"approve"`) || strings.Contains(readyCapability.Body.String(), `"evaluate"`) || !strings.Contains(readyCapability.Body.String(), `"revision":4`) {
		t.Fatalf("ready capability = %s", readyCapability.Body.String())
	}
	apply := authoring.ApplyChangeSetRequest{Scope: scope, ChangeSetID: created.ID, ExpectedRevision: 3, CandidateDigest: ready.CandidateDigest, Reason: "Create the reviewed workforce", Actor: authoring.ChangeSetActor{Type: "forged", ID: "browser"}}
	staleApply := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/apply", mustJSON(t, apply), "apply-stable")
	if staleApply.Code != http.StatusConflict {
		t.Fatalf("stale apply = %d %s", staleApply.Code, staleApply.Body.String())
	}
	apply.ExpectedRevision = ready.Revision
	appliedResponse := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/apply", mustJSON(t, apply), "apply-stable")
	var applied authoring.ChangeSet
	if appliedResponse.Code != http.StatusCreated || json.NewDecoder(appliedResponse.Body).Decode(&applied) != nil || applied.Status != authoring.ChangeSetApplied || applied.ApplyReceipt == nil || len(applied.ApplyReceipt.Resources) < 4 || applied.ApplyReceipt.Actor.ID != "configured-security" || applied.ApplyReceipt.Reason != apply.Reason {
		t.Fatalf("applied = %d %s", appliedResponse.Code, appliedResponse.Body.String())
	}
	replayApply := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+created.ID+"/apply", mustJSON(t, apply), "apply-stable")
	if replayApply.Code != http.StatusOK || !strings.Contains(replayApply.Body.String(), applied.ApplyReceipt.ID) {
		t.Fatalf("apply replay = %d %s", replayApply.Code, replayApply.Body.String())
	}
	appliedCapability := performAgentRunRequest(t, api.Handler(), http.MethodGet, capabilityPath, "", "")
	if strings.Contains(appliedCapability.Body.String(), `"evaluate"`) || strings.Contains(appliedCapability.Body.String(), `"approve"`) || strings.Contains(appliedCapability.Body.String(), `"apply"`) || !strings.Contains(appliedCapability.Body.String(), `"revision":5`) {
		t.Fatalf("applied capability = %s", appliedCapability.Body.String())
	}
	rejectedCreate := createRequest
	rejectedCreate.Prompt = "Create a rejected research Team"
	rejectedCreate.IdempotencyKey = "create-rejected"
	rejectedPointer, _, rejectedErr := api.authoringChanges.Create(context.Background(), rejectedCreate)
	if rejectedErr != nil {
		t.Fatal(rejectedErr)
	}
	rejectedCandidate := *rejectedPointer
	denial := authoring.SubmitChangeSetEvaluationRequest{Scope: scope, ChangeSetID: rejectedCandidate.ID, ExpectedRevision: rejectedCandidate.Revision, CandidateDigest: rejectedCandidate.CandidateDigest, Allowed: false, Findings: []authoring.ChangeSetPolicyFinding{{PolicyID: "production", Code: "denied", Message: "Production policy denied the candidate."}}, Actor: authoring.ChangeSetActor{Type: "forged", ID: "browser"}}
	deniedResponse := performAgentRunRequest(t, api.Handler(), http.MethodPost, "/api/v1/authoring/workforce/change-sets/"+rejectedCandidate.ID+"/evaluations", mustJSON(t, denial), "deny-header")
	var rejected authoring.ChangeSet
	if deniedResponse.Code != http.StatusCreated || json.NewDecoder(deniedResponse.Body).Decode(&rejected) != nil || rejected.Status != authoring.ChangeSetRejected || rejected.Evaluations[0].Actor.ID != "configured-policy" {
		t.Fatalf("denied = %d %s", deniedResponse.Code, deniedResponse.Body.String())
	}
	rejectedCapability := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities?scopeKind=tenant&scopeId=one&changeSetId="+rejected.ID, "", "")
	if strings.Contains(rejectedCapability.Body.String(), `"evaluate"`) || strings.Contains(rejectedCapability.Body.String(), `"approve"`) || strings.Contains(rejectedCapability.Body.String(), `"apply"`) {
		t.Fatalf("rejected capability = %s", rejectedCapability.Body.String())
	}
}

func TestWorkforceCapabilityDoesNotAdvertiseEvaluationBeforeCandidateExists(t *testing.T) {
	store, err := runtime.NewSQLiteStore(filepath.Join(t.TempDir(), "pending.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := NewServer(store, zap.NewNop().Sugar())
	compiler, _ := authoring.NewCompiler(governedAuthoringGenerator{})
	api.SetWorkforceAuthoringCompiler(compiler)
	api.SetWorkforceLifecycleAuthorizer(governedFixtureAuthority{role: "operator", actor: "configured-operator"})
	prepared, _, err := api.authoringChanges.Prepare(context.Background(), authoring.CreateChangeSetRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "pending"}, Prompt: "Create a research Team",
		Actor: authoring.ChangeSetActor{Type: "user", ID: "requester"}, IdempotencyKey: "prepare-1",
	})
	if err != nil || prepared.Status != authoring.ChangeSetEvaluating || prepared.CandidateDigest != "" {
		t.Fatalf("prepared=%#v err=%v", prepared, err)
	}
	response := performAgentRunRequest(t, api.Handler(), http.MethodGet, "/api/v1/capabilities?scopeKind=tenant&scopeId=pending&changeSetId="+prepared.ID, "", "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"evaluate"`) || !strings.Contains(response.Body.String(), `"revision":1`) {
		t.Fatalf("pre-generation capability = %d %s", response.Code, response.Body.String())
	}
}

func mustJSON(t *testing.T, value interface{}) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
