package commands

import (
	"fmt"
	"os"
)

const version = "0.1.0"

// Execute dispatches to the appropriate subcommand.
// Args should be os.Args[1:].
func Execute(args []string) {
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	switch args[0] {
	case "run":
		runCmd(args[1:])
	case "daemon":
		daemonCmd(args[1:])
	case "validate":
		validateCmd(args[1:])
	case "skill":
		skillCmd(args[1:])
	case "workflow":
		workflowCmd(args[1:])
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
  run       Execute a workflow from an HCL file
  daemon    Start the daemon with trigger-driven execution
  validate  Validate a workflow HCL file
  skill     Manage skills (install, list)
  workflow  Manage workflows (create)
  version   Print version
  help      Print this help message

Use "openseal <command> --help" for more information about a command.`)
}