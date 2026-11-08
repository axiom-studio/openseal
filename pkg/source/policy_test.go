package source

import (
	"net/http"
	"testing"
)

func TestSourcePolicyAuthorizesExactBoundedSource(t *testing.T) {
	policy := Policy{ID: "public-reddit-research", Version: "2026-07-13", Enabled: true, MaximumItems: 10, RetentionDays: 30, Sources: []PolicySource{{Host: "www.reddit.com", PathPrefixes: []string{"/r/kubernetes"}}}}
	decision, err := policy.Authorize("https://www.reddit.com/r/kubernetes/new/.rss?limit=5", 5)
	if err != nil {
		t.Fatal(err)
	}
	if decision.PolicyID != policy.ID || decision.PolicyVersion != policy.Version || decision.SourceHost != "www.reddit.com" || decision.PathPrefix != "/r/kubernetes" || decision.MaximumItems != 10 || decision.RetentionDays != 30 {
		t.Fatalf("policy decision mismatch: %#v", decision)
	}
	if err := decision.Authorize("https://www.reddit.com/r/kubernetes/comments/thread", 5); err != nil {
		t.Fatal(err)
	}
	if err := decision.Authorize("https://old.reddit.com/r/kubernetes/comments/thread", 5); err == nil {
		t.Fatal("redirect escaped the exact authorized host")
	}
}

func TestPolicyBindsExactReadMethodAndRejectsAmbiguousBounds(t *testing.T) {
	policy := Policy{ID: "forums", Version: "2", Enabled: true, MaximumItems: 3, RetentionDays: 7,
		Sources: []PolicySource{{Host: "api.example.com", PathPrefixes: []string{"/v1/forum"}, Methods: []string{http.MethodHead}}}}
	decision, err := policy.AuthorizeRequest("https://api.example.com/v1/forum/threads", http.MethodHead, 3)
	if err != nil || decision.Method != http.MethodHead {
		t.Fatalf("HEAD decision = %#v, %v", decision, err)
	}
	if err := decision.AuthorizeRequest("https://api.example.com/v1/forum/threads", http.MethodGet, 3); err == nil {
		t.Fatal("decision widened HEAD authority to GET")
	}
	if _, err := policy.Authorize("https://api.example.com/v1/forum/threads", 1); err == nil {
		t.Fatal("legacy GET helper widened explicit HEAD-only policy")
	}

	invalid := policy
	invalid.Sources = []PolicySource{{Host: "api.example.com", PathPrefixes: []string{"/v1//forum"}, Methods: []string{http.MethodPost}}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("ambiguous path and write method were accepted")
	}
	invalid.Sources = []PolicySource{{Host: "api.example.com", PathPrefixes: []string{"/v1/forum", "/v1/forum"}}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("duplicate path authority was accepted")
	}
}

func TestSourcePolicyFailsClosed(t *testing.T) {
	policy := Policy{ID: "research", Version: "1", Enabled: true, MaximumItems: 5, Sources: []PolicySource{{Host: "*.example.com", PathPrefixes: []string{"/forums"}}}}
	for _, test := range []struct {
		name, url string
		items     int
	}{
		{"wildcard apex", "https://example.com/forums", 1},
		{"wrong host", "https://attacker.example.net/forums", 1},
		{"prefix confusion", "https://community.example.com/forumsevil", 1},
		{"userinfo", "https://community.example.com@attacker.test/forums", 1},
		{"fragment", "https://community.example.com/forums#secret", 1},
		{"too many", "https://community.example.com/forums/topic", 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := policy.Authorize(test.url, test.items); err == nil {
				t.Fatal("disallowed source was authorized")
			}
		})
	}
	if _, err := (Policy{ID: "bad", Version: "1", Enabled: true, MaximumItems: 1, Sources: []PolicySource{{Host: "example.com", PathPrefixes: []string{"relative"}}}}).Authorize("https://example.com/relative", 1); err == nil {
		t.Fatal("invalid policy was accepted")
	}
}

func TestSourcePolicyWildcardMatchesSubdomainsOnly(t *testing.T) {
	policy := Policy{ID: "forums", Version: "1", Enabled: true, MaximumItems: 1, Sources: []PolicySource{{Host: "*.example.com"}}}
	if _, err := policy.Authorize("https://community.example.com/", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := policy.Authorize("https://deep.community.example.com/", 1); err != nil {
		t.Fatal(err)
	}
}
