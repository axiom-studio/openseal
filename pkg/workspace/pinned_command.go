package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"
)

// A run_command can be pinned to one version of a chat file: it runs only if
// the file at a path holds an exact content digest. This is part of
// run_command's file contract, set by the host, never by the model (its
// input schema does not offer it):
//
//	"expectedDigest": {"path": "scripts/report.py", "digest": "sha256:<hex>", "filesVersion": 12}
//
// Before the command runs, a host whose sandbox working copy holds other
// content at path restores it from the chat files snapshot filesVersion,
// after checking that snapshot's file has digest. If it cannot, the command
// is refused without running. A restored file is not a change the command
// made: it is saved back to the chat's files only if the command changes it.
// The result then carries expectedDigest (what ran) or refused (why
// nothing ran) beside the usual exitStatus, stdout and stderr.
//
// A routine that runs an approved script pins it this way (HostedTurnRequest
// CommandPins): the host attaches the pin to every run_command that invokes
// the script, so an edited script never runs in the routine's place, and
// reports what those commands did (HostedTurnResponse PinnedCommands).

// ExpectedDigestArgument is the host-only run_command argument that pins a
// file version, and the result field that echoes it once verified.
const ExpectedDigestArgument = "expectedDigest"

// RefusedResult is the run_command result field that says why a pinned
// command did not run.
const RefusedResult = "refused"

// ExpectedDigest is the file version a pinned command must run with.
type ExpectedDigest struct {
	Path         string `json:"path"`
	Digest       string `json:"digest"`
	FilesVersion int64  `json:"filesVersion,omitempty"`
}

// CommandPin is a chat file version the commands of a turn that invoke it
// must run, with the exact command a routine approved for it.
type CommandPin struct {
	Path         string `json:"path"`
	Digest       string `json:"digest"`
	FilesVersion int64  `json:"filesVersion"`
	// Command is the approved command, normalized to single spaces.
	Command string `json:"command"`
	// TimeoutSeconds applies when a command is exactly Command.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
}

// MaximumCommandRecordOutputTail bounds the output a CommandRecord keeps.
const MaximumCommandRecordOutputTail = 4 << 10

// MaximumPinnedCommandRecords bounds the records one turn returns.
const MaximumPinnedCommandRecords = 64

// CommandRecord is what one pinned run_command did, as the host observed it.
type CommandRecord struct {
	Command          string `json:"command"`
	WorkingDirectory string `json:"workingDirectory,omitempty"`
	// Path is the pinned file.
	Path string `json:"path"`
	// Digest is the version the command ran with; empty when it was refused.
	Digest string `json:"digest,omitempty"`
	// ExitCode is the command's exit status, -1 when it did not exit.
	ExitCode int  `json:"exitCode"`
	TimedOut bool `json:"timedOut,omitempty"`
	// Refused says why the command did not run.
	Refused string `json:"refused,omitempty"`
	// OutputTail is the last bytes of stderr, or of stdout when stderr is
	// empty, at most MaximumCommandRecordOutputTail.
	OutputTail string `json:"outputTail,omitempty"`
}

// Succeeded reports whether the record is a clean run of the pinned version.
func (r CommandRecord) Succeeded(digest string) bool {
	return r.Refused == "" && !r.TimedOut && r.ExitCode == 0 && r.Digest != "" && r.Digest == digest
}

// Runs reports whether the record ran exactly pin's approved command from
// the chat files root.
func (r CommandRecord) Runs(pin CommandPin) bool {
	return r.Path == pin.Path && rootDirectory(r.WorkingDirectory) && normalizeCommand(r.Command) == pin.Command
}

// Validate checks a host-reported record against the pins it ran under.
func (r CommandRecord) Validate(pins []CommandPin) error {
	var pin *CommandPin
	for index := range pins {
		if pins[index].Path == r.Path {
			pin = &pins[index]
		}
	}
	if pin == nil {
		return errors.New("pinned command record names a file the turn did not pin")
	}
	if r.Digest != "" && r.Digest != pin.Digest {
		return errors.New("pinned command ran another version of its file")
	}
	if r.Digest == "" && strings.TrimSpace(r.Refused) == "" {
		return errors.New("pinned command record must say what version ran or why it was refused")
	}
	if r.ExitCode < -1 || r.ExitCode > 255 {
		return errors.New("pinned command exit code is out of range")
	}
	for _, text := range []string{r.Command, r.WorkingDirectory, r.Refused} {
		if len(text) > 20000 || !utf8.ValidString(text) {
			return errors.New("pinned command record text is invalid")
		}
	}
	if len(r.OutputTail) > MaximumCommandRecordOutputTail || !utf8.ValidString(r.OutputTail) {
		return fmt.Errorf("pinned command output tail must be valid text of at most %d bytes", MaximumCommandRecordOutputTail)
	}
	return nil
}

// PinRunCommand returns run_command arguments with the pin of the file the
// command invokes, and that pin, or the arguments without any expectedDigest
// and nil when it invokes none. A command invokes a pinned file when it names
// its path, from the chat files root or from workingDirectory. A command that
// is exactly the pin's approved command also runs with its timeout.
func PinRunCommand(pins []CommandPin, arguments map[string]interface{}) (map[string]interface{}, *CommandPin) {
	result := make(map[string]interface{}, len(arguments)+1)
	for name, value := range arguments {
		result[name] = value
	}
	delete(result, ExpectedDigestArgument)
	command, _ := result["command"].(string)
	directory, _ := result["workingDirectory"].(string)
	for index := range pins {
		pin := pins[index]
		if !invokes(command, directory, pin.Path) {
			continue
		}
		result[ExpectedDigestArgument] = map[string]interface{}{"path": pin.Path, "digest": pin.Digest, "filesVersion": pin.FilesVersion}
		if pin.TimeoutSeconds > 0 && rootDirectory(directory) && normalizeCommand(command) == pin.Command {
			result["timeoutSeconds"] = pin.TimeoutSeconds
		}
		return result, &pin
	}
	return result, nil
}

// ExpectedDigestOf reads the pin from run_command arguments or a result.
func ExpectedDigestOf(values map[string]interface{}) (*ExpectedDigest, bool) {
	raw, ok := values[ExpectedDigestArgument]
	if !ok || raw == nil {
		return nil, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil, false
	}
	var expected ExpectedDigest
	if err := json.Unmarshal(encoded, &expected); err != nil || expected.Path == "" || expected.Digest == "" {
		return nil, false
	}
	return &expected, true
}

// RecordPinnedCommand is the record of a run_command made with pinned
// arguments (from PinRunCommand) and the result the host returned for it.
func RecordPinnedCommand(arguments, result map[string]interface{}) (CommandRecord, bool) {
	expected, ok := ExpectedDigestOf(arguments)
	if !ok {
		return CommandRecord{}, false
	}
	command, _ := arguments["command"].(string)
	directory, _ := arguments["workingDirectory"].(string)
	record := CommandRecord{Command: tail(command, 20000), Path: expected.Path, ExitCode: -1}
	if !rootDirectory(directory) {
		record.WorkingDirectory = tail(directory, 4096)
	}
	if refused, _ := result[RefusedResult].(string); strings.TrimSpace(refused) != "" {
		record.Refused = tail(refused, 2000)
		return record, true
	}
	if ran, ok := ExpectedDigestOf(result); ok && ran.Path == expected.Path && ran.Digest == expected.Digest {
		record.Digest = ran.Digest
	} else {
		record.Refused = "the host did not confirm the pinned version ran"
	}
	record.ExitCode = exitCodeOf(result["exitStatus"])
	record.TimedOut, _ = result["timedOut"].(bool)
	output, _ := result["stderr"].(string)
	if strings.TrimSpace(output) == "" {
		output, _ = result["stdout"].(string)
	}
	record.OutputTail = tail(output, MaximumCommandRecordOutputTail)
	return record, true
}

func exitCodeOf(value interface{}) int {
	var code int
	switch typed := value.(type) {
	case float64:
		code = int(typed)
	case int:
		code = typed
	case int64:
		code = int(typed)
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return -1
		}
		code = int(parsed)
	default:
		return -1
	}
	return min(max(code, -1), 255)
}

func tail(text string, limit int) string {
	text = strings.ToValidUTF8(text, "�")
	if len(text) <= limit {
		return text
	}
	text = text[len(text)-limit:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}
	return text
}

func normalizeCommand(command string) string {
	return strings.Join(strings.Fields(command), " ")
}

func rootDirectory(directory string) bool {
	directory = strings.TrimSpace(directory)
	return directory == "" || path.Clean(directory) == "."
}

// invokes reports whether command, run from directory, names file.
func invokes(command, directory, file string) bool {
	if file == "" {
		return false
	}
	if strings.Contains(command, file) {
		return true
	}
	if rootDirectory(directory) {
		return false
	}
	base := strings.TrimPrefix(path.Clean(strings.TrimSpace(directory)), "./")
	if relative, ok := strings.CutPrefix(file, base+"/"); ok && relative != "" {
		return strings.Contains(command, relative)
	}
	return false
}
