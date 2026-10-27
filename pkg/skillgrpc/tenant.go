package skillgrpc

import (
	"fmt"
	"strings"

	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
)

// ExecutionContext fields of the Skills SDK skill.proto.
const (
	executionContextTenantField        protowire.Number = 5
	executionContextConfigField        protowire.Number = 6 // google.protobuf.Struct
	executionContextConfigVersionField protowire.Number = 7
)

// setExecutionContextConfig writes tenant_id (5), config (6) and
// config_version (7).
//
// TODO(skill-config): set the generated fields directly once openseal depends
// on a skills.sdk release whose skillpb declares them; until then the same
// wire bytes go through the message's unknown fields.
func setExecutionContextConfig(execCtx *skillpb.ExecutionContext, tenantID string, config map[string]interface{}, version string) error {
	if execCtx == nil {
		return nil
	}
	message := execCtx.ProtoReflect()
	setString(message, executionContextTenantField, strings.TrimSpace(tenantID))
	setString(message, executionContextConfigVersionField, strings.TrimSpace(version))
	if config == nil {
		return nil
	}
	value, err := structpb.NewStruct(config)
	if err != nil {
		return fmt.Errorf("encode Skill config: %w", err)
	}
	if field := message.Descriptor().Fields().ByNumber(executionContextConfigField); field != nil {
		message.Set(field, protoreflect.ValueOfMessage(value.ProtoReflect()))
		return nil
	}
	encoded, err := proto.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode Skill config: %w", err)
	}
	raw := protowire.AppendTag(nil, executionContextConfigField, protowire.BytesType)
	message.SetUnknown(append(message.GetUnknown(), protowire.AppendBytes(raw, encoded)...))
	return nil
}

func setString(message protoreflect.Message, number protowire.Number, value string) {
	if value == "" {
		return
	}
	if field := message.Descriptor().Fields().ByNumber(number); field != nil {
		message.Set(field, protoreflect.ValueOfString(value))
		return
	}
	raw := protowire.AppendTag(nil, number, protowire.BytesType)
	message.SetUnknown(append(message.GetUnknown(), protowire.AppendString(raw, value)...))
}
