package client

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// pairSecond is the unit of the server's interval and expiry; tests shrink it.
var pairSecond = time.Second

// PairOptions describe this computer to HelloSeal.
type PairOptions struct {
	Server  string
	Name    string
	Version string
	// Cwd is offered as the first folder in the confirm dialog.
	Cwd string
	// Out receives the code and instructions.
	Out io.Writer
	// OpenBrowser opens the confirm page; failures are ignored. Nil skips it.
	OpenBrowser func(url string) error
}

// Pair runs the device-code flow: it asks HelloSeal for a code, shows it,
// polls until the person confirms or refuses it, and saves the connection.
func Pair(ctx context.Context, store Store, options PairOptions) (Config, error) {
	key, err := store.KeyOrCreate()
	if err != nil {
		return Config{}, fmt.Errorf("create this computer's key: %w", err)
	}
	api := NewAPI(options.Server, "", nil)
	api.HTTP.Timeout = 30 * time.Second
	start, err := api.DeviceStart(ctx, bridge.DeviceStartRequest{
		Name:      options.Name,
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		Version:   options.Version,
		PublicKey: bridge.EncodePublicKey(key.Public().(ed25519.PublicKey)),
		Cwd:       options.Cwd,
	})
	if err != nil {
		return Config{}, fmt.Errorf("start pairing with %s: %w", options.Server, err)
	}
	out := options.Out
	if out == nil {
		out = io.Discard
	}
	link := start.VerificationURIComplete
	if link == "" {
		link = start.VerificationURI
	}
	fmt.Fprintf(out, "\nYour code: %s\n\nConfirm it in HelloSeal: %s\n\n", start.UserCode, link)
	if options.OpenBrowser != nil && link != "" {
		_ = options.OpenBrowser(link)
	}
	fmt.Fprintln(out, "Waiting for you to confirm...")

	interval := time.Duration(start.Interval) * pairSecond
	if interval <= 0 {
		interval = 3 * pairSecond
	}
	expires := 600 * pairSecond
	if start.ExpiresIn > 0 {
		expires = time.Duration(start.ExpiresIn) * pairSecond
	}
	ctx, cancel := context.WithTimeout(ctx, expires+interval)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return Config{}, errors.New("the code expired before it was confirmed; run helloseal connect again")
			}
			return Config{}, ctx.Err()
		case <-time.After(interval):
		}
		poll, err := api.DevicePoll(ctx, start.DeviceCode)
		if err != nil {
			var status *StatusError
			if errors.As(err, &status) && status.Status < 500 && status.Status != 429 {
				return Config{}, fmt.Errorf("pairing failed: %w", err)
			}
			continue // network trouble or a busy server: keep polling
		}
		switch poll.Status {
		case bridge.StatusPending:
			continue
		case bridge.StatusApproved:
			if poll.ComputerID == "" {
				return Config{}, errors.New("HelloSeal confirmed the code but sent no computer id")
			}
			config := Config{Server: options.Server, ComputerID: poll.ComputerID, Name: options.Name}
			if err := store.Save(config); err != nil {
				return Config{}, fmt.Errorf("save the connection: %w", err)
			}
			return config, nil
		case bridge.StatusDenied:
			return Config{}, errors.New("the code was refused in HelloSeal")
		case bridge.StatusExpired:
			return Config{}, errors.New("the code expired before it was confirmed; run helloseal connect again")
		default:
			return Config{}, fmt.Errorf("HelloSeal answered an unknown pairing status %q", poll.Status)
		}
	}
}

// OpenBrowser opens url in the default browser.
func OpenBrowser(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return errors.New("no display")
		}
		command = exec.Command("xdg-open", url)
	}
	command.Stdout, command.Stderr = nil, nil
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

// Disconnect removes this computer from HelloSeal and deletes the saved
// connection and key. When HelloSeal already forgot the computer, only the
// local files go.
func Disconnect(ctx context.Context, store Store, config Config) error {
	key, err := store.Key()
	if err != nil {
		return err
	}
	api := NewAPI(config.Server, config.ComputerID, key)
	api.HTTP.Timeout = 30 * time.Second
	if err := api.Disconnect(ctx); err != nil && !errors.Is(err, ErrDisconnected) {
		return fmt.Errorf("could not reach %s to disconnect (%w); try again, or remove the computer in Settings → Computers", config.Server, err)
	}
	return store.Delete()
}
