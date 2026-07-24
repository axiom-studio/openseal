package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestSearchWorkforceSkillsPreservesSearchIntent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if r.URL.Path != "/api/v1/authoring/workforce/skills" || query.Get("scopeKind") != "tenant" ||
			query.Get("scopeId") != "one" || query.Get("query") != "reddit research" ||
			query.Get("cursor") != "opaque" || query.Get("limit") != "50" ||
			query.Get("maximumRisk") != "read" || len(query["requiredAction"]) != 2 {
			t.Fatalf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"nextCursor":"next"}`))
	}))
	defer server.Close()
	page, err := NewKernelHTTPClient(server.URL, server.Client()).SearchWorkforceSkills(context.Background(), authoring.SkillSearchRequest{
		Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Query: "reddit research",
		RequiredActions: []string{"search", "read"}, MaximumRisk: capability.RiskLevelRead,
		Cursor: "opaque", Limit: 50,
	})
	if err != nil || page.NextCursor != "next" {
		t.Fatalf("page=%#v err=%v", page, err)
	}
}
