package models

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/chatpath"
	"github.com/axiom-studio/openseal/pkg/workspace"
)

const (
	// MaximumRoutineScriptSeconds bounds one run of the script: a chat
	// sandbox command never runs longer.
	MaximumRoutineScriptSeconds = workspace.ChatCommandSeconds
	// MaximumRoutineScriptCommand bounds the exact approved command.
	MaximumRoutineScriptCommand = 2000
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
	RoutineScriptDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// A plain word needs no shell quoting: no spaces, quotes, globbing,
	// expansion, redirection, pipes or command separators.
	routineScriptPlainWord    = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)
	routineScriptInterpreters = map[string]bool{"python3": true, "node": true, "bash": true}
)

func (s *RoutineScript) Validate() error {
	if s == nil {
		return errors.New("routine script is required")
	}
	if err := ValidateRoutineScriptPath(s.Path); err != nil {
		return err
	}
	if !RoutineScriptDigestPattern.MatchString(s.Digest) || s.FilesVersion < 1 {
		return errors.New("routine script must be pinned to a version of a chat file")
	}
	if s.TimeoutSeconds < 1 || s.TimeoutSeconds > MaximumRoutineScriptSeconds {
		return fmt.Errorf("routine script timeout must be 1 to %d seconds", MaximumRoutineScriptSeconds)
	}
	command, err := NormalizeRoutineScriptCommand(s.Command, s.Path)
	if err != nil {
		return err
	}
	if command != s.Command {
		return errors.New("routine script command is not normalized")
	}
	return nil
}

func CloneRoutineScript(s *RoutineScript) *RoutineScript {
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

func ValidateRoutineScriptPath(value string) error {
	clean, err := chatpath.Clean(value)
	if err != nil || clean != value {
		return errors.New("routine script path must be a normalized path in this chat's files, such as scripts/report.py")
	}
	if !routineScriptPlainWord.MatchString(value) || strings.HasPrefix(value, "-") {
		return errors.New("routine script path may use only letters, digits and - _ . / so it can be run as is")
	}
	return nil
}

// NormalizeRoutineScriptCommand returns the command with single spaces when
// it runs only path: an interpreter, path, then plain arguments.
func NormalizeRoutineScriptCommand(command, path string) (string, error) {
	if len(command) > MaximumRoutineScriptCommand {
		return "", fmt.Errorf("routine script command is longer than %d bytes", MaximumRoutineScriptCommand)
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
