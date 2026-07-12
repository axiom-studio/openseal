package source

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestParseRSSAndAtomDeterministically(t *testing.T) {
	rss := `<rss><channel><title>Operator forum</title><item><guid>thread-7</guid><link>https://forum.example/thread/7</link><title>Restart recovery</title><description><![CDATA[<p>Runs should resume after a pod restart.</p>]]></description><pubDate>Sun, 13 Jul 2026 02:00:00 +0000</pubDate></item></channel></rss>`
	first, err := ParseFeed([]byte(rss), "https://forum.example/feed.xml", 10)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := ParseFeed([]byte(rss), "https://forum.example/feed.xml", 10)
	if first.NextCursor != "thread-7" || !reflect.DeepEqual(first.Observations[0], second.Observations[0]) ||
		first.Observations[0].SourceURI != "https://forum.example/thread/7" ||
		!strings.HasPrefix(first.Observations[0].ContentDigest, "sha256:") || strings.Contains(first.Observations[0].Summary, "<p>") {
		t.Fatalf("RSS normalization mismatch: %#v", first)
	}

	atom := `<feed xmlns="http://www.w3.org/2005/Atom"><title>Release notes</title><entry><id>release-1</id><title>Version 1</title><summary>Durable monitors</summary><updated>2026-07-13T02:01:00Z</updated><link href="/release/1" rel="alternate"/></entry></feed>`
	parsed, err := ParseFeed([]byte(atom), "https://updates.example/feed", 1)
	if err != nil || parsed.Observations[0].SourceURI != "https://updates.example/release/1" || parsed.NextCursor != "release-1" {
		t.Fatalf("Atom normalization mismatch: %#v err=%v", parsed, err)
	}
}

func TestParseFeedFailsClosed(t *testing.T) {
	cases := []struct {
		name, body, url string
		limit           int
	}{
		{"non-https feed", `<rss/>`, "http://example.test/feed", 10},
		{"invalid limit", `<rss/>`, "https://example.test/feed", 0},
		{"private entry scheme", `<rss><channel><item><guid>x</guid><link>file:///etc/passwd</link><title>x</title><pubDate>2026-07-13T02:00:00Z</pubDate></item></channel></rss>`, "https://example.test/feed", 10},
		{"missing time", `<rss><channel><item><guid>x</guid><link>https://example.test/x</link><title>x</title></item></channel></rss>`, "https://example.test/feed", 10},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseFeed([]byte(test.body), test.url, test.limit); err == nil {
				t.Fatal("unsafe or incomplete feed was accepted")
			}
		})
	}
}

func TestSourceSkillContractIsTypedAndReadOnly(t *testing.T) {
	definition := SkillDefinition()
	if err := skill.NewCatalog().Register(context.Background(), definition); err != nil {
		t.Fatalf("source Skill contract is invalid: %v", err)
	}
	action := definition.Actions[ObserveFeed]
	if definition.ID != SkillID || action.Risk != "read" || action.SideEffect != "read" ||
		len(action.Permissions) != 1 || action.Permissions[0] != "network:https:read" || action.OutputSchema == nil {
		t.Fatalf("source Skill contract mismatch: %#v", definition)
	}
	properties := action.OutputSchema["properties"].(map[string]interface{})
	if properties["observationRefs"] == nil || properties["checkpointRevision"] == nil || properties["sourceObservations"] != nil {
		t.Fatalf("source Skill exposed transient rather than canonical output: %#v", action.OutputSchema)
	}
}
