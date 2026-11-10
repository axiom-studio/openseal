// Command helloseal-bridge is the HelloSeal bridge: it connects this computer to
// HelloSeal so agents can run commands and use files in the folders you
// allow. It builds with CGO_ENABLED=0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
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
  connect     Pair this computer with HelloSeal, then start the bridge
  start       Run the bridge so your agents can use this computer
  status      Show which HelloSeal this computer is connected to
  disconnect  Remove this computer from HelloSeal
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
	return serve(ctx, store, config)
}

func startCmd(args []string) int {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	fs.Parse(args)
	store, err := client.DefaultStore()
	if err != nil {
		return fail(err)
	}
	config, err := store.Load()
	if err != nil {
		return fail(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, store, config)
}

func serve(ctx context.Context, store client.Store, config client.Config) int {
	key, err := store.Key()
	if err != nil {
		return fail(err)
	}
	fmt.Printf("HelloSeal bridge running for %s. Press Ctrl+C to stop.\n", config.Name)
	err = client.Serve(ctx, client.NewAPI(config.Server, config.ComputerID, key), client.ServeOptions{Log: os.Stdout})
	if errors.Is(err, client.ErrDisconnected) {
		fmt.Fprintln(os.Stderr, client.ErrDisconnected.Error())
		return 1
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
	return 0
}

func disconnectCmd(args []string) int {
	fs := flag.NewFlagSet("disconnect", flag.ExitOnError)
	fs.Parse(args)
	store, err := client.DefaultStore()
	if err != nil {
		return fail(err)
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
