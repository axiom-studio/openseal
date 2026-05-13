package commands

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/axiom-studio/openseal/internal/workflow"
	"github.com/axiom-studio/openseal/pkg/executor"
	"go.uber.org/zap"
)

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	help := fs.Bool("help", false, "print help for run")
	fs.Parse(args)

	if *help || fs.NArg() == 0 {
		fmt.Println(`Usage: openseal run <workflow.hcl> [options]

Execute a workflow defined in an HCL file.

Arguments:
  <workflow.hcl>  Path to the workflow HCL file

Options:
  --help          Print this help message`)
		return
	}

	path := fs.Arg(0)

	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	sugar := logger.Sugar()

	wf, err := workflow.LoadWorkflow(path)
	if err != nil {
		sugar.Fatalf("failed to load workflow: %v", err)
	}

	sugar.Infow("workflow loaded",
		"name", wf.Name,
		"nodes", len(wf.Nodes),
		"edges", len(wf.Edges),
		"source", wf.SourceFile,
	)

	reg := executor.NewRegistry(nil)
	pe := executor.NewPipelineExecutor(reg, sugar)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	nodes := convertNodes(wf.Nodes)
	connections := convertEdges(wf.Edges)

	startNodeId := ""
	if len(nodes) > 0 {
		startNodeId = nodes[0].Id
	}

	result, err := pe.Execute(ctx, 0, nodes, connections, startNodeId, nil, nil)
	if err != nil {
		sugar.Fatalf("workflow execution failed: %v", err)
	}

	sugar.Infow("workflow completed",
		"status", result.Status,
		"duration", result.CompletedAt.Sub(result.StartedAt),
	)

	fmt.Println("\n--- Node Outputs ---")
	for _, nr := range result.NodeResults {
		out, _ := json.MarshalIndent(nr.Output, "", "  ")
		fmt.Printf("\n[%s] %s (%s):\n%s\n", nr.NodeId, nr.NodeName, nr.NodeType, out)
	}
}

func convertNodes(nodes []workflow.Node) []*executor.NodeDefinition {
	out := make([]*executor.NodeDefinition, len(nodes))
	for i, n := range nodes {
		out[i] = &executor.NodeDefinition{
			Id:     n.ID,
			Name:   n.ID,
			Type:   n.Type,
			Config: n.Config,
		}
	}
	return out
}

func convertEdges(edges []workflow.Edge) []*executor.ConnectionDefinition {
	out := make([]*executor.ConnectionDefinition, len(edges))
	for i, e := range edges {
		out[i] = &executor.ConnectionDefinition{
			Id:           fmt.Sprintf("edge-%d", i),
			SourceNodeId: e.From,
			TargetNodeId: e.To,
			Label:        e.Condition,
		}
	}
	return out
}