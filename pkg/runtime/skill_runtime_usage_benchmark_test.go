package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func BenchmarkSkillRuntimeUsageTerminalHistory(b *testing.B) {
	for _, size := range []int{100, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			store, err := NewSQLiteStore(b.TempDir() + "/history.db")
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			scope := Scope{Kind: "tenant", ID: "benchmark"}
			run, err := NewPortfolioService(store).CreateAgentRun(context.Background(), CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Historical finished task", Source: RunSourceManual})
			if err != nil {
				b.Fatal(err)
			}
			if _, err = store.db.Exec(`UPDATE agent_runs SET status='completed' WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, run.ID); err != nil {
				b.Fatal(err)
			}
			tx, err := store.db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			statement, err := tx.Prepare(`INSERT INTO action_calls(id,scope_kind,scope_id,run_id,status,available_at,revision,created_at,payload,deployment_id,binding_id,binding_revision,skill_id,skill_version) VALUES(?,?,?,?,'succeeded',?,1,?,?,'agent','account',1,'skill','1')`)
			if err != nil {
				b.Fatal(err)
			}
			payload := "invalid JSON " + strings.Repeat("historical-output ", 128)
			for i := 0; i < size; i++ {
				if _, err = statement.Exec(fmt.Sprint(i), scope.Kind, scope.ID, run.ID, time.Now(), time.Now(), payload); err != nil {
					b.Fatal(err)
				}
			}
			if err = statement.Close(); err != nil {
				b.Fatal(err)
			}
			if err = tx.Commit(); err != nil {
				b.Fatal(err)
			}
			b.Run("usage", func(b *testing.B) {
				filter := SkillRuntimeUsageFilter{Scope: scope, SkillID: "skill", SkillVersion: "1", DeploymentID: "agent", BindingID: "account"}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if busy, err := store.HasSkillRuntimeUsage(context.Background(), filter); err != nil || busy {
						b.Fatalf("unexpected usage: %v %v", busy, err)
					}
				}
			})
			b.Run("retention", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if refs, err := store.ListReferencedSkillRuntimeVersions(context.Background(), scope); err != nil || len(refs) != 0 {
						b.Fatalf("unexpected references: %#v %v", refs, err)
					}
				}
			})
		})
	}
}
