package skillgrpc

import (
	"context"
	"sync"
	"testing"
	"time"

	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type verifiedRegistryTransport struct {
	mu          sync.Mutex
	healthCalls int
	health      func(context.Context, int) (*skillpb.HealthResponse, error)
}

func (s *verifiedRegistryTransport) Health(ctx context.Context, _ *skillpb.HealthRequest, _ ...grpc.CallOption) (*skillpb.HealthResponse, error) {
	s.mu.Lock()
	s.healthCalls++
	call := s.healthCalls
	s.mu.Unlock()
	return s.health(ctx, call)
}

func (*verifiedRegistryTransport) GetNodeTypes(context.Context, *skillpb.GetNodeTypesRequest, ...grpc.CallOption) (*skillpb.GetNodeTypesResponse, error) {
	return &skillpb.GetNodeTypesResponse{NodeTypes: []string{"send-message"}}, nil
}

func (*verifiedRegistryTransport) Execute(context.Context, *skillpb.ExecuteRequest, ...grpc.CallOption) (*skillpb.ExecuteResponse, error) {
	return nil, status.Error(codes.Unimplemented, "execution is not needed for registration")
}

func (*verifiedRegistryTransport) GetNodeSchema(context.Context, *skillpb.GetNodeSchemaRequest, ...grpc.CallOption) (*skillpb.GetNodeSchemaResponse, error) {
	return nil, status.Error(codes.Unimplemented, "schema is not needed for registration")
}

func configureVerifiedRegistryTransport(registry *Registry, health func(context.Context, int) (*skillpb.HealthResponse, error)) string {
	transport := &verifiedRegistryTransport{health: health}
	registry.connect = func(ctx context.Context, address string) (*Client, error) {
		// Connect learns the reported Skill ID from the first health response.
		// Verification must independently validate the final health before publish.
		initial, err := transport.Health(ctx, &skillpb.HealthRequest{})
		if err != nil {
			return nil, err
		}
		return &Client{client: transport, skillID: initial.SkillId, address: address}, nil
	}
	return "in-process-skill"
}

func TestRegisterAsVerifiedPublishesOnlyAfterHealthVerification(t *testing.T) {
	registry := NewRegistry()
	t.Cleanup(func() { _ = registry.Close() })
	key := "tenant:1\x00skill:chat\x00version:2.2.8"
	checking, release := make(chan struct{}), make(chan struct{})
	address := configureVerifiedRegistryTransport(registry, func(ctx context.Context, call int) (*skillpb.HealthResponse, error) {
		if call == 2 {
			close(checking)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &skillpb.HealthResponse{Healthy: true, SkillId: "chat", Version: "2.2.8"}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type registration struct {
		client *Client
		err    error
	}
	result := make(chan registration, 1)
	go func() {
		client, err := registry.RegisterAsVerified(ctx, key, address, "chat", "2.2.8")
		result <- registration{client: client, err: err}
	}()
	select {
	case <-checking:
	case <-ctx.Done():
		t.Fatal("health verification did not start")
	}
	if registry.GetClient(key) != nil || registry.GetClientForSkillType(key, "send-message") != nil || registry.HasType("send-message") {
		t.Fatal("unverified registration became available for dispatch")
	}
	close(release)
	registered := <-result
	if registered.err != nil {
		t.Fatal(registered.err)
	}
	if registered.client == nil || registry.GetClientForSkillType(key, "send-message") != registered.client {
		t.Fatal("verified registration is unavailable")
	}
}

func TestRegisterAsVerifiedRejectsUnhealthyOrDifferentIdentityWithoutReplacingExistingClient(t *testing.T) {
	for name, test := range map[string]struct {
		initial *skillpb.HealthResponse
		final   *skillpb.HealthResponse
		err     error
	}{
		"unhealthy":       {final: &skillpb.HealthResponse{Healthy: false, SkillId: "chat", Version: "2.2.8"}},
		"another Skill":   {final: &skillpb.HealthResponse{Healthy: true, SkillId: "other", Version: "2.2.8"}},
		"newer version":   {final: &skillpb.HealthResponse{Healthy: true, SkillId: "chat", Version: "2.2.11"}},
		"missing version": {final: &skillpb.HealthResponse{Healthy: true, SkillId: "chat"}},
		"changed identity": {
			initial: &skillpb.HealthResponse{Healthy: true, SkillId: "other", Version: "2.2.8"},
			final:   &skillpb.HealthResponse{Healthy: true, SkillId: "chat", Version: "2.2.8"},
		},
		"health error": {err: status.Error(codes.Unavailable, "health unavailable")},
	} {
		t.Run(name, func(t *testing.T) {
			registry := NewRegistry()
			key := "tenant:1\x00skill:chat\x00version:2.2.8"
			previous := &Client{skillID: "chat"}
			registry.clients[key] = previous
			registry.typesBySkill[key] = map[string]struct{}{"previous-node": {}}
			registry.skillsByType["previous-node"] = map[string]struct{}{key: {}}
			t.Cleanup(func() { _ = registry.Close() })
			address := configureVerifiedRegistryTransport(registry, func(_ context.Context, call int) (*skillpb.HealthResponse, error) {
				if call == 1 {
					if test.initial != nil {
						return test.initial, nil
					}
					return &skillpb.HealthResponse{Healthy: true, SkillId: "chat", Version: "2.2.8"}, nil
				}
				return test.final, test.err
			})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			client, err := registry.RegisterAsVerified(ctx, key, address, "chat", "2.2.8")
			if err == nil || client != nil {
				t.Fatalf("invalid registration was accepted: %#v, %v", client, err)
			}
			if registry.GetClient(key) != previous || registry.GetClientForSkillType(key, "previous-node") != previous ||
				registry.GetClientForSkillType(key, "send-message") != nil || registry.HasType("send-message") {
				t.Fatal("failed verification changed the existing client or node mappings")
			}
		})
	}
}

func TestRegisterAsVerifiedRequiresExactAdvertisedAuthority(t *testing.T) {
	registry := NewRegistry()
	for _, values := range [][3]string{{"", "chat", "2.2.8"}, {"key", "", "2.2.8"}, {"key", "chat", ""}} {
		if client, err := registry.RegisterAsVerified(context.Background(), values[0], "unused", values[1], values[2]); err == nil || client != nil {
			t.Fatalf("incomplete authority was accepted: %q", values)
		}
	}
}
