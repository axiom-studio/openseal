package runtime

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestSQLiteAgentRenamePreservesDigestFenceAndSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	original := testWorkforceChangeSet(scope, "named-agent")
	original.Result.Candidate.Agents = []*agent.AgentDefinition{{ID: "stable-agent", DisplayName: "Marginalia"}}
	saved, _, err := store.CreateChangeSet(t.Context(), original, "named-agent", "request")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := store.RenameChangeSetAgent(t.Context(), scope, saved.ID, saved.Revision, "Paper Trail", saved.Actor, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if renamed.CandidateDigest == saved.CandidateDigest || renamed.Result.Candidate.Agents[0].ID != "stable-agent" {
		t.Fatal("candidate identity/digest incorrect")
	}
	stale := *saved
	stale.Revision++
	if _, err := store.UpdateChangeSet(t.Context(), &stale, saved.Revision); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatal("stale review accepted", err)
	}
	if _, err := store.RenameChangeSetAgent(t.Context(), scope, saved.ID, saved.Revision, "Different", saved.Actor, time.Now()); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatal("stale rename accepted", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.GetChangeSet(t.Context(), scope, saved.ID)
	if err != nil || restored.AgentName != "Paper Trail" || restored.CandidateDigest != renamed.CandidateDigest {
		t.Fatal("rename lost after restart", err)
	}
	restored.Revision++
	if _, err := reopened.UpdateChangeSet(t.Context(), restored, renamed.Revision); err != nil {
		t.Fatal("new digest not installed in storage", err)
	}
}
