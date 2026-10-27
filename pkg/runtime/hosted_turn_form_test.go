package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func TestHostedTurnFormCompilesOnlyAuthorizedNativeWorkspaceOperations(t *testing.T) {
	var operations []workspace.Operation
	for _, operation := range workspace.Operations(workspace.ChatAuthority("chat")) {
		if operation.Name != workspace.OperationRunCommand {
			operations = append(operations, operation)
		}
	}
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
	action := capability.ModelAction{Name: "browser.click", BindingID: "browser-primary", BindingRevision: 3, InputSchema: map[string]interface{}{
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
	if response.ProposedAction.BindingID != action.BindingID || response.ProposedAction.BindingRevision != action.BindingRevision {
		t.Fatal("compiled action lost its authorized binding identity")
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

	schema, err = HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{CanForkSelf: true})
	if err != nil {
		t.Fatal(err)
	}
	properties = schema["properties"].(map[string]interface{})
	if _, exists := properties["proposedFork"]; !exists {
		t.Fatal("self-fork proposal was removed from schema")
	}
	if _, exists := properties["proposedDelegation"]; exists {
		t.Fatal("self-fork authority exposed delegation")
	}
}

func TestHostedTurnFormSchemaOffersExactSkillDispositions(t *testing.T) {
	schema, err := HostedTurnFormJSONSchema(nil, HostedTurnFormAuthority{
		SkillPromptReferences: []string{"skill:summarize@1", "skill:research@2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selections := schema["properties"].(map[string]interface{})["skillSelections"].(map[string]interface{})
	// Omitted dispositions are recorded as not applied by the kernel.
	if _, bounded := selections["minItems"]; bounded || selections["maxItems"] != 2 {
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

func testAnswerVisual() *presentation.Visual {
	return &presentation.Visual{Title: "Your phone plan", Components: []presentation.Component{
		{ID: "saving", Type: "stat_row", Summary: "Switching saves £96 a year.", StatRow: &presentation.StatRow{Stats: []presentation.Stat{
			{Label: "Yearly saving", Value: 96, Format: presentation.StatCurrency, Currency: "GBP"},
		}}},
	}}
}

func answerVisualForm(visual *presentation.Visual) HostedTurnForm {
	return HostedTurnForm{
		SchemaVersion: HostedTurnFormSchemaVersion, SkillSelections: []HostedSkillSelection{}, Decisions: []TurnDecision{},
		OutputSummary: "Not quite.", Visual: visual, ContinuationCheckpoint: map[string]interface{}{}, NextRunStatus: AgentRunStatusCompleted,
		RunOutput: map[string]interface{}{"report": "Not quite."}, CompletionEvidenceRefs: []string{}, EvidenceClaims: []EvidenceClaim{},
	}
}

func TestHostedTurnFormSchemaOffersAnswerVisual(t *testing.T) {
	schema, err := HostedTurnFormJSONSchema(nil)
	if err != nil {
		t.Fatal(err)
	}
	visual, ok := schema["properties"].(map[string]interface{})["visual"].(map[string]interface{})
	if !ok || visual["additionalProperties"] != false || visual["properties"].(map[string]interface{})["components"] == nil {
		t.Fatalf("visual schema = %#v", visual)
	}
	description, _ := visual["description"].(string)
	if !strings.Contains(description, presentation.VisualRubric()) || !strings.Contains(description, "userAsks") {
		t.Fatalf("visual description = %q", description)
	}
	for _, required := range schema["required"].([]interface{}) {
		if required == "visual" {
			t.Fatal("visual must stay optional")
		}
	}
	encoded, _ := json.Marshal(visual)
	lower := strings.ToLower(string(encoded))
	for _, word := range []string{"presentation", "surface", "artifact", "publish"} {
		if strings.Contains(lower, word) {
			t.Fatalf("visual schema mentions %q", word)
		}
	}
	if strings.Contains(string(mustMarshalSchema(t, schema)), "presentation_") {
		t.Fatal("reflected schema still carries presentation definitions")
	}
}

func mustMarshalSchema(t *testing.T, schema map[string]interface{}) []byte {
	t.Helper()
	encoded, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestHostedTurnFormCompilesAnswerVisual(t *testing.T) {
	response, err := CompileHostedTurnForm(answerVisualForm(testAnswerVisual()), nil)
	if err != nil || response.Visual == nil || response.Visual.Title != "Your phone plan" {
		t.Fatalf("response=%#v error=%v", response, err)
	}
	form, err := HostedTurnFormFromResponse(*response)
	if err != nil || !reflect.DeepEqual(form.Visual, testAnswerVisual()) {
		t.Fatalf("round trip = %#v, %v", form.Visual, err)
	}
	response.Visual.Components[0].ID = "changed"
	if form.Visual.Components[0].ID != "saving" {
		t.Fatal("round trip shares component storage")
	}

	action := capability.ModelAction{Name: "notes.save", InputSchema: map[string]interface{}{"type": "object"}}
	bound := testAnswerVisual()
	bound.Components = append(bound.Components, presentation.Component{ID: "next", Type: "actions", Actions: []presentation.Action{
		{ID: "stop", Label: "Stop", Kind: presentation.ActionEntity, Command: "stop", Binding: &presentation.Binding{Kind: presentation.BindingRunbook, ID: "rb-1"}},
	}})
	for name, tc := range map[string]struct {
		form    func() HostedTurnForm
		options []HostedTurnFormOption
		want    string
	}{
		"with action": {form: func() HostedTurnForm {
			form := answerVisualForm(testAnswerVisual())
			form.NextRunStatus = AgentRunStatusRunning
			form.ProposedAction = &HostedTurnActionForm{Capability: action.Name, Summary: "Save", IdempotencyKey: "save", Arguments: map[string]interface{}{}}
			return form
		}, want: "leave visual out when you propose"},
		"with task": {form: func() HostedTurnForm {
			form := answerVisualForm(testAnswerVisual())
			form.NextRunStatus, form.RunOutput, form.OutputSummary = AgentRunStatusRunning, map[string]interface{}{}, ""
			form.ProposedTask = &TurnTaskProposal{TaskKey: "t", Goal: "g", Acknowledgment: "On it."}
			return form
		}, want: "leave visual out when you propose"},
		"still running": {form: func() HostedTurnForm {
			form := answerVisualForm(testAnswerVisual())
			form.NextRunStatus = AgentRunStatusRunning
			return form
		}, want: "set nextRunStatus to completed"},
		"invalid": {form: func() HostedTurnForm {
			form := answerVisualForm(testAnswerVisual())
			form.Visual.Components[0].Summary = ""
			return form
		}, want: "visual: components[0]"},
		"binding rejected by host": {form: func() HostedTurnForm { return answerVisualForm(bound) }, options: []HostedTurnFormOption{
			WithVisualHostChecks(context.Background(), presentation.ValidateOptions{Bindings: presentation.BindingResolverFunc(func(context.Context, presentation.Binding) error {
				return errors.New("not part of this chat")
			})}),
		}, want: "not part of this chat"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CompileHostedTurnForm(tc.form(), []capability.ModelAction{action}, tc.options...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			lower := strings.ToLower(err.Error())
			for _, word := range []string{"presentation", "surface", "artifact", "publish"} {
				if strings.Contains(lower, word) {
					t.Fatalf("model-facing error mentions %q: %v", word, err)
				}
			}
		})
	}
	// Without host hooks compilation checks structure only; the runner
	// rechecks the binding with the host's resolver.
	if _, err := CompileHostedTurnForm(answerVisualForm(bound), nil); err != nil {
		t.Fatalf("structural compile rejected a bound visual: %v", err)
	}
}
