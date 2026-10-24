package commands

import (
	"fmt"
	"os"
)

// version is stamped by release builds with
// -ldflags "-X github.com/axiom-studio/openseal/cmd/openseal/commands.version=<semver>".
// Local builds report the dev version, which sorts before every release.
var version = "0.0.0-dev"

// Execute dispatches to the appropriate subcommand.
// Args should be os.Args[1:].
func Execute(args []string) {
	if len(args) == 0 {
		tuiCmd(nil)
		return
	}

	switch args[0] {
	case "tui":
		tuiCmd(args[1:])
	case "run":
		runCmd(args[1:])
	case "daemon":
		daemonCmd(args[1:])
	case "validate":
		validateCmd(args[1:])
	case "workflow":
		workflowCmd(args[1:])
	case "skill":
		skillCmd(args[1:])
	case "bundle":
		bundleCmd(args[1:])
	case "version":
		fmt.Printf("OpenSeal v%s\n", version)
	case "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", args[0])
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Println(`OpenSeal - Agent execution platform

Usage:
  openseal <command> [arguments]

Commands:
  tui       Open the prompt-first Agent and Team workspace (default)
  run       Execute a workflow from an HCL file
  daemon    Start the durable Agent and Team kernel
  validate  Validate a workflow HCL file
  workflow  Manage workflows (create)
  skill     Export canonical bundled Skill manifests
  bundle    Validate, inspect, diff, and plan portable workforce bundles
  version   Print version
  help      Print this help message

Use "openseal <command> --help" for more information about a command.`)
}
