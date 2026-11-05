package toolfile

import (
	"encoding/base64"
	"errors"
	"reflect"
	"strings"
	"testing"
)

var pngBytes = []byte("\x89PNG\r\n\x1a\n0000IHDR fake png body")

func TestCheckAcceptsFilesOnlyWhereTheSchemaTakesFileContent(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"image":   map[string]interface{}{"type": "string", "contentEncoding": "base64", "contentMediaType": "image/png"},
		"cover":   map[string]interface{}{"type": "string", "format": "data-url"},
		"raw":     map[string]interface{}{"type": "string", "format": "byte"},
		"audio":   map[string]interface{}{"type": "string", "contentMediaType": "audio/mpeg"},
		"pages":   map[string]interface{}{"type": "array", "items": map[string]interface{}{"$ref": "#/$defs/page"}},
		"link":    map[string]interface{}{"type": "string", "format": "uri", "description": "base64 image data"},
		"caption": map[string]interface{}{"type": "string"},
	}, "$defs": map[string]interface{}{"page": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"data": map[string]interface{}{"anyOf": []interface{}{map[string]interface{}{"type": "null"}, map[string]interface{}{"type": "string", "contentEncoding": "base64"}}},
	}}}}
	value := map[string]interface{}{
		"image": FileValue("tool-output/a/shot.png"), "cover": FileValue("/images/cover.jpg"), "raw": FileValue("docs/x.bin"),
		"audio": FileValue("media/a.mp3"), "caption": "hello",
		"pages": []interface{}{map[string]interface{}{"data": FileValue("docs/p1.pdf")}},
	}
	checked, references, err := Check(schema, value, "/arguments")
	if err != nil {
		t.Fatal(err)
	}
	object := checked.(map[string]interface{})
	if object["image"] != "AAAA" || !strings.HasPrefix(object["cover"].(string), "data:") || object["caption"] != "hello" ||
		object["pages"].([]interface{})[0].(map[string]interface{})["data"] != "AAAA" {
		t.Fatalf("checked = %#v", checked)
	}
	byPointer := map[string]Reference{}
	for _, reference := range references {
		byPointer[reference.Pointer] = reference
	}
	want := map[string]Reference{
		"/arguments/image":        {Pointer: "/arguments/image", Path: "tool-output/a/shot.png", Encoding: Base64},
		"/arguments/cover":        {Pointer: "/arguments/cover", Path: "images/cover.jpg", Encoding: DataURL},
		"/arguments/raw":          {Pointer: "/arguments/raw", Path: "docs/x.bin", Encoding: Base64},
		"/arguments/audio":        {Pointer: "/arguments/audio", Path: "media/a.mp3", Encoding: Base64},
		"/arguments/pages/0/data": {Pointer: "/arguments/pages/0/data", Path: "docs/p1.pdf", Encoding: Base64},
	}
	if !reflect.DeepEqual(byPointer, want) {
		t.Fatalf("references = %#v", byPointer)
	}
	if value["image"].(map[string]interface{})[Key] != "tool-output/a/shot.png" {
		t.Fatal("Check changed its input")
	}

	for _, field := range []string{"link", "caption"} {
		_, _, err := Check(schema, map[string]interface{}{field: FileValue("images/a.png")}, "")
		var refused *ErrNoFiles
		if !errors.As(err, &refused) || refused.Field != field || !strings.Contains(err.Error(), "takes no files in that field") {
			t.Fatalf("%s: err = %v", field, err)
		}
	}
	if _, _, err := Check(nil, map[string]interface{}{"x": FileValue("a.png")}, ""); err == nil {
		t.Fatal("a schema-less tool took a file")
	}
	if _, _, err := Check(schema, map[string]interface{}{"image": FileValue("../etc/passwd")}, ""); err == nil {
		t.Fatal("a path outside the chat was accepted")
	}
}

func TestExtractTakesDeclaredBinaryFieldsOut(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"thumbnail": map[string]interface{}{"type": "string", "contentEncoding": "base64"},
		"report":    map[string]interface{}{"type": "string", "contentMediaType": "application/pdf"},
		"title":     map[string]interface{}{"type": "string"},
		"note":      map[string]interface{}{"type": "string", "contentMediaType": "application/json"},
	}}
	pdf := []byte("%PDF-1.7 fake")
	output := map[string]interface{}{
		"thumbnail": base64.StdEncoding.EncodeToString(pngBytes), "report": base64.StdEncoding.EncodeToString(pdf),
		"title": base64.StdEncoding.EncodeToString([]byte("looks like base64")), "note": "{}",
	}
	result, payloads := Extract(schema, output)
	if len(payloads) != 2 {
		t.Fatalf("payloads = %#v", payloads)
	}
	object := result.(map[string]interface{})
	if !IsRef(object["thumbnail"]) || !IsRef(object["report"]) || object["title"] != output["title"] || object["note"] != "{}" {
		t.Fatalf("result = %#v", result)
	}
	for _, payload := range payloads {
		switch payload.Pointer {
		case "/thumbnail":
			if payload.MimeType != "image/png" || string(payload.Data) != string(pngBytes) {
				t.Fatalf("thumbnail = %#v", payload)
			}
		case "/report":
			if payload.MimeType != "application/pdf" || payload.Name != "report" {
				t.Fatalf("report = %#v", payload)
			}
		default:
			t.Fatalf("payload at %s", payload.Pointer)
		}
	}
	// Kept, the result validates against the declared schema again.
	if err := Set(result, "/thumbnail", payloads[0].Ref("tool-output/c/thumbnail.png", "").Value()); err != nil {
		t.Fatal(err)
	}
	checked := CheckOutput(schema, result).(map[string]interface{})
	if checked["thumbnail"] != "AAAA" || checked["report"] != "AAAA" {
		t.Fatalf("checked = %#v", checked)
	}
}

func TestExtractMCPContent(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	result := map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "two screens"},
			map[string]interface{}{"type": "image", "data": encoded, "mimeType": "image/png"},
			map[string]interface{}{"type": "audio", "data": base64.StdEncoding.EncodeToString([]byte("ID3")), "mimeType": "audio/mpeg"},
			map[string]interface{}{"type": "resource", "resource": map[string]interface{}{"uri": "file:///reports/q3%20summary.pdf", "mimeType": "application/pdf", "blob": base64.StdEncoding.EncodeToString([]byte("%PDF-1.4"))}},
			map[string]interface{}{"type": "resource", "resource": map[string]interface{}{"uri": "mem://notes", "mimeType": "text/plain", "text": "inline"}},
			map[string]interface{}{"type": "resource_link", "uri": "https://example.com/a.png", "name": "a"},
		},
		"structuredContent": map[string]interface{}{"screens": []interface{}{map[string]interface{}{"png": encoded, "id": "s1"}}},
	}
	outputSchema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"screens": map[string]interface{}{
		"type": "array", "items": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
			"png": map[string]interface{}{"type": "string", "contentEncoding": "base64", "contentMediaType": "image/png"}, "id": map[string]interface{}{"type": "string"},
		}},
	}}}
	extracted, payloads := ExtractMCP(result, outputSchema)
	if len(payloads) != 4 {
		t.Fatalf("payloads = %#v", payloads)
	}
	content := extracted["content"].([]interface{})
	if content[0].(map[string]interface{})["type"] != "text" || !IsRef(content[1]) || !IsRef(content[2]) || !IsRef(content[3]) ||
		content[4].(map[string]interface{})["type"] != "resource" || content[5].(map[string]interface{})["type"] != "resource_link" {
		t.Fatalf("content = %#v", content)
	}
	if result["content"].([]interface{})[1].(map[string]interface{})["data"] != encoded {
		t.Fatal("ExtractMCP changed its input")
	}
	used := map[string]bool{}
	var paths []string
	for _, payload := range payloads {
		paths = append(paths, Path("call:01/x", payload, used))
	}
	want := []string{"tool-output/call-01-x/image-2.png", "tool-output/call-01-x/audio-3.mp3", "tool-output/call-01-x/q3-summary.pdf", "tool-output/call-01-x/png.png"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v", paths)
	}
	if payloads[2].URI != "file:///reports/q3%20summary.pdf" || payloads[3].Pointer != "/structuredContent/screens/0/png" {
		t.Fatalf("payloads = %#v", payloads)
	}
	for index, payload := range payloads {
		if err := Set(extracted, payload.Pointer, payload.Ref(paths[index], "").Value()); err != nil {
			t.Fatal(err)
		}
	}
	if got := RefPaths(extracted); !reflect.DeepEqual(got, []string{
		"tool-output/call-01-x/audio-3.mp3", "tool-output/call-01-x/image-2.png", "tool-output/call-01-x/png.png", "tool-output/call-01-x/q3-summary.pdf",
	}) {
		t.Fatalf("ref paths = %v", got)
	}
	again := Path("call:01/x", Payload{Name: "image-2", MimeType: "image/png"}, used)
	if again != "tool-output/call-01-x/image-2-2.png" {
		t.Fatalf("collision = %s", again)
	}
}

func TestModelSchemaOffersFileRefsAtFileFields(t *testing.T) {
	schema := map[string]interface{}{"type": "object", "properties": map[string]interface{}{
		"image": map[string]interface{}{"type": "string", "contentEncoding": "base64"},
		"name":  map[string]interface{}{"type": "string"},
	}}
	projected := ModelSchema(schema)
	image := projected["properties"].(map[string]interface{})["image"].(map[string]interface{})
	branches, _ := image["anyOf"].([]interface{})
	if len(branches) != 2 || branches[1].(map[string]interface{})["required"].([]interface{})[0] != Key {
		t.Fatalf("image = %#v", image)
	}
	if _, ok := schema["properties"].(map[string]interface{})["image"].(map[string]interface{})["anyOf"]; ok {
		t.Fatal("ModelSchema changed its input")
	}
	// The projected schema still takes the same files.
	if _, references, err := Check(projected, map[string]interface{}{"image": FileValue("a.png")}, ""); err != nil || len(references) != 1 {
		t.Fatalf("references = %v, err = %v", references, err)
	}
	plain := map[string]interface{}{"type": "object", "properties": map[string]interface{}{"name": map[string]interface{}{"type": "string"}}}
	if got := ModelSchema(plain); !reflect.DeepEqual(got, plain) {
		t.Fatalf("plain = %#v", got)
	}
}

func TestEncodeDecode(t *testing.T) {
	for _, encoding := range []string{Base64, DataURL} {
		text, err := Encode(encoding, "image/png", pngBytes)
		if err != nil {
			t.Fatal(err)
		}
		data, mimeType, err := Decode(text)
		if err != nil || string(data) != string(pngBytes) || (encoding == DataURL && mimeType != "image/png") {
			t.Fatalf("%s: %q %q %v", encoding, data, mimeType, err)
		}
	}
	if _, err := Encode("hex", "", nil); err == nil {
		t.Fatal("unknown encoding")
	}
	if _, _, err := Decode("not base64!"); err == nil {
		t.Fatal("decoded junk")
	}
}
