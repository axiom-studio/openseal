package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

type sqliteProfileGenerator struct{ calls int }

func (g *sqliteProfileGenerator) GenerateProfile(_ context.Context, request authoring.GenerateRequest) (authoring.IdentityProfile, error) {
	g.calls++
	return authoring.IdentityProfile{Name: "Orbit", Purpose: "Help research markets", Behavior: "Discover tools and guide account setup during conversation.", Personality: "Warm and curious", OperatingPrinciples: []string{"Work from verified information."}}, nil
}

func TestSQLiteProfileCreationRemainsIdentityOnlyAcrossRestartAndApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profiles.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	g := &sqliteProfileGenerator{}
	compiler, err := authoring.NewCompiler(g)
	if err != nil {
		t.Fatal(err)
	}
	service, err := authoring.NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	request := authoring.CreateChangeSetRequest{ProfileOnly: true, Scope: capability.ScopeReference{Kind: "tenant", ID: "one"}, Prompt: "Create a Slack research assistant and schedule daily reports.", Actor: authoring.ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "new-profile"}
	prepared, _, err := service.Prepare(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err = authoring.NewChangeSetService(compiler, store)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := service.Get(t.Context(), request.Scope, prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !restored.ProfileOnly || !restored.Generation.Request.ProfileOnly || g.calls != 0 {
		t.Fatal("durable profile mode was lost or provider called before generation")
	}
	compiled, err := service.GeneratePrepared(t.Context(), restored.Scope, restored.ID, restored.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := authoring.ValidateProfileChangeSet(compiled); err != nil {
		t.Fatal(err)
	}
	if len(compiled.Result.Candidate.Agents) != 1 || len(compiled.RequiredCredentials) != 0 || compiled.Result.Candidate.Agents[0].Runbook != nil || g.calls != 1 {
		t.Fatal("profile created workflow/setup or duplicate provider request")
	}
	ready, _, err := service.SubmitEvaluation(t.Context(), authoring.SubmitChangeSetEvaluationRequest{Scope: compiled.Scope, ChangeSetID: compiled.ID, ExpectedRevision: compiled.Revision, CandidateDigest: compiled.CandidateDigest, Allowed: true, Actor: request.Actor, IdempotencyKey: "approve-profile"})
	if err != nil {
		t.Fatal(err)
	}
	apply := authoring.ApplyChangeSetRequest{Scope: ready.Scope, ChangeSetID: ready.ID, ExpectedRevision: ready.Revision, CandidateDigest: ready.CandidateDigest, Reason: "Create Agent", Actor: request.Actor, IdempotencyKey: "apply-profile"}
	applied, _, err := service.Apply(t.Context(), apply)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != authoring.ChangeSetApplied || len(applied.ApplyReceipt.Resources) != 2 {
		t.Fatalf("profile apply=%#v", applied.ApplyReceipt)
	}
	replay, found, err := service.Apply(t.Context(), apply)
	if err != nil || !found || replay.ApplyReceipt.ID != applied.ApplyReceipt.ID || g.calls != 1 {
		t.Fatalf("replay=%v err=%v calls=%d", found, err, g.calls)
	}
}
