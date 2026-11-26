package presentation

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Visual blocks (contract 2026-11-25): an answer carries its visuals inline
// as generic-directive blocks, one top-level component per block:
//
//	:::chart{#spend kind=bar title="Spend by month"}
//	| Month | Spend |
//	|-------|-------|
//	| Jan   | 320   |
//	:::
//
// Attributes are the envelope (id, title, summary, tone) and the payload's
// top-level fields. The body is nested blocks and markdown for a layout,
// the text for a text type, a leading GFM table for table-shaped data, and
// a YAML mapping of the rest. Blocks are read into one Visual through the
// same lenient path as every other visual (Admit), so a slip in a block is
// read, and a block that still fails costs only itself.

// placeholderType marks where a posted block stood (PlacedAnswer). It is
// not a component type: a block of this type is left out like any unknown
// type.
const placeholderType = "visual"

// Block is one top-level block of an answer, in answer order. Start and
// End are the byte offsets of the whole block, its fence lines included.
// An open block runs to the end of the answer and is not Closed.
type Block struct {
	ID     string
	Type   string
	Start  int
	End    int
	Closed bool
}

// BlockDrop is a part of an answer's visual left out, with the top-level
// block it belongs to. Whole reports that the block itself is gone, not
// only a part of it.
type BlockDrop struct {
	BlockID string `json:"blockId"`
	Whole   bool   `json:"whole,omitempty"`
	Drop
}

var (
	containerOpen = regexp.MustCompile(`^ {0,3}(:{3,})\s*([A-Za-z_][A-Za-z0-9_-]*)\s*(\{.*\})?\s*$`)
	leafLine      = regexp.MustCompile(`^ {0,3}::([A-Za-z_][A-Za-z0-9_-]*)\s*(\{.*\})?\s*$`)
	fenceClose    = regexp.MustCompile(`^ {0,3}(:{3,})\s*$`)
	codeFence     = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
)

// blockNode is one parsed block. items holds its body in order: a string
// per body line, a *blockNode per nested block.
type blockNode struct {
	typ        string
	attrs      map[string]interface{}
	fence      int
	start, end int
	closed     bool
	items      []interface{}
}

// body is the node's body text, nested blocks included as written.
func (n *blockNode) body(answer string) string {
	var lines []string
	for _, item := range n.items {
		switch value := item.(type) {
		case string:
			lines = append(lines, value)
		case *blockNode:
			lines = append(lines, strings.TrimRight(answer[value.start:value.end], "\n"))
		}
	}
	return strings.Join(lines, "\n")
}

// parseBlocks reads the top-level blocks of answer. Lines inside a code
// fence are never read as blocks.
func parseBlocks(answer string) []*blockNode {
	var (
		top   []*blockNode
		stack []*blockNode
		code  string
	)
	attach := func(node *blockNode) {
		if len(stack) == 0 {
			top = append(top, node)
			return
		}
		parent := stack[len(stack)-1]
		parent.items = append(parent.items, node)
	}
	offset := 0
	for offset < len(answer) {
		next := strings.IndexByte(answer[offset:], '\n')
		end := len(answer)
		if next >= 0 {
			end = offset + next + 1
		}
		line := strings.TrimRight(answer[offset:end], "\r\n")
		start := offset
		offset = end
		if code != "" {
			if strings.HasPrefix(strings.TrimSpace(line), code) && strings.Trim(strings.TrimSpace(line), code[:1]) == "" {
				code = ""
			}
			if len(stack) > 0 {
				stack[len(stack)-1].items = append(stack[len(stack)-1].items, line)
			}
			continue
		}
		if match := codeFence.FindStringSubmatch(line); match != nil {
			code = match[1]
			if len(stack) > 0 {
				stack[len(stack)-1].items = append(stack[len(stack)-1].items, line)
			}
			continue
		}
		if match := fenceClose.FindStringSubmatch(line); match != nil && len(stack) > 0 && len(match[1]) >= stack[len(stack)-1].fence {
			node := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			node.closed, node.end = true, end
			attach(node)
			continue
		}
		if match := containerOpen.FindStringSubmatch(line); match != nil {
			stack = append(stack, &blockNode{typ: strings.ToLower(match[2]), attrs: parseAttributes(match[3]), fence: len(match[1]), start: start})
			continue
		}
		if match := leafLine.FindStringSubmatch(line); match != nil {
			attach(&blockNode{typ: strings.ToLower(match[1]), attrs: parseAttributes(match[2]), start: start, end: end, closed: true})
			continue
		}
		if len(stack) > 0 {
			stack[len(stack)-1].items = append(stack[len(stack)-1].items, line)
		}
	}
	// Blocks still open run to the end of the answer; the outermost is the
	// top-level block.
	for len(stack) > 0 {
		node := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node.end = len(answer)
		attach(node)
	}
	return top
}

// parseAttributes reads `{#id .class key=value key="value" flag}`. A key
// with dots (binding.kind=routine) sets a nested field.
func parseAttributes(raw string) map[string]interface{} {
	attrs := map[string]interface{}{}
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 {
		return attrs
	}
	text := []rune(raw[1 : len(raw)-1])
	index := 0
	skipSpace := func() {
		for index < len(text) && (text[index] == ' ' || text[index] == '\t' || text[index] == ',') {
			index++
		}
	}
	readWord := func() string {
		begin := index
		for index < len(text) && !strings.ContainsRune(" \t=,}", text[index]) {
			index++
		}
		return string(text[begin:index])
	}
	readValue := func() interface{} {
		if index < len(text) && (text[index] == '[' || text[index] == '{') {
			if end := bracketEnd(text, index); end > 0 {
				begin := index
				index = end
				return bracketedValue(string(text[begin:end]))
			}
		}
		if index < len(text) && (text[index] == '"' || text[index] == '\'') {
			quote := text[index]
			index++
			var value strings.Builder
			for index < len(text) && text[index] != quote {
				if text[index] == '\\' && index+1 < len(text) {
					index++
				}
				value.WriteRune(text[index])
				index++
			}
			index++ // the closing quote
			return value.String()
		}
		return readWord()
	}
	for skipSpace(); index < len(text); skipSpace() {
		switch text[index] {
		case '#':
			index++
			attrs["id"] = readWord()
			continue
		case '.':
			index++
			readWord() // classes mean nothing here
			continue
		}
		key := readWord()
		if key == "" {
			index++
			continue
		}
		if index < len(text) && text[index] == '=' {
			index++
			setPath(attrs, key, readValue())
			continue
		}
		setPath(attrs, key, true)
	}
	return attrs
}

// bracketEnd is the index just past the bracket group that opens at begin,
// skipping quoted text; -1 when the group never closes.
func bracketEnd(text []rune, begin int) int {
	depth := 0
	var quote rune
	for index := begin; index < len(text); index++ {
		switch r := text[index]; {
		case quote != 0:
			if r == '\\' {
				index++
			} else if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '[' || r == '{':
			depth++
		case r == ']' || r == '}':
			depth--
			if depth == 0 {
				return index + 1
			}
		}
	}
	return -1
}

// bracketedValue reads an attribute value written as a list or an object:
// JSON or YAML flow ([{"url": …}], {kind: agent}), or an object of
// attributes ({kind=agent id=x}). A model writes an object field in the
// header this way as often as in the body; anything else stays text.
func bracketedValue(raw string) interface{} {
	var value interface{}
	if err := yaml.Unmarshal([]byte(raw), &value); err == nil {
		switch typed := plainYAML(value).(type) {
		case []interface{}:
			return typed
		case map[string]interface{}:
			if !hasNullField(typed) {
				return typed
			}
		}
	}
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") && strings.Contains(raw, "=") {
		return parseAttributes(raw)
	}
	return raw
}

// hasNullField reports a key with no value, which is how YAML reads
// {kind=agent id=x}.
func hasNullField(object map[string]interface{}) bool {
	for _, value := range object {
		if value == nil {
			return true
		}
	}
	return false
}

func setPath(object map[string]interface{}, key string, value interface{}) {
	parts := strings.Split(key, ".")
	for _, part := range parts[:len(parts)-1] {
		next, _ := object[part].(map[string]interface{})
		if next == nil {
			next = map[string]interface{}{}
			object[part] = next
		}
		object = next
	}
	object[parts[len(parts)-1]] = value
}

// envelopeKeys are the component fields a block's attributes or body set
// on the component itself rather than on its payload.
var envelopeKeys = map[string]bool{"id": true, "title": true, "summary": true, "tone": true, "binding": true, "sources": true}

// ParseAnswer lists the top-level blocks of answer with the ids ReadBlocks
// gives them.
func ParseAnswer(answer string) []Block {
	nodes := parseBlocks(answer)
	ids := blockIDs(nodes)
	blocks := make([]Block, len(nodes))
	for index, node := range nodes {
		blocks[index] = Block{ID: ids[index], Type: node.typ, Start: node.start, End: node.end, Closed: node.closed}
	}
	return blocks
}

// blockIDs gives each top-level block its id: the one it names, else
// <type>-<n> by the count of its type; a repeated id gets -2, -3….
func blockIDs(nodes []*blockNode) []string {
	ids := make([]string, len(nodes))
	used := map[string]bool{}
	counts := map[string]int{}
	for index, node := range nodes {
		counts[node.typ]++
		id, _ := node.attrs["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			id = node.typ + "-" + strconv.Itoa(counts[node.typ])
		}
		unique := id
		for suffix := 2; used[unique]; suffix++ {
			unique = id + "-" + strconv.Itoa(suffix)
		}
		used[unique] = true
		ids[index] = unique
	}
	return ids
}

// ReadBlocks reads every closed top-level block of answer into one Visual:
// one top-level component per block, in answer order, through the lenient
// path every visual takes (Admit, with the host checks of opts unless
// opts.SkipHostChecks). It returns nil when the answer has no closed block
// or none survives, and the drops by block. Open blocks are not read.
func ReadBlocks(ctx context.Context, answer string, opts ValidateOptions) (*Visual, []BlockDrop) {
	nodes := parseBlocks(answer)
	ids := blockIDs(nodes)
	var (
		components []interface{}
		drops      []BlockDrop
		owner      = map[string]string{} // component id → top-level block id
		expected   []string
		title      string
	)
	for index, node := range nodes {
		if !node.closed {
			continue
		}
		component, err := readBlock(answer, node, ids[index])
		if err != nil {
			drops = append(drops, BlockDrop{BlockID: ids[index], Whole: true, Drop: Drop{ComponentID: ids[index], Reason: err.Error()}})
			continue
		}
		indexIDs(component, ids[index], owner)
		components = append(components, component)
		expected = append(expected, ids[index])
		if title == "" {
			title, _ = component["title"].(string)
		}
	}
	if len(components) == 0 {
		return nil, drops
	}
	if strings.TrimSpace(title) == "" {
		title = "Card"
	}
	raw, err := json.Marshal(map[string]interface{}{"title": title, "components": components})
	if err != nil {
		return nil, append(drops, wholeDrops(expected, err.Error())...)
	}
	document, err := visualDocument(raw)
	if err != nil {
		return nil, append(drops, wholeDrops(expected, err.Error())...)
	}
	visual, admitDrops, _, err := admit(ctx, document, opts)
	kept := map[string]bool{}
	if visual != nil {
		for _, component := range visual.Components {
			kept[component.ID] = true
		}
	}
	for _, drop := range admitDrops {
		blockID := owner[drop.ComponentID]
		if blockID == "" && len(expected) == 1 {
			blockID = expected[0]
		}
		drops = append(drops, BlockDrop{BlockID: blockID, Whole: blockID != "" && !kept[blockID] && drop.ComponentID == blockID, Drop: drop})
	}
	// Every block admit left out is a whole drop, also one that went
	// because a part of it did.
	for _, id := range expected {
		if kept[id] || hasWholeDrop(drops, id) {
			continue
		}
		reason := "it could not be read"
		if err != nil {
			reason = err.Error()
		}
		for _, drop := range drops {
			if drop.BlockID == id && drop.Reason != "" {
				reason = drop.Reason
				break
			}
		}
		drops = append(drops, BlockDrop{BlockID: id, Whole: true, Drop: Drop{ComponentID: id, Reason: reason}})
	}
	if visual == nil || len(visual.Components) == 0 {
		return nil, drops
	}
	return visual, drops
}

func wholeDrops(ids []string, reason string) []BlockDrop {
	drops := make([]BlockDrop, len(ids))
	for index, id := range ids {
		drops[index] = BlockDrop{BlockID: id, Whole: true, Drop: Drop{ComponentID: id, Reason: reason}}
	}
	return drops
}

func hasWholeDrop(drops []BlockDrop, id string) bool {
	for _, drop := range drops {
		if drop.BlockID == id && drop.Whole {
			return true
		}
	}
	return false
}

// indexIDs maps the id of component and of everything nested in it to the
// top-level block it belongs to.
func indexIDs(value interface{}, blockID string, owner map[string]string) {
	switch typed := value.(type) {
	case map[string]interface{}:
		if id, ok := typed["id"].(string); ok && id != "" {
			if _, taken := owner[id]; !taken {
				owner[id] = blockID
			}
		}
		for _, nested := range typed {
			indexIDs(nested, blockID, owner)
		}
	case []interface{}:
		for _, nested := range typed {
			indexIDs(nested, blockID, owner)
		}
	}
}

// readBlock reads one block into its component's JSON.
func readBlock(answer string, node *blockNode, id string) (map[string]interface{}, error) {
	if node.typ == placeholderType {
		return nil, fmt.Errorf("::%s only marks where a visual of an earlier answer stood; write the block itself", placeholderType)
	}
	spec, known := catalogue[node.typ]
	if !known {
		return nil, fmt.Errorf("%q is not a visual type; use one of: %s", node.typ, strings.Join(TypeNames(), ", "))
	}
	component := map[string]interface{}{"type": node.typ}
	payload := map[string]interface{}{}
	for key, value := range node.attrs {
		if envelopeKeys[key] {
			component[key] = value
		} else {
			payload[key] = value
		}
	}
	component["id"] = id
	switch {
	case node.typ == "tabs":
		tabs, err := readTabs(answer, node, id)
		if err != nil {
			return nil, err
		}
		component["tabs"] = tabs
		return component, nil
	case spec.children.max > 0:
		component["children"] = readChildren(answer, node.items, id)
	case spec.payload == "text":
		if text := strings.TrimSpace(node.body(answer)); text != "" {
			component["text"] = text
		}
		return component, nil
	default:
		if err := readBody(node.typ, node.body(answer), component, payload); err != nil {
			return nil, err
		}
	}
	if spec.payload != "" {
		if list, ok := payload[listPayloadKey].([]interface{}); ok {
			component[spec.payload] = list
		} else if len(payload) > 0 || !spec.payloadOptional {
			component[spec.payload] = payload
		}
	}
	return component, nil
}

// listPayloadKey holds a payload that is itself a list (actions) while the
// body is read.
const listPayloadKey = "\x00list"

// readChildren reads a layout's body: nested blocks are its children and
// the markdown between them becomes text children.
func readChildren(answer string, items []interface{}, parentID string) []interface{} {
	var (
		children []interface{}
		prose    []string
		texts    int
		nested   []*blockNode
	)
	for _, item := range items {
		if node, ok := item.(*blockNode); ok {
			nested = append(nested, node)
		}
	}
	ids := blockIDs(nested)
	position := 0
	flush := func() {
		text := strings.TrimSpace(strings.Join(prose, "\n"))
		prose = nil
		if text == "" {
			return
		}
		texts++
		children = append(children, map[string]interface{}{"id": fmt.Sprintf("%s-text-%d", parentID, texts), "type": "text", "text": text})
	}
	for _, item := range items {
		switch value := item.(type) {
		case string:
			prose = append(prose, value)
		case *blockNode:
			flush()
			childID := ids[position]
			position++
			if !strings.HasPrefix(childID, parentID+"-") && value.attrs["id"] == nil {
				childID = parentID + "-" + childID
			}
			child, err := readBlock(answer, value, childID)
			if err != nil {
				// An unreadable child is left out as a component of no
				// known type would be; its siblings stay.
				continue
			}
			children = append(children, child)
		}
	}
	flush()
	return children
}

// readTabs reads a tabs block: one :::tab{label="…"} per tab, its body read
// as a layout's.
func readTabs(answer string, node *blockNode, id string) ([]interface{}, error) {
	var tabs []interface{}
	for _, item := range node.items {
		tab, ok := item.(*blockNode)
		if !ok {
			continue
		}
		label, _ := tab.attrs["label"].(string)
		if label == "" {
			label, _ = tab.attrs["title"].(string)
		}
		tabID := fmt.Sprintf("%s-tab-%d", id, len(tabs)+1)
		tabs = append(tabs, map[string]interface{}{"label": label, "children": readChildren(answer, tab.items, tabID)})
	}
	if len(tabs) == 0 {
		return nil, fmt.Errorf("tabs holds one :::tab{label=\"…\"} block per tab")
	}
	return tabs, nil
}

// readBody reads the body of a payload type: a leading GFM table, then a
// YAML mapping (or list, for a list payload) of the rest.
func readBody(componentType, body string, component, payload map[string]interface{}) error {
	lines := strings.Split(strings.Trim(body, "\n"), "\n")
	// The table is the first run of | lines whose second line is a
	// delimiter row; it may come before or after the YAML fields.
	start, end := len(lines), len(lines)
	for index := 0; index+1 < len(lines); index++ {
		if isTableLine(lines[index]) && isTableLine(lines[index+1]) && strings.Contains(lines[index+1], "-") && strings.Trim(lines[index+1], "|:- \t") == "" {
			start, end = index, index
			for end < len(lines) && isTableLine(lines[end]) {
				end++
			}
			break
		}
	}
	rest := strings.Join(append(append([]string{}, lines[:start]...), lines[end:]...), "\n")
	if strings.TrimSpace(rest) != "" {
		fields, err := readYAML(componentType, rest)
		if err != nil {
			return err
		}
		for key, value := range fields {
			if envelopeKeys[key] {
				component[key] = value
			} else {
				payload[key] = value
			}
		}
	}
	if end-start >= 2 {
		header, rows := readTable(lines[start:end])
		if header == nil {
			return fmt.Errorf("the table in the %s block needs a header row, a |---| row and data rows", componentType)
		}
		if err := fillFromTable(componentType, header, rows, payload); err != nil {
			return err
		}
	}
	return nil
}

func isTableLine(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "|")
}

// readYAML reads a YAML (or JSON) body. A body that nests the payload under
// its own key (chart: {…}) is read as the payload.
func readYAML(componentType, text string) (map[string]interface{}, error) {
	var value interface{}
	if err := yaml.Unmarshal([]byte(text), &value); err != nil {
		return nil, fmt.Errorf("the body of the %s block is not a YAML mapping: %v", componentType, err)
	}
	value = plainYAML(value)
	if list, ok := value.([]interface{}); ok {
		return map[string]interface{}{listPayloadKey: list}, nil
	}
	fields, ok := value.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("the body of the %s block must be a YAML mapping of its fields", componentType)
	}
	if spec := catalogue[componentType]; spec.payload != "" && len(fields) == 1 {
		switch nested := fields[spec.payload].(type) {
		case map[string]interface{}:
			return nested, nil
		case []interface{}:
			return map[string]interface{}{listPayloadKey: nested}, nil
		}
	}
	return fields, nil
}

// plainYAML turns what yaml.v3 decodes into JSON-shaped values.
func plainYAML(value interface{}) interface{} {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, nested := range typed {
			typed[key] = plainYAML(nested)
		}
		return typed
	case map[interface{}]interface{}:
		out := make(map[string]interface{}, len(typed))
		for key, nested := range typed {
			out[fmt.Sprint(key)] = plainYAML(nested)
		}
		return out
	case []interface{}:
		for index, nested := range typed {
			typed[index] = plainYAML(nested)
		}
		return typed
	case time.Time:
		if typed.Hour() == 0 && typed.Minute() == 0 && typed.Second() == 0 {
			return typed.Format("2006-01-02")
		}
		return typed.Format(time.RFC3339)
	default:
		return typed
	}
}

// readTable reads GFM table lines into the header cells and the data rows.
// It returns a nil header when the second line is not a delimiter row.
func readTable(lines []string) ([]string, [][]string) {
	header := tableCells(lines[0])
	delimiter := tableCells(lines[1])
	for _, cell := range delimiter {
		if strings.Trim(cell, ":- ") != "" || !strings.Contains(cell, "-") {
			return nil, nil
		}
	}
	var rows [][]string
	for _, line := range lines[2:] {
		cells := tableCells(line)
		for len(cells) < len(header) {
			cells = append(cells, "")
		}
		rows = append(rows, cells[:len(header)])
	}
	return header, rows
}

func tableCells(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "|")
	if strings.HasSuffix(line, "|") && !strings.HasSuffix(line, `\|`) {
		line = line[:len(line)-1]
	}
	var (
		cells []string
		cell  strings.Builder
	)
	for index := 0; index < len(line); index++ {
		if line[index] == '\\' && index+1 < len(line) && line[index+1] == '|' {
			cell.WriteByte('|')
			index++
			continue
		}
		if line[index] == '|' {
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
			continue
		}
		cell.WriteByte(line[index])
	}
	return append(cells, strings.TrimSpace(cell.String()))
}

// fillFromTable puts a table's data into the payload: a chart's labels and
// series, a table's columns and rows, else the payload's one list of flat
// objects (or the payload itself, when it is that list).
func fillFromTable(componentType string, header []string, rows [][]string, payload map[string]interface{}) error {
	switch componentType {
	case "chart":
		series := make([]interface{}, 0, len(header)-1)
		for column := 1; column < len(header); column++ {
			points := make([]interface{}, 0, len(rows))
			for _, row := range rows {
				points = append(points, map[string]interface{}{"label": row[0], "value": numericCell(row[column])})
			}
			series = append(series, map[string]interface{}{"name": header[column], "points": points})
		}
		payload["series"] = series
		return nil
	case "table":
		columns, _ := payload["columns"].([]interface{})
		keys := make([]string, len(header))
		for index, label := range header {
			keys[index] = slug(label)
			if index < len(columns) {
				if column, ok := columns[index].(map[string]interface{}); ok {
					if key, ok := column["key"].(string); ok && key != "" {
						keys[index] = key
					}
					continue
				}
			}
			columns = append(columns, map[string]interface{}{"key": keys[index], "label": label})
		}
		payload["columns"] = columns
		payload["rows"] = rowObjects(keys, rows)
		return nil
	}
	schema := payloadSchema(componentType)
	if listOfFlatObjects(schema) != nil {
		keys := matchKeys(header, listOfFlatObjects(schema))
		payload[listPayloadKey] = rowObjects(keys, rows)
		return nil
	}
	properties, _ := schema["properties"].(map[string]interface{})
	var target string
	for key, property := range properties {
		if items := listOfFlatObjects(property.(map[string]interface{})); items != nil {
			if target != "" {
				return fmt.Errorf("a %s block cannot take a table; write its fields as YAML", componentType)
			}
			target = key
		}
	}
	if target == "" {
		return fmt.Errorf("a %s block cannot take a table; write its fields as YAML", componentType)
	}
	items := listOfFlatObjects(properties[target].(map[string]interface{}))
	payload[target] = rowObjects(matchKeys(header, items), rows)
	return nil
}

// listOfFlatObjects returns the item properties of a list schema whose items
// are objects of scalars only, else nil.
func listOfFlatObjects(schema map[string]interface{}) map[string]interface{} {
	if schema["type"] != "array" {
		return nil
	}
	items, _ := schema["items"].(map[string]interface{})
	properties, _ := items["properties"].(map[string]interface{})
	if len(properties) == 0 {
		return nil
	}
	for _, property := range properties {
		switch property.(map[string]interface{})["type"] {
		case "string", "number", "integer", "boolean":
		default:
			if _, isEnum := property.(map[string]interface{})["enum"]; !isEnum {
				return nil
			}
		}
	}
	return properties
}

// matchKeys names each column by the item field its header names (any case,
// spaces or dashes for underscores or camelCase), else by its slug.
func matchKeys(header []string, properties map[string]interface{}) []string {
	keys := make([]string, len(header))
	for index, label := range header {
		keys[index] = slug(label)
		for key := range properties {
			if squash(key) == squash(label) {
				keys[index] = key
				break
			}
		}
	}
	return keys
}

func squash(text string) string {
	return strings.Map(func(r rune) rune {
		if r == ' ' || r == '_' || r == '-' {
			return -1
		}
		return r
	}, strings.ToLower(text))
}

func rowObjects(keys []string, rows [][]string) []interface{} {
	objects := make([]interface{}, 0, len(rows))
	for _, row := range rows {
		object := map[string]interface{}{}
		for index, key := range keys {
			if index < len(row) && row[index] != "" {
				object[key] = row[index]
			}
		}
		objects = append(objects, object)
	}
	return objects
}

var slugRun = regexp.MustCompile(`[^a-z0-9]+`)

// slug is a column key from its label: lowercase words joined by _.
func slug(label string) string {
	key := strings.Trim(slugRun.ReplaceAllString(strings.ToLower(label), "_"), "_")
	if key == "" {
		return "column"
	}
	return key
}

// numericCell reads a chart cell as a number where it has one obvious
// reading (a currency sign, a percent sign or thousands separators around
// it); anything else stays text for the schema to refuse.
func numericCell(cell string) interface{} {
	trimmed := strings.TrimSpace(cell)
	trimmed = strings.TrimSuffix(trimmed, "%")
	trimmed = strings.TrimLeft(trimmed, "$€£¥₹")
	if value, ok := parseNumber(trimmed); ok {
		return value
	}
	return cell
}

// PlacedAnswer replaces each top-level block of answer with its placeholder
// `::visual{#id}`, the form a posted message stores. The ids are the ones
// ReadBlocks gives. Blocks in drop are removed with their line.
func PlacedAnswer(answer string, dropped map[string]bool) string {
	blocks := ParseAnswer(answer)
	var b strings.Builder
	last := 0
	for _, block := range blocks {
		b.WriteString(answer[last:block.Start])
		last = block.End
		if dropped[block.ID] || !block.Closed {
			continue
		}
		fmt.Fprintf(&b, "::%s{#%s}\n", placeholderType, block.ID)
	}
	b.WriteString(answer[last:])
	return collapseBlankLines(b.String())
}

// PlainAnswer is answer without its blocks or placeholders, and without
// the stored files it shows (an image goes, a file link reads as its
// label): the text for channels, notifications, copies and search.
func PlainAnswer(answer string) string {
	blocks := ParseAnswer(answer)
	var b strings.Builder
	last := 0
	for _, block := range blocks {
		b.WriteString(answer[last:block.Start])
		last = block.End
	}
	b.WriteString(answer[last:])
	return collapseBlankLines(plainLinks(b.String()))
}

var blankRun = regexp.MustCompile(`\n{3,}`)

func collapseBlankLines(text string) string {
	return strings.TrimSpace(blankRun.ReplaceAllString(text, "\n\n"))
}

// ReplaceBlocks swaps the blocks named in repairs (block id → the full
// corrected block) into answer and returns it with the ids it did not
// find.
func ReplaceBlocks(answer string, repairs map[string]string) (string, []string) {
	blocks := ParseAnswer(answer)
	found := map[string]bool{}
	var b strings.Builder
	last := 0
	for _, block := range blocks {
		repair, ok := repairs[block.ID]
		if !ok {
			continue
		}
		found[block.ID] = true
		b.WriteString(answer[last:block.Start])
		b.WriteString(strings.TrimRight(repair, "\n") + "\n")
		last = block.End
	}
	b.WriteString(answer[last:])
	var missing []string
	for id := range repairs {
		if !found[id] {
			missing = append(missing, id)
		}
	}
	return b.String(), missing
}

// NeedsHost reports a component the host must still check or stamp before
// it can be drawn: a binding, a place, an image of this chat or a link. A
// preview shows it as a placeholder until the answer is posted.
func NeedsHost(component Component) bool {
	if component.Binding != nil {
		return true
	}
	surface := Surface{Schema: Schema, Components: []Component{component}}
	if len(PlaceIDs(surface)) > 0 {
		return true
	}
	encoded, err := json.Marshal(component)
	if err != nil {
		return true
	}
	text := string(encoded)
	return strings.Contains(text, `"artifactId"`) || strings.Contains(text, `"url"`)
}

// PreviewBlocks are the blocks of a streaming answer the client can draw
// before the answer is posted: the closed ones, read without the host,
// except those that still need it (NeedsHost).
func PreviewBlocks(ctx context.Context, answer string) []Component {
	visual, _ := ReadBlocks(ctx, answer, ValidateOptions{SkipHostChecks: true})
	if visual == nil {
		return nil
	}
	var shown []Component
	for _, component := range visual.Components {
		if !NeedsHost(component) {
			shown = append(shown, component)
		}
	}
	return shown
}
