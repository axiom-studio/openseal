package skillgrpc

import (
	"context"
	"testing"

	"github.com/axiom-studio/skills.sdk/executor"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

// wireTenantID decodes ExecutionContext.tenant_id (field 5) from the encoded
// message, the way a Skill built on the SDK's skill.proto reads it.
func wireTenantID(t *testing.T, encoded []byte) (string, int) {
	t.Helper()
	tenant, seen := "", 0
	for len(encoded) > 0 {
		number, kind, n := protowire.ConsumeTag(encoded)
		if n < 0 {
			t.Fatal(protowire.ParseError(n))
		}
		encoded = encoded[n:]
		if number == 5 && kind == protowire.BytesType {
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
	if tenant, seen := wireTenantID(t, encoded); tenant != "tenant-a" || seen != 1 {
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
	if _, seen := wireTenantID(t, encoded); seen != 0 {
		t.Fatal("tenant must come only from the host's authenticated ExecutionContext")
	}
}
