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
	if decision.PolicyID != policy.ID || decision.PolicyVersion != policy.Version || decision.SourceHost != "www.reddit.com" || decision.PathPrefix != "/r/kubernetes" || decision.MaximumItems != 10 {
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

func TestSourcePolicyAuthorizesExplicitOutreachOnly(t *testing.T) {
	policy := Policy{
		ID: "community-research", Version: "1", Enabled: true, MaximumItems: 5,
		Sources:  []PolicySource{{Host: "hooks.example.com", PathPrefixes: []string{"/community/replies"}}},
		Outreach: &OutreachPolicy{Enabled: true, ApprovalPolicy: "human-review", MaximumBytes: 500},
	}
	decision, err := policy.AuthorizeOutreach("https://hooks.example.com/community/replies/thread-1?reply=1", 120, "human-review")
	if err != nil {
		t.Fatal(err)
	}
	if decision.PolicyID != policy.ID || decision.PolicyVersion != policy.Version || decision.SourceHost != "hooks.example.com" ||
		decision.PathPrefix != "/community/replies" || decision.ApprovalPolicy != "human-review" || decision.MaximumBytes != 500 {
		t.Fatalf("outreach decision mismatch: %#v", decision)
	}
	if err := decision.Authorize("https://hooks.example.com/community/replies/thread-1", 500, "human-review"); err != nil {
		t.Fatal(err)
	}
}

func TestSourcePolicyOutreachFailsClosed(t *testing.T) {
	base := Policy{
		ID: "community-research", Version: "1", Enabled: true, MaximumItems: 5,
		Sources: []PolicySource{{Host: "hooks.example.com", PathPrefixes: []string{"/community/replies"}}},
	}
	if _, err := base.AuthorizeOutreach("https://hooks.example.com/community/replies/thread-1", 10, "human-review"); err == nil {
		t.Fatal("read authorization silently granted write authority")
	}
	disabled := base
	disabled.Outreach = &OutreachPolicy{}
	if err := disabled.Validate(); err != nil {
		t.Fatalf("explicit disabled outreach should not require write policy fields: %v", err)
	}

	policy := base
	policy.Outreach = &OutreachPolicy{Enabled: true, ApprovalPolicy: "human-review", MaximumBytes: 500}
	tests := []struct {
		name, target, approval string
		bodyBytes              int
	}{
		{name: "wrong host", target: "https://attacker.example/community/replies/thread-1", approval: "human-review", bodyBytes: 10},
		{name: "prefix confusion", target: "https://hooks.example.com/community/replies-evil/thread-1", approval: "human-review", bodyBytes: 10},
		{name: "userinfo", target: "https://hooks.example.com@attacker.example/community/replies/thread-1", approval: "human-review", bodyBytes: 10},
		{name: "fragment", target: "https://hooks.example.com/community/replies/thread-1#secret", approval: "human-review", bodyBytes: 10},
		{name: "wrong approval", target: "https://hooks.example.com/community/replies/thread-1", approval: "auto", bodyBytes: 10},
		{name: "empty body", target: "https://hooks.example.com/community/replies/thread-1", approval: "human-review", bodyBytes: 0},
		{name: "oversized body", target: "https://hooks.example.com/community/replies/thread-1", approval: "human-review", bodyBytes: 501},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := policy.AuthorizeOutreach(test.target, test.bodyBytes, test.approval); err == nil {
				t.Fatal("disallowed outreach was authorized")
			}
		})
	}

	invalid := base
	invalid.Outreach = &OutreachPolicy{Enabled: true, MaximumBytes: 100}
	if err := invalid.Validate(); err == nil {
		t.Fatal("enabled outreach without approval policy was accepted")
	}
	invalid.Outreach = &OutreachPolicy{Enabled: true, ApprovalPolicy: "human-review", MaximumBytes: 20001}
	if err := invalid.Validate(); err == nil {
		t.Fatal("outreach body limit above the portable maximum was accepted")
	}
}
