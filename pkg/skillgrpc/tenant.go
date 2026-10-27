package skillgrpc

import (
	"strings"

	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// executionContextTenantField is ExecutionContext.tenant_id in the Skills SDK
// skill.proto.
const executionContextTenantField protowire.Number = 5

// setExecutionContextTenant writes ExecutionContext.tenant_id (field 5).
//
// TODO(skill-config): set execCtx.TenantId directly once openseal depends on a
// skills.sdk release whose skillpb declares tenant_id; this writes the same
// wire bytes through the message's unknown fields until then.
func setExecutionContextTenant(execCtx *skillpb.ExecutionContext, tenantID string) {
	tenantID = strings.TrimSpace(tenantID)
	if execCtx == nil || tenantID == "" {
		return
	}
	message := execCtx.ProtoReflect()
	if field := message.Descriptor().Fields().ByNumber(executionContextTenantField); field != nil {
		message.Set(field, protoreflect.ValueOfString(tenantID))
		return
	}
	raw := protowire.AppendTag(nil, executionContextTenantField, protowire.BytesType)
	raw = protowire.AppendString(raw, tenantID)
	message.SetUnknown(append(message.GetUnknown(), raw...))
}
