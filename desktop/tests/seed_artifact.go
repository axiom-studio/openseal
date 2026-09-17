//go:build ignore

// Seeds immutable artifact versions into an isolated UI-test workspace.
package main

import (
	"bytes"
	"context"
	"flag"
	"path/filepath"
	"time"

	"github.com/axiom-studio/openseal/pkg/artifact"
	"github.com/axiom-studio/openseal/pkg/runtime"
)

func main() {
	db := flag.String("db", "", "isolated test database")
	flag.Parse()
	if *db == "" {
		panic("-db required")
	}
	store, err := runtime.NewSQLiteStore(*db)
	check(err)
	defer store.Close()
	content, err := artifact.NewLocalStore(filepath.Join(filepath.Dir(*db), "artifacts"))
	check(err)
	ctx := context.Background()
	now := time.Now().UTC()
	scope := runtime.Scope{Kind: "local", ID: "default"}
	run := &runtime.AgentRun{ID: "ui-artifact-run", RootRunID: "ui-artifact-run", Kind: runtime.RunKindAgentWork, Scope: scope, Owner: runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "synthetic-analyst"}, AssignedAgentID: "synthetic-analyst", Goal: "Inspect synthetic task artifacts", Source: runtime.RunSourceManual, Status: runtime.AgentRunStatusCompleted, Revision: 1, AvailableAt: now, CreatedAt: now, UpdatedAt: now}
	check(store.CreateAgentRun(ctx, run))
	for _, fixture := range []struct {
		id, name, mime string
		version        int64
		body           []byte
	}{
		{"ui:résumé", "report.txt", "text/plain", 1, []byte("First saved report.\n")},
		{"ui:résumé", "report.txt", "text/plain", 2, []byte("Verified task artifact.\nUncertainty remains explicit.\n")},
		{"ui-binary", "evidence.bin", "application/octet-stream", 1, []byte{0, 255, 128, 13, 10, 1, 254}},
	} {
		saved, err := content.Put(ctx, runtime.ArtifactContentWrite{Scope: scope, MediaType: fixture.mime, Reader: bytes.NewReader(fixture.body), SizeBytes: int64(len(fixture.body))})
		check(err)
		_, err = runtime.NewArtifactCatalog(store).Register(ctx, runtime.RegisterArtifactRequest{ExpectedLatestVersion: fixture.version - 1, Artifact: &runtime.Artifact{ID: fixture.id, Version: fixture.version, Scope: scope, Name: fixture.name, MediaType: fixture.mime, ContentRef: saved.ContentRef, Digest: saved.Digest, SizeBytes: saved.SizeBytes, Classification: "internal", Provenance: runtime.ArtifactProvenance{Producer: runtime.ActivityActor{Type: "agent", ID: "synthetic-analyst"}, RunID: run.ID}}})
		check(err)
	}
}
func check(err error) {
	if err != nil {
		panic(err)
	}
}
