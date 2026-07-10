package clawhub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistrySupportsOwnerQualifiedLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("ownerHandle") != "acme" {
			http.Error(w, "owner required", http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/skills/release":
			json.NewEncoder(w).Encode(map[string]interface{}{"skill": map[string]interface{}{"slug": "release", "displayName": "Release"}, "latestVersion": map[string]interface{}{"version": "1.2.0"}, "owner": map[string]interface{}{"handle": "acme"}})
		case "/skills/release/versions":
			json.NewEncoder(w).Encode(map[string]interface{}{"items": []map[string]interface{}{{"version": "1.2.0", "createdAt": 10}}, "nextCursor": nil})
		case "/skills/release/versions/1.2.0":
			json.NewEncoder(w).Encode(map[string]interface{}{"version": map[string]interface{}{"version": "1.2.0", "createdAt": 10, "files": []map[string]interface{}{{"path": "SKILL.md", "size": 20, "sha256": "abc"}}, "security": map[string]interface{}{"status": "clean", "hasWarnings": false}}})
		case "/skills/release/file":
			if r.URL.Query().Get("path") != "SKILL.md" || r.URL.Query().Get("version") != "1.2.0" {
				http.Error(w, "bad file query", http.StatusBadRequest)
				return
			}
			w.Write([]byte("---\nname: release\n---"))
		case "/skills/release/verify":
			json.NewEncoder(w).Encode(map[string]interface{}{"schema": "clawhub.skill.verify.v1", "ok": true, "decision": "pass", "slug": "release", "displayName": "Release", "version": "1.2.0", "resolvedFrom": "version", "createdAt": 10})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClawHubClient(server.URL)
	ref, err := ParseSkillReference("@acme/release")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if detail, err := client.InspectSkill(ctx, ref); err != nil || detail.Slug != "release" || detail.Name != "Release" || detail.Version != "1.2.0" || detail.Owner != "acme" {
		t.Fatalf("inspect = %#v, %v", detail, err)
	}
	if versions, err := client.ListVersions(ctx, ref, 25, ""); err != nil || len(versions.Items) != 1 {
		t.Fatalf("versions = %#v, %v", versions, err)
	}
	if version, err := client.GetVersion(ctx, ref, "1.2.0"); err != nil || len(version.Files) != 1 || version.Security.Status != "clean" {
		t.Fatalf("version = %#v, %v", version, err)
	}
	if file, err := client.GetFile(ctx, ref, "SKILL.md", "1.2.0", ""); err != nil || len(file) == 0 {
		t.Fatalf("file = %q, %v", file, err)
	}
	if verification, err := client.VerifySkill(ctx, ref, "1.2.0", ""); err != nil || !verification.OK {
		t.Fatalf("verify = %#v, %v", verification, err)
	}
}

func TestRegistryDiscoveryAndVerificationAreStrict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			if r.URL.Query().Get("nonSuspiciousOnly") != "true" {
				http.Error(w, "safe filter missing", http.StatusBadRequest)
				return
			}
			json.NewEncoder(w).Encode(map[string]interface{}{"results": []map[string]interface{}{{"slug": "safe", "displayName": "Safe"}}, "nextCursor": "next"})
		case "/skills/bad/verify":
			json.NewEncoder(w).Encode(map[string]interface{}{"schema": "unknown", "ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClawHubClient(server.URL)
	page, err := client.SearchSkills(context.Background(), SearchRequest{Query: "safe", NonSuspiciousOnly: true})
	if err != nil || len(page.Items) != 1 || page.NextCursor != "next" {
		t.Fatalf("search = %#v, %v", page, err)
	}
	if _, err := client.VerifySkill(context.Background(), SkillReference{Slug: "bad"}, "", ""); err == nil {
		t.Fatal("unknown verification envelopes must fail closed")
	}
}
