package skillgrpc

import (
	"context"
	"testing"

	"github.com/axiom-studio/skills.sdk/executor"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// wireField decodes one length-delimited ExecutionContext field from the
// encoded message, the way a Skill built on the SDK's skill.proto reads it.
func wireField(t *testing.T, encoded []byte, field protowire.Number) (string, int) {
	t.Helper()
	tenant, seen := "", 0
	for len(encoded) > 0 {
		number, kind, n := protowire.ConsumeTag(encoded)
		if n < 0 {
			t.Fatal(protowire.ParseError(n))
		}
		encoded = encoded[n:]
		if number == field && kind == protowire.BytesType {
			value, m := protowire.ConsumeString(encoded)
			if m < 0 {
				t.Fatal(protowire.ParseError(m))
			}
			tenant, seen, encoded = value, seen+1, encoded[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(number, kind, encoded)
		if m < 0 {
			t.Fatal(protowire.ParseError(m))
		}
		encoded = encoded[m:]
	}
	return tenant, seen
}

func TestExecuteWithContextSendsTenantAsExecutionContextField5(t *testing.T) {
	transport := &captureSkillClient{}
	client := &Client{client: transport}
	step := &executor.StepDefinition{Id: "node-1", Type: "example.action", Config: map[string]interface{}{}}
	if _, err := client.ExecuteWithContext(context.Background(), step, &contextResolver{}, ExecutionContext{
		RunID: "run-1", TenantID: " tenant-a ", Variables: map[string]string{"tenantId": "spoofed"},
	}); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(transport.request.Context)
	if err != nil {
		t.Fatal(err)
	}
	if tenant, seen := wireField(t, encoded, 5); tenant != "tenant-a" || seen != 1 {
		t.Fatalf("tenant_id on the wire = %q (seen %d)", tenant, seen)
	}
}

func TestExecuteWithoutTenantSendsNoTenantField(t *testing.T) {
	transport := &captureSkillClient{}
	client := &Client{client: transport}
	step := &executor.StepDefinition{Id: "node-1", Type: "example.action", Config: map[string]interface{}{}}
	resolver := &contextResolver{context: map[string]interface{}{"run": map[string]interface{}{"id": "run-2", "tenantId": "from-run-data"}}}
	if _, err := client.Execute(context.Background(), step, resolver); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(transport.request.Context)
	if err != nil {
		t.Fatal(err)
	}
	if _, seen := wireField(t, encoded, 5); seen != 0 {
		t.Fatal("tenant must come only from the host's authenticated ExecutionContext")
	}
}

func TestExecuteWithContextSendsConfigAsFields6And7(t *testing.T) {
	transport := &captureSkillClient{}
	client := &Client{client: transport}
	step := &executor.StepDefinition{Id: "node-1", Type: "example.action", Config: map[string]interface{}{}}
	config := map[string]interface{}{"searchProvider": "tavily", "tavilyApiKey": "secret", "maxResults": 10, "fallbackToYahoo": true}
	if _, err := client.ExecuteWithContext(context.Background(), step, &contextResolver{}, ExecutionContext{
		RunID: "run-1", TenantID: "7", Config: config, ConfigVersion: "v1-abc",
	}); err != nil {
		t.Fatal(err)
	}
	encoded, err := proto.Marshal(transport.request.Context)
	if err != nil {
		t.Fatal(err)
	}
	if version, seen := wireField(t, encoded, 7); version != "v1-abc" || seen != 1 {
		t.Fatalf("config_version = %q (%d)", version, seen)
	}
	raw, seen := wireField(t, encoded, 6)
	if seen != 1 {
		t.Fatalf("config seen %d times", seen)
	}
	var decoded structpb.Struct
	if err := proto.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	got := decoded.AsMap()
	if got["searchProvider"] != "tavily" || got["tavilyApiKey"] != "secret" || got["maxResults"] != float64(10) || got["fallbackToYahoo"] != true || len(got) != 4 {
		t.Fatalf("config = %#v", got)
	}
	if _, err := client.ExecuteWithContext(context.Background(), step, &contextResolver{}, ExecutionContext{Config: map[string]interface{}{"bad": make(chan int)}}); err == nil {
		t.Fatal("a config value that cannot be encoded was sent")
	}
	transport.request = nil
	if _, err := client.ExecuteWithContext(context.Background(), step, &contextResolver{}, ExecutionContext{RunID: "run-2"}); err != nil {
		t.Fatal(err)
	}
	encoded, _ = proto.Marshal(transport.request.Context)
	if _, seen := wireField(t, encoded, 6); seen != 0 {
		t.Fatal("a call without config carried a config field")
	}
}
