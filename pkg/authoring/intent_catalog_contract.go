package authoring

import "sort"

// Scope the model's Skill choices to the same catalog that the deterministic
// validator uses. The base schema is cloned so tenants cannot share choices.
func authoringIntentContractForCatalog(catalog CapabilityCatalog) authoringProviderContract {
	contract := authoringIntentContract()
	contract.Schema = func() (map[string]interface{}, error) {
		schema, err := AuthoringIntentJSONSchema()
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(catalog.Skills))
		for id := range catalog.Skills {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) > 0 {
			constrainAuthoringSkillReferences(schema, ids)
		}
		return schema, nil
	}
	return contract
}

func constrainAuthoringSkillReferences(value interface{}, ids []string) {
	switch node := value.(type) {
	case map[string]interface{}:
		if properties, ok := node["properties"].(map[string]interface{}); ok {
			if id, ok := properties["catalogId"].(map[string]interface{}); ok {
				id["enum"] = ids
			}
			if list, ok := properties["skillCatalogIds"].(map[string]interface{}); ok {
				if item, ok := list["items"].(map[string]interface{}); ok {
					item["enum"] = ids
				}
			}
		}
		for _, child := range node {
			constrainAuthoringSkillReferences(child, ids)
		}
	case []interface{}:
		for _, child := range node {
			constrainAuthoringSkillReferences(child, ids)
		}
	}
}
