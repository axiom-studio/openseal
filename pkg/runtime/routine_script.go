package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/chatpath"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

// A routine can run a saved script: a chat file the Agent wrote and tested
// with workspace.run_command. Approving the routine's brief pins the file as
// it was then. Each occurrence is an ordinary agent run in the chat whose
// model runs exactly that version with run_command and then reports (see
// hosted_turn_routine_script.go). The script changing later in the chat
// never changes what the routine runs; the routine shows the drift and the
// user can approve the new version (set_script).

const (
	// MaximumRoutineScriptSeconds bounds one run of the script: a chat
	// sandbox command never runs longer.
	MaximumRoutineScriptSeconds = workspace.ChatCommandSeconds
	// DefaultRoutineScriptSeconds applies when the proposal names no timeout.
	DefaultRoutineScriptSeconds = 300
	// maximumRoutineScriptCommand bounds the exact approved command.
	maximumRoutineScriptCommand = 2000

	// RoutineScriptContextKey carries an occurrence's pinned script in its
	// Run context, written once by the kernel when the occurrence starts.
	RoutineScriptContextKey = "routineScript"
	// routineScriptFailureKind is the final-failure kind of a script that
	// failed or did not run.
	routineScriptFailureKind = "script"
)

// RoutineScript is the exact script a routine runs. Path, Command and
// TimeoutSeconds come from the approved proposal; Digest and FilesVersion are
// pinned by the kernel from the chat's files when the routine is proposed,
// never taken from model input.
type RoutineScript struct {
	// Path is the script's normalized path in the routine's chat files.
	Path string `json:"path"`
	// Digest is the content hash (sha256:<hex>) of the approved version.
	Digest string `json:"digest"`
	// FilesVersion is the immutable chat files snapshot the approved
	// version was read from; that snapshot holds Path at Digest.
	FilesVersion int64 `json:"filesVersion"`
	// Command runs only Path: python3, node or bash, then Path, then plain
	// arguments.
	Command string `json:"command"`
	// TimeoutSeconds bounds the run, at most MaximumRoutineScriptSeconds.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// ApprovedAt is when the user's approval of this version was applied
	// (create_task or set_script). It is set by the kernel, never proposed,
	// and is not part of the approved arguments.
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
}

var (
	routineScriptDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// A plain word needs no shell quoting: no spaces, quotes, globbing,
	// expansion, redirection, pipes or command separators.
	routineScriptPlainWord    = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	routineScriptInterpreters = map[string]bool{"python3": true, "node": true, "bash": true}
)

func (s *RoutineScript) Validate() error {
	if s == nil {
		return errors.New("routine script is required")
	}
	if err := validateRoutineScriptPath(s.Path); err != nil {
		return err
	}
	if !routineScriptDigest.MatchString(s.Digest) || s.FilesVersion < 1 {
		return errors.New("routine script must be pinned to a version of a chat file")
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > MaximumRoutineScriptSeconds {
		return fmt.Errorf("routine script timeout must be 1 to %d seconds", MaximumRoutineScriptSeconds)
	}
	command, err := normalizeRoutineScriptCommand(s.Command, s.Path)
	if err != nil {
		return err
	}
	if command != s.Command {
		return errors.New("routine script command is not normalized")
	}
	return nil
}

func cloneRoutineScript(s *RoutineScript) *RoutineScript {
	if s == nil {
		return nil
	}
	copy := *s
	if s.ApprovedAt != nil {
		approved := *s.ApprovedAt
		copy.ApprovedAt = &approved
	}
	return &copy
}

func validateRoutineScriptPath(value string) error {
	clean, err := chatpath.Clean(value)
	if err != nil || clean != value {
		return errors.New("routine script path must be a normalized path in this chat's files, such as scripts/report.py")
	}
	if !routineScriptPlainWord.MatchString(value) || strings.HasPrefix(value, "-") {
		return errors.New("routine script path may use only letters, digits and - _ . / so it can be run as is")
	}
	return nil
}

// normalizeRoutineScriptCommand returns the command with single spaces when
// it runs only path: an interpreter, path, then plain arguments.
func normalizeRoutineScriptCommand(command, path string) (string, error) {
	if len(command) > maximumRoutineScriptCommand {
		return "", fmt.Errorf("routine script command is longer than %d bytes", maximumRoutineScriptCommand)
	}
	for _, r := range command {
		if r < 0x20 && r != ' ' && r != '\t' || r == 0x7f {
			return "", errors.New("routine script command must be one line")
		}
	}
	fields := strings.Fields(command)
	if len(fields) < 2 || !routineScriptInterpreters[fields[0]] || fields[1] != path {
		return "", fmt.Errorf("routine script command must run only %s: python3 %s, node %s or bash %s, optionally followed by plain arguments", path, path, path, path)
	}
	for _, argument := range fields[2:] {
		if !routineScriptPlainWord.MatchString(argument) {
			return "", fmt.Errorf("routine script argument %q is not plain: use only letters, digits and - _ . / = : , @ %% +, with no quotes, spaces or shell characters", argument)
		}
	}
	return strings.Join(fields, " "), nil
}

// ChatFile is one file in a chat's files as the host's authoritative store
// holds it.
type ChatFile struct {
	Path         string
	Digest       string
	SizeBytes    int64
	FilesVersion int64
}

// ErrChatFileNotFound means a chat's files have no file at a path.
var ErrChatFileNotFound = errors.New("chat file not found")

// ChatFileResolver reads a chat's files from the host's authoritative store
// (see pkg/chatfs). version 0 reads the latest snapshot; any other version
// reads that immutable snapshot. The chat is identified by its persisted
// conversation and owner, never by model input.
type ChatFileResolver interface {
	ResolveChatFile(ctx context.Context, scope Scope, owner ObjectiveOwner, conversationID, path string, version int64) (*ChatFile, error)
}

// routineScriptProposal is what a model proposes: digest and filesVersion
// are always replaced by the kernel.
type routineScriptProposal struct {
	Path           string `json:"path"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
	Digest         string `json:"digest,omitempty"`
	FilesVersion   int64  `json:"filesVersion,omitempty"`
}

// pinRoutineScriptArguments resolves a proposed create_task script against
// the chat's current files and writes the pinned script back into the
// arguments, so the digested, approved arguments carry exactly the version
// the brief shows. Any digest the model supplied is discarded.
func pinRoutineScriptArguments(ctx context.Context, files ChatFileResolver, run *AgentRun, arguments map[string]interface{}) error {
	raw, present := arguments["script"]
	if !present || raw == nil {
		delete(arguments, "script")
		return nil
	}
	object, ok := raw.(map[string]interface{})
	if !ok {
		return errors.New("routine script must be an object with path and command")
	}
	var proposal routineScriptProposal
	if err := decodeScheduledTaskArguments(object, &proposal); err != nil {
		return fmt.Errorf("routine script is invalid: %w", err)
	}
	path := strings.TrimSpace(proposal.Path)
	if clean, err := chatpath.Clean(path); err == nil {
		path = clean
	}
	if err := validateRoutineScriptPath(path); err != nil {
		return err
	}
	command, err := normalizeRoutineScriptCommand(proposal.Command, path)
	if err != nil {
		return err
	}
	timeout := proposal.TimeoutSeconds
	if timeout == 0 {
		timeout = DefaultRoutineScriptSeconds
	}
	conversationID, _ := run.Context[conversationRunContextConversationID].(string)
	file, err := currentRoutineScriptFile(ctx, files, run.Scope, run.Owner, conversationID, path)
	if err != nil {
		return err
	}
	script := &RoutineScript{Path: path, Digest: file.Digest, FilesVersion: file.FilesVersion, Command: command, TimeoutSeconds: timeout}
	if err := script.Validate(); err != nil {
		return err
	}
	arguments["script"] = routineScriptMap(script)
	return nil
}

func currentRoutineScriptFile(ctx context.Context, files ChatFileResolver, scope Scope, owner ObjectiveOwner, conversationID, path string) (*ChatFile, error) {
	if files == nil {
		return nil, errors.New("routines cannot run scripts on this host")
	}
	file, err := files.ResolveChatFile(ctx, scope, owner, conversationID, path, 0)
	if errors.Is(err, ErrChatFileNotFound) || err == nil && file == nil {
		return nil, fmt.Errorf("%s is not in this chat's files: write it and test it with run_command before scheduling it", path)
	}
	if err != nil {
		return nil, fmt.Errorf("read %s from this chat's files: %w", path, err)
	}
	if file.Path != path || !routineScriptDigest.MatchString(file.Digest) || file.FilesVersion < 1 {
		return nil, fmt.Errorf("%s has no stored version to pin", path)
	}
	return file, nil
}

// verifyRoutineScriptPin proves the pinned version exists in the chat's
// immutable files snapshot it was pinned from.
func verifyRoutineScriptPin(ctx context.Context, files ChatFileResolver, scope Scope, owner ObjectiveOwner, conversationID string, script *RoutineScript) error {
	if script == nil {
		return nil
	}
	if err := script.Validate(); err != nil {
		return err
	}
	if files == nil {
		return errors.New("routines cannot run scripts on this host")
	}
	file, err := files.ResolveChatFile(ctx, scope, owner, conversationID, script.Path, script.FilesVersion)
	if err != nil || file == nil || file.Path != script.Path || file.Digest != script.Digest {
		return fmt.Errorf("the approved version of %s is not in this chat's files", script.Path)
	}
	return nil
}

func routineScriptMap(script *RoutineScript) map[string]interface{} {
	if script == nil {
		return nil
	}
	values := map[string]interface{}{
		"path": script.Path, "digest": script.Digest, "filesVersion": script.FilesVersion,
		"command": script.Command, "timeoutSeconds": script.TimeoutSeconds,
	}
	if script.ApprovedAt != nil {
		values["approvedAt"] = script.ApprovedAt.UTC().Format(time.RFC3339Nano)
	}
	return values
}

func decodeRoutineScript(value interface{}) (*RoutineScript, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.DisallowUnknownFields()
	var script RoutineScript
	if err := decoder.Decode(&script); err != nil {
		return nil, err
	}
	if err := script.Validate(); err != nil {
		return nil, err
	}
	return &script, nil
}

// RoutineScriptStatus is a script routine's pinned version beside the chat
// file's current one. Changed is true when the file now holds other content
// (or is gone); the routine keeps running the pinned version either way.
type RoutineScriptStatus struct {
	ActivationID string         `json:"activationId"`
	Revision     int64          `json:"revision"`
	Pinned       *RoutineScript `json:"pinned"`
	// CurrentDigest and CurrentFilesVersion describe the file now; both are
	// empty when the file was deleted.
	CurrentDigest       string `json:"currentDigest,omitempty"`
	CurrentFilesVersion int64  `json:"currentFilesVersion,omitempty"`
	Missing             bool   `json:"missing,omitempty"`
	Changed             bool   `json:"changed"`
}

// ErrRoutineHasNoScript is returned for a routine that runs no script.
var ErrRoutineHasNoScript = errors.New("this routine does not run a script")

// ResolveRoutineScriptStatus compares a script routine's pinned version with
// its chat file now.
func ResolveRoutineScriptStatus(ctx context.Context, files ChatFileResolver, activation *RunbookActivation) (*RoutineScriptStatus, error) {
	if activation == nil || activation.Task == nil || activation.Task.Script == nil {
		return nil, ErrRoutineHasNoScript
	}
	if files == nil {
		return nil, errors.New("routines cannot run scripts on this host")
	}
	pinned := activation.Task.Script
	status := &RoutineScriptStatus{ActivationID: activation.ID, Revision: activation.Revision, Pinned: cloneRoutineScript(pinned)}
	file, err := files.ResolveChatFile(ctx, activation.Scope, activation.Owner, activation.Task.ConversationID, pinned.Path, 0)
	switch {
	case errors.Is(err, ErrChatFileNotFound) || err == nil && file == nil:
		status.Missing, status.Changed = true, true
	case err != nil:
		return nil, err
	default:
		status.CurrentDigest, status.CurrentFilesVersion = file.Digest, file.FilesVersion
		status.Changed = file.Digest != pinned.Digest
	}
	return status, nil
}

// RoutineScriptStatus reports whether a script routine's chat file changed
// since its script was approved.
func (s *RunbookActivationService) RoutineScriptStatus(ctx context.Context, files ChatFileResolver, scope Scope, id string) (*RoutineScriptStatus, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("Runbook activation store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	activation, err := s.store.GetRunbookActivation(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if activation == nil {
		return nil, ErrRunbookActivationNotFound
	}
	return ResolveRoutineScriptStatus(ctx, files, activation)
}

// RepinScript makes a routine run another approved version of its script.
// Only the approved set_script action calls it; actionID makes its replay
// idempotent. createdBy records the changing person on a routine that has
// no creator yet.
func (s *RunbookActivationService) RepinScript(ctx context.Context, scope Scope, id string, expectedRevision int64, script *RoutineScript, actionID, createdBy string) (*RunbookActivation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("Runbook activation store is not configured")
	}
	if expectedRevision < 1 {
		return nil, ErrRunbookActivationRevision
	}
	current, err := s.store.GetRunbookActivation(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, ErrRunbookActivationNotFound
	}
	if current.Task == nil || current.Task.Script == nil {
		return nil, ErrRoutineHasNoScript
	}
	if current.Revision != expectedRevision {
		return nil, ErrRunbookActivationRevision
	}
	if current.Status == RunbookActivationRetired {
		return nil, errors.New("a cancelled routine's script cannot change")
	}
	if script == nil || script.Path != current.Task.Script.Path || script.Command != current.Task.Script.Command || script.TimeoutSeconds != current.Task.Script.TimeoutSeconds {
		return nil, errors.New("only the routine's script version can change")
	}
	next := cloneRunbookActivation(current)
	next.Task.Script = cloneRoutineScript(script)
	approved := s.now().UTC()
	next.Task.Script.ApprovedAt = &approved
	next.LifecycleActionID = actionID
	if next.Task.CreatedBy == "" {
		next.Task.CreatedBy = createdBy
	}
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := next.Validate(); err != nil {
		return nil, err
	}
	if err := s.store.UpdateRunbookActivation(ctx, next, current.Revision); err != nil {
		return nil, err
	}
	return cloneRunbookActivation(next), nil
}

type scheduledTaskSetScriptArguments struct {
	ActivationID     string `json:"activationId"`
	ExpectedRevision int64  `json:"expectedRevision"`
	Digest           string `json:"digest,omitempty"`
	FilesVersion     int64  `json:"filesVersion,omitempty"`
}

// ownedScriptRoutine loads a script routine the run's owner owns.
func ownedScriptRoutine(ctx context.Context, store runbookActionStore, run *AgentRun, id string) (*RunbookActivation, error) {
	if strings.TrimSpace(id) == "" {
		return nil, errors.New("changing a routine's script requires its activationId")
	}
	a, err := store.GetRunbookActivation(ctx, run.Scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if a == nil || a.Owner != run.Owner {
		return nil, errors.New("routine is not owned by this Agent")
	}
	if a.Task == nil || a.Task.Script == nil {
		return nil, ErrRoutineHasNoScript
	}
	if a.Status == RunbookActivationRetired {
		return nil, errors.New("a cancelled routine's script cannot change")
	}
	return a, nil
}

// pinRoutineScriptRepinArguments writes the routine's chat file's current
// version into proposed set_script arguments, replacing any model value.
func pinRoutineScriptRepinArguments(ctx context.Context, store runbookActionStore, files ChatFileResolver, run *AgentRun, arguments map[string]interface{}) error {
	id, _ := arguments["activationId"].(string)
	a, err := ownedScriptRoutine(ctx, store, run, id)
	if err != nil {
		return err
	}
	file, err := currentRoutineScriptFile(ctx, files, a.Scope, a.Owner, a.Task.ConversationID, a.Task.Script.Path)
	if err != nil {
		return err
	}
	if file.Digest == a.Task.Script.Digest {
		return errors.New("the routine already runs the current version of " + a.Task.Script.Path)
	}
	arguments["digest"], arguments["filesVersion"] = file.Digest, file.FilesVersion
	return nil
}

// resolveOwnedTaskScript returns the routine and the script version the
// set_script arguments pin.
func resolveOwnedTaskScript(ctx context.Context, store runbookActionStore, files ChatFileResolver, run *AgentRun, arguments map[string]interface{}) (*scheduledTaskSetScriptArguments, *RunbookActivation, *RoutineScript, error) {
	var args scheduledTaskSetScriptArguments
	if err := decodeScheduledTaskArguments(arguments, &args); err != nil {
		return nil, nil, nil, err
	}
	if args.ExpectedRevision < 1 {
		return nil, nil, nil, errors.New("changing a routine's script requires its expectedRevision")
	}
	a, err := ownedScriptRoutine(ctx, store, run, args.ActivationID)
	if err != nil {
		return nil, nil, nil, err
	}
	if a.Revision != args.ExpectedRevision {
		return nil, nil, nil, ErrRunbookActivationRevision
	}
	script := cloneRoutineScript(a.Task.Script)
	script.Digest, script.FilesVersion, script.ApprovedAt = args.Digest, args.FilesVersion, nil
	if err := verifyRoutineScriptPin(ctx, files, a.Scope, a.Owner, a.Task.ConversationID, script); err != nil {
		return nil, nil, nil, err
	}
	if script.Digest == a.Task.Script.Digest {
		return nil, nil, nil, errors.New("the routine already runs this version of " + script.Path)
	}
	return &args, a, script, nil
}

// routineScriptChangePreview is the brief the user approves again before a
// routine runs a new version of its script.
func routineScriptChangePreview(ctx context.Context, store runbookActionStore, a *RunbookActivation, script *RoutineScript) (map[string]interface{}, error) {
	preview := map[string]interface{}{
		"resourceType": runbookActivationResourceType, "operation": RunbookActionSetScript, "activationId": a.ID,
		"goal": a.Task.Goal, "schedule": a.Trigger.Schedule, "conversationId": a.Task.ConversationID,
		"script": routineScriptMap(script), "previousScript": routineScriptMap(a.Task.Script),
	}
	objective, err := store.GetObjective(ctx, a.Scope, a.ObjectiveID)
	if err != nil {
		return nil, err
	}
	if objective != nil {
		preview["title"] = objective.Title
	}
	if len(a.Task.Grants) > 0 {
		preview["grants"] = routineBriefPreview(&RoutineBrief{Grants: a.Task.Grants})["grants"]
	}
	return preview, nil
}
