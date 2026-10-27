// Package fileinput is the "file" type of a Skill action input: a file from
// the chat the action runs in. The model passes the file's path; the host
// hands the Skill a File, whose bytes the Skill streams from its URL, so file
// content never travels through the model or inside the call.
package fileinput

import (
	"sort"
	"strings"
)

// Type is the input schema type of a file input. It is only allowed on an
// action's top-level input properties.
const Type = "file"

// MaximumBytes is the largest file a host hands a Skill.
const MaximumBytes = 100 << 20

// File is what a Skill receives in place of a file input's path.
type File struct {
	// Name is the file's base name, such as report.pdf.
	Name string `json:"name"`
	// Path is the file's path in the chat, as the model passed it.
	Path      string `json:"path"`
	MediaType string `json:"mediaType"`
	SizeBytes int64  `json:"sizeBytes"`
	// URL serves this one file's bytes for a short while.
	URL string `json:"url"`
}

const pathGuidance = "A file in this chat, by its path, such as docs/report.pdf. Files of up to 100 MB can be passed."

// Properties are the names of schema's file inputs.
func Properties(schema map[string]interface{}) []string {
	properties, _ := schema["properties"].(map[string]interface{})
	var names []string
	for name, property := range properties {
		if definition, ok := property.(map[string]interface{}); ok && definition["type"] == Type {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// Schema is schema with each file input written as what the model passes and
// JSON Schema validates: the file's path as a string. It returns schema
// itself when there are no file inputs.
func Schema(schema map[string]interface{}) map[string]interface{} {
	names := Properties(schema)
	if len(names) == 0 {
		return schema
	}
	result := make(map[string]interface{}, len(schema))
	for key, value := range schema {
		result[key] = value
	}
	properties := make(map[string]interface{})
	for name, property := range schema["properties"].(map[string]interface{}) {
		properties[name] = property
	}
	for _, name := range names {
		file := properties[name].(map[string]interface{})
		path := map[string]interface{}{"type": "string", "minLength": 1, "maxLength": 1024}
		description, _ := file["description"].(string)
		if description = strings.TrimSpace(description); description == "" {
			path["description"] = pathGuidance
		} else {
			path["description"] = description + " " + pathGuidance
		}
		properties[name] = path
	}
	result["properties"] = properties
	return result
}
