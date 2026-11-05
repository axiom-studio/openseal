// Package toolfile moves files in and out of tool calls, for every tool: MCP
// servers, Skills, and the same calls made from a chat's VM.
//
// Out: a host takes each binary payload out of a tool's result (MCP image and
// audio content, embedded resource blobs, and base64 fields the tool's output
// schema declares as binary), keeps it as a file in the chat's files under
// tool-output/<call>/, and puts a Ref in its place, so results stay small and
// the model reads or presents the file by path.
//
// In: an argument value {"$file": "<path>"} names a chat file. The kernel
// accepts it only where the tool's input schema takes file content (a base64
// string or a data URL) and tells the host where it goes; the host puts the
// file's content there, in that shape, just before calling the tool.
package toolfile

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/axiom-studio/openseal/pkg/chatpath"
)

const (
	// Key is the one key of an argument value that names a chat file.
	Key = "$file"
	// RefType is the type of a Ref.
	RefType = "file"
	// Folder is where the files tools return are kept.
	Folder = chatpath.ToolOutput
	// MaxFileBytes bounds one file in or out of a tool call (the chat-file
	// cap); MaxCallBytes bounds all the files one call takes.
	MaxFileBytes int64 = 100 << 20
	MaxCallBytes int64 = 500 << 20
)

// Encodings an input field can take a file in.
const (
	Base64  = "base64"
	DataURL = "data-url"
)

// Ref is what a tool result carries in place of a file's bytes. Path is the
// file in the chat's files; it is empty, with Note saying why, when the file
// could not be kept.
type Ref struct {
	Type      string `json:"type"`
	Path      string `json:"path,omitempty"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	// URI is an MCP embedded resource's own URI.
	URI  string `json:"uri,omitempty"`
	Note string `json:"note,omitempty"`
}

// Value is the Ref as a JSON object.
func (r Ref) Value() map[string]interface{} {
	value := map[string]interface{}{"type": RefType, "mimeType": r.MimeType, "sizeBytes": r.SizeBytes}
	if r.Path != "" {
		value["path"] = r.Path
	}
	if r.URI != "" {
		value["uri"] = r.URI
	}
	if r.Note != "" {
		value["note"] = r.Note
	}
	return value
}

// IsRef reports whether value is a Ref.
func IsRef(value interface{}) bool {
	object, ok := value.(map[string]interface{})
	if !ok || object["type"] != RefType {
		return false
	}
	_, mime := object["mimeType"].(string)
	switch object["sizeBytes"].(type) {
	case float64, int64, int:
		return mime
	}
	return false
}

// RefPaths lists the kept files a result refers to, each once, sorted.
func RefPaths(value interface{}) []string {
	seen := map[string]bool{}
	var walk func(interface{})
	walk = func(value interface{}) {
		switch typed := value.(type) {
		case map[string]interface{}:
			if IsRef(typed) {
				if name, _ := typed["path"].(string); name != "" {
					seen[name] = true
				}
				return
			}
			for _, child := range typed {
				walk(child)
			}
		case []interface{}:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	paths := make([]string, 0, len(seen))
	for name := range seen {
		paths = append(paths, name)
	}
	sort.Strings(paths)
	return paths
}

// Payload is one binary payload taken out of a tool result: its bytes, the
// name it suggests and where in the result it was (an RFC 6901 pointer).
type Payload struct {
	Pointer  string `json:"pointer"`
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Data     []byte `json:"data"`
	URI      string `json:"uri,omitempty"`
}

// Ref is the payload's Ref once kept at path ("" with note when not kept).
func (p Payload) Ref(path, note string) Ref {
	return Ref{Type: RefType, Path: path, MimeType: p.MimeType, SizeBytes: int64(len(p.Data)), URI: p.URI, Note: note}
}

// Reference is a {"$file": path} argument the kernel accepted: where it is in
// the tool's transport arguments, the chat path it names and the encoding the
// field takes.
type Reference struct {
	Pointer  string `json:"pointer"`
	Path     string `json:"path"`
	Encoding string `json:"encoding"`
}

// Link is a Reference its host resolved to the file's bytes: what the file is
// and a short-lived URL to read it from.
type Link struct {
	Reference
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
	URL       string `json:"url"`
}

// RefPath returns the path of an argument value {"$file": "<path>"}.
func RefPath(value interface{}) (string, bool) {
	object, ok := value.(map[string]interface{})
	if !ok || len(object) != 1 {
		return "", false
	}
	name, ok := object[Key].(string)
	return name, ok
}

// FileValue is the argument value naming the chat file at name.
func FileValue(name string) map[string]interface{} {
	return map[string]interface{}{Key: name}
}

// Encode is data in the shape encoding asks for.
func Encode(encoding, mimeType string, data []byte) (string, error) {
	switch encoding {
	case Base64:
		return base64.StdEncoding.EncodeToString(data), nil
	case DataURL:
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return "data:" + chatpath.BaseMediaType(mimeType) + ";base64," + base64.StdEncoding.EncodeToString(data), nil
	}
	return "", fmt.Errorf("unknown file encoding %q", encoding)
}

// Decode reads base64 text or a base64 data URL; mimeType is the data URL's.
func Decode(text string) (data []byte, mimeType string, err error) {
	text = strings.TrimSpace(text)
	if rest, ok := strings.CutPrefix(text, "data:"); ok {
		header, body, found := strings.Cut(rest, ",")
		if !found || !strings.HasSuffix(header, ";base64") {
			return nil, "", errors.New("not a base64 data URL")
		}
		mimeType = chatpath.BaseMediaType(strings.TrimSuffix(header, ";base64"))
		text = body
	}
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, text)
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err = encoding.DecodeString(text); err == nil {
			return data, mimeType, nil
		}
	}
	return nil, "", errors.New("not base64")
}

// Path is where a payload of the call is kept: tool-output/<call>/<name>.<ext>,
// unique among used, which it records.
func Path(actionCallID string, payload Payload, used map[string]bool) string {
	directory := Folder + segment(actionCallID, "call") + "/"
	base := path.Base(strings.TrimSpace(payload.Name))
	ext := chatpath.Extension(base, payload.MimeType)
	if ext == "" {
		ext = ".bin"
	}
	stem := chatpath.Slug(strings.TrimSuffix(base, path.Ext(base)), "file")
	candidate := directory + stem + ext
	for n := 2; used[candidate]; n++ {
		candidate = directory + stem + "-" + strconv.Itoa(n) + ext
	}
	used[candidate] = true
	return candidate
}

// segment keeps a path segment to letters, digits, dots, dashes and
// underscores.
func segment(value, fallback string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, strings.TrimSpace(value))
	cleaned = strings.Trim(cleaned, ".-")
	if len(cleaned) > 128 {
		cleaned = cleaned[:128]
	}
	if cleaned == "" {
		return fallback
	}
	return cleaned
}

// Set puts value at pointer in root, which must lead to an existing object
// member or array element (or a new member of an existing object).
func Set(root interface{}, pointer string, value interface{}) error {
	if pointer == "" {
		return errors.New("a file cannot replace the whole value")
	}
	tokens, err := splitPointer(pointer)
	if err != nil {
		return err
	}
	current := root
	for index, token := range tokens {
		last := index == len(tokens)-1
		switch typed := current.(type) {
		case map[string]interface{}:
			if last {
				typed[token] = value
				return nil
			}
			next, ok := typed[token]
			if !ok {
				return fmt.Errorf("%s is not in the value", pointer)
			}
			current = next
		case []interface{}:
			position, err := strconv.Atoi(token)
			if err != nil || position < 0 || position >= len(typed) {
				return fmt.Errorf("%s is not in the value", pointer)
			}
			if last {
				typed[position] = value
				return nil
			}
			current = typed[position]
		default:
			return fmt.Errorf("%s is not in the value", pointer)
		}
	}
	return nil
}

// Get is the value at pointer in root.
func Get(root interface{}, pointer string) (interface{}, bool) {
	tokens, err := splitPointer(pointer)
	if err != nil {
		return nil, false
	}
	current := root
	for _, token := range tokens {
		switch typed := current.(type) {
		case map[string]interface{}:
			next, ok := typed[token]
			if !ok {
				return nil, false
			}
			current = next
		case []interface{}:
			position, err := strconv.Atoi(token)
			if err != nil || position < 0 || position >= len(typed) {
				return nil, false
			}
			current = typed[position]
		default:
			return nil, false
		}
	}
	return current, true
}

func splitPointer(pointer string) ([]string, error) {
	if pointer == "" {
		return nil, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, fmt.Errorf("%q is not a JSON pointer", pointer)
	}
	tokens := strings.Split(pointer[1:], "/")
	for index, token := range tokens {
		tokens[index] = strings.ReplaceAll(strings.ReplaceAll(token, "~1", "/"), "~0", "~")
	}
	return tokens, nil
}

// Escape is a JSON pointer token for key.
func Escape(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

// Field names a pointer the way a person reads it: images[0].data.
func Field(pointer string) string {
	tokens, err := splitPointer(pointer)
	if err != nil || len(tokens) == 0 {
		return "the input"
	}
	var field strings.Builder
	for _, token := range tokens {
		if _, err := strconv.Atoi(token); err == nil && field.Len() > 0 {
			field.WriteString("[" + token + "]")
			continue
		}
		if field.Len() > 0 {
			field.WriteString(".")
		}
		field.WriteString(token)
	}
	return field.String()
}

// sniff is a payload's media type when nothing declares one.
func sniff(data []byte) string {
	return chatpath.BaseMediaType(http.DetectContentType(data))
}
