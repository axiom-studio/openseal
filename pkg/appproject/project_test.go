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
