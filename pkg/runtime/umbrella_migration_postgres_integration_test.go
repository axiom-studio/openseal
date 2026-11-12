//go:build integration

package runtime

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Migrations 71 to 73 strip role Skill grants, move setup requests to their
// conversation's umbrella and give gateways an owner on an existing database.
func TestPostgresUmbrellaMigrationsRewriteStoredRecords(t *testing.T) {
	pgSchema := newTestPostgresSchema(t, "umbrella_")
	dsn := pgSchema.dsn
	schema := pgSchema.name
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pgSchema.db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+pgSchema.quoted()+` CASCADE`)
		_ = store.Close()
	})
	exec := func(query string, args ...interface{}) {
		t.Helper()
		if _, err := pgSchema.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	exec(`INSERT INTO `+pgSchema.table("team_definitions")+` (id, version, digest, created_at, payload) VALUES ('crew', '1', 'sha256:stale', $1, $2::jsonb)`, now,
		`{"id":"crew","version":"1","displayName":"Crew","purpose":"Help","roles":[{"id":"member","displayName":"Member","purpose":"Help","skillGrants":[{"skillId":"forum","skillVersion":"1","maximumRisk":"read"}]}],"approvals":{"maximumRisk":"read"},"digest":"sha256:stale"}`)
	exec(`INSERT INTO `+pgSchema.table("team_deployments")+` (scope_kind, scope_id, id, definition_id, active_version, revision, updated_at, payload) VALUES ('tenant','one','crew','crew','1',1,$1,$2::jsonb)`, now,
		`{"id":"crew","roster":[{"id":"member","roleId":"member","agentDeploymentId":"member"}]}`)
	for _, chat := range []string{"team-chat"} {
		exec(`INSERT INTO `+pgSchema.table("conversations")+` (scope_kind, scope_id, id, owner_type, owner_id, status, idempotency_key, last_sequence, revision, created_at, updated_at, payload, space_id)
			VALUES ('tenant','one',$1,'team','crew','active',$1,0,1,$2,$2,'{}'::jsonb,'personal')`, chat, now)
	}
	insert := func(id, deploymentID, status string) {
		exec(`INSERT INTO `+pgSchema.table("skill_setup_requests")+` (scope_kind, scope_id, id, deployment_id, conversation_id, status, revision, created_at, payload)
			VALUES ('tenant','one',$1,$2,'team-chat',$3,1,$4,$5::jsonb)`, id, deploymentID, status, now,
			`{"id":"`+id+`","deploymentId":"`+deploymentID+`","status":"`+status+`","revision":1}`)
	}
	insert("pending-member", "member", "pending")
	insert("pending-gone", "gone", "pending")
	insert("history", "member", "resolved")
	if _, err := NewExternalConversationGatewayService(store).Create(ctx, CreateExternalConversationGatewayRequest{ID: "legacy", Name: "legacy",
		Gateway: externalConversationGatewayFixture(Scope{Kind: "tenant", ID: "one"}), Actor: ActivityActor{Type: "test", ID: "operator"}, Reason: "create"}); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE ` + pgSchema.table("external_conversation_gateways") + ` SET payload = payload #- '{gateway,owner}'`)
	exec(`DELETE FROM ` + pgSchema.table("schema_migrations") + ` WHERE version IN (71, 72, 73)`)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	var payload, digest string
	if err := pgSchema.db.QueryRowContext(ctx, `SELECT payload::text, digest FROM `+pgSchema.table("team_definitions")+` WHERE id = 'crew'`).Scan(&payload, &digest); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, "skillGrants") || digest == "sha256:stale" || !strings.Contains(payload, digest) {
		t.Fatalf("Team definition = %s digest %s", payload, digest)
	}
	rows := map[string][3]string{}
	result, err := pgSchema.db.QueryContext(ctx, `SELECT id, deployment_id, status, payload->'owner'->>'id', payload->>'agentId' FROM `+pgSchema.table("skill_setup_requests"))
	if err != nil {
		t.Fatal(err)
	}
	for result.Next() {
		var id, deployment, status, owner, agent string
		if err := result.Scan(&id, &deployment, &status, &owner, &agent); err != nil {
			t.Fatal(err)
		}
		rows[id] = [3]string{deployment + "/" + owner, status, agent}
	}
	result.Close()
	gateway, err := store.GetExternalConversationGateway(ctx, Scope{Kind: "tenant", ID: "one"}, "legacy")
	if err != nil || gateway.Gateway.Owner != (ObjectiveOwner{Type: OwnerTypeAgent, ID: gateway.Gateway.DeploymentID}) {
		t.Fatalf("backfilled gateway = %#v, %v", gateway, err)
	}
	if len(rows) != 2 || rows["pending-member"] != [3]string{"crew/crew", "pending", "member"} || rows["pending-gone"] != [3]string{"crew/crew", "dismissed", "gone"} {
		t.Fatalf("setup requests after migration = %#v", rows)
	}
}
