package sqlite

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/runtime/storetest"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

func TestSQLiteMigrationStripsTeamSkillGrantsAndRecomputesDigest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "grants.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestSQLiteDB(t, path)
	teams := kernelteam.NewRegistryWithStore(store, kernelagent.NewRegistryWithStore(store))
	registered, err := teams.RegisterDefinition(ctx, storetest.TeamDefinition("1"))
	if err != nil {
		t.Fatal(err)
	}
	var payload string
	if err := db.QueryRowContext(ctx, `SELECT payload FROM team_definitions WHERE id = ? AND version = ?`, registered.ID, registered.Version).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	var document map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &document); err != nil {
		t.Fatal(err)
	}
	document["roles"] = []interface{}{map[string]interface{}{
		"id": "researcher", "displayName": "Researcher", "purpose": "Find evidence",
		"skillGrants": []interface{}{map[string]interface{}{"skillId": "forum", "skillVersion": "1", "maximumRisk": "read"}},
	}}
	document["digest"] = "sha256:stale"
	encoded, _ := json.Marshal(document)
	if _, err := db.ExecContext(ctx, `UPDATE team_definitions SET payload = ?, digest = 'sha256:stale' WHERE id = ? AND version = ?`, string(encoded), registered.ID, registered.Version); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var migrated, digest string
	if err := db.QueryRowContext(ctx, `SELECT payload, digest FROM team_definitions WHERE id = ? AND version = ?`, registered.ID, registered.Version).Scan(&migrated, &digest); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(migrated, "skillGrants") || digest != registered.Digest || !strings.Contains(migrated, registered.Digest) {
		t.Fatalf("migrated definition digest=%s (want %s) payload=%s", digest, registered.Digest, migrated)
	}
}
