package skillgrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/axiom-studio/skills.sdk/executor"
	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/resolver"
	"google.golang.org/grpc/resolver/manual"
)

type roundRobinSkillServer struct {
	skillpb.UnimplementedSkillServiceServer
	backend   string
	health    *skillpb.HealthResponse
	nodeTypes []string
}

func (s *roundRobinSkillServer) Health(context.Context, *skillpb.HealthRequest) (*skillpb.HealthResponse, error) {
	return s.health, nil
}

func (s *roundRobinSkillServer) GetNodeTypes(context.Context, *skillpb.GetNodeTypesRequest) (*skillpb.GetNodeTypesResponse, error) {
	return &skillpb.GetNodeTypesResponse{NodeTypes: s.nodeTypes}, nil
}

func (s *roundRobinSkillServer) Execute(context.Context, *skillpb.ExecuteRequest) (*skillpb.ExecuteResponse, error) {
	backend, err := json.Marshal(s.backend)
	if err != nil {
		return nil, err
	}
	return &skillpb.ExecuteResponse{Output: map[string][]byte{"backend": backend}}, nil
}

func startRoundRobinSkillServer(t *testing.T, skill *roundRobinSkillServer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	skillpb.RegisterSkillServiceServer(server, skill)
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	go func() { _ = server.Serve(listener) }()
	return listener.Addr().String()
}

var roundRobinResolverSequence atomic.Uint64

// These tests are deliberately sequential: resolver.Register changes gRPC's
// global builder registry. Each connection receives its own manual resolver.
func roundRobinTarget(addresses ...string) string {
	scheme := fmt.Sprintf("skillgrpc-test-%d", roundRobinResolverSequence.Add(1))
	builder := manual.NewBuilderWithScheme(scheme)
	state := resolver.State{}
	for _, address := range addresses {
		state.Addresses = append(state.Addresses, resolver.Address{Addr: address})
	}
	builder.InitialState(state)
	resolver.Register(builder)
	return scheme + ":///workers"
}

func healthyRoundRobinSkill(backend string) *roundRobinSkillServer {
	return &roundRobinSkillServer{
		backend:   backend,
		health:    &skillpb.HealthResponse{Healthy: true, SkillId: "chat", Version: "2.2.8"},
		nodeTypes: []string{"send-message"},
	}
}

func executeRoundRobinSkill(t *testing.T, ctx context.Context, client *Client) string {
	t.Helper()
	result, err := client.ExecuteWithContext(ctx,
		&executor.StepDefinition{Id: "send", Type: "send-message"},
		&contextResolver{}, ExecutionContext{RunID: "round-robin-run"})
	if err != nil {
		t.Fatal(err)
	}
	backend, ok := result.Output["backend"].(string)
	if !ok {
		t.Fatalf("missing backend identity in output: %#v", result.Output)
	}
	return backend
}

func requireRoundRobinExecution(t *testing.T, ctx context.Context, client *Client) {
	t.Helper()
	// Connect may complete when only the first SubConn is ready. Wait for real
	// executions to reach both independent TCP servers before checking a batch.
	warmed := make(map[string]bool)
	for len(warmed) < 2 {
		warmed[executeRoundRobinSkill(t, ctx, client)] = true
		if len(warmed) < 2 {
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				t.Fatalf("executions did not reach both workers: %v", warmed)
			}
		}
	}
	counts := make(map[string]int)
	for range 20 {
		counts[executeRoundRobinSkill(t, ctx, client)]++
	}
	if counts["first"] != 10 || counts["second"] != 10 {
		t.Fatalf("round-robin executions did not use both workers evenly: %v", counts)
	}
}

func TestClientRoundRobinBalancesExecutionsAcrossIndependentServers(t *testing.T) {
	first := startRoundRobinSkillServer(t, healthyRoundRobinSkill("first"))
	second := startRoundRobinSkillServer(t, healthyRoundRobinSkill("second"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := NewClientWithOptions(roundRobinTarget(first, second), ClientOptions{RoundRobin: true})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	requireRoundRobinExecution(t, ctx, client)
}

func TestClientDefaultRetainsSingleBackendSelection(t *testing.T) {
	for name, newClient := range map[string]func(string) *Client{
		"legacy constructor": NewClient,
		"zero options": func(address string) *Client {
			return NewClientWithOptions(address, ClientOptions{})
		},
	} {
		t.Run(name, func(t *testing.T) {
			first := startRoundRobinSkillServer(t, healthyRoundRobinSkill("first"))
			second := startRoundRobinSkillServer(t, healthyRoundRobinSkill("second"))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client := newClient(roundRobinTarget(first, second))
			t.Cleanup(func() { _ = client.Close() })
			if err := client.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			counts := make(map[string]int)
			for range 20 {
				counts[executeRoundRobinSkill(t, ctx, client)]++
			}
			if len(counts) != 1 {
				t.Fatalf("default transport unexpectedly balanced executions: %v", counts)
			}
		})
	}
}

func TestRegisterAsVerifiedWithOptionsPublishesBalancedClient(t *testing.T) {
	first := startRoundRobinSkillServer(t, healthyRoundRobinSkill("first"))
	second := startRoundRobinSkillServer(t, healthyRoundRobinSkill("second"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	registry := NewRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	key := "shared:exact-chat-artifact"
	client, err := registry.RegisterAsVerifiedWithOptions(ctx, key, roundRobinTarget(first, second), "chat", "2.2.8", ClientOptions{RoundRobin: true})
	if err != nil {
		t.Fatal(err)
	}
	if registry.GetClient(key) != client || registry.GetClientForSkillType(key, "send-message") != client {
		t.Fatal("verified balanced client was not published with its node types")
	}
	requireRoundRobinExecution(t, ctx, client)
}

func TestRegisterAsVerifiedWithOptionsRejectsMismatchAndPreservesExistingClient(t *testing.T) {
	for name, health := range map[string]*skillpb.HealthResponse{
		"unhealthy":       {Healthy: false, SkillId: "chat", Version: "2.2.8"},
		"different skill": {Healthy: true, SkillId: "other", Version: "2.2.8"},
		"newer version":   {Healthy: true, SkillId: "chat", Version: "2.2.11"},
		"missing version": {Healthy: true, SkillId: "chat"},
	} {
		t.Run(name, func(t *testing.T) {
			previousAddress := startRoundRobinSkillServer(t, healthyRoundRobinSkill("previous"))
			candidateAddress := startRoundRobinSkillServer(t, &roundRobinSkillServer{
				backend: "candidate", health: health, nodeTypes: []string{"candidate-node"},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			registry := NewRegistry()
			t.Cleanup(func() { _ = registry.Close() })
			key := "shared:exact-chat-artifact"
			previous, err := registry.RegisterAsVerified(ctx, key, previousAddress, "chat", "2.2.8")
			if err != nil {
				t.Fatal(err)
			}
			client, err := registry.RegisterAsVerifiedWithOptions(ctx, key, roundRobinTarget(candidateAddress), "chat", "2.2.8", ClientOptions{RoundRobin: true})
			if err == nil || client != nil {
				t.Fatalf("invalid candidate was published: %#v, %v", client, err)
			}
			if registry.GetClient(key) != previous || registry.GetClientForSkillType(key, "send-message") != previous || registry.HasType("candidate-node") {
				t.Fatal("failed verification changed the current client or node mappings")
			}
			if got := executeRoundRobinSkill(t, ctx, previous); got != "previous" {
				t.Fatalf("previous client stopped serving after rejected replacement: %s", got)
			}
		})
	}
}

func TestRegisterAsVerifiedWithOptionsRequiresExactAdvertisedAuthority(t *testing.T) {
	registry := NewRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	for _, values := range [][3]string{{"", "chat", "2.2.8"}, {"key", "", "2.2.8"}, {"key", "chat", ""}} {
		client, err := registry.RegisterAsVerifiedWithOptions(context.Background(), values[0], "unused", values[1], values[2], ClientOptions{RoundRobin: true})
		if err == nil || client != nil {
			t.Fatalf("incomplete authority was accepted: %q", values)
		}
	}
}
