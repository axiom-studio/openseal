package creation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
)

// Render creates artifact bytes locally in the host without a service or network call.
func Render(ctx context.Context, kind string, config map[string]interface{}) (map[string]interface{}, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, _ := config["artifactId"].(string)
	name, _ := config["filename"].(string)
	title, _ := config["title"].(string)
	requirement, _ := config["requirementName"].(string)
	version, ok := integer(config["expectedLatestVersion"])
	if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`).MatchString(id) || !ok || version < 0 || version > 1000000 || !textBound(title, 300) || !textBound(requirement, 128) || strings.ContainsAny(name, "/\\\r\n\x00") || len(name) > 245 {
		return nil, errors.New("invalid artifact identity, version, title, filename, or deliverable")
	}
	var data []byte
	var err error
	switch kind {
	case Page:
		if !strings.HasSuffix(name, ".html") || len(name) <= 5 {
			return nil, errors.New("page filename must end in .html")
		}
		source, _ := config["html"].(string)
		if !textBound(source, 500000) || strings.ContainsRune(source, 0) {
			return nil, errors.New("page HTML is empty, invalid, or too large")
		}
		tokenizer := html.NewTokenizer(strings.NewReader(source))
		hasHTML, hasBody := false, false
		for {
			kind := tokenizer.Next()
			if kind == html.ErrorToken {
				break
			}
			if kind == html.StartTagToken {
				name, _ := tokenizer.TagName()
				switch string(name) {
				case "html":
					hasHTML = true
				case "body":
					hasBody = true
				}
			}
		}
		if !hasHTML || !hasBody {
			return nil, errors.New("page must be a complete HTML document with html and body elements")
		}
		data = []byte(source)
	case Workbook:
		if !strings.HasSuffix(name, ".xlsx") || len(name) <= 5 {
			return nil, errors.New("workbook filename must end in .xlsx")
		}
		raw, marshalErr := json.Marshal(config["sheets"])
		if marshalErr != nil || len(raw) > 8<<20 {
			return nil, errors.New("invalid or oversized workbook input")
		}
		var sheets []Sheet
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&sheets); err != nil {
			return nil, fmt.Errorf("invalid sheets: %w", err)
		}
		data, err = RenderWorkbook(ctx, sheets)
	default:
		return nil, errors.New("unsupported creation Skill")
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 8<<20 {
		return nil, errors.New("rendered artifact exceeds 8 MiB")
	}
	return map[string]interface{}{"_transientArtifactBase64": base64.StdEncoding.EncodeToString(data)}, nil
}
func textBound(s string, max int) bool {
	return strings.TrimSpace(s) != "" && utf8.ValidString(s) && utf8.RuneCountInString(s) <= max
}
func integer(value interface{}) (int64, bool) {
	switch n := value.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		if n >= 0 && n <= 1000000 && math.Trunc(n) == n {
			return int64(n), true
		}
	case json.Number:
		v, err := n.Int64()
		return v, err == nil
	}
	return 0, false
}
