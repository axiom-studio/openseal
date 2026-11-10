package client

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// systemdService is the bridge as a systemd --user unit (Linux).
type systemdService struct{ ServiceOptions }

const systemdUnitName = ServiceName + ".service"

func (s systemdService) unitPath() string {
	return filepath.Join(s.ConfigHome, "systemd", "user", systemdUnitName)
}

// systemdUnit restarts the bridge 5s after any exit but ExitNotConnected,
// and starts it with the person's user manager (at login, or at boot with
// lingering on).
func systemdUnit(exe string) string {
	return `[Unit]
Description=HelloSeal bridge: lets your HelloSeal agents use this computer
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=` + systemdQuote(exe) + ` start
Restart=always
RestartSec=5
RestartPreventExitStatus=` + strconv.Itoa(ExitNotConnected) + `

[Install]
WantedBy=default.target
`
}

// systemdQuote quotes a path for ExecStart: a double-quoted word with \ and
// " escaped, and % doubled so systemd does not expand it.
func systemdQuote(path string) string {
	path = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(path)
	return `"` + path + `"`
}

func (s systemdService) systemctl(ctx context.Context, args ...string) (string, error) {
	return s.Run(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

// reachable reports whether a systemd user manager answers.
func (s systemdService) reachable(ctx context.Context) bool {
	_, err := s.systemctl(ctx, "show-environment")
	return err == nil
}

func noSystemdUser() error {
	return &ErrNoServiceManager{Message: "This computer has no systemd user session, so helloseal-bridge can't keep itself running in the background. " +
		"Run helloseal-bridge start yourself (in tmux or screen it keeps running after you close the terminal). " +
		"On a computer with systemd, loginctl enable-linger lets it run while you're logged out."}
}

func (s systemdService) Install(ctx context.Context) error {
	if !s.reachable(ctx) {
		return noSystemdUser()
	}
	out, _ := s.systemctl(ctx, "is-active", systemdUnitName)
	wasActive := strings.TrimSpace(out) == "active"
	if err := writeServiceFile(s.unitPath(), []byte(systemdUnit(s.Exe))); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "--now", systemdUnitName}} {
		if out, err := s.systemctl(ctx, args...); err != nil {
			return runFailed("systemctl --user "+strings.Join(args, " "), out, err)
		}
	}
	// enable --now leaves a running bridge on its old binary and connection.
	if wasActive {
		return s.Restart(ctx)
	}
	return nil
}

func (s systemdService) Uninstall(ctx context.Context) error {
	if !exists(s.unitPath()) {
		return nil
	}
	if s.reachable(ctx) {
		if out, err := s.systemctl(ctx, "disable", "--now", systemdUnitName); err != nil {
			return runFailed("systemctl --user disable --now "+systemdUnitName, out, err)
		}
	}
	if err := os.Remove(s.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if s.reachable(ctx) {
		_, _ = s.systemctl(ctx, "daemon-reload")
	}
	return nil
}

func (s systemdService) Status(ctx context.Context) (ServiceStatus, error) {
	if !exists(s.unitPath()) {
		return ServiceStatus{}, nil
	}
	if !s.reachable(ctx) {
		return ServiceStatus{Installed: true, Detail: "no systemd user session"}, nil
	}
	// is-active exits non-zero for anything but active; its word is the answer.
	out, _ := s.systemctl(ctx, "is-active", systemdUnitName)
	state := strings.TrimSpace(out)
	return ServiceStatus{Installed: true, Running: state == "active", Detail: state}, nil
}

func (s systemdService) Restart(ctx context.Context) error {
	if !exists(s.unitPath()) {
		return ErrServiceNotInstalled
	}
	if out, err := s.systemctl(ctx, "restart", systemdUnitName); err != nil {
		return runFailed("systemctl --user restart "+systemdUnitName, out, err)
	}
	return nil
}

func (s systemdService) Logs() string { return "journalctl --user -u " + systemdUnitName + " -f" }

func (s systemdService) Notes() []string {
	return []string{"It starts when you log in. To keep it running while you're logged out, run: loginctl enable-linger"}
}
