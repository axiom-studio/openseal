package chatpath

import "testing"

func TestPathsAreCanonicalAndStayInsideTheChat(t *testing.T) {
	for raw, want := range map[string]string{
		"images/hero.png": "images/hero.png", "/images/hero.png": "images/hero.png", " apps/x/./src/App.tsx ": "apps/x/src/App.tsx",
		"a/b/../c.txt": "a/c.txt", ".github/workflows/ci.yml": ".github/workflows/ci.yml", "notes/Q3 plan ✓.md": "notes/Q3 plan ✓.md",
	} {
		if got, err := Clean(raw); err != nil || got != want {
			t.Errorf("Clean(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "/", ".", "..", "../x", "a/../../x", "a\\b", "a\x00b", "a\nb", string([]byte{0xff})} {
		if got, err := Clean(raw); err == nil {
			t.Errorf("Clean(%q) = %q; want an error", raw, got)
		}
	}
	if got, ok := FromImport("/images/hero.png"); !ok || got != "images/hero.png" {
		t.Errorf("FromImport = %q %v", got, ok)
	}
	for _, specifier := range []string{"react", "./App", "//cdn.example/x.js", "https://esm.sh/react"} {
		if _, ok := FromImport(specifier); ok {
			t.Errorf("FromImport(%q) named a chat file", specifier)
		}
	}
}

func TestKindsOfFiles(t *testing.T) {
	if MediaTypeOf("src/App.tsx") != "text/typescript" || !Textual(MediaTypeOf("src/App.tsx")) || Textual(MediaTypeOf("hero.png")) {
		t.Fatal("media types")
	}
	if Folder("", "image/png", false) != Images || Folder("", "image/png", true) != Uploads || Folder("app", "", false) != Apps {
		t.Fatal("folders")
	}
	if Extension("Report.PDF", "application/pdf") != ".pdf" || Extension("x.weird-long-ext", "") != "" {
		t.Fatal("extensions")
	}
}
