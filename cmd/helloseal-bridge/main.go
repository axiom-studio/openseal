// Command helloseal-bridge is the HelloSeal bridge: it connects this computer to
// HelloSeal so agents can run commands and use files in the folders you
// allow. It builds with CGO_ENABLED=0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/axiom-studio/openseal/pkg/computer/client"
)

// version is stamped by the release build with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 1
	}
	switch args[0] {
	case "connect":
		return connectCmd(args[1:])
	case "start":
		return startCmd(args[1:])
	case "status":
		return statusCmd(args[1:])
	case "disconnect":
		return disconnectCmd(args[1:])
	case "service":
		return serviceCmd(args[1:])
	case "version", "--version", "-v":
		fmt.Printf("helloseal-bridge %s\n", version)
		return 0
	case "help", "--help", "-h":
		printUsage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		printUsage()
		return 1
	}
}

func printUsage() {
	fmt.Println(`helloseal-bridge - connect this computer to HelloSeal

Usage:
  helloseal-bridge <command> [arguments]

Commands:
  connect     Pair this computer with HelloSeal and run the bridge in this
              terminal
  start       Run the bridge in this terminal; the background service runs this
  service     install, uninstall, status or restart the background service,
              which keeps the bridge running after you close the terminal
  status      Show which HelloSeal this computer is connected to
  disconnect  Remove this computer from HelloSeal and stop the service
  version     Print version
  help        Print this help message

Commands run as your user, only in the folders you allow in
Settings → Computers.`)
}

func connectCmd(args []string) int {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	server := fs.String("server", "", "HelloSeal URL (default: $HELLOSEAL_SERVER, the saved server, or "+client.DefaultServer+")")
	name := fs.String("name", "", "name shown in HelloSeal (default: this computer's hostname)")
	fs.Parse(args)

	store, err := client.DefaultStore()
	if err != nil {
		return fail(err)
	}
	saved, _ := store.Load()
	computerName := *name
	if computerName == "" {
		if computerName, err = os.Hostname(); err != nil || computerName == "" {
			computerName = "My computer"
		}
	}
	cwd, _ := os.Getwd()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := client.Pair(ctx, store, client.PairOptions{
		Server:      client.ResolveServer(*server, saved.Server),
		Name:        computerName,
		Version:     version,
		Cwd:         cwd,
		Out:         os.Stdout,
		OpenBrowser: client.OpenBrowser,
	})
	if err != nil {
		if ctx.Err() != nil {
			return 130
		}
		return fail(err)
	}
	fmt.Printf("Connected %s to %s.\n", config.Name, config.Server)
	serviceHint(ctx, store, os.Stdout, true)
	return serve(ctx, store, config, os.Stdout)
}

// serviceHintFile marks that the person has been told how to run the bridge
// in the background, so a terminal run says it once, not every time.
const serviceHintFile = "service-hint-shown"

// serviceHint tells the person, on the bridge's first run in a terminal, how
// to keep it running after the terminal closes. The bridge runs in the
// foreground unless they install the service themselves.
func serviceHint(ctx context.Context, store client.Store, out io.Writer, always bool) {
	marker := filepath.Join(store.Dir, serviceHintFile)
	if _, err := os.Stat(marker); err == nil && !always {
		return
	}
	if service, err := newService(store); err == nil {
		if status, err := service.Status(ctx); err == nil && status.Installed {
			return
		}
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "The bridge runs in this terminal and stops when you close it.")
	fmt.Fprintln(out, "To keep it running in the background and start it when you log in, press Ctrl+C and run:")
	fmt.Fprintln(out, "  helloseal-bridge service install")
	fmt.Fprintln(out, "")
	_ = os.WriteFile(marker, nil, 0o600)
}

func newService(store client.Store) (client.ServiceManager, error) {
	options, err := client.DefaultServiceOptions()
	if err != nil {
		return nil, err
	}
	return client.NewService(options, store), nil
}

func serviceCmd(args []string) int {
	usage := "usage: helloseal-bridge service install|uninstall|status|restart"
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, usage)
		return 1
	}
	store, err := client.DefaultStore()
	if err != nil {
		return fail(err)
	}
	service, err := newService(store)
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch args[0] {
	case "install":
		if _, err := store.Load(); err != nil {
			return fail(err)
		}
		if err := service.Install(ctx); err != nil {
			return fail(err)
		}
		fmt.Println("The HelloSeal bridge is running in the background.")
		for _, note := range service.Notes() {
			fmt.Println(note)
		}
		fmt.Printf("Logs: %s\n", service.Logs())
	case "uninstall":
		if err := service.Uninstall(ctx); err != nil {
			return fail(err)
		}
		fmt.Println("The background service is removed. Run helloseal-bridge start to run the bridge in a terminal.")
	case "status":
		fmt.Println(serviceLine(ctx, service))
	case "restart":
		if err := service.Restart(ctx); err != nil {
			return fail(err)
		}
		fmt.Println("Restarted the background service.")
	default:
		fmt.Fprintln(os.Stderr, usage)
		return 1
	}
	return 0
}

// serviceLine says in one line whether the background service runs.
func serviceLine(ctx context.Context, service client.ServiceManager) string {
	status, err := service.Status(ctx)
	switch {
	case err != nil:
		return "Background service: unknown (" + err.Error() + ")"
	case !status.Installed:
		return "Background service: not installed (helloseal-bridge service install)"
	case status.Running:
		return "Background service: running (" + status.Detail + "). Logs: " + service.Logs()
	}
	return "Background service: installed, not running (" + status.Detail + "). Logs: " + service.Logs()
}

func startCmd(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	logPath := fs.String("log", "", "append the bridge's output to this file (the Windows service, which has no console, uses it)")
	fs.Parse(args)
	out := io.Writer(os.Stdout)
	if *logPath != "" {
		file, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fail(err)
		}
		defer file.Close()
		out, os.Stderr = file, file
	}
	store, err := client.DefaultStore()
	if err != nil {
		return fail(err)
	}
	config, err := store.Load()
	if errors.Is(err, client.ErrNotConnected) {
		fmt.Fprintln(os.Stderr, "helloseal-bridge:", err)
		return client.ExitNotConnected
	}
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The service runs start with --log; only a person at a terminal needs the hint.
	if *logPath == "" {
		serviceHint(ctx, store, out, false)
	}
	return serve(ctx, store, config, out)
}

func serve(ctx context.Context, store client.Store, config client.Config, out io.Writer) int {
	key, err := store.Key()
	if errors.Is(err, client.ErrNotConnected) {
		fmt.Fprintln(os.Stderr, "helloseal-bridge:", err)
		return client.ExitNotConnected
	}
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(out, "HelloSeal bridge running for %s. Press Ctrl+C to stop.\n", config.Name)
	err = client.Serve(ctx, client.NewAPI(config.Server, config.ComputerID, key), client.ServeOptions{Log: out})
	if errors.Is(err, client.ErrDisconnected) {
		// HelloSeal no longer knows this computer, so its key is useless.
		// Forgetting it tells the background service to stop restarting.
		_ = store.Delete()
		fmt.Fprintln(os.Stderr, client.ErrDisconnected.Error())
		return client.ExitNotConnected
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

func statusCmd(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	fs.Parse(args)
	store, err := client.DefaultStore()
	if err != nil {
		return fail(err)
	}
	config, err := store.Load()
	if errors.Is(err, client.ErrNotConnected) {
		fmt.Println("Not connected. Run helloseal-bridge connect.")
		return 0
	}
	if err != nil {
		return fail(err)
	}
	fmt.Printf("Connected to %s as %s.\nComputer id: %s\nConfig: %s\n", config.Server, config.Name, config.ComputerID, store.Dir)
	if service, err := newService(store); err == nil {
		fmt.Println(serviceLine(context.Background(), service))
	}
	return 0
}

func disconnectCmd(args []string) int {
	fs := flag.NewFlagSet("disconnect", flag.ExitOnError)
	fs.Parse(args)
	store, err := client.DefaultStore()
	if err != nil {
		return fail(err)
	}
	// The service goes first, so it cannot restart against a connection
	// that is going away.
	if service, err := newService(store); err == nil {
		if err := service.Uninstall(context.Background()); err != nil {
			fmt.Fprintf(os.Stderr, "Could not remove the background service: %v\n", err)
		}
	}
	config, err := store.Load()
	if errors.Is(err, client.ErrNotConnected) {
		fmt.Println("Not connected.")
		return 0
	}
	if err != nil {
		return fail(err)
	}
	if err := client.Disconnect(context.Background(), store, config); err != nil {
		return fail(err)
	}
	fmt.Printf("Disconnected %s from %s.\n", config.Name, config.Server)
	return 0
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "helloseal-bridge:", err)
	return 1
}
