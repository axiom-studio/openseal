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
	"github.com/axiom-studio/openseal/internal/server"
	"github.com/axiom-studio/openseal/internal/workflow"
	"github.com/axiom-studio/openseal/pkg/executor"
	"github.com/axiom-studio/openseal/pkg/runtime"
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

	configMissing := false
	if _, statErr := os.Stat(*configPath); os.IsNotExist(statErr) {
		configMissing = true
	}

	cfg, err := daemon.LoadDaemonConfig(*configPath)
	if err != nil {
		sugar.Fatalf("failed to load config: %v", err)
	}

	if configMissing {
		sugar.Infow("auto-created default config", "path", *configPath)
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

	reg := executor.NewRegistry(nil)
	pe := executor.NewPipelineExecutor(reg, sugar)

	// Execution store
	store := runtime.NewMemoryStore(100)

	// Worker pool for async execution
	pool := runtime.NewWorkerPool(pe, store, sugar, 4, nil)
	scheduler := runtime.NewScheduler(pool, store)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool.Start(ctx)

	// API server for web GUI
	apiServer := server.NewServer(reg, pe, store, sugar)
	apiServer.SetWorkflows(workflows)

	go func() {
		if err := apiServer.ListenAndServe(cfg.API.ListenAddr); err != nil && err != http.ErrServerClosed {
			sugar.Errorw("API server error", "error", err)
		}
	}()

	tm := daemon.NewTriggerManager(sugar, cfg.Webhook.BaseURL)

	instanceToWorkflow, err := tm.RegisterAll(ctx, cfg)
	if err != nil {
		sugar.Fatalf("failed to register triggers: %v", err)
	}

	// Build workflow entries once for scheduling
	workflowEntries := make(map[string]runtime.WorkflowEntry)
	for _, wf := range workflows {
		entry := runtime.WorkflowEntry{
			Name: wf.Name,
		}
		for _, n := range wf.Nodes {
			entry.Nodes = append(entry.Nodes, &executor.NodeDefinition{
				Id:     n.ID,
				Name:   n.ID,
				Type:   n.Type,
				Config: n.Config,
			})
		}
		for i, e := range wf.Edges {
			entry.Connections = append(entry.Connections, &executor.ConnectionDefinition{
				Id:           fmt.Sprintf("edge-%d", i),
				SourceNodeId: e.From,
				TargetNodeId: e.To,
				Label:        e.Condition,
			})
		}
		if len(entry.Nodes) > 0 {
			entry.StartNodeID = entry.Nodes[0].Id
		}
		workflowEntries[wf.SourceFile] = entry
	}

	handler := trigger.TriggerHandler(func(ctx context.Context, instanceID int, event trigger.TriggerEvent) error {
		wfFile, ok := instanceToWorkflow[instanceID]
		if !ok {
			sugar.Warnw("no workflow for instance", "instanceId", instanceID)
			return nil
		}
		entry, ok := workflowEntries[wfFile]
		if !ok {
			sugar.Warnw("workflow entry not found", "file", wfFile)
			return nil
		}
		sugar.Infow("scheduling workflow from trigger", "workflow", entry.Name, "instanceId", instanceID)

		triggerData := map[string]interface{}{
			"type":   event.Type,
			"source": event.Source,
			"data":   event.Payload,
		}
		_, err := scheduler.Schedule(ctx, entry, triggerData)
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
	webhookServer := &http.Server{
		Addr:    cfg.Webhook.ListenAddr,
		Handler: mux,
	}

	go func() {
		if err := webhookServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
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
	pool.Stop()
	webhookServer.Shutdown(ctx)
	apiServer.Shutdown(ctx)
}
