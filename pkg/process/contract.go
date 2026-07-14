// Package process defines the portable, argument-safe transport used when an
// imported Skill declares a command-line executable. Hosts remain responsible
// for provisioning and isolating the process; OpenSeal never invokes a shell.
package process

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/axiom-studio/openseal/pkg/capability"
)

const (
	TransportName       = "openseal.process.exec"
	ExecutableKey       = "executable"
	ArgumentsKey        = "arguments"
	InstallersKey       = "installers"
	EnvironmentNamesKey = "environmentNames"
	TimeoutSecondsKey   = "timeoutSeconds"

	MaxArguments      = 128
	MaxArgumentBytes  = 16 << 10
	MaxTimeoutSeconds = 15 * 60
)

var executableName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,127}$`)

// ValidExecutableName reports whether value is a basename that can be passed
// directly as argv[0]. Paths and shell fragments are never portable actions.
func ValidExecutableName(value string) bool {
	return executableName.MatchString(strings.TrimSpace(value))
}

// Invocation is the non-shell process request delivered to a governed host.
// Credentials are resolved separately and appear only under names explicitly
// declared in EnvironmentNames.
type Invocation struct {
	Executable       string
	Arguments        []string
	Installers       []capability.Installer
	EnvironmentNames []string
	TimeoutSeconds   int
}

// DecodeInvocation validates a materialized transport envelope. It accepts
// JSON-shaped values because invocations commonly cross an HTTP boundary.
func DecodeInvocation(value map[string]interface{}) (*Invocation, error) {
	if value == nil {
		return nil, errors.New("process invocation is required")
	}
	executable, _ := value[ExecutableKey].(string)
	executable = strings.TrimSpace(executable)
	if !ValidExecutableName(executable) {
		return nil, errors.New("process executable must be a portable basename")
	}
	arguments, err := decodeStrings(value[ArgumentsKey], ArgumentsKey, MaxArguments, MaxArgumentBytes)
	if err != nil {
		return nil, err
	}
	environmentNames, err := decodeStrings(value[EnvironmentNamesKey], EnvironmentNamesKey, 64, 256)
	if err != nil {
		return nil, err
	}
	seenEnvironment := make(map[string]bool, len(environmentNames))
	for _, name := range environmentNames {
		if !executableName.MatchString(name) || seenEnvironment[name] {
			return nil, errors.New("process environment names must be unique portable identifiers")
		}
		seenEnvironment[name] = true
	}
	timeout, err := decodeInteger(value[TimeoutSecondsKey])
	if err != nil || timeout < 1 || timeout > MaxTimeoutSeconds {
		return nil, fmt.Errorf("process timeoutSeconds must be between 1 and %d", MaxTimeoutSeconds)
	}
	installers, err := decodeInstallers(value[InstallersKey])
	if err != nil {
		return nil, err
	}
	for _, installer := range installers {
		if !installerProvides(installer, executable) {
			return nil, fmt.Errorf("process installer %q does not provide executable %q", installer.ID, executable)
		}
	}
	return &Invocation{
		Executable: executable, Arguments: arguments, Installers: installers,
		EnvironmentNames: environmentNames, TimeoutSeconds: timeout,
	}, nil
}

func decodeStrings(value interface{}, label string, maximumCount, maximumBytes int) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("process %s must be a string array", label)
	}
	var result []string
	if err := json.Unmarshal(encoded, &result); err != nil || len(result) > maximumCount {
		return nil, fmt.Errorf("process %s must contain at most %d strings", label, maximumCount)
	}
	for _, item := range result {
		if len(item) > maximumBytes || strings.ContainsRune(item, 0) {
			return nil, fmt.Errorf("process %s contains an invalid value", label)
		}
	}
	return result, nil
}

func decodeInteger(value interface{}) (int, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(encoded)))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, err
	}
	parsed, err := number.Int64()
	return int(parsed), err
}

func decodeInstallers(value interface{}) ([]capability.Installer, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("process installers are invalid")
	}
	var result []capability.Installer
	if err := json.Unmarshal(encoded, &result); err != nil || len(result) > 16 {
		return nil, errors.New("process installers are invalid")
	}
	for _, installer := range result {
		if strings.TrimSpace(installer.Kind) == "" {
			return nil, errors.New("process installer kind is required")
		}
	}
	return result, nil
}

func installerProvides(installer capability.Installer, executable string) bool {
	for _, provided := range installer.Executables {
		if strings.TrimSpace(provided) == executable {
			return true
		}
	}
	return false
}
