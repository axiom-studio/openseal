package commands

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"text/template"

	"github.com/axiom-studio/openseal/pkg/workflow"
)

func workflowCmd(args []string) {
	if len(args) == 0 {
		fmt.Println(`Usage: openseal workflow <subcommand> [options]

Subcommands:
  create   Create a new workflow HCL file

Use "openseal workflow <subcommand> --help" for more information.`)
		os.Exit(1)
	}

	switch args[0] {
	case "create":
		workflowCreateCmd(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown workflow subcommand: %s\n", args[0])
		os.Exit(1)
	}
}

func workflowCreateCmd(args []string) {
	fs := flag.NewFlagSet("workflow create", flag.ExitOnError)
	name := fs.String("name", "", "workflow name (required)")
	desc := fs.String("description", "", "workflow description")
	outputPath := fs.String("output", "", "output file path (default: <name>.hcl)")
	help := fs.Bool("help", false, "print help for workflow create")
	fs.Parse(args)

	if *help {
		fmt.Println(`Usage: openseal workflow create [options]

Create a new workflow HCL file.

Options:
  --name <string>         Workflow name (required)
  --description <string>  Workflow description
  --output <path>         Output file path (default: <name>.hcl)
  --help                  Print this help message`)
		return
	}

	if *name == "" {
		fmt.Fprintln(os.Stderr, "error: --name is required")
		fs.Usage()
		os.Exit(1)
	}

	triggerNode, err := workflow.NewNodeBuilder("webhook", "trigger").
		SetParam("path", "/"+strings.ToLower(*name)).
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building trigger node: %v\n", err)
		os.Exit(1)
	}

	actionNode, err := workflow.NewNodeBuilder("http", "action").
		SetParam("url", "https://example.com").
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building action node: %v\n", err)
		os.Exit(1)
	}

	wf, err := workflow.New(*name).
		SetDescription(*desc).
		AddNode(triggerNode).
		AddNode(actionNode).
		AddEdge("trigger", "action").
		Build()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building workflow: %v\n", err)
		os.Exit(1)
	}

	outFile := *outputPath
	if outFile == "" {
		outFile = strings.ToLower(*name) + ".hcl"
	}

	hclContent := renderWorkflowHCL(wf)
	if err := os.WriteFile(outFile, []byte(hclContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing file: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Created workflow %q at %s\n", wf.Name, outFile)
}

const hclTemplate = `workflow "{{.Name}}" {
{{- if .Description}}
  description = "{{.Description}}"
{{- end}}
{{range .Nodes}}
  node {{.Type}} "{{.ID}}" {
  {{- range $k, $v := .Config}}
    {{ $k }} = {{ $v | hclValue }}
  {{- end}}
  }
{{end}}
{{range .Edges}}
  edge "{{.From}}" "{{.To}}" {
  {{- if .Condition}}
    condition = "{{.Condition}}"
  {{- end}}
  }
{{end}}
}
`

func renderWorkflowHCL(wf *workflow.Workflow) string {
	tmpl, err := template.New("workflow").Funcs(template.FuncMap{
		"hclValue": formatValue,
	}).Parse(hclTemplate)
	if err != nil {
		return fmt.Sprintf("# template error: %v\n", err)
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, wf); err != nil {
		return fmt.Sprintf("# render error: %v\n", err)
	}
	return buf.String()
}

func formatValue(v interface{}) string {
	switch val := v.(type) {
	case string:
		return fmt.Sprintf("%q", val)
	case bool:
		if val {
			return "true"
		}
		return "false"
	case int:
		return fmt.Sprintf("%d", val)
	case float64:
		return fmt.Sprintf("%g", val)
	default:
		return fmt.Sprintf("%q", fmt.Sprintf("%v", val))
	}
}