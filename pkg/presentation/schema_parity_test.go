package presentation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// strictPayloads is every payload key's strict schema, as one map keyed by
// payload, with text at its widest.
func strictPayloads() map[string]interface{} {
	properties := map[string]interface{}{"text": textPayloadSchema()}
	for key, schema := range payloadSchemas() {
		properties[key] = schema
	}
	return properties
}

// paritySeeds are valid components, at least one per type, that fill in as
// many optional fields as their type allows together. The parity test
// mutates each one field at a time.
var paritySeeds = []string{
	`{"id":"a","type":"text","title":"T","tone":"info","summary":"s","text":"Hello"}`,
	`{"id":"a","type":"caption","text":"Source: ONS"}`,
	`{"id":"a","type":"divider","text":"Or"}`,
	`{"id":"a","type":"section","title":"T","children":[{"id":"c1","type":"text","text":"x"}]}`,
	`{"id":"a","type":"grid","grid":{"columns":2},"children":[{"id":"c1","type":"text","text":"x"},{"id":"c2","type":"text","text":"y"}]}`,
	`{"id":"a","type":"tabs","tabs":[{"label":"A","children":[{"id":"c1","type":"text","text":"x"}]},{"label":"B","children":[{"id":"c2","type":"text","text":"y"}]}]}`,
	`{"id":"a","type":"carousel","children":[{"id":"c1","type":"text","text":"x"},{"id":"c2","type":"text","text":"y"}]}`,
	`{"id":"a","type":"chart","summary":"s","chart":{"kind":"bar","xLabel":"x","yLabel":"y","format":"currency","currency":"GBP","unit":"u",
		"series":[{"name":"S","points":[{"label":"a","value":1},{"label":"b","value":2}]}],"annotations":[{"x":"a","label":"L"}],
		"referenceLine":{"value":1,"label":"avg"},"highlight":{"series":"S","x":"a"}}}`,
	`{"id":"a","type":"diagram","summary":"s","diagram":{"layout":"flow","direction":"right",
		"nodes":[{"id":"n","label":"N","icon":"house","group":"g","detail":"d"},{"id":"m","label":"M"}],
		"edges":[{"from":"n","to":"m","label":"to"}],"groups":[{"id":"g","label":"G"}]}}`,
	`{"id":"a","type":"form","summary":"s","form":{"description":"d","submitLabel":"Go","fields":[
		{"id":"f1","type":"select","label":"L","description":"d","required":true,"placeholder":"p","options":[{"label":"A","value":"a"}]},
		{"id":"f2","type":"number","label":"N","min":0,"max":10}]}}`,
	`{"id":"a","type":"table","summary":"s","table":{"columns":[{"key":"name","label":"Name","kind":"text"},{"key":"price","label":"Price","kind":"currency","currency":"GBP"}],
		"rows":[{"name":"x","price":3}]}}`,
	`{"id":"a","type":"stat_row","summary":"s","stat_row":{"stats":[{"label":"L","value":1,"format":"currency","currency":"GBP","unit":"u","delta":"+1",
		"trend":"up","goodDirection":"up","sparkline":[1,2]}]}}`,
	`{"id":"a","type":"actions","actions":[{"id":"b1","label":"Yes","kind":"respond","icon":"check","value":"yes"}]}`,
	`{"id":"a","type":"callout","summary":"s","tone":"warning","callout":{"icon":"lightbulb","body":"b"}}`,
	`{"id":"a","type":"steps","summary":"s","steps":{"items":[{"id":"x","title":"T","detail":"d","icon":"house","image":{"artifactId":"img","version":1}},{"id":"y","title":"U"}],"checkable":true}}`,
	`{"id":"a","type":"comparison","summary":"s","comparison":{"attributes":[{"key":"data","label":"Data"}],"options":[
		{"id":"x","name":"X","image":{"artifactId":"img","version":1},"badge":"B","price":{"value":1,"format":"currency","currency":"GBP","unit":"u","per":"month"},
		 "values":{"data":"1"},"pros":["p"],"cons":["c"]},
		{"id":"y","name":"Y","values":{"data":""}}],"recommended":"x","pickable":true}}`,
	`{"id":"a","type":"timeline","summary":"s","timeline":{"events":[{"id":"x","when":"1789","date":"1789-07-14","title":"T","detail":"d","icon":"house",
		"image":{"artifactId":"img","version":1}},{"id":"y","when":"1790","title":"U"}]}}`,
	`{"id":"a","type":"image","summary":"s","image":{"image":{"artifactId":"img","version":1},"caption":"c","aspect":"square"}}`,
	`{"id":"a","type":"gallery","summary":"s","gallery":{"images":[{"image":{"artifactId":"img","version":1},"caption":"c"},{"image":{"artifactId":"img2","version":1}}],"layout":"grid"}}`,
	`{"id":"a","type":"choices","summary":"s","choices":{"chips":[{"id":"x","label":"X"},{"id":"y","label":"Y"}]}}`,
	`{"id":"a","type":"checklist","summary":"s","checklist":{"items":[{"id":"x","label":"X","group":"g"}]}}`,
	`{"id":"a","type":"quiz","summary":"s","quiz":{"question":"Q","options":[{"id":"x","label":"X","image":{"artifactId":"img","version":1}},{"id":"y","label":"Y"}],
		"allowMultiple":true,"answerKey":{"correct":["x"],"explanation":"e"}}}`,
	`{"id":"a","type":"labeled_image","summary":"s","labeled_image":{"image":{"artifactId":"img","version":1},"markers":[{"id":"x","x":0.5,"y":0.5,"label":"L","detail":"d"}]}}`,
	`{"id":"a","type":"quantity_visual","summary":"s","quantity_visual":{"kind":"number_line","min":0,"max":10,"ticks":1,"points":[{"value":1,"label":"p"}],
		"ranges":[{"from":1,"to":2,"label":"r"}],"steps":[{"label":"s","min":0,"max":10,"ticks":1,"points":[{"value":1,"label":"p"}],"ranges":[{"from":1,"to":2,"label":"r"}]}]}}`,
	`{"id":"a","type":"quantity_visual","summary":"s","quantity_visual":{"kind":"fraction_bars","fractions":[{"numerator":1,"denominator":24,"label":"l"}],
		"steps":[{"label":"s","fractions":[{"numerator":1,"denominator":24,"label":"l"}]}]}}`,
	`{"id":"a","type":"quantity_visual","summary":"s","quantity_visual":{"kind":"unit_blocks","groups":[{"count":3,"label":"l"}],"steps":[{"label":"s","groups":[{"count":3,"label":"l"}]}]}}`,
	`{"id":"a","type":"quantity_visual","summary":"s","quantity_visual":{"kind":"percent_waffle","parts":[{"value":30,"label":"l"}],"steps":[{"label":"s","parts":[{"value":30,"label":"l"}]}]}}`,
	`{"id":"a","type":"quantity_visual","summary":"s","quantity_visual":{"kind":"scale_compare","items":[{"label":"l","value":{"value":1,"format":"currency","currency":"GBP","unit":"u"}},{"label":"m","value":{"value":2}}],
		"steps":[{"label":"s","items":[{"label":"l","value":{"value":1}},{"label":"m","value":{"value":2}}]}]}}`,
	`{"id":"a","type":"math","summary":"s","math":{"steps":[{"latex":"x=1","note":"n"}],"reveal":"stepwise"}}`,
	`{"id":"a","type":"definition","summary":"s","definition":{"term":"T","phonetic":"p","partOfSpeech":"noun","meaning":"m","example":"e","image":{"artifactId":"img","version":1}}}`,
	`{"id":"a","type":"flashcards","summary":"s","flashcards":{"cards":[{"id":"x","front":"F","back":"B","image":{"artifactId":"img","version":1}},{"id":"y","front":"G","back":"C"}],"shuffle":true}}`,
	`{"id":"a","type":"before_after","summary":"s","before_after":{"mode":"diff","before":{"text":"a"},"after":{"text":"b"},"labels":{"before":"Old","after":"New"}}}`,
	`{"id":"a","type":"before_after","summary":"s","before_after":{"mode":"slider","before":{"image":{"artifactId":"img","version":1}},"after":{"image":{"artifactId":"img2","version":1}}}}`,
	`{"id":"a","type":"draft","summary":"s","binding":{"kind":"approval","id":"ap1"},"draft":{"channel":"email","to":"Sam","subject":"Hi","body":"Hello"}}`,
	`{"id":"a","type":"recipe","summary":"s","recipe":{"servings":4,"time":{"prep":10,"cook":20},"ingredients":[{"id":"x","qty":1,"unit":"g","name":"N","group":"g"}],
		"steps":[{"id":"s1","text":"t"}],"image":{"artifactId":"img","version":1},"checkable":true}}`,
	`{"id":"a","type":"item","summary":"s","item":{"name":"N","image":{"artifactId":"img","version":1},"price":{"value":1,"format":"currency","currency":"GBP","unit":"u","per":"month"},
		"rating":{"value":4,"count":10},"facts":["f"],"badge":"b"}}`,
	`{"id":"a","type":"analogy","summary":"s","analogy":{"from":{"label":"F","icon":"house"},"to":{"label":"T","icon":"car"},"mapping":[{"a":"x","b":"y"}]}}`,
	`{"id":"a","type":"agenda","summary":"s","agenda":{"timezone":"Europe/London","days":[{"date":"2026-11-02","label":"Mon",
		"items":[{"id":"x","start":"09:00","end":"10:00","title":"T","place":"p","tone":"info"}]}],"freeSlots":[{"date":"2026-11-02","start":"11:00","end":"12:00"}]}}`,
	`{"id":"a","type":"itinerary","summary":"s","itinerary":{"days":[{"id":"d1","label":"Day 1","date":"2026-11-02","items":[
		{"id":"x","time":"09:00","title":"T","place":"p","detail":"d","image":{"artifactId":"img","version":1},"location":"place:4b2f9c0e1a77"},
		{"id":"y","title":"U","travel":{"mode":"walk","minutes":10}}]}]}}`,
	`{"id":"a","type":"place","summary":"s","place":{"name":"N","category":"c","image":{"artifactId":"img","version":1},"address":"a","rating":{"value":4,"count":3},
		"priceLevel":2,"hours":["Mon 9-5"],"facts":["f"],"location":"place:4b2f9c0e1a77"}}`,
	`{"id":"a","type":"breakdown","summary":"s","breakdown":{"kind":"treemap","format":"currency","currency":"GBP","unit":"u","items":[{"id":"x","label":"X","value":1,"icon":"house"},{"id":"y","label":"Y","value":2}]}}`,
	`{"id":"a","type":"progress","summary":"s","progress":{"kind":"bar","items":[{"id":"x","label":"X","value":1,"target":2,"format":"currency","currency":"GBP","unit":"u"}]}}`,
	`{"id":"a","type":"poll","summary":"s","poll":{"question":"Q","options":[{"id":"x","label":"X","image":{"artifactId":"img","version":1}},{"id":"y","label":"Y"}],
		"multiple":true,"closesAt":"2026-11-01T18:00:00Z"}}`,
	`{"id":"a","type":"explorer","summary":"s","explorer":{"inputs":[{"id":"rate","label":"Rate","kind":"slider","min":0,"max":10,"step":1,"default":5,"unit":"%","format":"percent"}],
		"outputs":[{"id":"out","label":"Out","expr":"rate*2","format":"currency","currency":"GBP","unit":"u","digits":2}],
		"chart":{"x":"t","t":{"from":0,"to":10,"label":"years"},"points":11,"series":[{"label":"S","expr":"rate*t"}]}}}`,
	`{"id":"a","type":"routine","summary":"s","binding":{"kind":"runbook","id":"rb1"},"routine":{"items":[{"label":"L","text":"T"}]}}`,
	`{"id":"a","type":"tasks","summary":"s","tasks":{"rows":[{"binding":{"kind":"conversation_task","id":"t1"},"label":"L"}]}}`,
	`{"id":"a","type":"agent","summary":"s","binding":{"kind":"agent","id":"ag1"},"agent":{"headline":"H","points":["p"]}}`,
	`{"id":"a","type":"map","summary":"s","map":{"pins":[{"id":"x","place":"place:4b2f9c0e1a77","label":"L","detail":"d"}],"connect":true}}`,
}

// parityExceptions are probes whose outcome depends on other fields, so a
// single-field change cannot tell schema and validator apart. Each matches
// "type:field:probe" keys and says why; the test fails if one stops
// applying.
var parityExceptions = []struct {
	pattern *regexp.Regexp
	reason  string
}{
	{regexp.MustCompile(`:[A-Za-z_.\[\]0-9]*\.(currency|format):left out$`),
		"currency goes with format currency and nothing else; both are described on Currency and Format"},
	{regexp.MustCompile(`:[A-Za-z_.\[\]0-9]*(value|from|x|y|numerator|count):missing$`),
		"a missing JSON number decodes as 0, which is in range; the schema is stricter, never looser"},
	{regexp.MustCompile(`^place:place.priceLevel:0$`), "0 is how an unset price level decodes"},
	{regexp.MustCompile(`^place:place.name:empty$`), "a place with a location takes its name from find_place"},
	{regexp.MustCompile(`^quantity_visual:quantity_visual(\.steps\[0\])?\.(fractions|groups|parts|items|min|max):left out$`),
		"each quantity kind needs its own fields; the kind's description names them"},
	{regexp.MustCompile(`^before_after:before_after\.(before|after)\.(text|image):(left out|empty|4001 chars)$`),
		"each side is text or an image, not both; Side's description says so"},
	{regexp.MustCompile(`^explorer:explorer\.(inputs\[0\]\.(min|max|step)|chart\.t):left out$`),
		"a slider needs min, max and step, and a chart over t needs t; the descriptions say so"},
	{regexp.MustCompile(`^form:form\.fields\[0\]\.options:left out$`), "select and radio fields need options; other fields take none"},
	{regexp.MustCompile(`^diagram:diagram\.groups:left out$`), "a node's group must name one of groups"},
	{regexp.MustCompile(`^chart:chart\.(annotations\[0\]\.x|highlight\.(series|x)):\d+ chars$`),
		"these name a point label or series name, whose own limits are the same"},
	{regexp.MustCompile(`^agenda:agenda\.timezone:65 chars$`), "no IANA zone name is that long, so the zone check fires first"},
}

// TestSchemaMatchesValidator checks that the schema the model is given and
// Validate agree on every limit, required field, enum and closed object:
// for each type it changes one field of a valid component at a time, to
// the schema's limit and just past it, and asserts Validate accepts and
// rejects exactly as the schema says.
func TestSchemaMatchesValidator(t *testing.T) {
	covered := map[string]bool{}
	used := make([]bool, len(parityExceptions))
	for _, raw := range paritySeeds {
		var seed map[string]interface{}
		if err := json.Unmarshal([]byte(raw), &seed); err != nil {
			t.Fatalf("seed %s: %v", raw, err)
		}
		name := seed["type"].(string)
		covered[name] = true
		if err := validateComponentJSON(seed); err != nil {
			t.Fatalf("seed %s is not valid: %v", name, err)
		}
		probe := &parityProbe{t: t, name: name, seed: seed, used: used}
		probe.visit(componentTypeSchema(name, componentRef()), nil, "")
	}
	for _, name := range TypeNames() {
		if !covered[name] {
			t.Errorf("no parity seed for %s", name)
		}
	}
	for index, exception := range parityExceptions {
		if !used[index] {
			t.Errorf("parity exception %s no longer applies; remove it", exception.pattern)
		}
	}
}

func validateComponentJSON(component interface{}) error {
	raw, err := json.Marshal(map[string]interface{}{"schema": Schema, "title": "T", "state": "ready", "components": []interface{}{component}})
	if err != nil {
		return err
	}
	_, err = Decode(raw)
	return err
}

type parityProbe struct {
	t    *testing.T
	name string
	seed map[string]interface{}
	used []bool
}

// step is one key or index on the way from the component to a field.
type step interface{}

// check applies change to a copy of the seed and validates it. wantOK
// says whether the schema admits the result; marker, when set, is text a
// rejection must contain to count as the expected one (and an acceptance
// must not contain, for errors caused by other fields).
func (p *parityProbe) check(field, probe string, change func(root interface{}), wantOK bool, marker string) {
	p.t.Helper()
	root := deepCopy(p.seed)
	change(root)
	err := validateComponentJSON(root)
	key := p.name + ":" + field + ":" + probe
	var agree bool
	if wantOK {
		agree = err == nil || (marker != "" && !strings.Contains(err.Error(), marker))
	} else {
		agree = err != nil && (marker == "" || strings.Contains(err.Error(), marker))
	}
	if agree {
		return
	}
	for index, exception := range parityExceptions {
		if exception.pattern.MatchString(key) {
			p.used[index] = true
			return
		}
	}
	if wantOK {
		p.t.Errorf("%s: schema allows it but Validate rejects: %v", key, err)
	} else {
		p.t.Errorf("%s: schema rejects it but Validate %s", key, describeOutcome(err))
	}
}

func describeOutcome(err error) string {
	if err == nil {
		return "accepts it"
	}
	return "rejects it for another reason: " + err.Error()
}

func (p *parityProbe) visit(schema map[string]interface{}, path []step, field string) {
	if _, ref := schema["$ref"]; ref {
		return
	}
	if members, ok := schema["anyOf"].([]interface{}); ok {
		value := get(p.seed, path)
		for _, member := range members {
			if jsonType(value) == member.(map[string]interface{})["type"] {
				p.visit(member.(map[string]interface{}), path, field)
			}
		}
		return
	}
	switch schema["type"] {
	case "object":
		p.object(schema, path, field)
	case "array":
		p.array(schema, path, field)
	case "string":
		p.str(schema, path, field)
	case "integer", "number":
		p.number(schema, path, field)
	}
}

func (p *parityProbe) object(schema map[string]interface{}, path []step, field string) {
	node, ok := get(p.seed, path).(map[string]interface{})
	if !ok {
		return
	}
	properties, _ := schema["properties"].(map[string]interface{})
	required := map[string]bool{}
	list, _ := schema["required"].([]interface{})
	for _, key := range list {
		required[key.(string)] = true
	}
	keys := make([]string, 0, len(properties))
	for key := range properties {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		at := join(field, key)
		child := append(append([]step{}, path...), key)
		_, present := node[key]
		if required[key] {
			p.check(at, "missing", func(root interface{}) { remove(root, child) }, false, "")
		} else if present {
			p.check(at, "left out", func(root interface{}) { remove(root, child) }, true, "")
		}
		if present {
			p.visit(properties[key].(map[string]interface{}), child, at)
			continue
		}
		p.absent(properties[key].(map[string]interface{}), child, at)
	}
	if additional, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		entries := make([]string, 0, len(node))
		for key := range node {
			entries = append(entries, key)
		}
		sort.Strings(entries)
		for _, key := range entries {
			p.visit(additional, append(append([]step{}, path...), key), join(field, key))
		}
		return
	}
	if schema["additionalProperties"] == false {
		p.check(join(field, "unknown_key"), "added", func(root interface{}) {
			set(root, append(append([]step{}, path...), "unknown_key"), "x")
		}, false, `unknown field "unknown_key"`)
	}
}

// absent probes an optional field the seed leaves out: the struct must
// know the key, and a plain string must take the schema's lengths.
func (p *parityProbe) absent(schema map[string]interface{}, path []step, field string) {
	p.check(field, "known key", func(root interface{}) { set(root, path, sampleValue(schema)) }, true, `unknown field "`+lastKey(path)+`"`)
	if schema["type"] == "string" && schema["pattern"] == nil && schema["enum"] == nil {
		set(p.seed, path, "x")
		defer remove(p.seed, path)
		p.str(schema, path, field)
	}
}

func (p *parityProbe) array(schema map[string]interface{}, path []step, field string) {
	list, ok := get(p.seed, path).([]interface{})
	if !ok || len(list) == 0 {
		return
	}
	resize := func(count int) func(root interface{}) {
		return func(root interface{}) {
			items := get(root, path).([]interface{})
			out := make([]interface{}, 0, count)
			for index := 0; index < count; index++ {
				if index < len(items) {
					out = append(out, items[index])
					continue
				}
				out = append(out, renameIDs(deepCopy(items[0]), index))
			}
			set(root, path, out)
		}
	}
	if maximum, ok := intKeyword(schema, "maxItems"); ok {
		p.check(field, fmt.Sprintf("%d items", maximum), resize(maximum), true, fmt.Sprintf("(got %d)", maximum))
		p.check(field, fmt.Sprintf("%d items", maximum+1), resize(maximum+1), false, fmt.Sprintf("(got %d)", maximum+1))
	}
	if minimum, ok := intKeyword(schema, "minItems"); ok && minimum > 0 && len(list) >= minimum {
		p.check(field, fmt.Sprintf("%d items", minimum-1), resize(minimum-1), false, "")
	}
	items := schema["items"].(map[string]interface{})
	for index := range list {
		p.visit(items, append(append([]step{}, path...), index), fmt.Sprintf("%s[%d]", field, index))
	}
}

func (p *parityProbe) str(schema map[string]interface{}, path []step, field string) {
	if values, ok := schema["enum"].([]interface{}); ok {
		p.check(field, "not in enum", func(root interface{}) { set(root, path, "not-a-value") }, false, "")
		for _, value := range values {
			if get(p.seed, path) == value {
				return
			}
		}
		p.t.Errorf("%s:%s seed value is not in the enum", p.name, field)
		return
	}
	if schema["pattern"] != nil {
		return
	}
	sized := func(length int) func(root interface{}) {
		return func(root interface{}) { set(root, path, strings.Repeat("x", length)) }
	}
	maximum, bounded := intKeyword(schema, "maxLength")
	if !bounded {
		// An unbounded string must not meet a hidden length limit.
		p.check(field, "long", sized(5000), true, "characters (got 5000)")
		return
	}
	p.check(field, fmt.Sprintf("%d chars", maximum), sized(maximum), true, fmt.Sprintf("characters (got %d)", maximum))
	p.check(field, fmt.Sprintf("%d chars", maximum+1), sized(maximum+1), false, fmt.Sprintf("characters (got %d)", maximum+1))
	minimum, _ := intKeyword(schema, "minLength")
	if minimum > 0 {
		p.check(field, "empty", sized(0), false, "")
	} else {
		p.check(field, "empty", sized(0), true, field)
	}
}

func (p *parityProbe) number(schema map[string]interface{}, path []step, field string) {
	value := func(number float64) func(root interface{}) {
		return func(root interface{}) { set(root, path, number) }
	}
	below := 1.0
	if schema["type"] == "number" {
		below = 0.5
	}
	if minimum, ok := floatKeyword(schema, "minimum"); ok {
		p.check(field, fmt.Sprintf("%g", minimum), value(minimum), true, field)
		p.check(field, fmt.Sprintf("%g", minimum-below), value(minimum-below), false, field)
	}
	if minimum, ok := floatKeyword(schema, "exclusiveMinimum"); ok {
		p.check(field, fmt.Sprintf("%g", minimum), value(minimum), false, field)
	}
	if maximum, ok := floatKeyword(schema, "maximum"); ok {
		p.check(field, fmt.Sprintf("%g", maximum), value(maximum), true, field)
		p.check(field, fmt.Sprintf("%g", maximum+below), value(maximum+below), false, field)
	}
}

func sampleValue(schema map[string]interface{}) interface{} {
	switch schema["type"] {
	case "object":
		return map[string]interface{}{}
	case "array":
		return []interface{}{}
	case "integer", "number":
		return 1.0
	case "boolean":
		return true
	}
	return "x"
}

// renameIDs makes a copied list item distinct from the original: every
// id, key and label gets a suffix that every id pattern admits.
func renameIDs(value interface{}, index int) interface{} {
	switch node := value.(type) {
	case map[string]interface{}:
		for key, child := range node {
			if text, ok := child.(string); ok && (key == "id" || key == "key" || key == "label" || key == "name") {
				node[key] = fmt.Sprintf("%s_%d", text, index)
				continue
			}
			node[key] = renameIDs(child, index)
		}
	case []interface{}:
		for position, child := range node {
			node[position] = renameIDs(child, index)
		}
	case string:
		return fmt.Sprintf("%s_%d", node, index)
	}
	return value
}

func join(field, key string) string {
	if field == "" {
		return key
	}
	return field + "." + key
}

func lastKey(path []step) string {
	return path[len(path)-1].(string)
}

func intKeyword(schema map[string]interface{}, keyword string) (int, bool) {
	value, ok := schema[keyword].(int)
	return value, ok
}

func floatKeyword(schema map[string]interface{}, keyword string) (float64, bool) {
	switch value := schema[keyword].(type) {
	case int:
		return float64(value), true
	case float64:
		return value, true
	}
	return 0, false
}

func jsonType(value interface{}) string {
	switch value.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case map[string]interface{}:
		return "object"
	}
	return ""
}

func deepCopy(value interface{}) interface{} {
	raw, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	var out interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(err)
	}
	return out
}

func get(root interface{}, path []step) interface{} {
	node := root
	for _, part := range path {
		switch key := part.(type) {
		case string:
			object, ok := node.(map[string]interface{})
			if !ok {
				return nil
			}
			node = object[key]
		case int:
			list, ok := node.([]interface{})
			if !ok || key >= len(list) {
				return nil
			}
			node = list[key]
		}
	}
	return node
}

func set(root interface{}, path []step, value interface{}) {
	parent := get(root, path[:len(path)-1])
	switch key := path[len(path)-1].(type) {
	case string:
		parent.(map[string]interface{})[key] = value
	case int:
		parent.([]interface{})[key] = value
	}
}

func remove(root interface{}, path []step) {
	if object, ok := get(root, path[:len(path)-1]).(map[string]interface{}); ok {
		delete(object, path[len(path)-1].(string))
	}
}
