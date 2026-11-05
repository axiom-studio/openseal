package toolfile

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/chatpath"
)

// Only explicit schema markers make a field file content; a description or
// format: uri never does.
//
//   - contentEncoding: base64, or format: byte (OpenAPI): a base64 string.
//   - format: data-url (or data-uri): a base64 data URL.
//   - contentMediaType of a binary media type with no contentEncoding: a
//     base64 string, since such content cannot be JSON text.
func binary(schema map[string]interface{}) (encoding, mediaType string, ok bool) {
	if !allowsString(schema["type"]) {
		return "", "", false
	}
	mediaType, _ = schema["contentMediaType"].(string)
	mediaType = chatpath.BaseMediaType(mediaType)
	contentEncoding, _ := schema["contentEncoding"].(string)
	format, _ := schema["format"].(string)
	switch {
	case strings.EqualFold(strings.TrimSpace(contentEncoding), "base64"), format == "byte":
		return Base64, mediaType, true
	case format == "data-url", format == "data-uri":
		return DataURL, mediaType, true
	case contentEncoding == "" && mediaType != "" && !chatpath.Textual(mediaType):
		return Base64, mediaType, true
	}
	return "", "", false
}

func allowsString(kind interface{}) bool {
	switch typed := kind.(type) {
	case nil:
		return true
	case string:
		return typed == "string"
	case []interface{}:
		for _, item := range typed {
			if item == "string" {
				return true
			}
		}
	}
	return false
}

// node is the schemas that apply at one place in a value: a schema with its
// $ref, allOf, anyOf and oneOf followed, so a field is file content when any
// schema that may apply there says so.
type node struct {
	root    map[string]interface{}
	schemas []map[string]interface{}
}

const maximumSchemaDepth = 32

func newNode(root map[string]interface{}) node {
	n := node{root: root}
	if root != nil {
		n.schemas = n.expand(root, 0, nil)
	}
	return n
}

func (n node) expand(schema map[string]interface{}, depth int, into []map[string]interface{}) []map[string]interface{} {
	if schema == nil || depth > maximumSchemaDepth {
		return into
	}
	into = append(into, schema)
	if reference, ok := schema["$ref"].(string); ok {
		if target := n.resolve(reference); target != nil {
			into = n.expand(target, depth+1, into)
		}
	}
	for _, keyword := range []string{"allOf", "anyOf", "oneOf"} {
		branches, _ := schema[keyword].([]interface{})
		for _, branch := range branches {
			if child, ok := branch.(map[string]interface{}); ok {
				into = n.expand(child, depth+1, into)
			}
		}
	}
	return into
}

// resolve follows a local reference ("#", "#/$defs/x", "#/definitions/x").
func (n node) resolve(reference string) map[string]interface{} {
	if !strings.HasPrefix(reference, "#") {
		return nil
	}
	target, ok := Get(n.root, strings.TrimPrefix(reference, "#"))
	if !ok {
		return nil
	}
	schema, _ := target.(map[string]interface{})
	return schema
}

func (n node) child(children []map[string]interface{}) node {
	next := node{root: n.root}
	for _, child := range children {
		next.schemas = n.expand(child, 0, next.schemas)
	}
	return next
}

func (n node) property(name string) node {
	var children []map[string]interface{}
	for _, schema := range n.schemas {
		properties, _ := schema["properties"].(map[string]interface{})
		if property, ok := properties[name].(map[string]interface{}); ok {
			children = append(children, property)
			continue
		}
		if additional, ok := schema["additionalProperties"].(map[string]interface{}); ok {
			children = append(children, additional)
		}
	}
	return n.child(children)
}

func (n node) item(index int) node {
	var children []map[string]interface{}
	for _, schema := range n.schemas {
		if prefix, ok := schema["prefixItems"].([]interface{}); ok && index < len(prefix) {
			if item, ok := prefix[index].(map[string]interface{}); ok {
				children = append(children, item)
			}
			continue
		}
		switch items := schema["items"].(type) {
		case map[string]interface{}:
			children = append(children, items)
		case []interface{}:
			// Draft 7 tuples.
			if index < len(items) {
				if item, ok := items[index].(map[string]interface{}); ok {
					children = append(children, item)
				}
			} else if additional, ok := schema["additionalItems"].(map[string]interface{}); ok {
				children = append(children, additional)
			}
		}
	}
	return n.child(children)
}

// binary reports the encoding a file takes here, when any schema that applies
// takes file content.
func (n node) binary() (encoding, mediaType string, ok bool) {
	for _, schema := range n.schemas {
		if encoding, mediaType, ok = binary(schema); ok {
			return encoding, mediaType, true
		}
	}
	return "", "", false
}

// placeholder stands in for a file's content while the kernel validates a
// value against its schema; the host replaces it with the real content.
func placeholder(encoding string) string {
	if encoding == DataURL {
		return "data:application/octet-stream;base64,AAAA"
	}
	return "AAAA"
}

// ErrNoFiles is a {"$file": …} value in a field whose schema takes no file
// content.
type ErrNoFiles struct{ Field string }

func (e *ErrNoFiles) Error() string {
	return fmt.Sprintf("%s: this tool takes no files in that field; pass {\"$file\": \"<path>\"} only where its input schema takes file content (base64 or a data URL)", e.Field)
}

// Check finds the {"$file": path} values of value, a tool's arguments under
// schema found at prefix in its whole input. It returns value with each one
// replaced by a placeholder of its field's shape, for schema validation, and
// where each goes. A file in a field that takes no file content, or a path
// that cannot be a chat file, is refused.
func Check(schema map[string]interface{}, value interface{}, prefix string) (interface{}, []Reference, error) {
	var references []Reference
	checked, err := check(newNode(schema), value, prefix, &references)
	if err != nil {
		return nil, nil, err
	}
	return checked, references, nil
}

func check(n node, value interface{}, pointer string, references *[]Reference) (interface{}, error) {
	if name, ok := RefPath(value); ok {
		encoding, _, takesFiles := n.binary()
		if !takesFiles {
			return nil, &ErrNoFiles{Field: Field(pointer)}
		}
		clean, err := chatpath.Clean(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", Field(pointer), err)
		}
		*references = append(*references, Reference{Pointer: pointer, Path: clean, Encoding: encoding})
		return placeholder(encoding), nil
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			checked, err := check(n.property(key), child, pointer+"/"+Escape(key), references)
			if err != nil {
				return nil, err
			}
			result[key] = checked
		}
		return result, nil
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			checked, err := check(n.item(index), child, pointer+"/"+strconv.Itoa(index), references)
			if err != nil {
				return nil, err
			}
			result[index] = checked
		}
		return result, nil
	}
	return value, nil
}

// CheckOutput is a tool's output with each Ref its host put in a file
// content field replaced by a placeholder, for validation against the output
// schema the tool declares.
func CheckOutput(schema map[string]interface{}, value interface{}) interface{} {
	return checkOutput(newNode(schema), value)
}

func checkOutput(n node, value interface{}) interface{} {
	if IsRef(value) {
		if encoding, _, ok := n.binary(); ok {
			return placeholder(encoding)
		}
	}
	switch typed := value.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			result[key] = checkOutput(n.property(key), child)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			result[index] = checkOutput(n.item(index), child)
		}
		return result
	}
	return value
}

// Extract takes each binary payload out of value: every non-empty base64
// string in a field schema declares as file content. It returns value with
// a Ref (without a path yet) in each payload's place.
func Extract(schema map[string]interface{}, value interface{}) (interface{}, []Payload) {
	var payloads []Payload
	result := extract(newNode(schema), value, "", "file", &payloads)
	return result, payloads
}

func extract(n node, value interface{}, pointer, name string, payloads *[]Payload) interface{} {
	switch typed := value.(type) {
	case string:
		encoding, mediaType, ok := n.binary()
		if !ok || typed == "" {
			return typed
		}
		data, declared, err := Decode(typed)
		if err != nil || (encoding == DataURL && declared == "") {
			return typed
		}
		payload := Payload{Pointer: pointer, Name: name, MimeType: firstNonEmpty(declared, mediaType, sniff(data)), Data: data}
		*payloads = append(*payloads, payload)
		return payload.Ref("", "").Value()
	case map[string]interface{}:
		result := make(map[string]interface{}, len(typed))
		for key, child := range typed {
			result[key] = extract(n.property(key), child, pointer+"/"+Escape(key), key, payloads)
		}
		return result
	case []interface{}:
		result := make([]interface{}, len(typed))
		for index, child := range typed {
			result[index] = extract(n.item(index), child, pointer+"/"+strconv.Itoa(index), name+"-"+strconv.Itoa(index+1), payloads)
		}
		return result
	}
	return value
}

// ExtractMCP takes the binary payloads out of an MCP tools/call result:
// image and audio content, embedded resource blobs, and file content fields
// of structuredContent under the tool's output schema. Text, text resources
// and resource links stay as they are.
func ExtractMCP(result map[string]interface{}, outputSchema map[string]interface{}) (map[string]interface{}, []Payload) {
	if result == nil {
		return nil, nil
	}
	var payloads []Payload
	extracted := make(map[string]interface{}, len(result))
	for key, value := range result {
		extracted[key] = value
	}
	if content, ok := result["content"].([]interface{}); ok {
		items := make([]interface{}, len(content))
		for index, raw := range content {
			items[index] = raw
			item, _ := raw.(map[string]interface{})
			pointer := "/content/" + strconv.Itoa(index)
			kind, _ := item["type"].(string)
			switch kind {
			case "image", "audio":
				text, _ := item["data"].(string)
				data, declared, err := Decode(text)
				if text == "" || err != nil {
					continue
				}
				mimeType, _ := item["mimeType"].(string)
				payload := Payload{Pointer: pointer, Name: kind + "-" + strconv.Itoa(index+1), MimeType: firstNonEmpty(chatpath.BaseMediaType(mimeType), declared, sniff(data)), Data: data}
				payloads = append(payloads, payload)
				items[index] = payload.Ref("", "").Value()
			case "resource":
				resource, _ := item["resource"].(map[string]interface{})
				text, _ := resource["blob"].(string)
				if text == "" {
					continue
				}
				data, _, err := Decode(text)
				if err != nil {
					continue
				}
				uri, _ := resource["uri"].(string)
				mimeType, _ := resource["mimeType"].(string)
				name := "resource-" + strconv.Itoa(index+1)
				if base := resourceName(uri); base != "" {
					name = base
				}
				payload := Payload{Pointer: pointer, Name: name, MimeType: firstNonEmpty(chatpath.BaseMediaType(mimeType), chatpath.MediaTypeOf(name), sniff(data)), Data: data, URI: uri}
				if payload.MimeType == "application/octet-stream" {
					payload.MimeType = firstNonEmpty(sniff(data), payload.MimeType)
				}
				payloads = append(payloads, payload)
				items[index] = payload.Ref("", "").Value()
			}
		}
		extracted["content"] = items
	}
	if structured, ok := result["structuredContent"]; ok && outputSchema != nil {
		value, found := extractUnder(outputSchema, structured, "/structuredContent")
		extracted["structuredContent"] = value
		payloads = append(payloads, found...)
	}
	return extracted, payloads
}

func extractUnder(schema map[string]interface{}, value interface{}, prefix string) (interface{}, []Payload) {
	var payloads []Payload
	result := extract(newNode(schema), value, prefix, "file", &payloads)
	return result, payloads
}

// resourceName is the last segment of a resource URI, if it names a file.
func resourceName(uri string) string {
	uri = strings.TrimSpace(uri)
	if cut := strings.IndexAny(uri, "?#"); cut >= 0 {
		uri = uri[:cut]
	}
	uri = strings.TrimRight(uri, "/")
	if slash := strings.LastIndex(uri, "/"); slash >= 0 {
		uri = uri[slash+1:]
	} else if colon := strings.LastIndex(uri, ":"); colon >= 0 {
		uri = uri[colon+1:]
	}
	if unescaped, err := url.PathUnescape(uri); err == nil {
		uri = unescaped
	}
	return strings.TrimSpace(uri)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// HasFileFields reports whether schema takes file content anywhere.
func HasFileFields(schema map[string]interface{}) bool {
	found := false
	visitSchemas(schema, func(child map[string]interface{}) {
		if _, _, ok := binary(child); ok {
			found = true
		}
	})
	return found
}

func visitSchemas(schema map[string]interface{}, visit func(map[string]interface{})) {
	if schema == nil {
		return
	}
	visit(schema)
	for _, keyword := range []string{"properties", "$defs", "definitions", "patternProperties"} {
		children, _ := schema[keyword].(map[string]interface{})
		for _, child := range children {
			if nested, ok := child.(map[string]interface{}); ok {
				visitSchemas(nested, visit)
			}
		}
	}
	for _, keyword := range []string{"items", "additionalProperties", "additionalItems", "allOf", "anyOf", "oneOf", "prefixItems"} {
		switch child := schema[keyword].(type) {
		case map[string]interface{}:
			visitSchemas(child, visit)
		case []interface{}:
			for _, item := range child {
				if nested, ok := item.(map[string]interface{}); ok {
					visitSchemas(nested, visit)
				}
			}
		}
	}
}

// fileRefSchema is the {"$file": path} a model may pass for file content.
func fileRefSchema() map[string]interface{} {
	return map[string]interface{}{
		"type":                 "object",
		"description":          "A file in this chat's files, by path, such as tool-output/<call>/image-1.png or images/photo.jpg; the tool receives its content.",
		"additionalProperties": false,
		"required":             []interface{}{Key},
		"properties":           map[string]interface{}{Key: map[string]interface{}{"type": "string", "minLength": 1, "maxLength": chatpath.MaximumPathBytes}},
	}
}

// ModelSchema is schema as a model sees it: each field that takes file
// content also accepts {"$file": path}. A schema without such fields is
// returned as it is.
func ModelSchema(schema map[string]interface{}) map[string]interface{} {
	if !HasFileFields(schema) {
		return schema
	}
	projected, _ := modelSchema(schema).(map[string]interface{})
	return projected
}

func modelSchema(value interface{}) interface{} {
	schema, ok := value.(map[string]interface{})
	if !ok {
		return value
	}
	if _, _, takesFiles := binary(schema); takesFiles {
		return map[string]interface{}{"anyOf": []interface{}{cloneSchema(schema), fileRefSchema()}}
	}
	result := make(map[string]interface{}, len(schema))
	for key, child := range schema {
		switch key {
		case "properties", "$defs", "definitions", "patternProperties":
			children, ok := child.(map[string]interface{})
			if !ok {
				result[key] = child
				continue
			}
			projected := make(map[string]interface{}, len(children))
			for name, nested := range children {
				projected[name] = modelSchema(nested)
			}
			result[key] = projected
		case "items", "additionalProperties", "additionalItems", "allOf", "anyOf", "oneOf", "prefixItems":
			switch typed := child.(type) {
			case map[string]interface{}:
				result[key] = modelSchema(typed)
			case []interface{}:
				projected := make([]interface{}, len(typed))
				for index, nested := range typed {
					projected[index] = modelSchema(nested)
				}
				result[key] = projected
			default:
				result[key] = child
			}
		default:
			result[key] = child
		}
	}
	return result
}

func cloneSchema(schema map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(schema))
	for key, value := range schema {
		result[key] = value
	}
	return result
}
