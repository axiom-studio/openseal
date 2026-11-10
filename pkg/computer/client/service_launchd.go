package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// launchdService is the bridge as a LaunchAgent (macOS).
type launchdService struct {
	ServiceOptions
	// ConfigFile is the saved connection; launchd keeps the bridge alive
	// while it exists.
	ConfigFile string
}

const launchdLabel = "ai.helloseal.bridge"

func (s launchdService) plistPath() string {
	return filepath.Join(s.Home, "Library", "LaunchAgents", launchdLabel+".plist")
}

func (s launchdService) logPath() string {
	return filepath.Join(s.Home, "Library", "Logs", ServiceName+".log")
}

func (s launchdService) domain() string { return "gui/" + strconv.Itoa(s.UID) }

func (s launchdService) target() string { return s.domain() + "/" + launchdLabel }

// xmlText escapes text for an XML element. Quotes stay as they are: they
// need no escaping there, and a task's arguments read better with them.
func xmlText(value string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(value)
}

// launchdPlist starts the bridge at login (RunAtLoad) and keeps it alive
// while the connection is saved: the bridge deletes it when HelloSeal
// disconnects the computer, and launchd then lets it rest.
func launchdPlist(exe, configFile, logPath string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlText(exe) + `</string>
		<string>start</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>PathState</key>
		<dict>
			<key>` + xmlText(configFile) + `</key>
			<true/>
		</dict>
	</dict>
	<key>ThrottleInterval</key>
	<integer>5</integer>
	<key>StandardOutPath</key>
	<string>` + xmlText(logPath) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlText(logPath) + `</string>
</dict>
</plist>
`
}

func (s launchdService) Install(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.logPath()), 0o755); err != nil {
		return err
	}
	if err := writeServiceFile(s.plistPath(), []byte(launchdPlist(s.Exe, s.ConfigFile, s.logPath()))); err != nil {
		return err
	}
	// A loaded agent must be booted out first: bootstrap refuses a loaded
	// label, and a reinstall must pick up the new binary and connection.
	_, _ = s.Run(ctx, "launchctl", "bootout", s.target())
	_, _ = s.Run(ctx, "launchctl", "enable", s.target())
	if out, err := s.Run(ctx, "launchctl", "bootstrap", s.domain(), s.plistPath()); err != nil {
		return runFailed("launchctl bootstrap "+s.domain(), out, err)
	}
	return nil
}

func (s launchdService) Uninstall(ctx context.Context) error {
	if !exists(s.plistPath()) {
		return nil
	}
	_, _ = s.Run(ctx, "launchctl", "bootout", s.target())
	if err := os.Remove(s.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s launchdService) Status(ctx context.Context) (ServiceStatus, error) {
	if !exists(s.plistPath()) {
		return ServiceStatus{}, nil
	}
	out, err := s.Run(ctx, "launchctl", "print", s.target())
	if err != nil {
		return ServiceStatus{Installed: true, Detail: "not loaded"}, nil
	}
	state := "loaded"
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok && strings.TrimSpace(key) == "state" {
			state = strings.TrimSpace(value)
			break
		}
	}
	return ServiceStatus{Installed: true, Running: state == "running", Detail: state}, nil
}

func (s launchdService) Restart(ctx context.Context) error {
	if !exists(s.plistPath()) {
		return ErrServiceNotInstalled
	}
	if out, err := s.Run(ctx, "launchctl", "kickstart", "-k", s.target()); err != nil {
		return runFailed("launchctl kickstart -k "+s.target(), out, err)
	}
	return nil
}

func (s launchdService) Logs() string { return s.logPath() }

func (s launchdService) Notes() []string { return []string{"It starts when you log in."} }
