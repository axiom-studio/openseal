package fileinput

import (
	"reflect"
	"strings"
	"testing"
)

func TestSchemaWritesFileInputsAsChatPaths(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"file":  map[string]interface{}{"type": "file", "description": "The file to upload."},
		"cover": map[string]interface{}{"type": "file"},
		"name":  map[string]interface{}{"type": "string"},
	}, "required": []interface{}{"file"}}
	if names := Properties(schema); !reflect.DeepEqual(names, []string{"cover", "file"}) {
		t.Fatalf("file inputs = %v", names)
	}
	projected := Schema(schema)
	properties := projected["properties"].(map[string]interface{})
	file := properties["file"].(map[string]interface{})
	if file["type"] != "string" || !strings.HasPrefix(file["description"].(string), "The file to upload. A file in this chat") ||
		properties["cover"].(map[string]interface{})["type"] != "string" || properties["name"].(map[string]interface{})["type"] != "string" {
		t.Fatalf("projected = %#v", projected)
	}
	if schema["properties"].(map[string]interface{})["file"].(map[string]interface{})["type"] != Type {
		t.Fatal("projection changed the Skill's own schema")
	}
	plain := map[string]interface{}{"type": "object"}
	if got := Schema(plain); !reflect.DeepEqual(got, plain) {
		t.Fatalf("schema without files changed: %#v", got)
	}
}
