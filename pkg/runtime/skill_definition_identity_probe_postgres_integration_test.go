//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/google/uuid"
)

func TestPostgresSkillDefinitionIdentityProbeUsesLiveIndexedMetadata(t *testing.T) {
	dsn := os.Getenv("OPENSEAL_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set OPENSEAL_TEST_POSTGRES_DSN to run PostgreSQL integration tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	open := func(schema string) *PostgresStore {
		t.Helper()
		store, err := NewPostgresStore(ctx, dsn, WithPostgresSchema(schema))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	primary := open("openseal_skill_identity_" + uuid.NewString()[:8])
	unrelated := open("openseal_other_identity_" + uuid.NewString()[:8])
	t.Cleanup(func() {
		_, _ = primary.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+primary.quotedSchema()+` CASCADE`)
		_, _ = unrelated.db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+unrelated.quotedSchema()+` CASCADE`)
	})
	replica := open(primary.schema)
	testSkillDefinitionIdentityProbeContract(t, primary, unrelated)
	before, err := replica.ListSkillDefinitionIdentities(ctx, "research", "1.0.0")
	if err != nil || len(before) != 3 {
		t.Fatalf("replica identities = %#v, %v", before, err)
	}
	if err := skill.NewCatalogWithStore(replica).Register(ctx, skillIdentityProbeDefinition("research", "1.0.0", "clawhub::@replica/research")); err != nil {
		t.Fatal(err)
	}
	after, err := primary.ListSkillDefinitionIdentities(ctx, "research", "1.0.0")
	if err != nil || len(after) != 4 || after[2].SourceIdentity != "clawhub::@replica/research" {
		t.Fatalf("publisher installed by replica was not immediately visible = %#v, %v", after, err)
	}

	// Make the performance contract meaningful as the shared catalog grows.
	// Only this disposable schema changes its payload storage setting.
	if _, err := primary.db.ExecContext(ctx, `ALTER TABLE `+primary.table("skill_definitions")+` ALTER COLUMN payload SET STORAGE EXTERNAL`); err != nil {
		t.Fatal(err)
	}
	large := skillIdentityProbeDefinition("research", "1.0.0", "clawhub::@large/research")
	large.Prompt.Instructions = strings.Repeat("Keep the immutable payload outside the metadata read path. ", 512)
	if err := skill.NewCatalogWithStore(primary).Register(ctx, large); err != nil {
		t.Fatal(err)
	}
	expectedMatchingRows := len(after) + 1
	if _, err := primary.db.ExecContext(ctx, `INSERT INTO `+primary.table("skill_definitions")+`(id, version, source_identity, payload)
		SELECT 'unrelated-'||n, '1.0.0', '', '{"name":"Unrelated"}'::jsonb FROM generate_series(1,10000) n`); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.db.ExecContext(ctx, `VACUUM (ANALYZE) `+primary.table("skill_definitions")); err != nil {
		t.Fatal(err)
	}
	var payload []byte
	if err := primary.db.QueryRowContext(ctx, `EXPLAIN (ANALYZE, VERBOSE, BUFFERS, FORMAT JSON)
		SELECT id, version, source_identity FROM `+primary.table("skill_definitions")+` WHERE id = $1 AND version = $2 ORDER BY source_identity`, "research", "1.0.0").Scan(&payload); err != nil {
		t.Fatal(err)
	}
	type planNode struct {
		NodeType     string     `json:"Node Type"`
		RelationName string     `json:"Relation Name"`
		IndexName    string     `json:"Index Name"`
		IndexCond    string     `json:"Index Cond"`
		Output       []string   `json:"Output"`
		HeapFetches  int        `json:"Heap Fetches"`
		ActualRows   float64    `json:"Actual Rows"`
		ActualLoops  float64    `json:"Actual Loops"`
		Plans        []planNode `json:"Plans"`
	}
	var explain []struct{ Plan planNode }
	if err := json.Unmarshal(payload, &explain); err != nil || len(explain) != 1 {
		t.Fatalf("decode metadata lookup plan: %v", err)
	}
	indexed := false
	var inspect func(planNode)
	inspect = func(node planNode) {
		if node.RelationName == "skill_definitions" {
			if node.NodeType != "Index Only Scan" || node.IndexName != "skill_definitions_pkey" || !skillIdentityProbePlanPredicateMatches(node.IndexCond) {
				t.Fatalf("identity metadata did not use the exact covering primary key lookup: %s", payload)
			}
			// Index-only scans can inspect matching heap tuples for MVCC
			// visibility while their pages are not marked all-visible. This
			// append-only fixture never updates or deletes matching versions,
			// so allow at most one visibility check per exact matching row.
			// The external payload remains outside the projected columns.
			if !skillIdentityProbePlanVisibilityBounded(node.HeapFetches, node.ActualRows, node.ActualLoops, expectedMatchingRows) {
				t.Fatalf("identity metadata scanned beyond its exact matching rows: %s", payload)
			}
			columns := make([]string, len(node.Output))
			for i, column := range node.Output {
				columns[i] = strings.TrimPrefix(column, "skill_definitions.")
			}
			if !reflect.DeepEqual(columns, []string{"id", "version", "source_identity"}) {
				t.Fatalf("identity metadata fetched immutable payload: %s", payload)
			}
			t.Logf("metadata visibility checks: heap=%d, matching rows=%.0f, loops=%.0f; projected columns=%v", node.HeapFetches, node.ActualRows, node.ActualLoops, columns)
			indexed = true
		}
		for _, child := range node.Plans {
			inspect(child)
		}
	}
	inspect(explain[0].Plan)
	if !indexed {
		t.Fatalf("metadata query lost its indexed definition lookup: %s", payload)
	}
	t.Logf("metadata query plan: %s", payload)

	identity := capability.SkillIdentity{ID: "broken", Version: "1", SourceIdentity: "clawhub::@publisher/broken"}
	if _, err := primary.db.ExecContext(ctx, `INSERT INTO `+primary.table("skill_definitions")+`(id, version, source_identity, payload) VALUES($1, $2, $3, '{"id":"broken","version":"1","name":"Broken"}'::jsonb)`, identity.ID, identity.Version, identity.SourceIdentity); err != nil {
		t.Fatal(err)
	}
	got, err := replica.ListSkillDefinitionIdentities(ctx, identity.ID, identity.Version)
	if err != nil || !reflect.DeepEqual(got, []capability.SkillIdentity{identity}) {
		t.Fatalf("identity probe decoded invalid immutable payload = %#v, %v", got, err)
	}
	if definitions, err := replica.ListSkillDefinitionVariants(ctx, identity.ID, identity.Version); err == nil || len(definitions) != 0 {
		t.Fatalf("hydration accepted inconsistent source provenance = %#v, %v", definitions, err)
	}
	if definition, err := skill.NewCatalogWithStore(replica).GetDefinitionVariant(ctx, identity.ID, identity.Version, identity.SourceIdentity); err == nil || definition != nil {
		t.Fatalf("cold catalog accepted invalid immutable payload = %#v, %v", definition, err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if got, err := replica.ListSkillDefinitionIdentities(canceled, identity.ID, identity.Version); !errors.Is(err, context.Canceled) || len(got) != 0 {
		t.Fatalf("canceled replica identity probe = %#v, %v", got, err)
	}
	if err := replica.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := replica.ListSkillDefinitionIdentities(ctx, identity.ID, identity.Version); err == nil || len(got) != 0 {
		t.Fatalf("closed replica identity probe = %#v, %v", got, err)
	}
}

func skillIdentityProbePlanPredicateMatches(condition string) bool {
	// PostgreSQL VERBOSE qualifies references with the relation name. Remove
	// only this exact qualifier and retain both exact indexed predicates.
	condition = strings.ReplaceAll(condition, "skill_definitions.", "")
	return condition == "((id = 'research'::text) AND (version = '1.0.0'::text))"
}

func skillIdentityProbePlanVisibilityBounded(heapFetches int, actualRows, actualLoops float64, expectedRows int) bool {
	return expectedRows > 0 && actualRows == float64(expectedRows) && actualLoops == 1 && heapFetches >= 0 && heapFetches <= expectedRows
}

func TestPostgresSkillDefinitionIdentityProbePlanVisibilityBound(t *testing.T) {
	for _, test := range []struct {
		name        string
		heapFetches int
		actualRows  float64
		actualLoops float64
		want        bool
	}{
		{name: "all-visible", heapFetches: 0, actualRows: 5, actualLoops: 1, want: true},
		{name: "visibility-check-per-row", heapFetches: 5, actualRows: 5, actualLoops: 1, want: true},
		{name: "partly-all-visible", heapFetches: 3, actualRows: 5, actualLoops: 1, want: true},
		{name: "extra-heap-fetch", heapFetches: 6, actualRows: 5, actualLoops: 1},
		{name: "unbounded-heap-scan", heapFetches: 10000, actualRows: 5, actualLoops: 1},
		{name: "negative-heap-fetch", heapFetches: -1, actualRows: 5, actualLoops: 1},
		{name: "unexpected-rows", heapFetches: 5, actualRows: 6, actualLoops: 1},
		{name: "unexecuted-plan", heapFetches: 0, actualRows: 5, actualLoops: 0},
		{name: "repeated-plan", heapFetches: 5, actualRows: 5, actualLoops: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := skillIdentityProbePlanVisibilityBounded(test.heapFetches, test.actualRows, test.actualLoops, 5); got != test.want {
				t.Fatalf("visibility bound = %t, want %t", got, test.want)
			}
		})
	}
}

func TestPostgresSkillDefinitionIdentityProbePlanPredicateQualification(t *testing.T) {
	for _, condition := range []string{
		"((id = 'research'::text) AND (version = '1.0.0'::text))",
		"((skill_definitions.id = 'research'::text) AND (skill_definitions.version = '1.0.0'::text))",
	} {
		if !skillIdentityProbePlanPredicateMatches(condition) {
			t.Fatalf("valid indexed identity predicates were rejected: %s", condition)
		}
	}
	for _, condition := range []string{
		"(skill_definitions.id = 'research'::text)",
		"((foreign.id = 'research'::text) AND (foreign.version = '1.0.0'::text))",
		"((skill_definitions.id = 'foreign'::text) AND (skill_definitions.version = '1.0.0'::text))",
		"((skill_definitions.id = 'research'::text) AND (skill_definitions.version = '2.0.0'::text))",
		"((skill_definitions.id = 'research'::text) OR (skill_definitions.version = '1.0.0'::text))",
	} {
		if skillIdentityProbePlanPredicateMatches(condition) {
			t.Fatalf("unproven indexed identity predicates were accepted: %s", condition)
		}
	}
}
