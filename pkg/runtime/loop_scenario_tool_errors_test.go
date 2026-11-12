package runtime

// Run-loop scenarios for tool errors (run-loop section 12 §9, tests-T16). The
// old hosted form refused an unauthorized capability or bad arguments by
// rejecting the whole form and counting corrections. On the loop each bad call
// is one tool entry in error that the model reads and answers on the next
// step; the Run goes on. The completion checks of turn_final_answer work the
// same way: the rejected final is an error tool result, never a posted answer.

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// toolErrorEntries is the Run's tool entries named name, in log order.
func toolErrorEntries(f *loopFixture, runID, name string) []*SessionEntry {
	f.t.Helper()
	var out []*SessionEntry
	for _, entry := range f.toolEntries(runID) {
		if entry.Tool != nil && entry.Tool.Name == name {
			out = append(out, entry)
		}
	}
	return out
}

// requireToolError checks that entry ended in error with an error result
// whose text contains every want.
func requireToolError(t *testing.T, entry *SessionEntry, want ...string) {
	t.Helper()
	if entry == nil || entry.State != ToolEntryError || entry.Tool == nil || entry.Tool.Result == nil || !entry.Tool.Result.IsError {
		t.Fatalf("tool entry = %#v, want an error result", entry)
	}
	text := toolEntryText(entry)
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Fatalf("tool error %q does not contain %q", text, w)
		}
	}
}

// sawToolError matches a step whose newest tool result for name is an error.
func sawToolError(name string) func(stepView) bool {
	return func(v stepView) bool {
		if v.Purpose() != ModelStepPurposeStep {
			return false
		}
		_, isError, ok := v.ToolResult(name)
		return ok && isError
	}
}

// TestLoopUnofferedToolIsAToolError: a call to a tool the step did not offer
// (an unknown name, a governed action the binding does not allow, a host tool
// without a Workspace) becomes an error tool entry. Nothing is dispatched,
// the model sees the error and answers, and the Run completes.
func TestLoopUnofferedToolIsAToolError(t *testing.T) {
	for _, name := range []string{
		"delete_everything",
		kitGovernedToolPrefix + loopFixtureSkillID + "_delete_all",
		hostToolName("read_file"),
	} {
		t.Run(name, func(t *testing.T) {
			eachStore(t, "unoffered_tool", func(t *testing.T, store KernelStore) {
				f := newLoopFixture(t, store, withRules(
					&modelRule{name: "answer", when: sawToolError(name), reply: func(v stepView) ModelStepResult {
						return say("I can't do that here.")
					}},
					&modelRule{name: "call", when: stepPurpose, times: 1, reply: func(v stepView) ModelStepResult {
						if v.HasTool(name) {
							t.Errorf("the step offered %s", name)
						}
						return act("Trying.", call(name, map[string]any{}))
					}},
				))
				conversation, run := f.startChat("clean everything up")
				done := f.driveToRest(run.ID, 3)
				f.mustStatus(done, AgentRunStatusCompleted)

				entries := toolErrorEntries(f, run.ID, name)
				if len(entries) != 1 {
					t.Fatalf("tool entries for %s = %d, want 1 (log %v)", name, len(entries), f.entryTypes(run.ID))
				}
				requireToolError(t, entries[0])
				if got := f.dispatch.count(""); got != 0 {
					t.Fatalf("%d actions dispatched for an unoffered tool", got)
				}
				for _, call := range f.calls(run.ID) {
					if call.Status == ActionCallStatusSucceeded || call.Status == ActionCallStatusRunning {
						t.Fatalf("unoffered tool produced a live action call %#v", call)
					}
				}
				if replies := f.agentReplies(conversation.ID); !slices.Equal(replies, []string{"I can't do that here."}) {
					t.Fatalf("replies = %q", replies)
				}
				if got := len(f.model.calls(ModelStepPurposeStep)); got != 2 {
					t.Fatalf("model steps = %d, want 2", got)
				}
			})
		})
	}
}

// TestLoopInvalidArgumentsAreAToolError: arguments outside the tool's schema,
// or that are not a JSON object at all, end the call in error with the
// reason; nothing is dispatched for it. The model fixes the call on the next
// step and that one runs.
func TestLoopInvalidArgumentsAreAToolError(t *testing.T) {
	type badCall struct {
		name   string
		result func(v stepView) ModelStepResult
		want   []string
	}
	lookup := func(v stepView, args string) ModelStepResult {
		return rawCall("Looking it up.", kitGovernedTool(v, "lookup"), args)
	}
	cases := []badCall{
		{name: "wrong_type", result: func(v stepView) ModelStepResult { return lookup(v, `{"id":7}`) }},
		{name: "unknown_field", result: func(v stepView) ModelStepResult { return lookup(v, `{"id":"7","extra":true}`) }},
		{name: "missing_required", result: func(v stepView) ModelStepResult { return lookup(v, `{}`) }},
		{name: "not_an_object", want: []string{"not a JSON object"}, result: func(v stepView) ModelStepResult {
			return ModelStepResult{Narration: "Looking it up.", Assistant: AssistantEntry{
				Text: "Looking it up.", StopReason: ModelStopToolCalls,
				ToolCalls: []ModelToolCall{{Name: kitGovernedTool(v, "lookup"), Arguments: json.RawMessage(`{}`), ArgumentsError: "The arguments were not a JSON object."}},
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eachStore(t, "invalid_arguments", func(t *testing.T, store KernelStore) {
				var tool string
				f := newLoopFixture(t, store, withRules(
					&modelRule{name: "bad", when: stepPurpose, times: 1, reply: func(v stepView) ModelStepResult {
						tool = kitGovernedTool(v, "lookup")
						return tc.result(v)
					}},
					&modelRule{name: "fix", when: func(v stepView) bool { return sawToolError(tool)(v) }, times: 1, reply: func(v stepView) ModelStepResult {
						return act("Fixing the call.", call(tool, map[string]any{"id": "7"}))
					}},
					&modelRule{name: "answer", when: stepPurpose, reply: func(v stepView) ModelStepResult {
						if content, isError, ok := v.ToolResult(tool); !ok || isError || !strings.Contains(content, "Issue 7") {
							t.Errorf("fixed lookup result = %q error=%v ok=%v", content, isError, ok)
						}
						return say("Issue 7 is open.")
					}},
				))
				conversation, run := f.startChat("what is the status of 7?")
				done := f.driveToRest(run.ID, 3)
				f.mustStatus(done, AgentRunStatusCompleted)

				entries := toolErrorEntries(f, run.ID, tool)
				if len(entries) != 2 {
					t.Fatalf("lookup entries = %d, want 2 (log %v)", len(entries), f.entryTypes(run.ID))
				}
				requireToolError(t, entries[0], tc.want...)
				if entries[1].State != ToolEntryCompleted {
					t.Fatalf("fixed lookup = %s: %q", entries[1].State, toolEntryText(entries[1]))
				}
				if got := f.dispatch.count("lookup"); got != 1 {
					t.Fatalf("lookup dispatched %d times, want only the fixed call", got)
				}
				if replies := f.agentReplies(conversation.ID); !slices.Equal(replies, []string{"Issue 7 is open."}) {
					t.Fatalf("replies = %q", replies)
				}
			})
		})
	}

	t.Run("control_tool", func(t *testing.T) {
		eachStore(t, "invalid_control_arguments", func(t *testing.T, store KernelStore) {
			final := string(ControlToolFinalAnswer)
			f := newLoopFixture(t, store, withRules(
				&modelRule{name: "fixed", when: sawToolError(final), reply: func(stepView) ModelStepResult {
					return finalAnswer(map[string]any{"answer": "All good."})
				}},
				&modelRule{name: "bad", when: stepPurpose, times: 1, reply: func(stepView) ModelStepResult {
					return finalAnswer(map[string]any{"answer": 5})
				}},
			))
			conversation, run := f.startChat("how is it going?")
			done := f.driveToRest(run.ID, 3)
			f.mustStatus(done, AgentRunStatusCompleted)
			entries := toolErrorEntries(f, run.ID, final)
			if len(entries) != 2 {
				t.Fatalf("final entries = %d, want 2 (log %v)", len(entries), f.entryTypes(run.ID))
			}
			requireToolError(t, entries[0])
			if entries[1].State != ToolEntryCompleted {
				t.Fatalf("fixed final = %s: %q", entries[1].State, toolEntryText(entries[1]))
			}
			if replies := f.agentReplies(conversation.ID); !slices.Equal(replies, []string{"All good."}) {
				t.Fatalf("replies = %q", replies)
			}
		})
	})
}

// toolErrorConnectorDefinition is a binding-tools Skill: every binding
// declares its own tools over one base action.
func toolErrorConnectorDefinition() *skill.Definition {
	input := map[string]interface{}{
		"type": "object", "additionalProperties": false,
		"properties": map[string]interface{}{
			"tool": map[string]interface{}{"type": "string"}, "hash": map[string]interface{}{"type": "string"},
			"arguments": map[string]interface{}{"type": "object"},
		},
		"required": []interface{}{"tool", "hash", "arguments"},
	}
	return &skill.Definition{
		ID: "openseal.connector", Version: "1.0.0", Name: "Connector", Transport: skill.TransportReference{Kind: "local", Endpoint: "internal://connector"},
		BindingTools: &skill.BindingToolsSpec{ModelPrefix: "connector", ReadAction: "read", InputArgument: "arguments", FixedArguments: []string{"tool", "hash"}},
		Actions: map[string]skill.Action{"read": {Name: "read", Description: "Call a read-only tool", InputSchema: input,
			Risk: skill.RiskLevelRead, SideEffect: skill.SideEffectRead, Idempotency: skill.IdempotencySupported}},
	}
}

// TestLoopToolNamesAreUniqueAcrossBindings: two bindings of one binding-tools
// Skill each offer their own "search". The step offers one tool per binding
// under distinct names (no name twice), and a call of either name runs on
// exactly that binding with that binding's fixed arguments.
func TestLoopToolNamesAreUniqueAcrossBindings(t *testing.T) {
	eachStore(t, "binding_tool_names", func(t *testing.T, store KernelStore) {
		names := map[string]string{} // slug -> offered tool name
		f := newLoopFixture(t, store, withRules(
			&modelRule{name: "linear", when: stepPurpose, times: 1, reply: func(v stepView) ModelStepResult {
				seen := map[string]bool{}
				for _, tool := range v.Request().Tools {
					if seen[tool.Name] {
						t.Errorf("tool %s offered twice", tool.Name)
					}
					seen[tool.Name] = true
				}
				for _, slug := range []string{"github", "linear"} {
					if !v.HasTool(names[slug]) {
						t.Errorf("the %s search (%s) is not offered: %s", slug, names[slug], v.describe())
					}
				}
				return act("Searching Linear.", call(names["linear"], map[string]any{"query": "open issues"}))
			}},
			&modelRule{name: "github", when: stepPurpose, times: 1, reply: func(v stepView) ModelStepResult {
				return act("Searching GitHub.", call(names["github"], map[string]any{"query": "open pulls"}))
			}},
			&modelRule{name: "answer", when: stepPurpose, reply: func(v stepView) ModelStepResult {
				for slug, want := range map[string]string{"linear": "linear results", "github": "github results"} {
					if content, isError, ok := v.ToolResult(names[slug]); !ok || isError || !strings.Contains(content, want) {
						t.Errorf("%s search result = %q error=%v ok=%v", slug, content, isError, ok)
					}
				}
				return say("Both searched.")
			}},
		))

		// Bind the connector twice to finn, beside fixture@1.0.0.
		if err := f.catalog.Register(f.ctx, toolErrorConnectorDefinition()); err != nil {
			t.Fatal(err)
		}
		skillScope := skill.ScopeReference{Kind: f.scope.Kind, ID: f.scope.ID}
		for _, slug := range []string{"github", "linear"} {
			if err := f.catalog.Bind(f.ctx, &skill.Binding{
				ID: slug + "-binding", Scope: skillScope, DeploymentID: loopFixtureAgent,
				SkillID: "openseal.connector", SkillVersion: "1.0.0", AllowedActions: []string{"read"}, MaximumRisk: skill.RiskLevelRead, Revision: 1,
				Slug: slug, Tools: []skill.BindingTool{{
					Name: "search", Description: "Search " + slug, Effect: skill.BindingToolEffectRead,
					InputSchema: map[string]interface{}{"type": "object", "properties": map[string]interface{}{"query": map[string]interface{}{"type": "string"}}, "required": []interface{}{"query"}},
					Arguments:   map[string]interface{}{"tool": "search", "hash": "sha256:" + slug},
				}},
			}); err != nil {
				t.Fatal(err)
			}
		}
		offered, err := f.catalog.ListModelActions(f.ctx, skillScope, loopFixtureAgent)
		if err != nil {
			t.Fatal(err)
		}
		f.offered = offered
		toolNames := LoopGovernedToolNames(offered)
		bySlug := map[string]string{}
		for _, action := range offered {
			if action.BindingTool != nil {
				bySlug[strings.TrimSuffix(action.BindingID, "-binding")] = action.Name
			}
		}
		for slug, action := range bySlug {
			names[slug] = toolNames[action]
		}
		if names["github"] == "" || names["linear"] == "" || names["github"] == names["linear"] {
			t.Fatalf("binding tool names = %v (actions %v)", names, bySlug)
		}
		f.dispatch.on("read", func(in ActionDispatchInput) (map[string]interface{}, error) {
			hash, _ := in.Arguments["hash"].(string)
			slug := strings.TrimPrefix(hash, "sha256:")
			if in.Call == nil || in.Call.BindingID != slug+"-binding" {
				t.Errorf("the %s search ran on binding %#v", slug, in.Call)
			}
			arguments, _ := in.Arguments["arguments"].(map[string]interface{})
			return map[string]interface{}{"results": slug + " results", "query": arguments["query"]}, nil
		})

		conversation, run := f.startChat("search both trackers")
		done := f.driveToRest(run.ID, 3)
		f.mustStatus(done, AgentRunStatusCompleted)

		calls := f.calls(run.ID)
		if len(calls) != 2 {
			t.Fatalf("action calls = %d, want 2", len(calls))
		}
		got := []string{calls[0].BindingID, calls[1].BindingID}
		slices.Sort(got)
		if !slices.Equal(got, []string{"github-binding", "linear-binding"}) || calls[0].Status != ActionCallStatusSucceeded || calls[1].Status != ActionCallStatusSucceeded {
			t.Fatalf("calls ran on %v (%s, %s)", got, calls[0].Status, calls[1].Status)
		}
		for _, call := range calls {
			if call.Origin != ActionOriginLoop {
				t.Fatalf("call %s origin = %s", call.ID, call.Origin)
			}
		}
		if f.dispatch.count("read") != 2 {
			t.Fatalf("read dispatched %d times", f.dispatch.count("read"))
		}
		if replies := f.agentReplies(conversation.ID); !slices.Equal(replies, []string{"Both searched."}) {
			t.Fatalf("replies = %q", replies)
		}
	})
}

