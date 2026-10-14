package appproject

import (
	"strings"
	"testing"
)

const app = "export default function App() { return <h1>Hello</h1>; }\n"

func starter(t *testing.T) Project {
	t.Helper()
	project, err := New("Market research", []File{{Path: Entry, Content: app}, {Path: "src/styles.css", Content: "h1 { color: teal; }"}})
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func TestNewValidatesPathsAndEntry(t *testing.T) {
	cases := map[string][]File{
		"missing entry":     {{Path: "src/other.tsx", Content: app}},
		"no default export": {{Path: Entry, Content: "export const App = 1"}},
		"parent path":       {{Path: Entry, Content: app}, {Path: "../secret.ts", Content: ""}},
		"absolute path":     {{Path: Entry, Content: app}, {Path: "/etc/passwd.ts", Content: ""}},
		"hidden folder":     {{Path: Entry, Content: app}, {Path: "src/.env.ts", Content: ""}},
		"node_modules":      {{Path: Entry, Content: app}, {Path: "node_modules/x/index.ts", Content: ""}},
		"javascript":        {{Path: Entry, Content: app}, {Path: "src/util.js", Content: ""}},
		"non canonical":     {{Path: Entry, Content: app}, {Path: "src//util.ts", Content: ""}},
		"duplicate":         {{Path: Entry, Content: app}, {Path: Entry, Content: app}},
		"bad json":          {{Path: Entry, Content: app}, {Path: "data.json", Content: "{"}},
		"binary":            {{Path: Entry, Content: app}, {Path: "src/a.ts", Content: "\x00"}},
		"too large":         {{Path: Entry, Content: app}, {Path: "src/big.ts", Content: strings.Repeat("a", MaximumFileBytes+1)}},
	}
	for name, files := range cases {
		if _, err := New("App", files); !IsValidationError(err) {
			t.Errorf("%s: expected a validation error, got %v", name, err)
		}
	}
	if _, err := New("", []File{{Path: Entry, Content: app}}); !IsValidationError(err) {
		t.Error("empty title should be rejected")
	}
}

func TestApplyWritesReplacesAndDeletesWithoutMutatingTheOriginal(t *testing.T) {
	project := starter(t)
	next, err := project.Apply("", Edits{
		Write:   []File{{Path: "src/chart.tsx", Content: "export const Chart = () => null;"}},
		Replace: []Replacement{{Path: "src/styles.css", Find: "teal", Replace: "navy"}},
		Delete:  []string{"src/chart.tsx"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next.Files["src/styles.css"] != "h1 { color: navy; }" || len(next.Files) != 2 || next.Title != "Market research" {
		t.Fatalf("unexpected project: %#v", next)
	}
	if project.Files["src/styles.css"] != "h1 { color: teal; }" {
		t.Fatal("Apply modified the original project")
	}
}

func TestApplyRejectsAmbiguousOrMissingReplacementsAtomically(t *testing.T) {
	project := starter(t)
	project.Files["src/styles.css"] = "a { color: teal; } b { color: teal; }"
	_, err := project.Apply("", Edits{
		Write:   []File{{Path: "src/new.ts", Content: "export {}"}},
		Replace: []Replacement{{Path: "src/styles.css", Find: "teal", Replace: "navy"}, {Path: "src/none.ts", Find: "x", Replace: "y"}},
	})
	if !IsValidationError(err) || !strings.Contains(err.Error(), "occurs 2 times") || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected both reasons, got %v", err)
	}
	if _, ok := project.Files["src/new.ts"]; ok {
		t.Fatal("a failed edit must not apply any change")
	}
}

func TestApplyCannotDeleteTheEntry(t *testing.T) {
	if _, err := starter(t).Apply("", Edits{Delete: []string{Entry}}); !IsValidationError(err) {
		t.Fatal("deleting the entry module should be rejected")
	}
	if _, err := starter(t).Apply("", Edits{}); !IsValidationError(err) {
		t.Fatal("an empty edit should be rejected")
	}
}

func TestMarshalAndParseRoundTrip(t *testing.T) {
	data, err := starter(t).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(data)
	if err != nil || parsed.Files[Entry] != app || parsed.Title != "Market research" {
		t.Fatalf("round trip failed: %#v %v", parsed, err)
	}
	if _, err := Parse([]byte(`{"schema":"openseal.dev/app/v1","title":"x","files":{},"extra":1}`)); err == nil {
		t.Fatal("unknown fields must be rejected")
	}
	if paths := starter(t).Paths(); strings.Join(paths, ",") != "src/App.tsx,src/styles.css" {
		t.Fatalf("paths not sorted: %v", paths)
	}
}

func TestReplacementFeedbackListsTheLineOfEachMatch(t *testing.T) {
	project := starter(t)
	project.Files["src/styles.css"] = "a { color: teal; }\nb { color: red; }\nc { color: teal; }\n"
	_, err := project.Apply("", Edits{Replace: []Replacement{{Path: "src/styles.css", Find: "teal", Replace: "navy"}}})
	if !IsValidationError(err) || !strings.HasPrefix(err.Error(), "app not saved: find text occurs 2 times in src/styles.css") || !strings.Contains(err.Error(), "lines 1, 3") || !strings.Contains(err.Error(), "longer find") {
		t.Fatalf("expected match lines, got %v", err)
	}
}

func TestReplacementFeedbackShowsNearestContextForAMissedFind(t *testing.T) {
	project := starter(t)
	var file strings.Builder
	for i := 1; i <= 40; i++ {
		file.WriteString("const value_" + string(rune('a'+i%26)) + " = " + strings.Repeat("1", i%5+1) + ";\n")
	}
	file.WriteString("function render() {\n  return <div className=\"card\">hi</div>;\n}\n")
	project.Files["src/view.tsx"] = file.String()
	_, err := project.Apply("", Edits{Replace: []Replacement{{Path: "src/view.tsx", Find: "\n  function render() {\n    return <div className='card'>hi</div>;", Replace: "x"}}})
	message := err.Error()
	if !IsValidationError(err) || !strings.Contains(message, "find text occurs 0 times in src/view.tsx") || !strings.Contains(message, "line 41") || !strings.Contains(message, "41: function render() {") || !strings.Contains(message, "31: ") || strings.Contains(message, "30: ") || !strings.Contains(message, "exactly") {
		t.Fatalf("expected numbered context around line 41, got %v", err)
	}
	if strings.Contains(message, "51: ") || strings.Contains(message, "\n52: ") {
		t.Fatalf("context should stop at the end of the file: %v", err)
	}

	project.Files["src/big.ts"] = strings.Repeat(strings.Repeat("y", 300)+"\n", 30) + "anchor line\n" + strings.Repeat(strings.Repeat("z", 300)+"\n", 30)
	_, err = project.Apply("", Edits{Replace: []Replacement{{Path: "src/big.ts", Find: "anchor line\nmissing", Replace: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "31: anchor line") || len(err.Error()) > 2048 {
		t.Fatalf("context should keep the anchor within 1.5 KB, got %d bytes: %v", len(err.Error()), err)
	}

	_, err = project.Apply("", Edits{Replace: []Replacement{{Path: "src/styles.css", Find: "  \nnowhere to be found", Replace: "x"}}})
	if err == nil || !strings.Contains(err.Error(), "read_app") || !strings.Contains(err.Error(), "occurs 0 times") {
		t.Fatalf("expected a read_app suggestion, got %v", err)
	}
}
