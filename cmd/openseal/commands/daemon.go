package commands

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/axiom-studio/openseal/internal/daemon"
	"github.com/axiom-studio/openseal/internal/workflow"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/trigger"
	"go.uber.org/zap"
)

func daemonCmd(args []string) {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	configPath := fs.String("config", "daemon.yaml", "path to daemon config file")
	help := fs.Bool("help", false, "print help for daemon")
	fs.Parse(args)

	if *help {
		fmt.Println(`Usage: openseal daemon [options]

Start the OpenSeal daemon with trigger-driven workflow execution.

Options:
  --config <path>   Path to daemon config file (default: daemon.yaml)
  --help            Print this help message`)
		return
	}

	logger, err := zap.NewProduction()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create logger: %v\n", err)
		os.Exit(1)
	}
	sugar := logger.Sugar()
	defer logger.Sync()

	cfg, err := daemon.LoadDaemonConfig(*configPath)
	if err != nil {
		sugar.Fatalf("failed to load config: %v", err)
	}

	sugar.Infow("daemon config loaded",
		"workflowsDir", cfg.WorkflowsDir,
		"logLevel", cfg.LogLevel,
		"webhookAddr", cfg.Webhook.ListenAddr,
	)

	workflows, err := workflow.LoadWorkflowDir(cfg.WorkflowsDir)
	if err != nil {
		sugar.Fatalf("failed to load workflows: %v", err)
	}

	workflowMap := make(map[string]*workflow.Workflow)
	for _, wf := range workflows {
		workflowMap[wf.SourceFile] = wf
		sugar.Infow("loaded workflow", "name", wf.Name, "source", wf.SourceFile)
	}

	reg := executor.NewEmptyRegistry()
	pe := executor.NewPipelineExecutor(reg, sugar)

	tm := daemon.NewTriggerManager(sugar, cfg.Webhook.BaseURL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	instanceToWorkflow, err := tm.RegisterAll(ctx, cfg)
	if err != nil {
		sugar.Fatalf("failed to register triggers: %v", err)
	}

	handler := trigger.TriggerHandler(func(ctx context.Context, instanceID int, event trigger.TriggerEvent) error {
		wfFile, ok := instanceToWorkflow[instanceID]
		if !ok {
			sugar.Warnw("no workflow for instance", "instanceId", instanceID)
			return nil
		}
		wf, ok := workflowMap[wfFile]
		if !ok {
			sugar.Warnw("workflow file not found", "file", wfFile)
			return nil
		}
		sugar.Infow("executing workflow from trigger", "workflow", wf.Name, "instanceId", instanceID)

		nodes := convertNodes(wf.Nodes)
		connections := convertEdges(wf.Edges)
		startNodeId := ""
		if len(nodes) > 0 {
			startNodeId = nodes[0].Id
		}

		triggerData := map[string]interface{}{
			"type":  event.Type,
			"source": event.Source,
			"data":  event.Payload,
		}
		_, err := pe.Execute(ctx, instanceID, nodes, connections, startNodeId, triggerData, nil)
		return err
	})

	if err := tm.Start(ctx, handler); err != nil {
		sugar.Fatalf("failed to start triggers: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	sugar.Infow("starting webhook server", "addr", cfg.Webhook.ListenAddr)
	server := &http.Server{
		Addr:    cfg.Webhook.ListenAddr,
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			sugar.Errorw("webhook server error", "error", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	sugar.Infow("shutting down", "signal", sig)

	if err := tm.Stop(ctx); err != nil {
		sugar.Errorw("trigger stop error", "error", err)
	}
	server.Shutdown(ctx)
}