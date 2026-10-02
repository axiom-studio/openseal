package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func BenchmarkSkillRuntimeMaintenanceMetadataTerminalHistory(b *testing.B) {
	for _, size := range []int{100, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			store, err := NewSQLiteStore(b.TempDir() + "/maintenance-metadata.db")
			if err != nil {
				b.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			scope := Scope{Kind: "tenant", ID: "maintenance-benchmark"}
			run, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, Goal: "Completed historical work", Source: RunSourceManual})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE agent_runs SET status='completed' WHERE scope_kind=? AND scope_id=? AND id=?`, scope.Kind, scope.ID, run.ID); err != nil {
				b.Fatal(err)
			}
			tx, err := store.db.Begin()
			if err != nil {
				b.Fatal(err)
			}
			history, err := tx.Prepare(`INSERT INTO action_calls(id,scope_kind,scope_id,run_id,status,available_at,revision,created_at,payload,deployment_id,binding_id,binding_revision,skill_id,skill_version) VALUES(?,?,?,?,'succeeded',?,1,?,?,'agent','account',1,'release','1')`)
			if err != nil {
				b.Fatal(err)
			}
			refs, err := tx.Prepare(`INSERT INTO skill_bindings(scope_kind,scope_id,deployment_id,id,skill_id,skill_version,source_identity,revision,payload) VALUES(?,?,'agent',?,'release','1','native::release',1,'{"disabled":true}')`)
			if err != nil {
				b.Fatal(err)
			}
			// Invalid historical output makes accidental action payload decoding
			// fail instead of disguising an unbounded metadata probe.
			payload := "invalid JSON " + strings.Repeat("terminal output ", 128)
			for i := 0; i < size; i++ {
				id := fmt.Sprint(i)
				if _, err := history.Exec(id, scope.Kind, scope.ID, run.ID, time.Now(), time.Now(), payload); err != nil {
					b.Fatal(err)
				}
				if _, err := refs.Exec(scope.Kind, scope.ID, id); err != nil {
					b.Fatal(err)
				}
			}
			if err := history.Close(); err != nil {
				b.Fatal(err)
			}
			if err := refs.Close(); err != nil {
				b.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				b.Fatal(err)
			}
			b.Run("logical_usage", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if busy, err := store.HasSkillRuntimeMaintenanceUsage(ctx, scope, "release"); err != nil || busy {
						b.Fatalf("terminal history remained busy: %v %v", busy, err)
					}
				}
			})
			b.Run("absent_gate", func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if gate, err := store.GetSkillRuntimeMaintenance(ctx, scope, "release"); gate != nil || !errors.Is(err, ErrSkillRuntimeMaintenanceNotFound) {
						b.Fatalf("unexpected gate: %#v %v", gate, err)
					}
				}
			})
		})
	}
}
