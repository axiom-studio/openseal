package runtime

import (
	"reflect"
	"slices"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestHostedTurnFormCompilesOnlyAuthorizedNativeWorkspaceOperations(t *testing.T) {
	authority := &workspace.Authority{Workspace: workspace.DefaultSpec()}
	operations := workspace.Operations(authority)
	form := HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, SkillSelections: []HostedSkillSelection{}, Decisions: []TurnDecision{},
		ProposedWorkspaceOperation: &HostedWorkspaceOperationForm{Operation: workspace.OperationReadFile, Summary: "Inspect config", Arguments: map[string]interface{}{"path": "config.yaml"}},
		ContinuationCheckpoint:     map[string]interface{}{}, NextRunStatus: AgentRunStatusRunning, RunOutput: map[string]interface{}{}, CompletionEvidenceRefs: []string{}, EvidenceClaims: []EvidenceClaim{},
	}
	response, err := CompileHostedTurnFormWithWorkspace(form, nil, operations)
	if err != nil || response.ProposedWorkspaceOperation == nil || response.ProposedWorkspaceOperation.Operation != workspace.OperationReadFile {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	form.ProposedWorkspaceOperation.Operation = workspace.OperationRunCommand
	if _, err := CompileHostedTurnFormWithWorkspace(form, nil, operations); err == nil {
		t.Fatal("disabled native command operation was accepted")
	}
	schema, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{WorkspaceOperations: operations})
	if err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]interface{})
	if properties["proposedWorkspaceOperation"] == nil {
		t.Fatalf("Workspace operation schema = %#v", properties)
	}
}

func TestHostedTurnFormCompilesAndRoundTripsExactActionArguments(t *testing.T) {
	action := capability.ModelAction{Name: "browser.click", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"target": map[string]interface{}{"type": "string", "pattern": "^s[1-9][0-9]*:e[1-9][0-9]*$"},
			"intent": map[string]interface{}{"type": "string"},
		},
		"required": []interface{}{"target", "intent"},
	}}
	form := HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, SkillSelections: []HostedSkillSelection{}, Decisions: []TurnDecision{},
		OutputSummary: "Open a real post", ContinuationCheckpoint: map[string]interface{}{"kept": true},
		NextRunStatus: AgentRunStatusRunning, RunOutput: map[string]interface{}{}, CompletionEvidenceRefs: []string{}, EvidenceClaims: []EvidenceClaim{},
		ProposedAction: &HostedTurnActionForm{
			Capability: action.Name, Summary: "Open the selected post", IdempotencyKey: "open-current-post",
			Arguments: map[string]interface{}{"target": "s3:e40", "intent": "Read the selected post"},
		},
	}
	response, err := CompileHostedTurnForm(form, []capability.ModelAction{action})
	if err != nil {
		t.Fatal(err)
	}
	if response.ProposedAction == nil || response.ProposedAction.InputRef != "/actionInputs/proposed" || response.ProposedAction.Capability != action.Name {
		t.Fatalf("response = %#v", response)
	}
	roundTrip, err := HostedTurnFormFromResponse(*response)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.ProposedAction.Arguments, form.ProposedAction.Arguments) {
		t.Fatalf("arguments = %#v", roundTrip.ProposedAction.Arguments)
	}
	if _, leaked := form.ContinuationCheckpoint["actionInputs"]; leaked {
		t.Fatal("compilation mutated the provider form checkpoint")
	}
}

func TestHostedTurnFormProjectsEnvelopeIdempotencyIntoActionContract(t *testing.T) {
	action := capability.ModelAction{
		Name: "browser.fill", SkillID: "browser", Version: "1", Action: "fill",
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{
				"target":         map[string]interface{}{"type": "string"},
				"idempotencyKey": map[string]interface{}{"type": "string", "minLength": 1},
			},
			"required": []interface{}{"target", "idempotencyKey"},
		},
	}
	form := HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion,
		ProposedAction: &HostedTurnActionForm{
			Capability: action.Name, Summary: "Fill the reviewed draft",
			IdempotencyKey: "turn-fill-1", Arguments: map[string]interface{}{"target": "s1:e42"},
		},
		NextRunStatus: AgentRunStatusRunning,
	}
	response, err := CompileHostedTurnForm(form, []capability.ModelAction{action})
	if err != nil {
		t.Fatal(err)
	}
	arguments := response.ContinuationCheckpoint["actionInputs"].(map[string]interface{})["proposed"].(map[string]interface{})
	if arguments["idempotencyKey"] != "turn-fill-1" {
		t.Fatalf("projected arguments = %#v", arguments)
	}
	if _, modelMutated := form.ProposedAction.Arguments["idempotencyKey"]; modelMutated {
		t.Fatalf("model form was mutated: %#v", form.ProposedAction.Arguments)
	}
}

func TestHostedTurnFormRejectsMissingOrInventedActionValues(t *testing.T) {
	action := capability.ModelAction{Name: "browser.click", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"target": map[string]interface{}{"type": "string"}},
		"required":   []interface{}{"target"},
	}}
	base := HostedTurnForm{SchemaVersion: HostedTurnFormSchemaVersion, OutputSummary: "open", NextRunStatus: AgentRunStatusRunning}
	base.ProposedAction = &HostedTurnActionForm{Capability: action.Name, Summary: "open", IdempotencyKey: "open-1", Arguments: map[string]interface{}{}}
	if _, err := CompileHostedTurnForm(base, []capability.ModelAction{action}); err == nil {
		t.Fatal("missing evidence-derived target was accepted")
	}
	base.ProposedAction.Arguments = map[string]interface{}{"target": "invented", "authority": "widened"}
	if _, err := CompileHostedTurnForm(base, []capability.ModelAction{action}); err == nil {
		t.Fatal("unknown action input was accepted")
	}
}

func TestHostedTurnFormSchemaUsesExactAuthorizedActionInputs(t *testing.T) {
	action := capability.ModelAction{Name: "browser.click", InputSchema: map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{"target": map[string]interface{}{"type": "string", "pattern": "^s[1-9]"}},
		"required":   []interface{}{"target"},
	}}
	schema, err := HostedTurnFormJSONSchema([]capability.ModelAction{action})
	if err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]interface{})
	if properties["schemaVersion"].(map[string]interface{})["const"] != HostedTurnFormSchemaVersion {
		t.Fatalf("schema version = %#v", properties["schemaVersion"])
	}
	statuses := properties["nextRunStatus"].(map[string]interface{})["enum"].([]string)
	if slices.Contains(statuses, "waiting") || !slices.Contains(statuses, string(AgentRunStatusWaitingForDependency)) {
		t.Fatalf("lifecycle statuses = %#v", statuses)
	}
	branches := properties["proposedAction"].(map[string]interface{})["oneOf"].([]interface{})
	arguments := branches[0].(map[string]interface{})["properties"].(map[string]interface{})["arguments"].(map[string]interface{})
	if arguments["additionalProperties"] != false || arguments["properties"].(map[string]interface{})["target"].(map[string]interface{})["pattern"] != "^s[1-9]" {
		t.Fatalf("arguments schema = %#v", arguments)
	}
}

func TestHostedTurnFormRequiresAndRoundTripsExternalApprovalReviewContext(t *testing.T) {
	action := capability.ModelAction{
		Name: "browser.commit", SideEffect: capability.SideEffectExternal,
		InputSchema: map[string]interface{}{
			"type": "object", "additionalProperties": false,
			"properties": map[string]interface{}{"target": map[string]interface{}{"type": "string"}},
			"required":   []interface{}{"target"},
		},
	}
	form := HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, OutputSummary: "Publish", NextRunStatus: AgentRunStatusRunning,
		ProposedAction: &HostedTurnActionForm{
			Capability: action.Name, Summary: "Publish reviewed comment", IdempotencyKey: "comment-42",
			Arguments: map[string]interface{}{"target": "s5:e67"},
		},
	}
	if _, err := CompileHostedTurnForm(form, []capability.ModelAction{action}); err == nil {
		t.Fatal("external action without approval review context was accepted")
	}
	form.ProposedAction.ReviewContext = &ApprovalReviewContext{
		Summary: "Post this exact comment", Target: "https://forum.example/posts/42",
		Audience: "Public forum readers", Content: "The exact proposed comment.", Purpose: "Answer the question",
		Consequences: []string{"Creates a public comment"},
		Facts:        []ApprovalReviewFact{{Label: "Community", Value: "example"}},
	}
	response, err := CompileHostedTurnForm(form, []capability.ModelAction{action})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := HostedTurnFormFromResponse(*response)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(roundTrip.ProposedAction.ReviewContext, form.ProposedAction.ReviewContext) {
		t.Fatalf("review context = %#v", roundTrip.ProposedAction.ReviewContext)
	}
	schema, err := HostedTurnFormJSONSchema([]capability.ModelAction{action})
	if err != nil {
		t.Fatal(err)
	}
	branch := schema["properties"].(map[string]interface{})["proposedAction"].(map[string]interface{})["oneOf"].([]interface{})[0].(map[string]interface{})
	required := branch["required"].([]string)
	if !slices.Contains(required, "reviewContext") {
		t.Fatalf("external action schema does not require reviewContext: %#v", required)
	}
}

func TestHostedTurnFormRejectsAmbiguousOrUnwakeableWaitingStatus(t *testing.T) {
	base := HostedTurnForm{SchemaVersion: HostedTurnFormSchemaVersion, OutputSummary: "wait"}
	base.NextRunStatus = AgentRunStatus("waiting")
	if _, err := CompileHostedTurnForm(base, nil); err == nil {
		t.Fatal("ambiguous waiting status was accepted")
	}
	base.NextRunStatus = AgentRunStatusWaitingForDependency
	if _, err := CompileHostedTurnForm(base, nil); err == nil {
		t.Fatal("waiting status without a wake condition was accepted")
	}
	base.WakeCondition = &WakeCondition{Type: "dependency", Reference: "child-run"}
	if _, err := CompileHostedTurnForm(base, nil); err != nil {
		t.Fatalf("typed waiting status was rejected: %v", err)
	}
}

func TestHostedTurnFormDerivesApprovalWaitFromGovernedAction(t *testing.T) {
	form := HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, OutputSummary: "Waiting for approval",
		NextRunStatus: AgentRunStatusWaitingForApproval,
		WakeCondition: &WakeCondition{Type: "approval", Reference: "model-invented-approval"},
	}
	response, err := CompileHostedTurnForm(form, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.NextRunStatus != AgentRunStatusRunning || response.WakeCondition != nil {
		t.Fatalf("model-authored approval wait was trusted: %#v", response)
	}
	if len(response.Decisions) != 1 || response.Decisions[0].Summary != "Continued the Run because approval state is derived from a governed action, not model output." {
		t.Fatalf("approval normalization was not audited: %#v", response.Decisions)
	}
	if form.NextRunStatus != AgentRunStatusWaitingForApproval || form.WakeCondition == nil {
		t.Fatalf("compiler mutated the caller form: %#v", form)
	}
}

func TestHostedTurnFormSchemaOmitsUnavailableProposalFamilies(t *testing.T) {
	schema, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	properties := schema["properties"].(map[string]interface{})
	for _, name := range []string{"proposedAction", "proposedDelegation", "proposedFork", "proposedRunbook"} {
		if _, exists := properties[name]; exists {
			t.Fatalf("unavailable proposal %s remains in schema", name)
		}
	}

	schema, err = HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{CanDelegate: true, CanInvokeRunbook: true})
	if err != nil {
		t.Fatal(err)
	}
	properties = schema["properties"].(map[string]interface{})
	for _, name := range []string{"proposedDelegation", "proposedFork", "proposedRunbook"} {
		if _, exists := properties[name]; !exists {
			t.Fatalf("authorized proposal %s was removed from schema", name)
		}
	}
}

func TestHostedTurnFormSchemaRequiresExactOfferedSkillDispositions(t *testing.T) {
	schema, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{
		SkillPromptReferences: []string{"skill:summarize@1", "skill:research@2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selections := schema["properties"].(map[string]interface{})["skillSelections"].(map[string]interface{})
	if selections["minItems"] != 2 || selections["maxItems"] != 2 {
		t.Fatalf("selection cardinality = %#v", selections)
	}
	branches := selections["items"].(map[string]interface{})["oneOf"].([]interface{})
	if got := branches[0].(map[string]interface{})["properties"].(map[string]interface{})["skillRef"].(map[string]interface{})["const"]; got != "skill:summarize@1" {
		t.Fatalf("first Skill reference = %#v", got)
	}
	if _, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{SkillPromptReferences: []string{"skill:summarize@1", "skill:summarize@1"}}); err == nil {
		t.Fatal("duplicate Skill prompt references were accepted")
	}
}
