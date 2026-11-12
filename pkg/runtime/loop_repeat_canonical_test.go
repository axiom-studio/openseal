package runtime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/activity"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

func decodeTestArguments(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var arguments map[string]interface{}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&arguments); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return arguments
}

func TestCanonicalToolArguments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"key order", `{"b":1,"a":{"d":2,"c":3}}`, `{"a":{"c":3,"d":2},"b":1}`},
		{"nulls removed recursively", `{"a":null,"b":{"c":null,"d":1},"e":[null,{"f":null}]}`, `{"b":{"d":1},"e":[null,{}]}`},
		{"integral float is integer", `{"a":1.0,"b":1,"c":1e3,"d":-0.0}`, `{"a":1,"b":1,"c":1000,"d":0}`},
		{"fraction keeps g format", `{"a":1.50,"b":0.1,"c":1e-7}`, `{"a":1.5,"b":0.1,"c":1e-07}`},
		{"big integer exact", `{"a":123456789012345678901234567890}`, `{"a":123456789012345678901234567890}`},
		{"control fields removed at top level", `{"_reviewSummary":"x","reviewContext":{"content":"y"},"evidenceRefs":["e"],"idempotencyKey":"k","id":"LIN-1"}`, `{"id":"LIN-1"}`},
		{"control fields kept when nested", `{"body":{"idempotencyKey":"k"}}`, `{"body":{"idempotencyKey":"k"}}`},
		{"strings booleans and array order untouched", `{"s":" A<b>&c ","t":true,"f":false,"l":[3,1,2]}`, `{"f":false,"l":[3,1,2],"s":" A<b>&c ","t":true}`},
		{"empty", `{}`, `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := canonicalToolArguments(decodeTestArguments(t, tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonical = %s, want %s", got, tc.want)
			}
		})
	}
	nilArgs, err := canonicalToolArguments(nil)
	if err != nil || string(nilArgs) != `{}` {
		t.Fatalf("nil arguments = %s, %v", nilArgs, err)
	}
	// Go-typed values from the executor canonicalize like decoded JSON.
	typed, err := canonicalToolArguments(map[string]interface{}{"n": 2.0, "i": int64(7), "m": map[string]interface{}{"z": nil}})
	if err != nil || string(typed) != `{"i":7,"m":{},"n":2}` {
		t.Fatalf("typed arguments = %s, %v", typed, err)
	}
}

func TestRepeatKeyIncludesIdentityAndExternalOperation(t *testing.T) {
	key := func(subject RepeatSubject) string {
		t.Helper()
		value, err := repeatKey(subject)
		if err != nil {
			t.Fatal(err)
		}
		if len(value) != 64 {
			t.Fatalf("key %q is not hex sha256", value)
		}
		return value
	}
	identity := repeatGovernedIdentity("linear", "get_issue", "binding-1")
	if identity != "linear.get_issue@binding-1" {
		t.Fatalf("identity = %q", identity)
	}
	base := key(RepeatSubject{Identity: identity, Arguments: decodeTestArguments(t, `{"id":"LIN-6815","limit":1}`)})

	if same := key(RepeatSubject{Identity: identity, Arguments: decodeTestArguments(t, `{"limit":1.0,"id":"LIN-6815","idempotencyKey":"other","extra":null}`)}); same != base {
		t.Fatal("canonically equal arguments must give the same key")
	}
	if other := key(RepeatSubject{Identity: repeatGovernedIdentity("linear", "get_issue", "binding-2"), Arguments: decodeTestArguments(t, `{"id":"LIN-6815","limit":1}`)}); other == base {
		t.Fatal("a different binding must give a different key")
	}
	if other := key(RepeatSubject{Identity: identity, Arguments: decodeTestArguments(t, `{"id":"LIN-6816","limit":1}`)}); other == base {
		t.Fatal("different arguments must give a different key")
	}

	withOperation := func(resource, operation string) string {
		return key(RepeatSubject{Identity: identity, Arguments: decodeTestArguments(t, `{"id":"LIN-6815","limit":1}`), ExternalOperation: &activity.ExternalOperationIdentity{Resource: resource, Operation: operation}})
	}
	sent := withOperation("https://Example.com/a?b=2&a=1#frag", "send")
	if sent == base {
		t.Fatal("an external operation must change the key")
	}
	if withOperation("https://example.com/a?a=1&b=2", "send") != sent {
		t.Fatal("the external resource must be canonicalized")
	}
	if withOperation("https://example.com/a?a=1&b=2", "delete") == sent {
		t.Fatal("the external operation name must be part of the key")
	}
	if withOperation("https://example.com/other", "send") == sent {
		t.Fatal("the external resource must be part of the key")
	}

	// The separator keeps identity and arguments apart.
	if key(RepeatSubject{Identity: "a", Arguments: map[string]interface{}{}}) == key(RepeatSubject{Identity: "a{}", Arguments: map[string]interface{}{}}) {
		t.Fatal("identity must be delimited")
	}
	if _, err := repeatKey(RepeatSubject{Arguments: map[string]interface{}{}}); err == nil {
		t.Fatal("empty identity must be rejected")
	}
	if _, err := repeatKey(RepeatSubject{Identity: identity, ExternalOperation: &activity.ExternalOperationIdentity{Resource: "", Operation: "send"}}); err == nil {
		t.Fatal("invalid external operation must be rejected")
	}
}

func TestRepeatEffectOf(t *testing.T) {
	cases := []struct {
		name       string
		kind       LoopToolKind
		sideEffect capability.SideEffect
		repeatable bool
		operation  string
		want       RepeatEffect
	}{
		{"governed none", LoopToolGoverned, capability.SideEffectNone, false, repeatGovernedOperation("linear", "list"), RepeatEffectRead},
		{"governed read", LoopToolGoverned, capability.SideEffectRead, false, repeatGovernedOperation("linear", "get_issue"), RepeatEffectRead},
		{"governed write", LoopToolGoverned, capability.SideEffectWrite, false, repeatGovernedOperation("linear", "create_issue"), RepeatEffectWrite},
		{"governed external", LoopToolGoverned, capability.SideEffectExternal, false, repeatGovernedOperation("gmail", "send"), RepeatEffectWrite},
		{"governed destructive", LoopToolGoverned, capability.SideEffectDestructive, false, repeatGovernedOperation("gmail", "delete"), RepeatEffectWrite},
		{"governed unknown side effect fails closed", LoopToolGoverned, "", false, repeatGovernedOperation("x", "y"), RepeatEffectWrite},
		{"governed repeatable write", LoopToolGoverned, capability.SideEffectWrite, true, repeatGovernedOperation("live-browser", "click"), RepeatEffectStateful},
		{"governed repeatable read stays read", LoopToolGoverned, capability.SideEffectRead, true, repeatGovernedOperation("live-browser", "snapshot"), RepeatEffectRead},
		{"card credential", LoopToolGoverned, capability.SideEffectWrite, false, repeatGovernedOperation(models.SkillManagementSkillID, agentrun.SkillActionRequestCredential), RepeatEffectControl},
		{"card setup", LoopToolGoverned, capability.SideEffectWrite, false, repeatGovernedOperation(models.SkillManagementSkillID, agentrun.SkillActionRequestSetup), RepeatEffectControl},
		{"card connection", LoopToolGoverned, capability.SideEffectWrite, false, repeatGovernedOperation(models.SkillManagementSkillID, agentrun.SkillActionRequestConnection), RepeatEffectControl},
		{"same action name on another skill is not a card", LoopToolGoverned, capability.SideEffectWrite, false, repeatGovernedOperation("other", agentrun.SkillActionRequestCredential), RepeatEffectWrite},

		{"host list_directory", LoopToolHost, capability.SideEffectRead, false, workspace.OperationListDirectory, RepeatEffectRead},
		{"host read_file", LoopToolHost, capability.SideEffectRead, false, workspace.OperationReadFile, RepeatEffectRead},
		{"host search_files", LoopToolHost, capability.SideEffectRead, false, workspace.OperationSearchFiles, RepeatEffectRead},
		{"host batch_read", LoopToolHost, capability.SideEffectRead, false, workspace.OperationBatchRead, RepeatEffectRead},
		{"host list_chats", LoopToolHost, capability.SideEffectRead, false, workspace.OperationListChats, RepeatEffectRead},
		{"host write_file", LoopToolHost, capability.SideEffectWrite, false, workspace.OperationWriteFile, RepeatEffectStateful},
		{"host edit_file", LoopToolHost, capability.SideEffectWrite, false, workspace.OperationEditFile, RepeatEffectStateful},
		{"host delete_file", LoopToolHost, capability.SideEffectWrite, false, workspace.OperationDeleteFile, RepeatEffectStateful},
		{"host move_file", LoopToolHost, capability.SideEffectWrite, false, workspace.OperationMoveFile, RepeatEffectStateful},
		{"host copy_from_chat", LoopToolHost, capability.SideEffectWrite, false, workspace.OperationCopyFromChat, RepeatEffectStateful},
		{"host run_command", LoopToolHost, capability.SideEffectWrite, false, workspace.OperationRunCommand, RepeatEffectStateful},

		{"kernel fork", LoopToolKernel, "", false, "control_fork", RepeatEffectWrite},
		{"kernel delegate", LoopToolKernel, "", false, "agent_delegate", RepeatEffectWrite},
		{"kernel delegate reply", LoopToolKernel, "", false, "agent_delegate_reply", RepeatEffectWrite},
		{"kernel run_operation", LoopToolKernel, "", false, "run_operation", RepeatEffectWrite},
		{"kernel start_task", LoopToolKernel, "", false, "conversation_start_task", RepeatEffectWrite},
		{"kernel ask_user", LoopToolKernel, "", false, "ask_user", RepeatEffectWrite},
		{"kernel react", LoopToolKernel, "", false, "react", RepeatEffectWrite},
		{"kernel search_capabilities", LoopToolKernel, "", false, "search_capabilities", RepeatEffectRead},
		{"kernel load_capabilities", LoopToolKernel, "", false, "load_capabilities", RepeatEffectControl},
		{"kernel wait", LoopToolKernel, "", false, "wait", RepeatEffectControl},
		{"kernel final", LoopToolKernel, "", false, "turn_final_answer", RepeatEffectControl},
		{"kernel decision final", LoopToolKernel, "", false, "agent_request_decision", RepeatEffectControl},
		{"kernel review final", LoopToolKernel, "", false, "agent_request_review", RepeatEffectControl},
		{"kernel progress", LoopToolKernel, "", false, "turn_report_progress", RepeatEffectControl},
		{"kernel unknown is a write", LoopToolKernel, "", false, "new_tool", RepeatEffectWrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := repeatEffectOf(tc.kind, tc.sideEffect, tc.repeatable, tc.operation); got != tc.want {
				t.Fatalf("repeatEffectOf = %q, want %q", got, tc.want)
			}
			if !tc.want.Valid() {
				t.Fatalf("%q is not valid", tc.want)
			}
		})
	}
	if RepeatEffect("other").Valid() {
		t.Fatal("unknown effect must be invalid")
	}
}

func TestToolResultDigest(t *testing.T) {
	result := func(status models.ToolResultStatus, parts ...models.ContentPart) *models.ToolResult {
		return &models.ToolResult{Status: status, IsError: status != models.ToolResultSucceeded, Content: parts}
	}
	base := modelstep.ToolResultDigest(result(models.ToolResultSucceeded, models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{"b":1,"a":2.0}`)}))
	if len(base) != 64 {
		t.Fatalf("digest %q", base)
	}
	if modelstep.ToolResultDigest(result(models.ToolResultSucceeded, models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{"a":2,"b":1}`)})) != base {
		t.Fatal("canonically equal content must give the same digest")
	}
	if modelstep.ToolResultDigest(result(models.ToolResultFailed, models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{"a":2,"b":1}`)})) == base {
		t.Fatal("status must be part of the digest")
	}
	if modelstep.ToolResultDigest(result(models.ToolResultSucceeded, models.ContentPart{Type: models.ContentPartJSON, Value: json.RawMessage(`{"a":3,"b":1}`)})) == base {
		t.Fatal("content must be part of the digest")
	}
	if modelstep.ToolResultDigest(result(models.ToolResultSucceeded)) != modelstep.ToolResultDigest(&models.ToolResult{Status: models.ToolResultSucceeded, Content: []models.ContentPart{}}) {
		t.Fatal("nil and empty content must digest alike")
	}
	if modelstep.ToolResultDigest(nil) != "" {
		t.Fatal("nil result has no digest")
	}
}
