package presentation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// FormatBlock writes a component as the block an answer carries: the
// inverse of ReadBlocks. The worked examples the model sees are written
// with it, so they always match what the host reads.
func FormatBlock(component Component) string {
	encoded, err := json.Marshal(component)
	if err != nil {
		return ""
	}
	var fields map[string]interface{}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return ""
	}
	return strings.TrimRight(formatNode(fields, 2+blockDepth(fields)), "\n")
}

// blockDepth is how many blocks deep a component's block goes: 1 for a
// component without nested blocks.
func blockDepth(fields map[string]interface{}) int {
	deepest := 0
	children, _ := fields["children"].([]interface{})
	for _, child := range children {
		if nested, ok := child.(map[string]interface{}); ok {
			deepest = max(deepest, blockDepth(nested))
		}
	}
	if tabs, ok := fields["tabs"].([]interface{}); ok && fields["type"] == "tabs" {
		for _, tab := range tabs {
			tabFields, _ := tab.(map[string]interface{})
			deepest = max(deepest, 1+blockDepth(map[string]interface{}{"children": tabFields["children"]}))
		}
	}
	return 1 + deepest
}

func formatNode(fields map[string]interface{}, fence int) string {
	typ, _ := fields["type"].(string)
	spec := catalogue[typ]
	colons := strings.Repeat(":", fence)
	attrs := []string{}
	if id, ok := fields["id"].(string); ok && id != "" {
		attrs = append(attrs, "#"+id)
	}
	for _, key := range []string{"title", "summary", "tone"} {
		if value, ok := fields[key]; ok {
			attrs = append(attrs, key+"="+attributeValue(value))
		}
	}
	rest := map[string]interface{}{}
	for _, key := range []string{"binding", "sources"} {
		if value, ok := fields[key]; ok {
			rest[key] = value
		}
	}
	var body []string
	switch {
	case typ == "tabs":
		tabs, _ := fields["tabs"].([]interface{})
		for _, tab := range tabs {
			tabFields, _ := tab.(map[string]interface{})
			inner := fence - 1
			var lines []string
			lines = append(lines, strings.Repeat(":", inner)+"tab{label="+attributeValue(tabFields["label"])+"}")
			children, _ := tabFields["children"].([]interface{})
			for _, child := range children {
				lines = append(lines, formatNode(child.(map[string]interface{}), inner-1))
			}
			lines = append(lines, strings.Repeat(":", inner))
			body = append(body, strings.Join(lines, "\n"))
		}
	case spec.children.max > 0:
		if payload, ok := fields[spec.payload].(map[string]interface{}); ok {
			attrs = append(attrs, scalarAttributes(payload)...)
		}
		children, _ := fields["children"].([]interface{})
		for _, child := range children {
			body = append(body, formatNode(child.(map[string]interface{}), fence-1))
		}
	case spec.payload == "text":
		if text, ok := fields["text"].(string); ok {
			body = append(body, text)
		}
	case spec.payload != "":
		switch payload := fields[spec.payload].(type) {
		case map[string]interface{}:
			attrs = append(attrs, scalarAttributes(payload)...)
			table := tableBody(typ, payload)
			for key, value := range payload {
				if !scalarAttribute(value) {
					rest[key] = value
				}
			}
			if len(rest) > 0 {
				body = append(body, yamlBody(rest))
			}
			if table != "" {
				body = append(body, table)
			}
			rest = nil
		case []interface{}:
			body = append(body, yamlBody(payload))
		}
	}
	if len(rest) > 0 {
		body = append(body, yamlBody(rest))
	}
	open := colons + typ
	if len(attrs) > 0 {
		open += "{" + strings.Join(attrs, " ") + "}"
	}
	lines := []string{open}
	lines = append(lines, body...)
	lines = append(lines, colons)
	return strings.Join(lines, "\n") + "\n"
}

// tableBody writes the table-shaped data of a chart or table as a GFM
// table and removes it from payload; it returns "" and leaves payload as
// it was when the data has no table form.
func tableBody(typ string, payload map[string]interface{}) string {
	switch typ {
	case "chart":
		series, _ := payload["series"].([]interface{})
		header, rows, ok := chartTable(series)
		if !ok {
			return ""
		}
		delete(payload, "series")
		return gfmTable(header, rows)
	case "table":
		columns, _ := payload["columns"].([]interface{})
		records, _ := payload["rows"].([]interface{})
		header := make([]string, len(columns))
		keys := make([]string, len(columns))
		plain := true
		for index, column := range columns {
			fields, _ := column.(map[string]interface{})
			header[index], _ = fields["label"].(string)
			keys[index], _ = fields["key"].(string)
			if len(fields) != 2 || keys[index] != slug(header[index]) {
				plain = false
			}
		}
		rows := make([][]string, 0, len(records))
		for _, record := range records {
			cells, _ := record.(map[string]interface{})
			row := make([]string, len(keys))
			for index, key := range keys {
				value, present := cells[key]
				if !present {
					continue
				}
				if !scalarAttribute(value) {
					return ""
				}
				row[index] = scalarText(value)
			}
			rows = append(rows, row)
		}
		delete(payload, "rows")
		if plain {
			delete(payload, "columns")
		}
		return gfmTable(header, rows)
	}
	return listTable(typ, payload)
}

// listTable writes the payload's one list of flat items as a GFM table, the
// way fillFromTable reads it back, and removes it from payload.
func listTable(typ string, payload map[string]interface{}) string {
	properties, _ := payloadSchema(typ)["properties"].(map[string]interface{})
	target := ""
	for key, property := range properties {
		if listOfFlatObjects(property.(map[string]interface{})) != nil {
			if target != "" {
				return ""
			}
			target = key
		}
	}
	items, _ := payload[target].([]interface{})
	if target == "" || len(items) == 0 {
		return ""
	}
	var header []string
	seen := map[string]bool{}
	for _, item := range items {
		fields, _ := item.(map[string]interface{})
		keys := make([]string, 0, len(fields))
		for key, value := range fields {
			if !scalarAttribute(value) {
				return ""
			}
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if !seen[key] {
				seen[key] = true
				header = append(header, key)
			}
		}
	}
	rows := make([][]string, len(items))
	for index, item := range items {
		fields, _ := item.(map[string]interface{})
		rows[index] = make([]string, len(header))
		for column, key := range header {
			if value, ok := fields[key]; ok {
				rows[index][column] = scalarText(value)
			}
		}
	}
	delete(payload, target)
	return gfmTable(header, rows)
}

// chartTable is a chart's series as table columns, when every series has
// the same point labels in the same order.
func chartTable(series []interface{}) ([]string, [][]string, bool) {
	if len(series) == 0 {
		return nil, nil, false
	}
	header := []string{"Label"}
	var labels []string
	columns := make([][]string, 0, len(series))
	for index, entry := range series {
		fields, _ := entry.(map[string]interface{})
		name, _ := fields["name"].(string)
		header = append(header, name)
		points, _ := fields["points"].([]interface{})
		var values []string
		for position, point := range points {
			pointFields, _ := point.(map[string]interface{})
			label, _ := pointFields["label"].(string)
			if index == 0 {
				labels = append(labels, label)
			} else if position >= len(labels) || labels[position] != label {
				return nil, nil, false
			}
			values = append(values, scalarText(pointFields["value"]))
		}
		if len(values) != len(labels) {
			return nil, nil, false
		}
		columns = append(columns, values)
	}
	rows := make([][]string, len(labels))
	for position, label := range labels {
		rows[position] = []string{label}
		for _, column := range columns {
			rows[position] = append(rows[position], column[position])
		}
	}
	return header, rows, true
}

func gfmTable(header []string, rows [][]string) string {
	escape := func(cell string) string { return strings.ReplaceAll(cell, "|", `\|`) }
	line := func(cells []string) string {
		escaped := make([]string, len(cells))
		for index, cell := range cells {
			escaped[index] = escape(cell)
		}
		return "| " + strings.Join(escaped, " | ") + " |"
	}
	delimiter := make([]string, len(header))
	for index := range delimiter {
		delimiter[index] = "---"
	}
	lines := []string{line(header), line(delimiter)}
	for _, row := range rows {
		lines = append(lines, line(row))
	}
	return strings.Join(lines, "\n")
}

func yamlBody(value interface{}) string {
	var b strings.Builder
	encoder := yaml.NewEncoder(&b)
	encoder.SetIndent(2)
	if err := encoder.Encode(value); err != nil {
		return ""
	}
	return strings.TrimRight(b.String(), "\n")
}

// scalarAttributes are the payload fields that go in a block's attributes,
// sorted by key; they are removed from payload.
func scalarAttributes(payload map[string]interface{}) []string {
	keys := make([]string, 0, len(payload))
	for key, value := range payload {
		if scalarAttribute(value) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	attrs := make([]string, len(keys))
	for index, key := range keys {
		attrs[index] = key + "=" + attributeValue(payload[key])
		delete(payload, key)
	}
	return attrs
}

// scalarAttribute reports a value that fits on the block's opening line.
func scalarAttribute(value interface{}) bool {
	switch typed := value.(type) {
	case string:
		return !strings.ContainsAny(typed, "\n")
	case float64, bool:
		return true
	}
	return false
}

func scalarText(value interface{}) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(typed)
	case nil:
		return ""
	}
	return fmt.Sprint(value)
}

var bareAttribute = regexp.MustCompile(`^[A-Za-z0-9_.:/+-]+$`)

func attributeValue(value interface{}) string {
	text := scalarText(value)
	if bareAttribute.MatchString(text) {
		return text
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text) + `"`
}
