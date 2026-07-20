package clawhub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

type exploreRegistryStub struct {
	Registry
	explore func(context.Context, ExploreRequest) (*SkillPage, error)
}

func (s exploreRegistryStub) ExploreSkills(ctx context.Context, request ExploreRequest) (*SkillPage, error) {
	return s.explore(ctx, request)
}

func TestExploreCatalogTraversesThreePagesAndDeduplicatesOverlap(t *testing.T) {
	requests := make([]ExploreRequest, 0, 3)
	registry := exploreRegistryStub{explore: func(_ context.Context, request ExploreRequest) (*SkillPage, error) {
		requests = append(requests, request)
		switch request.Cursor {
		case "":
			return &SkillPage{Items: []SkillSummary{{Slug: "alpha"}, {Slug: "beta"}}, NextCursor: "page-2"}, nil
		case "page-2":
			return &SkillPage{Items: []SkillSummary{{Slug: "beta"}, {Slug: "gamma"}}, NextCursor: "page-3"}, nil
		case "page-3":
			return &SkillPage{Items: []SkillSummary{{Slug: "delta"}}}, nil
		default:
			return nil, fmt.Errorf("unexpected cursor %q", request.Cursor)
		}
	}}

	snapshot, err := ExploreCatalog(context.Background(), registry, ExploreRequest{Limit: 100, Sort: "updated", NonSuspiciousOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	wantSlugs := []string{"alpha", "beta", "gamma", "delta"}
	gotSlugs := make([]string, 0, len(snapshot.Items))
	for _, item := range snapshot.Items {
		gotSlugs = append(gotSlugs, item.Slug)
	}
	if !reflect.DeepEqual(gotSlugs, wantSlugs) || snapshot.Pages != 3 || snapshot.DuplicateItemCount != 1 {
		t.Fatalf("snapshot = %#v; slugs = %#v", snapshot, gotSlugs)
	}
	if got := []string{requests[0].Cursor, requests[1].Cursor, requests[2].Cursor}; !reflect.DeepEqual(got, []string{"", "page-2", "page-3"}) {
		t.Fatalf("requested cursors = %#v", got)
	}
	for _, request := range requests {
		if request.Limit != 100 || request.Sort != "updated" || !request.NonSuspiciousOnly {
			t.Fatalf("discovery options were not preserved: %#v", request)
		}
	}
}

func TestExploreCatalogReportsPartialFailureAndRejectsCursorLoops(t *testing.T) {
	t.Run("mid traversal failure", func(t *testing.T) {
		upstreamErr := errors.New("registry unavailable")
		registry := exploreRegistryStub{explore: func(_ context.Context, request ExploreRequest) (*SkillPage, error) {
			if request.Cursor == "" {
				return &SkillPage{Items: []SkillSummary{{Slug: "alpha"}}, NextCursor: "page-2"}, nil
			}
			return nil, upstreamErr
		}}
		snapshot, err := ExploreCatalog(context.Background(), registry, ExploreRequest{Limit: 100})
		var traversalErr *CatalogTraversalError
		if !errors.As(err, &traversalErr) || !errors.Is(err, upstreamErr) || traversalErr.CompletedPages != 1 || traversalErr.UniqueItems != 1 || traversalErr.Cursor != "page-2" || len(snapshot.Items) != 1 {
			t.Fatalf("snapshot = %#v, error = %#v", snapshot, err)
		}
	})

	t.Run("cursor loop", func(t *testing.T) {
		calls := 0
		registry := exploreRegistryStub{explore: func(_ context.Context, request ExploreRequest) (*SkillPage, error) {
			calls++
			if request.Cursor == "" {
				return &SkillPage{Items: []SkillSummary{{Slug: "alpha"}}, NextCursor: "loop"}, nil
			}
			return &SkillPage{Items: []SkillSummary{{Slug: "beta"}}, NextCursor: "loop"}, nil
		}}
		snapshot, err := ExploreCatalog(context.Background(), registry, ExploreRequest{})
		var traversalErr *CatalogTraversalError
		if !errors.As(err, &traversalErr) || traversalErr.CompletedPages != 2 || traversalErr.UniqueItems != 2 || traversalErr.Cursor != "loop" || calls != 2 || len(snapshot.Items) != 2 {
			t.Fatalf("snapshot = %#v, calls = %d, error = %#v", snapshot, calls, err)
		}
	})
}

func TestParseSkillReferenceRejectsPathAndAmbiguousValues(t *testing.T) {
	valid := map[string]SkillReference{
		"summarize":               {Slug: "summarize"},
		"@sean-ford/summarize_v2": {Owner: "sean-ford", Slug: "summarize_v2"},
	}
	for value, expected := range valid {
		actual, err := ParseSkillReference(value)
		if err != nil || actual != expected {
			t.Fatalf("ParseSkillReference(%q) = %#v, %v; want %#v", value, actual, err, expected)
		}
	}

	for _, value := range []string{"", ".", "..", "../skill", "owner/../skill", "/skill", "owner/skill/extra", "owner%2Fskill", "-owner/skill", "owner/-skill", "owner/skill.name"} {
		if actual, err := ParseSkillReference(value); err == nil {
			t.Fatalf("ParseSkillReference(%q) unexpectedly accepted %#v", value, actual)
		}
	}
}

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
			json.NewEncoder(w).Encode(map[string]interface{}{"results": []map[string]interface{}{{"slug": "safe", "displayName": "Safe", "latestVersion": map[string]interface{}{"version": "1.2.3"}}}, "nextCursor": "next"})
		case "/skills/bad/verify":
			json.NewEncoder(w).Encode(map[string]interface{}{"schema": "unknown", "ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClawHubClient(server.URL)
	page, err := client.SearchSkills(context.Background(), SearchRequest{Query: "safe", NonSuspiciousOnly: true})
	if err != nil || len(page.Items) != 1 || page.Items[0].Version != "1.2.3" || page.NextCursor != "next" {
		t.Fatalf("search = %#v, %v", page, err)
	}
	if _, err := client.VerifySkill(context.Background(), SkillReference{Slug: "bad"}, "", ""); err == nil {
		t.Fatal("unknown verification envelopes must fail closed")
	}
}

func TestRegistryDownloadsExactOwnerQualifiedVersion(t *testing.T) {
	payload := []byte("zip-bytes")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/download" || r.URL.Query().Get("slug") != "research" || r.URL.Query().Get("ownerHandle") != "acme" || r.URL.Query().Get("version") != "1.2.3" {
			http.Error(w, "bad download identity", http.StatusBadRequest)
			return
		}
		w.Write(payload)
	}))
	defer server.Close()
	archive, err := NewClawHubClient(server.URL).DownloadArchive(context.Background(), SkillReference{Owner: "acme", Slug: "research"}, "1.2.3", "")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if string(archive.Bytes) != string(payload) || archive.SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("archive = %#v", archive)
	}
}

func TestRegistryRateLimitAndRetryAreContextAware(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "limited" {
			w.Header().Set("Retry-After", "30")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		http.Error(w, "temporary", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := NewClawHubClient(server.URL)
	_, err := client.SearchSkills(context.Background(), SearchRequest{Query: "limited"})
	var registryErr *ClawHubError
	if !errors.As(err, &registryErr) || registryErr.StatusCode != http.StatusTooManyRequests || registryErr.RetryAfter != 30 {
		t.Fatalf("rate limit error = %#v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = client.SearchSkills(ctx, SearchRequest{Query: "retry"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("context-aware retry = %v after %s", err, time.Since(started))
	}
}
