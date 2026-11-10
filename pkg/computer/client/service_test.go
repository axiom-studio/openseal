package client

import (
	"context"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// fakeRunner records every command and answers from a script, so no test
// installs a service on the machine running it.
type fakeRunner struct {
	commands []string
	answer   func(command string) (string, error)
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	command := strings.Join(append([]string{name}, args...), " ")
	f.commands = append(f.commands, command)
	if f.answer != nil {
		return f.answer(command)
	}
	return "", nil
}

func (f *fakeRunner) took(t *testing.T, want ...string) {
	t.Helper()
	if strings.Join(f.commands, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(f.commands, "\n"), strings.Join(want, "\n"))
	}
	f.commands = nil
}

var errExit = errors.New("exit status 3")

func wellFormed(t *testing.T, document string) {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(document))
	decoder.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	for {
		if _, err := decoder.Token(); err == io.EOF {
			return
		} else if err != nil {
			t.Fatalf("not well-formed XML: %v\n%s", err, document)
		}
	}
}

func TestSystemdUnitRestartsTheBridgeUnlessItIsNotConnected(t *testing.T) {
	unit := systemdUnit("/home/sam/my tools/100%/helloseal-bridge")
	for _, line := range []string{
		`ExecStart="/home/sam/my tools/100%%/helloseal-bridge" start`,
		"Restart=always", "RestartSec=5", "RestartPreventExitStatus=3", "WantedBy=default.target",
	} {
		if !strings.Contains(unit, line+"\n") {
			t.Errorf("unit lacks %q:\n%s", line, unit)
		}
	}
}

func TestSystemdServiceInstallsEnablesAndRemovesTheUnit(t *testing.T) {
	runner := &fakeRunner{answer: func(command string) (string, error) {
		if strings.HasSuffix(command, "is-active helloseal-bridge.service") {
			return "inactive\n", errExit
		}
		return "", nil
	}}
	home := t.TempDir()
	s := systemdService{ServiceOptions{Exe: "/usr/local/bin/helloseal-bridge", Home: home, ConfigHome: filepath.Join(home, ".config"), Run: runner.run}}
	if err := s.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.took(t, "systemctl --user show-environment", "systemctl --user is-active helloseal-bridge.service",
		"systemctl --user daemon-reload", "systemctl --user enable --now helloseal-bridge.service")
	data, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "helloseal-bridge.service"))
	if err != nil || string(data) != systemdUnit("/usr/local/bin/helloseal-bridge") {
		t.Fatalf("unit = %q, %v", data, err)
	}

	// A reinstall restarts the running bridge onto the new binary.
	runner.answer = func(command string) (string, error) {
		if strings.HasSuffix(command, "is-active helloseal-bridge.service") {
			return "active\n", nil
		}
		return "", nil
	}
	if err := s.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.took(t, "systemctl --user show-environment", "systemctl --user is-active helloseal-bridge.service",
		"systemctl --user daemon-reload", "systemctl --user enable --now helloseal-bridge.service", "systemctl --user restart helloseal-bridge.service")

	status, err := s.Status(t.Context())
	if err != nil || !status.Installed || !status.Running || status.Detail != "active" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	runner.commands = nil
	if err := s.Uninstall(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.took(t, "systemctl --user show-environment", "systemctl --user disable --now helloseal-bridge.service",
		"systemctl --user show-environment", "systemctl --user daemon-reload")
	if exists(s.unitPath()) {
		t.Fatal("the unit is still there")
	}
	if status, _ := s.Status(t.Context()); status.Installed {
		t.Fatalf("status after uninstall = %+v", status)
	}
	if err := s.Restart(t.Context()); !errors.Is(err, ErrServiceNotInstalled) {
		t.Fatalf("restart without a service = %v", err)
	}
}

func TestSystemdServiceWithoutAUserSessionSaysWhatToDo(t *testing.T) {
	runner := &fakeRunner{answer: func(string) (string, error) {
		return "Failed to connect to bus: No medium found", errExit
	}}
	home := t.TempDir()
	s := systemdService{ServiceOptions{Exe: "/usr/local/bin/helloseal-bridge", Home: home, ConfigHome: filepath.Join(home, ".config"), Run: runner.run}}
	err := s.Install(t.Context())
	var none *ErrNoServiceManager
	if !errors.As(err, &none) || !strings.Contains(none.Message, "Run helloseal-bridge start yourself") || !strings.Contains(none.Message, "loginctl enable-linger") {
		t.Fatalf("install = %v", err)
	}
	runner.took(t, "systemctl --user show-environment")
	if exists(s.unitPath()) {
		t.Fatal("a unit was written with no systemd to run it")
	}
}

func TestLaunchdPlistKeepsTheBridgeAliveWhileConnected(t *testing.T) {
	plist := launchdPlist("/Users/sam/bin/helloseal-bridge", "/Users/sam/Library/Application Support/helloseal/computer.json", "/Users/sam/Library/Logs/helloseal-bridge.log")
	wellFormed(t, plist)
	for _, fragment := range []string{
		"<key>Label</key>\n\t<string>ai.helloseal.bridge</string>",
		"<string>/Users/sam/bin/helloseal-bridge</string>\n\t\t<string>start</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>KeepAlive</key>\n\t<dict>\n\t\t<key>PathState</key>\n\t\t<dict>\n\t\t\t<key>/Users/sam/Library/Application Support/helloseal/computer.json</key>\n\t\t\t<true/>",
		"<key>StandardOutPath</key>\n\t<string>/Users/sam/Library/Logs/helloseal-bridge.log</string>",
	} {
		if !strings.Contains(plist, fragment) {
			t.Errorf("plist lacks %q", fragment)
		}
	}
	if escaped := launchdPlist("/Users/a&b/<bridge>", "/c", "/l"); !strings.Contains(escaped, "<string>/Users/a&amp;b/&lt;bridge&gt;</string>") {
		t.Fatalf("path not escaped:\n%s", escaped)
	}
}

func TestLaunchdServiceBootstrapsInTheGUIDomain(t *testing.T) {
	runner := &fakeRunner{}
	home := t.TempDir()
	s := launchdService{ServiceOptions: ServiceOptions{Exe: "/usr/local/bin/helloseal-bridge", Home: home, UID: 501, Run: runner.run}, ConfigFile: "/cfg/computer.json"}
	if err := s.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "ai.helloseal.bridge.plist")
	runner.took(t, "launchctl bootout gui/501/ai.helloseal.bridge", "launchctl enable gui/501/ai.helloseal.bridge", "launchctl bootstrap gui/501 "+plist)
	if data, err := os.ReadFile(plist); err != nil || string(data) != launchdPlist(s.Exe, "/cfg/computer.json", filepath.Join(home, "Library", "Logs", "helloseal-bridge.log")) {
		t.Fatalf("plist = %q, %v", data, err)
	}
	if s.Logs() != filepath.Join(home, "Library", "Logs", "helloseal-bridge.log") {
		t.Fatalf("logs = %s", s.Logs())
	}
	runner.answer = func(string) (string, error) {
		return "gui/501/ai.helloseal.bridge = {\n\tactive count = 1\n\tstate = running\n\tprogram = /usr/local/bin/helloseal-bridge\n}", nil
	}
	if status, err := s.Status(t.Context()); err != nil || !status.Running || status.Detail != "running" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	runner.commands = nil
	if err := s.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.took(t, "launchctl kickstart -k gui/501/ai.helloseal.bridge")
	if err := s.Uninstall(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.took(t, "launchctl bootout gui/501/ai.helloseal.bridge")
	if exists(plist) {
		t.Fatal("the plist is still there")
	}
}

func decodeUTF16(t *testing.T, data []byte) string {
	t.Helper()
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xFE || len(data)%2 != 0 {
		t.Fatalf("not UTF-16LE with a BOM: % x", data[:min(len(data), 4)])
	}
	units := make([]uint16, 0, len(data)/2-1)
	for i := 2; i < len(data); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(data[i:]))
	}
	return string(utf16.Decode(units))
}

func TestSchtasksTaskRunsTheHiddenLauncherAtLogon(t *testing.T) {
	task := schtasksXML(`STUDIO\sam`, `C:\Users\sam\AppData\Local\HelloSeal\helloseal-bridge.vbs`)
	wellFormed(t, task)
	for _, fragment := range []string{
		"<LogonTrigger>\n      <Enabled>true</Enabled>\n      <UserId>STUDIO\\sam</UserId>",
		"<LogonType>InteractiveToken</LogonType>\n      <RunLevel>LeastPrivilege</RunLevel>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<RestartOnFailure>\n      <Interval>PT1M</Interval>",
		"<Command>wscript.exe</Command>\n      <Arguments>//B //Nologo \"C:\\Users\\sam\\AppData\\Local\\HelloSeal\\helloseal-bridge.vbs\"</Arguments>",
	} {
		if !strings.Contains(task, fragment) {
			t.Errorf("task lacks %q", fragment)
		}
	}
	launcher := schtasksLauncher(`C:\Program Files\HelloSeal\helloseal-bridge.exe`, `C:\Users\sam\AppData\Local\HelloSeal\helloseal-bridge.log`)
	for _, fragment := range []string{
		// Window style 0 hides it; True waits, so the task runs as long as the bridge.
		`code = shell.Run("""" & "C:\Program Files\HelloSeal\helloseal-bridge.exe" & """ start --log """ & "C:\Users\sam\AppData\Local\HelloSeal\helloseal-bridge.log" & """", 0, True)`,
		"If code = 0 Or code = 3 Then Exit Do",
		"WScript.Sleep 5000",
	} {
		if !strings.Contains(launcher, fragment) {
			t.Errorf("launcher lacks %q:\n%s", fragment, launcher)
		}
	}
}

func TestSchtasksServiceRegistersAndRunsTheTask(t *testing.T) {
	appData := t.TempDir()
	var definition string
	runner := &fakeRunner{answer: func(command string) (string, error) {
		if strings.HasPrefix(command, "schtasks /Create") {
			data, err := os.ReadFile(filepath.Join(appData, "HelloSeal", "helloseal-bridge-task.xml"))
			if err != nil {
				return "", err
			}
			definition = decodeUTF16(t, data)
		}
		if strings.HasPrefix(command, "schtasks /Query") {
			return "Folder: \\\r\nTaskName:                             \\HelloSeal Bridge\r\nStatus:                               Running\r\n", nil
		}
		return "", nil
	}}
	s := schtasksService{ServiceOptions{Exe: `C:\HelloSeal\helloseal-bridge.exe`, LocalAppData: appData, User: `STUDIO\sam`, Run: runner.run}}
	if err := s.Install(t.Context()); err != nil {
		t.Fatal(err)
	}
	xmlPath := filepath.Join(appData, "HelloSeal", "helloseal-bridge-task.xml")
	runner.took(t, "schtasks /Create /TN HelloSeal Bridge /XML "+xmlPath+" /F", "schtasks /End /TN HelloSeal Bridge", "schtasks /Run /TN HelloSeal Bridge")
	if definition != strings.ReplaceAll(schtasksXML(`STUDIO\sam`, s.launcherPath()), "\n", "\r\n") {
		t.Fatalf("task definition = %q", definition)
	}
	if exists(xmlPath) {
		t.Fatal("the task definition was left behind")
	}
	if data, err := os.ReadFile(s.launcherPath()); err != nil || !strings.Contains(string(data), "helloseal-bridge.exe") {
		t.Fatalf("launcher = %q, %v", data, err)
	}
	if status, err := s.Status(t.Context()); err != nil || !status.Running || status.Detail != "Running" {
		t.Fatalf("status = %+v, %v", status, err)
	}
	runner.commands = nil
	if err := s.Uninstall(t.Context()); err != nil {
		t.Fatal(err)
	}
	runner.took(t, "schtasks /Query /TN HelloSeal Bridge /FO LIST", "schtasks /End /TN HelloSeal Bridge", "schtasks /Delete /TN HelloSeal Bridge /F")
	if exists(s.launcherPath()) {
		t.Fatal("the launcher is still there")
	}
}
