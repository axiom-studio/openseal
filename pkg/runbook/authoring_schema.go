package runbook

import (
	"encoding/json"
	"reflect"
	"strings"
)

type authoringStepKindProjection struct {
	PayloadField  string   `json:"payloadField"`
	PayloadFields []string `json:"payloadFields"`
}

type authoringSchemaProjection struct {
	DefinitionFields []string                                 `json:"definitionFields"`
	InterfaceFields  []string                                 `json:"interfaceFields"`
	StepFields       []string                                 `json:"stepFields"`
	StepKinds        map[StepKind]authoringStepKindProjection `json:"stepKinds"`
}

type stepPayloadType struct {
	kind  StepKind
	field string
	value interface{}
}

var stepPayloadTypes = []stepPayloadType{
	{kind: StepAction, field: "action", value: ActionStep{}},
	{kind: StepDelegate, field: "delegate", value: DelegateStep{}},
	{kind: StepDecision, field: "decision", value: DecisionStep{}},
	{kind: StepTransform, field: "transform", value: TransformStep{}},
	{kind: StepWait, field: "wait", value: WaitStep{}},
	{kind: StepFork, field: "fork", value: ForkStep{}},
	{kind: StepJoin, field: "join", value: JoinStep{}},
	{kind: StepForEach, field: "forEach", value: ForEachStep{}},
	{kind: StepLoopReturn, field: "loopReturn", value: LoopReturnStep{}},
	{kind: StepEnd, field: "end", value: EndStep{}},
}

var canonicalAuthoringSchemaProjection = buildAuthoringSchemaProjection()

// AuthoringSchemaProjection returns a compact, deterministic projection of the
// canonical portable Runbook JSON fields. Prompt adapters can give this to a
// model without maintaining a second hand-written Step schema.
func AuthoringSchemaProjection() string {
	return canonicalAuthoringSchemaProjection
}

// StepPayloadField resolves the only payload object admitted for one Step kind.
// It is shared by strict-schema diagnostics so repairs point to the canonical
// nested location instead of merely reporting an unknown root field.
func StepPayloadField(kind StepKind) (string, bool) {
	for _, candidate := range stepPayloadTypes {
		if candidate.kind == kind {
			return candidate.field, true
		}
	}
	return "", false
}

// StepPayloadFieldForJSONField resolves the canonical payload object for a
// field that was incorrectly placed on the Step root. It lets strict-schema
// repair diagnostics say where a known payload field belongs without
// duplicating payload schemas in authoring adapters.
func StepPayloadFieldForJSONField(kind StepKind, name string) (string, bool) {
	for _, candidate := range stepPayloadTypes {
		if candidate.kind == kind && containsJSONField(reflect.TypeOf(candidate.value), name) {
			return candidate.field, true
		}
	}
	return "", false
}

func buildAuthoringSchemaProjection() string {
	projection := authoringSchemaProjection{
		DefinitionFields: jsonFieldNames(reflect.TypeOf(Definition{})),
		InterfaceFields:  jsonFieldNames(reflect.TypeOf(Interface{})),
		StepFields:       jsonFieldNames(reflect.TypeOf(Step{})),
		StepKinds:        make(map[StepKind]authoringStepKindProjection, len(stepPayloadTypes)),
	}
	for _, candidate := range stepPayloadTypes {
		projection.StepKinds[candidate.kind] = authoringStepKindProjection{
			PayloadField:  candidate.field,
			PayloadFields: jsonFieldNames(reflect.TypeOf(candidate.value)),
		}
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		panic(err)
	}
	return "Portable Runbook schema projection (generated from canonical OpenSeal types): " + string(encoded)
}

func jsonFieldNames(value reflect.Type) []string {
	fields := make([]string, 0, value.NumField())
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		if field.PkgPath != "" {
			continue
		}
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		fields = append(fields, name)
	}
	return fields
}

func containsJSONField(value reflect.Type, expected string) bool {
	for _, field := range jsonFieldNames(value) {
		if field == expected {
			return true
		}
	}
	return false
}
