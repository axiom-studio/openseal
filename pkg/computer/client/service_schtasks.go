package client

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// schtasksService is the bridge as a Task Scheduler task that runs at the
// person's logon (Windows). The task runs a small launcher script with
// wscript, which starts the bridge with no window, so no console pops up at
// login, and starts it again 5 seconds after it fails: Task Scheduler's own
// restart on failure only covers a task that fails to launch.
type schtasksService struct{ ServiceOptions }

const schtasksName = "HelloSeal Bridge"

func (s schtasksService) dir() string { return filepath.Join(s.LocalAppData, "HelloSeal") }

func (s schtasksService) launcherPath() string { return filepath.Join(s.dir(), ServiceName+".vbs") }

func (s schtasksService) logPath() string { return filepath.Join(s.dir(), ServiceName+".log") }

// vbsQuote is a VBScript string literal: quotes doubled inside quotes.
func vbsQuote(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

// schtasksLauncher runs `helloseal-bridge start --log <log>` hidden and
// waits, until it ends cleanly or because the computer is not connected.
func schtasksLauncher(exe, logPath string) string {
	command := vbsQuote(`"`) + " & " + vbsQuote(exe) + " & " + vbsQuote(`" start --log "`) + " & " + vbsQuote(logPath) + " & " + vbsQuote(`"`)
	return `' Runs the HelloSeal bridge with no window for the "` + schtasksName + `" logon task.
Set shell = CreateObject("WScript.Shell")
Do
  code = shell.Run(` + command + `, 0, True)
  If code = 0 Or code = ` + strconv.Itoa(ExitNotConnected) + ` Then Exit Do
  WScript.Sleep 5000
Loop
WScript.Quit code
`
}

// schtasksXML is the task: at this person's logon, as them with their own
// rights, never stopped for time, battery or idleness.
func schtasksXML(user, launcher string) string {
	who := xmlText(user)
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>Keeps the HelloSeal bridge running so your HelloSeal agents can use this computer.</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>` + who + `</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>` + who + `</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>wscript.exe</Command>
      <Arguments>//B //Nologo ` + xmlText(`"`+launcher+`"`) + `</Arguments>
    </Exec>
  </Actions>
</Task>
`
}

// utf16File is text as schtasks /XML reads it reliably: UTF-16LE with a BOM.
func utf16File(text string) []byte {
	units := utf16.Encode([]rune(strings.ReplaceAll(text, "\n", "\r\n")))
	out := make([]byte, 2, 2+2*len(units))
	out[0], out[1] = 0xFF, 0xFE
	for _, unit := range units {
		out = binary.LittleEndian.AppendUint16(out, unit)
	}
	return out
}

func (s schtasksService) schtasks(ctx context.Context, args ...string) (string, error) {
	return s.Run(ctx, "schtasks", args...)
}

func (s schtasksService) Install(ctx context.Context) error {
	if strings.TrimSpace(s.User) == "" {
		return errors.New("could not tell which Windows account to run the bridge as")
	}
	if err := writeServiceFile(s.launcherPath(), []byte(strings.ReplaceAll(schtasksLauncher(s.Exe, s.logPath()), "\n", "\r\n"))); err != nil {
		return err
	}
	definition := filepath.Join(s.dir(), ServiceName+"-task.xml")
	if err := writeServiceFile(definition, utf16File(schtasksXML(s.User, s.launcherPath()))); err != nil {
		return err
	}
	defer os.Remove(definition)
	if out, err := s.schtasks(ctx, "/Create", "/TN", schtasksName, "/XML", definition, "/F"); err != nil {
		return runFailed("schtasks /Create", out, err)
	}
	// A reinstall stops the running bridge so the new binary and connection
	// take effect.
	_, _ = s.schtasks(ctx, "/End", "/TN", schtasksName)
	if out, err := s.schtasks(ctx, "/Run", "/TN", schtasksName); err != nil {
		return runFailed("schtasks /Run", out, err)
	}
	return nil
}

func (s schtasksService) installed(ctx context.Context) (string, bool) {
	out, err := s.schtasks(ctx, "/Query", "/TN", schtasksName, "/FO", "LIST")
	return out, err == nil
}

func (s schtasksService) Uninstall(ctx context.Context) error {
	if _, ok := s.installed(ctx); ok {
		_, _ = s.schtasks(ctx, "/End", "/TN", schtasksName)
		if out, err := s.schtasks(ctx, "/Delete", "/TN", schtasksName, "/F"); err != nil {
			return runFailed("schtasks /Delete", out, err)
		}
	}
	if err := os.Remove(s.launcherPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s schtasksService) Status(ctx context.Context) (ServiceStatus, error) {
	out, ok := s.installed(ctx)
	if !ok {
		return ServiceStatus{}, nil
	}
	state := "installed"
	for _, line := range strings.Split(out, "\n") {
		if key, value, found := strings.Cut(line, ":"); found && strings.TrimSpace(key) == "Status" {
			state = strings.TrimSpace(value)
			break
		}
	}
	return ServiceStatus{Installed: true, Running: state == "Running", Detail: state}, nil
}

func (s schtasksService) Restart(ctx context.Context) error {
	if _, ok := s.installed(ctx); !ok {
		return ErrServiceNotInstalled
	}
	_, _ = s.schtasks(ctx, "/End", "/TN", schtasksName)
	if out, err := s.schtasks(ctx, "/Run", "/TN", schtasksName); err != nil {
		return runFailed("schtasks /Run", out, err)
	}
	return nil
}

func (s schtasksService) Logs() string { return s.logPath() }

func (s schtasksService) Notes() []string { return []string{"It starts when you log in."} }
