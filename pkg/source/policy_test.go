package source

import "testing"

func TestSourcePolicyAuthorizesExactBoundedSource(t *testing.T) {
	policy := Policy{ID: "public-reddit-research", Version: "2026-07-13", Enabled: true, MaximumItems: 10, RetentionDays: 30, Sources: []PolicySource{{Host: "www.reddit.com", PathPrefixes: []string{"/r/kubernetes"}}}}
	decision, err := policy.Authorize("https://www.reddit.com/r/kubernetes/new/.rss?limit=5", 5)
	if err != nil {
		t.Fatal(err)
	}
	if decision.PolicyID != policy.ID || decision.PolicyVersion != policy.Version || decision.SourceHost != "www.reddit.com" || decision.PathPrefix != "/r/kubernetes" || decision.MaximumItems != 10 {
		t.Fatalf("policy decision mismatch: %#v", decision)
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
