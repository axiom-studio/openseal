//go:build integration

package runtime

import (
	"context"
	"errors"
	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
	"os"
	"testing"
	"time"
)

func TestPostgresAgentRenameIsReplicaSafe(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN")
	}
	schema := "openseal_name_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})
	replica, err := NewPostgresStore(t.Context(), dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer replica.Close()
	scope := capability.ScopeReference{Kind: "tenant", ID: "one"}
	original := testWorkforceChangeSet(scope, "named-agent")
	original.Result.Candidate.Agents = []*agent.AgentDefinition{{ID: "stable-agent", DisplayName: "Marginalia"}}
	_, _, err = primary.CreateChangeSet(t.Context(), original, "named-agent", "request")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := primary.RenameChangeSetAgent(t.Context(), scope, original.ID, original.Revision, "Paper Trail", original.Actor, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	restored, err := replica.GetChangeSet(t.Context(), scope, original.ID)
	if err != nil || restored.AgentName != "Paper Trail" || restored.CandidateDigest != renamed.CandidateDigest {
		t.Fatal("replica lost rename", err)
	}
	if _, err := replica.RenameChangeSetAgent(t.Context(), scope, original.ID, original.Revision, "Different", original.Actor, time.Now()); !errors.Is(err, authoring.ErrChangeSetRevision) {
		t.Fatal("replica accepted stale rename", err)
	}
	if _, err := replica.RenameChangeSetAgent(t.Context(), capability.ScopeReference{Kind: "tenant", ID: "two"}, original.ID, renamed.Revision, "Different", original.Actor, time.Now()); !errors.Is(err, authoring.ErrChangeSetNotFound) {
		t.Fatal("cross-tenant rename accepted", err)
	}
	restored.Revision++
	if _, err := replica.UpdateChangeSet(t.Context(), restored, renamed.Revision); err != nil {
		t.Fatal("digest column not updated", err)
	}
}
