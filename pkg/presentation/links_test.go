package presentation

import (
	"reflect"
	"testing"
)

func TestAnswerLinksReadsLinksAndImagesOutsideCode(t *testing.T) {
	answer := "Here ![a cat](images/cat.png) and [the report](<docs/q3 report.pdf> \"Q3\").\n" +
		"See [site](https://example.com), `[not](code.txt)` and \\[escaped](x.txt).\n" +
		"```\n![fenced](images/no.png)\n```\n" +
		"[nested [brackets]](notes/a(1).md)"
	var got []Link
	for _, link := range AnswerLinks(answer) {
		link.Start, link.End = 0, 0
		got = append(got, link)
	}
	want := []Link{
		{Image: true, Label: "a cat", Target: "images/cat.png"},
		{Label: "the report", Target: "docs/q3 report.pdf"},
		{Label: "site", Target: "https://example.com"},
		{Label: "nested [brackets]", Target: "notes/a(1).md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %#v", got)
	}
	links := AnswerLinks(answer)
	if answer[links[0].Start:links[0].End] != "![a cat](images/cat.png)" || answer[links[1].Start:links[1].End] != `[the report](<docs/q3 report.pdf> "Q3")` {
		t.Fatalf("spans = %q, %q", answer[links[0].Start:links[0].End], answer[links[1].Start:links[1].End])
	}
}

func TestFileTarget(t *testing.T) {
	for target, want := range map[string]bool{
		"images/cat.png": true, "./docs/a.pdf": true, "/docs/a.pdf": true, "file-123": true,
		"https://example.com/a.png": false, "mailto:a@b.c": false, "data:image/png;base64,xx": false,
		"#section": false, "//cdn.example.com/a.png": false, ArtifactContentURL("file-1", 2): false,
	} {
		if FileTarget(target) != want {
			t.Errorf("FileTarget(%q) = %v", target, !want)
		}
	}
}

func TestArtifactContentURLRoundTrips(t *testing.T) {
	link := ArtifactContentURL("file a/b", 3)
	if link != "/orchestrator/agent/kernel/v1/artifacts/file%20a%2Fb/content?version=3" {
		t.Fatalf("url = %q", link)
	}
	if id, version, ok := ArtifactContentRef(link); !ok || id != "file a/b" || version != 3 {
		t.Fatalf("ref = %q %d %v", id, version, ok)
	}
	for _, other := range []string{"images/cat.png", ArtifactContentPath + "x/content", ArtifactContentPath + "x/content?version=0"} {
		if _, _, ok := ArtifactContentRef(other); ok {
			t.Fatalf("%q read as a content URL", other)
		}
	}
}

func TestPlainAnswerDropsShownFiles(t *testing.T) {
	image := LinkTo(Link{Image: true, Label: "a cat"}, ArtifactContentURL("file-1", 1))
	file := LinkTo(Link{Label: "the report"}, ArtifactContentURL("file-2", 4))
	answer := "Here is your cat.\n\n" + image + "\n\nAnd " + file + ", plus [docs](https://example.com)."
	if got := PlainAnswer(answer); got != "Here is your cat.\n\nAnd the report, plus [docs](https://example.com)." {
		t.Fatalf("plain = %q", got)
	}
}
