package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// The bridge runs in the background as a per-person service, so it survives
// closing the terminal, logging out and back in, and restarts: a systemd
// --user unit on Linux, a LaunchAgent on macOS, a logon task on Windows.
// Each needs no root or admin. The service runs exactly `helloseal-bridge
// start`.
//
// The three managers below are plain Go that shells out through Runner, so
// every platform's files and commands are tested on any machine;
// service_<os>.go picks the one this platform uses.

// ServiceName names the service on every platform.
const ServiceName = "helloseal-bridge"

// ExitNotConnected is how `start` ends when this computer has no connection
// (never paired, or HelloSeal disconnected it). A service never restarts
// on it: nothing changes until the person connects again.
const ExitNotConnected = 3

// Runner runs a program and returns its combined output. Tests replace it so
// nothing is installed on the machine running them.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner runs the program for real.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// ServiceStatus is what `service status` reports.
type ServiceStatus struct {
	Installed bool
	Running   bool
	// Detail is the service manager's own word for its state, such as
	// active, running or Ready.
	Detail string
}

// ServiceManager installs and controls the bridge's background service.
type ServiceManager interface {
	// Install writes the service and starts it now and at every login; on a
	// reinstall it restarts it, so a new binary or connection takes effect.
	Install(ctx context.Context) error
	// Uninstall stops and removes it; nothing installed is no error.
	Uninstall(ctx context.Context) error
	Status(ctx context.Context) (ServiceStatus, error)
	Restart(ctx context.Context) error
	// Logs says where the service's output goes, as a person reads it.
	Logs() string
	// Notes are anything else worth telling the person after an install.
	Notes() []string
}

// ErrNoServiceManager means this computer cannot run the bridge as a
// service; its message says what to do instead.
type ErrNoServiceManager struct{ Message string }

func (e *ErrNoServiceManager) Error() string { return e.Message }

// ErrServiceNotInstalled is Restart without an installed service.
var ErrServiceNotInstalled = errors.New("the background service is not installed; run helloseal-bridge service install")

// ServiceOptions are the paths and identity a service is made from.
type ServiceOptions struct {
	// Exe is the bridge binary the service runs, absolute.
	Exe string
	// Home is the person's home folder.
	Home string
	// ConfigHome is where systemd looks for user units' config
	// ($XDG_CONFIG_HOME, else ~/.config).
	ConfigHome string
	// LocalAppData is %LOCALAPPDATA% on Windows.
	LocalAppData string
	// UID is the person's user id on macOS (launchctl's gui/<uid>).
	UID int
	// User is the person's account on Windows, DOMAIN\name.
	User string
	Run  Runner
}

// DefaultServiceOptions are this process's binary and this person's
// folders, with the real Runner.
func DefaultServiceOptions() (ServiceOptions, error) {
	exe, err := os.Executable()
	if err != nil {
		return ServiceOptions{}, fmt.Errorf("find the helloseal-bridge binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ServiceOptions{}, fmt.Errorf("find the home folder: %w", err)
	}
	options := ServiceOptions{Exe: exe, Home: home, ConfigHome: os.Getenv("XDG_CONFIG_HOME"), LocalAppData: os.Getenv("LOCALAPPDATA"), UID: os.Getuid(), Run: ExecRunner}
	if options.ConfigHome == "" {
		options.ConfigHome = filepath.Join(home, ".config")
	}
	if current, err := user.Current(); err == nil {
		options.User = current.Username
	}
	if runtime.GOOS == "windows" && options.LocalAppData == "" {
		return ServiceOptions{}, errors.New("LOCALAPPDATA is not set")
	}
	return options, nil
}

// writeServiceFile writes a service definition, making its folder.
func writeServiceFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runFailed is a service command's failure with what it printed.
func runFailed(command string, out string, err error) error {
	if text := strings.TrimSpace(out); text != "" {
		return fmt.Errorf("%s: %s", command, text)
	}
	return fmt.Errorf("%s: %w", command, err)
}
