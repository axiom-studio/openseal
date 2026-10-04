// Package skillgrpc provides a gRPC client for external skills
package skillgrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/axiom-studio/skills.sdk/executor"
	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/balancer/roundrobin" // Register the opt-in balancing policy.
	"google.golang.org/grpc/credentials/insecure"
)

// Client connects to external skill gRPC services
type Client struct {
	mu             sync.RWMutex
	conn           *grpc.ClientConn
	client         skillpb.SkillServiceClient
	skillID        string
	address        string
	options        ClientOptions
	retired        bool
	closed         bool
	inFlight       int
	closeCallbacks []func()
}

// ClientOptions controls the transport for a skill endpoint.
// The zero value preserves the default gRPC connection behavior.
type ClientOptions struct {
	// RoundRobin distributes RPCs across the ready addresses returned by the
	// target's resolver. Use a DNS target for a headless Service to resolve each
	// worker; a ClusterIP target still resolves only one address.
	RoundRobin bool
}

// ErrClientRetired means the client no longer accepts RPCs after replacement,
// removal, or explicit closure. Callers should resolve the current registration.
var ErrClientRetired = errors.New("skill client is retired")

// ExecutionContext identifies the durable execution invoking a managed Skill.
// Variables must contain non-secret metadata only. Credential material belongs
// exclusively in the bindings transport.
type ExecutionContext struct {
	RunID     string
	AgentID   string
	Namespace string
	Variables map[string]string
}

// NewClient creates a new gRPC skill client
func NewClient(address string) *Client {
	return NewClientWithOptions(address, ClientOptions{})
}

// NewClientWithOptions creates a gRPC skill client with explicit transport options.
func NewClientWithOptions(address string, options ClientOptions) *Client {
	return &Client{
		address: address,
		options: options,
	}
}

// Connect establishes connection to the skill service
func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.retired || c.closed {
		return ErrClientRetired
	}
	if c.conn != nil {
		return nil
	}

	options := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithTimeout(5 * time.Second),
	}
	if c.options.RoundRobin {
		options = append(options,
			// Keep the caller's explicit policy independent of resolver-supplied
			// service configuration, including DNS TXT records.
			grpc.WithDisableServiceConfig(),
			grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		)
	}
	conn, err := grpc.DialContext(ctx, c.address, options...)
	if err != nil {
		return fmt.Errorf("failed to connect to skill at %s: %w", c.address, err)
	}

	c.conn = conn
	c.client = skillpb.NewSkillServiceClient(conn)

	// Get skill info
	health, err := c.client.Health(ctx, &skillpb.HealthRequest{})
	if err != nil {
		conn.Close()
		c.conn = nil
		c.client = nil
		return fmt.Errorf("health check failed: %w", err)
	}

	c.skillID = health.SkillId
	return nil
}

// Close immediately closes the connection, including any in-flight RPCs.
// Registry replacement and removal use Retire to allow active calls to drain.
func (c *Client) Close() error {
	c.mu.Lock()
	action := c.detachConnectionLocked()
	c.mu.Unlock()
	return action.close()
}

// Retire rejects new RPCs and closes the connection after acquired RPCs finish.
// A running RPC retains the connection only for its own caller-controlled
// lifetime; retirement does not shorten its context deadline.
func (c *Client) Retire() error {
	return c.prepareRetirement(nil).close()
}

// prepareRetirement marks retirement before a registry publishes its replacement.
// The returned action must run outside registry and client locks. Drain callbacks
// likewise run outside both locks, after the underlying transport closes.
func (c *Client) prepareRetirement(onClose func()) clientCloseAction {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		if onClose != nil {
			return clientCloseAction{callbacks: []func(){onClose}}
		}
		return clientCloseAction{}
	}
	c.retired = true
	if onClose != nil {
		c.closeCallbacks = append(c.closeCallbacks, onClose)
	}
	if c.inFlight == 0 {
		return c.detachConnectionLocked()
	}
	return clientCloseAction{}
}

func (c *Client) acquireRPC() (skillpb.SkillServiceClient, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retired || c.closed {
		return nil, ErrClientRetired
	}
	if c.client == nil {
		return nil, fmt.Errorf("not connected to skill service")
	}
	c.inFlight++
	return c.client, nil
}

func (c *Client) releaseRPC() {
	c.mu.Lock()
	c.inFlight--
	var action clientCloseAction
	if c.retired && c.inFlight == 0 {
		action = c.detachConnectionLocked()
	}
	c.mu.Unlock()
	_ = action.close()
}

func (c *Client) detachConnectionLocked() clientCloseAction {
	if c.closed {
		return clientCloseAction{}
	}
	c.closed = true
	c.retired = true
	action := clientCloseAction{conn: c.conn, callbacks: c.closeCallbacks}
	c.conn = nil
	c.client = nil
	c.closeCallbacks = nil
	return action
}

type clientCloseAction struct {
	conn      *grpc.ClientConn
	callbacks []func()
}

func (a clientCloseAction) close() error {
	var err error
	if a.conn != nil {
		err = a.conn.Close()
	}
	for _, callback := range a.callbacks {
		callback()
	}
	return err
}

// Execute executes a node on the skill service
func (c *Client) Execute(ctx context.Context, step *executor.StepDefinition, resolver executor.TemplateResolver) (*executor.StepResult, error) {
	return c.ExecuteWithContext(ctx, step, resolver, executionContextFromResolver(resolver))
}

// ExecuteWithContext executes a node with authoritative durable execution
// identity supplied by the caller.
func (c *Client) ExecuteWithContext(
	ctx context.Context,
	step *executor.StepDefinition,
	resolver executor.TemplateResolver,
	execution ExecutionContext,
) (*executor.StepResult, error) {
	client, err := c.acquireRPC()
	if err != nil {
		return nil, err
	}
	defer c.releaseRPC()

	// Serialize config
	config := make(map[string][]byte)
	for k, v := range step.Config {
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("failed to serialize config %s: %w", k, err)
		}
		config[k] = data
	}

	// Get input from resolver (previous node output)
	input := make(map[string][]byte)
	if stepInput := resolver.GetStepOutput(step.Id); stepInput != nil {
		if m, ok := stepInput.(map[string]interface{}); ok {
			for k, v := range m {
				data, err := json.Marshal(v)
				if err != nil {
					return nil, fmt.Errorf("failed to serialize input %s: %w", k, err)
				}
				input[k] = data
			}
		}
	}

	// Get bindings from resolver
	// Bindings are resolved before workflow execution and contain things like
	// database connection strings, API keys, etc.
	bindings := make(map[string][]byte)
	if ctxProvider, ok := resolver.(interface{ GetContextData() map[string]interface{} }); ok {
		ctxData := ctxProvider.GetContextData()
		if bindingsData, ok := ctxData["bindings"]; ok {
			if bindingsMap, ok := bindingsData.(map[string]interface{}); ok {
				for k, v := range bindingsMap {
					data, err := json.Marshal(v)
					if err != nil {
						return nil, fmt.Errorf("failed to serialize binding %s: %w", k, err)
					}
					bindings[k] = data
				}
			}
		}
	}

	// Build context
	execCtx := &skillpb.ExecutionContext{
		RunId:     strings.TrimSpace(execution.RunID),
		AgentId:   strings.TrimSpace(execution.AgentID),
		Namespace: strings.TrimSpace(execution.Namespace),
		Variables: cloneVariables(execution.Variables),
	}

	// Build request
	req := &skillpb.ExecuteRequest{
		NodeId:   step.Id,
		NodeType: step.Type,
		Config:   config,
		Input:    input,
		Context:  execCtx,
		Bindings: bindings,
	}

	// Execute
	resp, err := client.Execute(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("execute failed: %w", err)
	}

	// Check for error
	if resp.Error != nil {
		if failure := NewActionError(resp.Error.Type, resp.Error.Message, resp.Error.Details); failure != nil {
			return nil, failure
		}
		return nil, fmt.Errorf("%s: %s", resp.Error.Type, resp.Error.Message)
	}

	// Deserialize output
	output := make(map[string]interface{})
	for k, v := range resp.Output {
		var val interface{}
		if err := json.Unmarshal(v, &val); err != nil {
			output[k] = string(v)
		} else {
			output[k] = val
		}
	}

	return &executor.StepResult{
		Output:   output,
		NextStep: resp.NextStep,
	}, nil
}

func executionContextFromResolver(resolver executor.TemplateResolver) ExecutionContext {
	provider, ok := resolver.(interface{ GetContextData() map[string]interface{} })
	if !ok {
		return ExecutionContext{}
	}
	data := provider.GetContextData()
	run, _ := data["run"].(map[string]interface{})
	self, _ := data["self"].(map[string]interface{})
	return ExecutionContext{
		RunID:     firstString(run, "id", "runId"),
		AgentID:   firstString(run, "agentId", "deploymentId"),
		Namespace: firstNonEmpty(firstString(run, "namespace"), firstString(self, "namespace")),
	}
}

func firstString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		if value, ok := values[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func cloneVariables(variables map[string]string) map[string]string {
	cloned := make(map[string]string, len(variables))
	for key, value := range variables {
		cloned[key] = value
	}
	return cloned
}

// GetNodeTypes returns the node types this skill provides
func (c *Client) GetNodeTypes(ctx context.Context) ([]string, error) {
	client, err := c.acquireRPC()
	if err != nil {
		return nil, err
	}
	defer c.releaseRPC()

	resp, err := client.GetNodeTypes(ctx, &skillpb.GetNodeTypesRequest{})
	if err != nil {
		return nil, err
	}

	return resp.NodeTypes, nil
}

// GetNodeSchema returns the schema for a node type
func (c *Client) GetNodeSchema(ctx context.Context, nodeType string) ([]byte, error) {
	client, err := c.acquireRPC()
	if err != nil {
		return nil, err
	}
	defer c.releaseRPC()

	resp, err := client.GetNodeSchema(ctx, &skillpb.GetNodeSchemaRequest{NodeType: nodeType})
	if err != nil {
		return nil, err
	}

	return resp.Schema, nil
}

// Health checks if the skill is healthy
func (c *Client) Health(ctx context.Context) (*HealthInfo, error) {
	client, err := c.acquireRPC()
	if err != nil {
		return nil, err
	}
	defer c.releaseRPC()

	resp, err := client.Health(ctx, &skillpb.HealthRequest{})
	if err != nil {
		return nil, err
	}

	return &HealthInfo{
		Healthy: resp.Healthy,
		SkillID: resp.SkillId,
		Version: resp.Version,
	}, nil
}

// SkillID returns the skill ID
func (c *Client) SkillID() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.skillID
}

// Address returns the address
func (c *Client) Address() string {
	return c.address
}

// HealthInfo contains health check information
type HealthInfo struct {
	Healthy bool
	SkillID string
	Version string
}
