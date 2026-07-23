package skillgrpc

import (
	"context"
	"testing"

	"github.com/axiom-studio/skills.sdk/executor"
	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/grpc"
)

type captureSkillClient struct {
	request *skillpb.ExecuteRequest
}

func (c *captureSkillClient) Execute(_ context.Context, request *skillpb.ExecuteRequest, _ ...grpc.CallOption) (*skillpb.ExecuteResponse, error) {
	c.request = request
	return &skillpb.ExecuteResponse{Output: map[string][]byte{"ok": []byte("true")}}, nil
}

func (*captureSkillClient) GetNodeTypes(context.Context, *skillpb.GetNodeTypesRequest, ...grpc.CallOption) (*skillpb.GetNodeTypesResponse, error) {
	return &skillpb.GetNodeTypesResponse{}, nil
}

func (*captureSkillClient) GetNodeSchema(context.Context, *skillpb.GetNodeSchemaRequest, ...grpc.CallOption) (*skillpb.GetNodeSchemaResponse, error) {
	return &skillpb.GetNodeSchemaResponse{}, nil
}

func (*captureSkillClient) Health(context.Context, *skillpb.HealthRequest, ...grpc.CallOption) (*skillpb.HealthResponse, error) {
	return &skillpb.HealthResponse{Healthy: true}, nil
}

type contextResolver struct {
	context map[string]interface{}
}

func (*contextResolver) ResolveString(value string) string { return value }
func (*contextResolver) ResolveMap(value map[string]interface{}) map[string]interface{} {
	return value
}
func (*contextResolver) EvaluateCondition(string) bool            { return true }
func (*contextResolver) SetVariable(string, interface{})          {}
func (*contextResolver) GetStepOutput(string) interface{}         { return nil }
func (*contextResolver) SetStepOutput(string, interface{})        {}
func (r *contextResolver) GetContextData() map[string]interface{} { return r.context }

func TestExecuteWithContextSerializesAuthoritativeIdentityWithoutBindingLeakage(t *testing.T) {
	transport := &captureSkillClient{}
	client := &Client{client: transport}
	resolver := &contextResolver{context: map[string]interface{}{
		"bindings": map[string]interface{}{"TOKEN": "secret-value"},
	}}
	step := &executor.StepDefinition{Id: "node-1", Type: "example.action", Config: map[string]interface{}{}}

	if _, err := client.ExecuteWithContext(context.Background(), step, resolver, ExecutionContext{
		RunID: " run-1 ", AgentID: " agent-1 ", Namespace: " operations ",
		Variables: map[string]string{"actionCallId": "call-1"},
	}); err != nil {
		t.Fatal(err)
	}

	if transport.request == nil || transport.request.Context == nil {
		t.Fatal("managed Skill did not receive execution context")
	}
	got := transport.request.Context
	if got.RunId != "run-1" || got.AgentId != "agent-1" || got.Namespace != "operations" ||
		got.Variables["actionCallId"] != "call-1" {
		t.Fatalf("execution context = %#v", got)
	}
	if _, leaked := got.Variables["TOKEN"]; leaked {
		t.Fatal("credential binding leaked into execution context variables")
	}
	if string(transport.request.Bindings["TOKEN"]) != `"secret-value"` {
		t.Fatalf("binding transport = %q", transport.request.Bindings["TOKEN"])
	}
}

func TestExecuteDerivesOnlyPortableIdentityFromResolver(t *testing.T) {
	transport := &captureSkillClient{}
	client := &Client{client: transport}
	resolver := &contextResolver{context: map[string]interface{}{
		"run": map[string]interface{}{
			"id": "run-2", "agentId": "agent-2", "namespace": "research",
		},
		"vars":     map[string]interface{}{"possibleSecret": "must-not-cross-context"},
		"bindings": map[string]interface{}{"API_KEY": "also-secret"},
	}}
	step := &executor.StepDefinition{Id: "node-2", Type: "example.action", Config: map[string]interface{}{}}

	if _, err := client.Execute(context.Background(), step, resolver); err != nil {
		t.Fatal(err)
	}

	got := transport.request.Context
	if got.RunId != "run-2" || got.AgentId != "agent-2" || got.Namespace != "research" {
		t.Fatalf("derived execution context = %#v", got)
	}
	if len(got.Variables) != 0 {
		t.Fatalf("untrusted resolver variables crossed managed Skill boundary: %#v", got.Variables)
	}
}
