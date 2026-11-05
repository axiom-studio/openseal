package authoring

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Scope the model's Skill choices to the same catalog that the deterministic
// validator uses. The base schema is cloned so tenants cannot share choices.
func authoringIntentContractForCatalog(catalog CapabilityCatalog) authoringProviderContract {
	contract := authoringIntentContract()
	ids := make([]string, 0, len(catalog.Skills))
	providerSet := map[string]bool{}
	for id, skill := range catalog.Skills {
		ids = append(ids, id)
		// selectConversationAdapter accepts an adapter's provider or its Skill id.
		for _, adapter := range skill.ConversationAdapters {
			if provider := strings.TrimSpace(adapter.Provider); provider != "" {
				providerSet[provider] = true
			}
			providerSet[id] = true
		}
	}
	sort.Strings(ids)
	providers := make([]string, 0, len(providerSet))
	for provider := range providerSet {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	contract.Schema = func() (map[string]interface{}, error) {
		schema, err := AuthoringIntentJSONSchema()
		if err != nil {
			return nil, err
		}
		if err := constrainAuthoringIntentSchema(schema, ids, providers); err != nil {
			return nil, fmt.Errorf("constrain authoring intent schema: %w", err)
		}
		return schema, nil
	}
	contract.DecodingSchema = func() (map[string]interface{}, error) {
		schema, err := contract.Schema()
		if err != nil {
			return nil, err
		}
		decoding, err := authoringIntentDecodingSchema(schema)
		if err != nil {
			return nil, fmt.Errorf("build authoring intent decoding schema: %w", err)
		}
		return decoding, nil
	}
	return contract
}

// constrainAuthoringIntentSchema expresses the validateAuthoringIntent counts
// and catalog references that plain JSON Schema keywords can carry, so
// constrained decoders cannot emit them. Every string list shares one $defs
// array, so each narrowed list is copied at its use site first.
func constrainAuthoringIntentSchema(schema map[string]interface{}, skillIDs, conversationProviders []string) error {
	constrainSkillIDs := func(owner map[string]interface{}, property string) error {
		list, err := narrowSchemaProperty(schema, owner, property)
		if err != nil {
			return err
		}
		if len(skillIDs) == 0 {
			list["maxItems"] = 0
		} else {
			list["items"] = map[string]interface{}{"type": "string", "enum": skillIDs}
		}
		return nil
	}
	agents, err := narrowSchemaProperty(schema, schema, "agents")
	if err != nil {
		return err
	}
	agents["minItems"] = 1
	agent, err := resolveSchemaRef(schema, agents["items"])
	if err != nil {
		return err
	}
	if len(skillIDs) == 0 {
		skills, err := narrowSchemaProperty(schema, agent, "skills")
		if err != nil {
			return err
		}
		skills["maxItems"] = 0
	} else {
		skill, err := schemaPropertyItems(schema, agent, "skills")
		if err != nil {
			return err
		}
		catalogID, ok := schemaProperties(skill)["catalogId"].(map[string]interface{})
		if !ok {
			return errors.New("skill selection schema has no catalogId property")
		}
		catalogID["enum"] = skillIDs
	}
	operation, err := schemaPropertyItems(schema, agent, "operations")
	if err != nil {
		return err
	}
	if err := constrainSkillIDs(operation, "skillCatalogIds"); err != nil {
		return err
	}
	conversations, err := narrowSchemaProperty(schema, schema, "conversations")
	if err != nil {
		return err
	}
	if len(conversationProviders) == 0 {
		// Without a conversation adapter no conversation compiles, so approval
		// cannot be delivered to one either.
		conversations["maxItems"] = 0
		schemaProperties(operation)["approvalDelivery"] = map[string]interface{}{"type": "string", "enum": []string{string(AuthoringApprovalDeliveryPlatform)}}
	} else {
		conversation, err := resolveSchemaRef(schema, conversations["items"])
		if err != nil {
			return err
		}
		schemaProperties(conversation)["provider"] = map[string]interface{}{"type": "string", "enum": conversationProviders}
	}
	team, err := resolveSchemaRef(schema, schemaProperties(schema)["team"])
	if err != nil {
		return err
	}
	roles, err := narrowSchemaProperty(schema, team, "roles")
	if err != nil {
		return err
	}
	roles["minItems"] = 1
	role, err := resolveSchemaRef(schema, roles["items"])
	if err != nil {
		return err
	}
	agentKeys, err := narrowSchemaProperty(schema, role, "agentKeys")
	if err != nil {
		return err
	}
	agentKeys["minItems"] = 1
	return constrainSkillIDs(role, "skillCatalogIds")
}

// authoringIntentDecodingSchema adds the validateAuthoringIntent conditionals
// that need schema combinators. Only grammar-constrained transports receive
// it: the combinators are expressed solely as oneOf of complete object
// branches, the form llama.cpp grammar conversion was verified to enforce.
func authoringIntentDecodingSchema(schema map[string]interface{}) (map[string]interface{}, error) {
	agents, err := resolveSchemaRef(schema, schemaProperties(schema)["agents"])
	if err != nil {
		return nil, err
	}
	agent, err := resolveSchemaRef(schema, agents["items"])
	if err != nil {
		return nil, err
	}
	operations, err := narrowSchemaProperty(schema, agent, "operations")
	if err != nil {
		return nil, err
	}
	operation, err := resolveSchemaRef(schema, operations["items"])
	if err != nil {
		return nil, err
	}
	branches := make([]interface{}, 0, 6)
	for _, wake := range (AuthoringOperationWake("")).ContractValues() {
		for _, delivery := range schemaEnumValues(schemaProperties(operation)["approvalDelivery"]) {
			branch := cloneSchemaValue(operation).(map[string]interface{})
			properties := schemaProperties(branch)
			properties["wake"] = map[string]interface{}{"type": "string", "enum": []string{wake}}
			properties["approvalDelivery"] = map[string]interface{}{"type": "string", "enum": []string{delivery}}
			switch AuthoringOperationWake(wake) {
			case AuthoringWakeSchedule:
				requireNonBlankSchemaString(branch, "schedule")
			case AuthoringWakeEvent:
				requireNonBlankSchemaString(branch, "eventType")
			}
			channels := map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "maxItems": 0}
			if AuthoringApprovalDelivery(delivery) == AuthoringApprovalDeliveryChannels {
				channels = map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "minItems": 1}
				appendSchemaRequired(branch, "approvalChannelKeys")
			}
			properties["approvalChannelKeys"] = channels
			branches = append(branches, branch)
		}
	}
	operations["items"] = map[string]interface{}{"oneOf": branches}

	base := cloneSchemaValue(schema).(map[string]interface{})
	defs := base["$defs"]
	delete(base, "$defs")
	delete(base, "$schema")
	single := cloneSchemaValue(base).(map[string]interface{})
	schemaProperties(single)["kind"] = map[string]interface{}{"type": "string", "enum": []string{string(AuthoringResourceAgent)}}
	delete(schemaProperties(single), "team")
	schemaProperties(single)["agents"].(map[string]interface{})["maxItems"] = 1
	multiple := cloneSchemaValue(base).(map[string]interface{})
	schemaProperties(multiple)["kind"] = map[string]interface{}{"type": "string", "enum": []string{string(AuthoringResourceTeam), string(AuthoringResourceWorkforce)}}
	appendSchemaRequired(multiple, "team")
	return map[string]interface{}{"$defs": defs, "oneOf": []interface{}{single, multiple}}, nil
}

func schemaEnumValues(node interface{}) []string {
	object, _ := node.(map[string]interface{})
	switch values := object["enum"].(type) {
	case []string:
		return values
	case []interface{}:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}
		return result
	}
	return nil
}

func schemaProperties(object map[string]interface{}) map[string]interface{} {
	properties, _ := object["properties"].(map[string]interface{})
	return properties
}

func requireNonBlankSchemaString(object map[string]interface{}, property string) {
	schemaProperties(object)[property] = map[string]interface{}{"type": "string", "minLength": 1, "pattern": `\S`}
	appendSchemaRequired(object, property)
}

func appendSchemaRequired(object map[string]interface{}, property string) {
	required, _ := object["required"].([]interface{})
	object["required"] = append(append([]interface{}(nil), required...), property)
}

// resolveSchemaRef returns the definition a local "#/$defs/<name>" reference
// names, or the node itself when it is already inline.
func resolveSchemaRef(root map[string]interface{}, node interface{}) (map[string]interface{}, error) {
	object, ok := node.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("schema node %v is not an object", node)
	}
	ref, ok := object["$ref"].(string)
	if !ok {
		return object, nil
	}
	defs, _ := root["$defs"].(map[string]interface{})
	definition, ok := defs[strings.TrimPrefix(ref, "#/$defs/")].(map[string]interface{})
	if !ok || !strings.HasPrefix(ref, "#/$defs/") {
		return nil, fmt.Errorf("schema reference %q does not name a local definition", ref)
	}
	return definition, nil
}

// narrowSchemaProperty replaces owner's property with an inline copy of its
// schema and returns the copy, leaving any shared definition untouched.
func narrowSchemaProperty(root, owner map[string]interface{}, property string) (map[string]interface{}, error) {
	properties := schemaProperties(owner)
	resolved, err := resolveSchemaRef(root, properties[property])
	if err != nil {
		return nil, fmt.Errorf("property %s: %w", property, err)
	}
	inline := cloneSchemaValue(resolved).(map[string]interface{})
	properties[property] = inline
	return inline, nil
}

func schemaPropertyItems(root, owner map[string]interface{}, property string) (map[string]interface{}, error) {
	list, err := resolveSchemaRef(root, schemaProperties(owner)[property])
	if err != nil {
		return nil, fmt.Errorf("property %s: %w", property, err)
	}
	items, err := resolveSchemaRef(root, list["items"])
	if err != nil {
		return nil, fmt.Errorf("property %s items: %w", property, err)
	}
	return items, nil
}

func cloneSchemaValue(value interface{}) interface{} {
	switch node := value.(type) {
	case map[string]interface{}:
		clone := make(map[string]interface{}, len(node))
		for key, child := range node {
			clone[key] = cloneSchemaValue(child)
		}
		return clone
	case []interface{}:
		clone := make([]interface{}, len(node))
		for index, child := range node {
			clone[index] = cloneSchemaValue(child)
		}
		return clone
	default:
		return value
	}
}
