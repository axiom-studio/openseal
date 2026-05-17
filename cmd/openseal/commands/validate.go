package commands

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/axiom-studio/openseal/internal/workflow"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/validation"
)

func validateCmd(args []string) {
	fs := flag.NewFlagSet("validate", flag.ExitOnError)
	jsonOut := fs.Bool("json", false, "output results as JSON")
	help := fs.Bool("help", false, "print help for validate")
	fs.Parse(args)

	if *help || fs.NArg() == 0 {
		fmt.Println(`Usage: openseal validate <workflow.hcl> [options]

Validate a workflow definition for structural and schema issues.

Arguments:
  <workflow.hcl>  Path to the workflow HCL file

Options:
  --json   Output results as JSON
  --help   Print this help message`)
		return
	}

	path := fs.Arg(0)

	reg := executor.NewRegistry(nil)
	validator := validation.NewValidator(reg)

	wf, err := workflow.LoadWorkflow(path)
	if err != nil {
		// Parse errors are still validation failures — present them nicely
		result := &validation.Result{
			Valid:   false,
			Summary: "1 error(s), 0 warning(s)",
			Issues: []validation.Issue{{
				Level:   "error",
				Message: fmt.Sprintf("parse error: %v", err),
			}},
		}
		if *jsonOut {
			out, _ := json.MarshalIndent(result, "", "  ")
			fmt.Println(string(out))
		} else {
			printValidationResult(result)
		}
		os.Exit(1)
	}

	result := validator.Validate(wf)

	if *jsonOut {
		out, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(out))
	} else {
		printValidationResult(result)
	}

	if !result.Valid {
		os.Exit(1)
	}
}

func printValidationResult(result *validation.Result) {
	if result.Valid {
		fmt.Println("✓ Valid workflow")
	} else {
		fmt.Println("✗ Invalid workflow")
	}
	fmt.Println()
	for _, issue := range result.Issues {
		icon := "⚠"
		if issue.Level == "error" {
			icon = "✗"
		}
		if issue.NodeID != "" {
			fmt.Printf("%s [%s] %s: %s\n", icon, issue.Level, issue.NodeID, issue.Message)
		} else if issue.EdgeID != "" {
			fmt.Printf("%s [%s] %s: %s\n", icon, issue.Level, issue.EdgeID, issue.Message)
		} else {
			fmt.Printf("%s [%s] %s\n", icon, issue.Level, issue.Message)
		}
	}
	if len(result.Issues) > 0 {
		fmt.Println()
	}
	fmt.Println(result.Summary)
}
