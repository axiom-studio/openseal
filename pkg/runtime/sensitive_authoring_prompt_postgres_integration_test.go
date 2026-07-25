//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/google/uuid"
)

func TestPostgresStartupRedactsLegacySensitiveAuthoringPrompt(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schema := "openseal_sensitive_" + uuid.NewString()[:8]
	primary, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_ = primary.Close()
	})

	now := time.Now().UTC()
	value := &authoring.ChangeSet{
		ID: "legacy-sensitive", Scope: capability.ScopeReference{Kind: "tenant", ID: "1"},
		Prompt: "Create an Agent, password: erase-me-now", PromptDigest: "legacy",
		Status: authoring.ChangeSetBlocked, Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	definition := &agent.AgentDefinition{
		ID: "legacy-agent", Version: "1", DisplayName: "Legacy Agent", Purpose: "Work safely",
		SystemPrompt: "Sign in with the supplied username and password erase-definition-secret.",
		Authority:    agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		Digest:       "legacy-definition-digest", CreatedAt: now,
	}
	value.Result.Candidate.Agents = []*agent.AgentDefinition{definition}
	definitionPayload, _ := json.Marshal(definition)
	if _, err := primary.db.ExecContext(ctx, `INSERT INTO `+primary.table("agent_definitions")+` (id,version,digest,created_at,payload) VALUES($1,$2,$3,$4,$5::jsonb)`, definition.ID, definition.Version, definition.Digest, now, string(definitionPayload)); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := primary.db.ExecContext(ctx, `INSERT INTO `+primary.table("workforce_change_sets")+`
		(scope_kind,scope_id,id,parent_id,status,revision,idempotency_key,request_digest,candidate_digest,created_at,updated_at,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb)`,
		"tenant", "1", value.ID, "", value.Status, 1, "legacy-key", "request", "candidate", now, now, string(payload)); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.db.ExecContext(ctx, `DELETE FROM `+primary.table("schema_migrations")+` WHERE version IN ($1,$2)`, sensitiveAuthoringPromptMigrationVersion, legacySensitiveAgentDefinitionMigrationVersion); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(ctx, value.Scope, value.ID)
	if err != nil || strings.Contains(restored.Prompt, "erase-me-now") || !strings.Contains(restored.Prompt, "[REDACTED]") {
		t.Fatalf("restored prompt was not redacted: prompt=%q err=%v", restored.Prompt, err)
	}
	if got := restored.Result.Candidate.Agents[0].SystemPrompt; strings.Contains(got, "erase-definition-secret") || got == definition.SystemPrompt {
		t.Fatalf("ChangeSet candidate Agent was not hardened: %q", got)
	}
	var stored string
	if err := restarted.db.QueryRowContext(ctx, `SELECT payload::text FROM `+restarted.table("workforce_change_sets")+` WHERE scope_kind=$1 AND scope_id=$2 AND id=$3`, "tenant", "1", value.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, "erase-me-now") {
		t.Fatal("durable PostgreSQL payload still contains the credential value")
	}
	restoredDefinition, err := restarted.GetDefinition(ctx, definition.ID, definition.Version)
	if err != nil || strings.Contains(restoredDefinition.SystemPrompt, "erase-definition-secret") || restoredDefinition.SystemPrompt == definition.SystemPrompt || restoredDefinition.Digest == definition.Digest {
		t.Fatalf("durable Agent definition was not safely migrated: definition=%#v err=%v", restoredDefinition, err)
	}
}
