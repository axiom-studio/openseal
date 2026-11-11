package runtime

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

// A Run's reply hands over some of the files it saved. The rest are working
// files: still in the catalog, left out of a results list, and results again
// the moment a later reply hands them over.
func TestWorkingFilesAreLeftOutOfResultsUntilHandedOver(t *testing.T) {
	ctx := context.Background()
	sqlite, err := NewSQLiteStore(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqlite.Close()
	scope := Scope{Kind: "tenant", ID: "one"}
	stores := map[string]ArtifactStore{"memory": NewMemoryStore(), "sqlite": sqlite}
	for name, store := range stores {
		catalog := NewArtifactCatalog(store)
		catalog.now = func() time.Time { return time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC) }
		for _, id := range []string{"report.pdf", "plot.py", "panel.png", "upload.csv"} {
			if _, err := catalog.Register(ctx, RegisterArtifactRequest{Artifact: validCatalogArtifact(id, 1)}); err != nil {
				t.Fatal(err)
			}
		}
		list := func(filter ArtifactFilter) []string {
			t.Helper()
			filter.Scope, filter.LatestOnly, filter.Limit = scope, true, 50
			artifacts, err := catalog.List(ctx, filter)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			ids := make([]string, 0, len(artifacts))
			for _, artifact := range artifacts {
				ids = append(ids, artifact.ID)
			}
			sort.Strings(ids)
			return ids
		}
		check := func(what string, got, want []string) {
			t.Helper()
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s: %s = %v, want %v", name, what, got, want)
			}
		}
		all := []string{"panel.png", "plot.py", "report.pdf", "upload.csv"}
		check("results before anything is marked", list(ArtifactFilter{Files: ArtifactFilesResults}), all)

		saved := []savedArtifact{{Ref: artifactRef("report.pdf")}, {Ref: artifactRef("plot.py")}, {Ref: artifactRef("panel.png")}, {Ref: artifactRef("plot.py")}}
		if err := markRunFiles(ctx, store, scope, saved, []ConversationReference{{Kind: ConversationReferenceRun, ID: "run-1"}, artifactRef("report.pdf")}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		check("results", list(ArtifactFilter{Files: ArtifactFilesResults}), []string{"report.pdf", "upload.csv"})
		check("working files", list(ArtifactFilter{Files: ArtifactFilesWorking}), []string{"panel.png", "plot.py"})
		check("everything", list(ArtifactFilter{}), all)
		// The paths that filter in Go agree with the SQL page.
		check("results with an evidence filter", list(ArtifactFilter{Files: ArtifactFilesResults, EvidenceTarget: "https://forum.example/research/thread-7"}),
			list(ArtifactFilter{Files: ArtifactFilesResults}))

		// A later reply hands the chart over: it is a result again.
		if err := markRunFiles(ctx, store, scope, []savedArtifact{{Ref: artifactRef("panel.png")}}, []ConversationReference{artifactRef("panel.png")}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		check("results after a later hand-over", list(ArtifactFilter{Files: ArtifactFilesResults}), []string{"panel.png", "report.pdf", "upload.csv"})

		if _, err := catalog.List(ctx, ArtifactFilter{Scope: scope, Files: "scratch"}); err == nil {
			t.Fatalf("%s: an unknown files value was accepted", name)
		}
	}
}

func artifactRef(id string) ConversationReference {
	return ConversationReference{Kind: ConversationReferenceArtifact, ID: id, Version: 1}
}
