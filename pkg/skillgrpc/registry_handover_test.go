package skillgrpc

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/skills.sdk/executor"
	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

type handoverSkillTransport struct {
	*verifiedRegistryTransport
	execute func(context.Context) (*skillpb.ExecuteResponse, error)
}

func (s *handoverSkillTransport) Execute(ctx context.Context, _ *skillpb.ExecuteRequest, _ ...grpc.CallOption) (*skillpb.ExecuteResponse, error) {
	return s.execute(ctx)
}

func newHandoverClient(t *testing.T, address string, execute func(context.Context) (*skillpb.ExecuteResponse, error)) (*Client, *grpc.ClientConn) {
	t.Helper()
	// NewClient creates an idle connection without dialing a socket. RPCs use
	// the in-process transport, while connection state proves actual closure.
	conn, err := grpc.NewClient("passthrough:///"+address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	transport := &handoverSkillTransport{
		verifiedRegistryTransport: &verifiedRegistryTransport{health: func(context.Context, int) (*skillpb.HealthResponse, error) {
			return &skillpb.HealthResponse{Healthy: true, SkillId: "chat", Version: "2.2.8"}, nil
		}},
		execute: execute,
	}
	return &Client{conn: conn, client: transport, skillID: "chat", address: address}, conn
}

func handoverResponse(runtime string) *skillpb.ExecuteResponse {
	return &skillpb.ExecuteResponse{Output: map[string][]byte{"runtime": []byte(`"` + runtime + `"`)}}
}

type handoverExecution struct {
	result *executor.StepResult
	err    error
}

func executeHandoverClient(ctx context.Context, client *Client) handoverExecution {
	result, err := client.ExecuteWithContext(ctx, &executor.StepDefinition{Id: "send", Type: "send-message"}, &contextResolver{}, ExecutionContext{RunID: "durable-run"})
	return handoverExecution{result: result, err: err}
}

func requireHandoverResult(t *testing.T, execution handoverExecution, runtime string) {
	t.Helper()
	if execution.err != nil || execution.result == nil || execution.result.Output["runtime"] != runtime {
		t.Fatalf("execution did not finish on %s: %#v, %v", runtime, execution.result, execution.err)
	}
}

func requireRetiredCount(t *testing.T, registry *Registry, want int) {
	t.Helper()
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	if len(registry.retired) != want {
		t.Fatalf("retired clients=%d, want=%d", len(registry.retired), want)
	}
}

func TestVerifiedSameVersionHandoverDrainsAcquiredExecution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	old, oldConn := newHandoverClient(t, "legacy-chat", func(ctx context.Context) (*skillpb.ExecuteResponse, error) {
		close(entered)
		select {
		case <-release:
			return handoverResponse("legacy"), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	current, currentConn := newHandoverClient(t, "exact-chat", func(context.Context) (*skillpb.ExecuteResponse, error) {
		return handoverResponse("exact"), nil
	})
	registry := NewRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	registry.connect = func(_ context.Context, address string) (*Client, error) {
		if address == old.Address() {
			return old, nil
		}
		return current, nil
	}
	key := "tenant:7\x00skill:chat\x00version:2.2.8"
	if _, err := registry.RegisterAsVerified(ctx, key, old.Address(), "chat", "2.2.8"); err != nil {
		t.Fatal(err)
	}
	done := make(chan handoverExecution, 1)
	go func() { done <- executeHandoverClient(ctx, registry.GetClientForSkillType(key, "send-message")) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("old execution did not start")
	}
	if _, err := registry.RegisterAsVerified(ctx, key, current.Address(), "chat", "2.2.8"); err != nil {
		t.Fatal(err)
	}
	if registry.GetClientForSkillType(key, "send-message") != current || oldConn.GetState() == connectivity.Shutdown {
		t.Fatal("handover did not publish the new client while preserving the active old connection")
	}
	requireRetiredCount(t, registry, 1)
	if stale := executeHandoverClient(ctx, old); !errors.Is(stale.err, ErrClientRetired) {
		t.Fatalf("stale client dispatched a new execution: %v", stale.err)
	}
	requireHandoverResult(t, executeHandoverClient(ctx, registry.GetClientForSkillType(key, "send-message")), "exact")
	close(release)
	requireHandoverResult(t, <-done, "legacy")
	if oldConn.GetState() != connectivity.Shutdown || currentConn.GetState() == connectivity.Shutdown {
		t.Fatal("draining did not close only the retired transport")
	}
	requireRetiredCount(t, registry, 0)
}

func TestUnregisterDrainsExecutionUntilItsCallerCancels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{})
	client, conn := newHandoverClient(t, "chat", func(ctx context.Context) (*skillpb.ExecuteResponse, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	registry := NewRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	registry.connect = func(context.Context, string) (*Client, error) { return client, nil }
	if _, err := registry.RegisterAsVerified(ctx, "exact-key", "chat", "chat", "2.2.8"); err != nil {
		t.Fatal(err)
	}
	done := make(chan handoverExecution, 1)
	go func() { done <- executeHandoverClient(ctx, client) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("execution did not start")
	}
	if err := registry.Unregister("exact-key"); err != nil {
		t.Fatal(err)
	}
	if registry.GetClient("exact-key") != nil || registry.HasType("send-message") || conn.GetState() == connectivity.Shutdown {
		t.Fatal("unregister retained dispatch authority or interrupted the acquired RPC")
	}
	requireRetiredCount(t, registry, 1)
	if stale := executeHandoverClient(ctx, client); !errors.Is(stale.err, ErrClientRetired) {
		t.Fatalf("unregistered client dispatched: %v", stale.err)
	}
	cancel()
	if execution := <-done; !errors.Is(execution.err, context.Canceled) {
		t.Fatalf("caller cancellation was lost: %v", execution.err)
	}
	if conn.GetState() != connectivity.Shutdown {
		t.Fatal("canceled caller left a retired connection open")
	}
	requireRetiredCount(t, registry, 0)
}

func TestRegistryCloseForceClosesRetiredActiveConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	entered := make(chan struct{})
	client, conn := newHandoverClient(t, "chat", func(ctx context.Context) (*skillpb.ExecuteResponse, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	registry := NewRegistry()
	registry.connect = func(context.Context, string) (*Client, error) { return client, nil }
	if _, err := registry.RegisterAsVerified(ctx, "exact-key", "chat", "chat", "2.2.8"); err != nil {
		t.Fatal(err)
	}
	done := make(chan handoverExecution, 1)
	go func() { done <- executeHandoverClient(ctx, client) }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("execution did not start")
	}
	if err := registry.Unregister("exact-key"); err != nil {
		t.Fatal(err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if conn.GetState() != connectivity.Shutdown {
		t.Fatal("shutdown failed to force close the draining connection")
	}
	requireRetiredCount(t, registry, 0)
	cancel()
	<-done
}

func TestRetireIdleClientClosesAndRejectsAllNewRPCs(t *testing.T) {
	client, conn := newHandoverClient(t, "idle-chat", func(context.Context) (*skillpb.ExecuteResponse, error) {
		t.Error("retired transport executed a new call")
		return handoverResponse("unexpected"), nil
	})
	if err := client.Retire(); err != nil {
		t.Fatal(err)
	}
	if conn.GetState() != connectivity.Shutdown {
		t.Fatal("idle retired connection remained open")
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"connect": func() error { return client.Connect(ctx) },
		"execute": func() error { return executeHandoverClient(ctx, client).err },
		"health":  func() error { _, err := client.Health(ctx); return err },
		"types":   func() error { _, err := client.GetNodeTypes(ctx); return err },
		"schema":  func() error { _, err := client.GetNodeSchema(ctx, "send-message"); return err },
	} {
		if err := call(); !errors.Is(err, ErrClientRetired) {
			t.Fatalf("retired %s did not reject the new RPC: %v", name, err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatalf("repeat close failed: %v", err)
	}
}

type handoverPreparingResolver struct {
	contextResolver
	ctx     context.Context
	entered chan struct{}
	release chan struct{}
}

func (r *handoverPreparingResolver) GetStepOutput(string) interface{} {
	close(r.entered)
	select {
	case <-r.release:
	case <-r.ctx.Done():
	}
	return nil
}

func TestExecuteAcquisitionCoversPreparationAndReleasesOnSerializationFailure(t *testing.T) {
	t.Run("preparation drains before closure", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		resolver := &handoverPreparingResolver{ctx: ctx, entered: make(chan struct{}), release: make(chan struct{})}
		client, conn := newHandoverClient(t, "preparing-chat", func(context.Context) (*skillpb.ExecuteResponse, error) {
			return handoverResponse("acquired"), nil
		})
		done := make(chan handoverExecution, 1)
		go func() {
			result, err := client.ExecuteWithContext(ctx, &executor.StepDefinition{Id: "send", Type: "send-message"}, resolver, ExecutionContext{})
			done <- handoverExecution{result: result, err: err}
		}()
		select {
		case <-resolver.entered:
		case <-ctx.Done():
			t.Fatal("execution preparation did not start")
		}
		if err := client.Retire(); err != nil {
			t.Fatal(err)
		}
		if conn.GetState() == connectivity.Shutdown {
			t.Fatal("retirement closed an execution still preparing its RPC")
		}
		close(resolver.release)
		requireHandoverResult(t, <-done, "acquired")
		if conn.GetState() != connectivity.Shutdown {
			t.Fatal("completed execution preparation left the retired connection open")
		}
	})
	t.Run("serialization failure releases acquisition", func(t *testing.T) {
		client, conn := newHandoverClient(t, "invalid-config-chat", func(context.Context) (*skillpb.ExecuteResponse, error) {
			t.Error("invalid configuration was dispatched")
			return nil, nil
		})
		_, err := client.ExecuteWithContext(context.Background(), &executor.StepDefinition{
			Id: "send", Type: "send-message", Config: map[string]interface{}{"invalid": make(chan int)},
		}, &contextResolver{}, ExecutionContext{})
		if err == nil {
			t.Fatal("invalid configuration was accepted")
		}
		if err := client.Retire(); err != nil {
			t.Fatal(err)
		}
		if conn.GetState() != connectivity.Shutdown {
			t.Fatal("serialization failure leaked its connection acquisition")
		}
	})
}
