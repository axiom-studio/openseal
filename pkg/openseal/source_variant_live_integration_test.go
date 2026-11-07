//go:build integration

package openseal

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/executor"
	sdkresolver "github.com/axiom-studio/skills.sdk/resolver"
	_ "github.com/lib/pq"
)

func executeLiveSourceVariantPrompt(t *testing.T, ctx context.Context, apiKey, baseURL, model, instructions, marker string) string {
	t.Helper()
	step := &executor.StepDefinition{Id: "live-source-variant", Type: executor.NodeTypeAI, Config: map[string]interface{}{
		"provider": "openai-compatible", "model": model, "apiKey": apiKey, "baseUrl": baseURL,
		"systemPrompt": instructions,
		"prompt":       "In one short sentence confirm that the exact governed research policy was selected. Preserve its required route marker.",
		"temperature":  float64(0), "maxTokens": float64(120),
	}}
	result, err := executor.NewAIExecutor().Execute(ctx, step, executor.NewResolver(sdkresolver.Config{}))
	if err != nil {
		t.Fatal(err)
	}
	response, _ := result.Output["response"].(string)
	if !strings.Contains(response, marker) {
		t.Fatalf("model response did not preserve exact route marker %s (response length %d)", marker, len(response))
	}
	return response
}

func assertSourceIdentitiesAbsent(t *testing.T, variants []struct {
	bindingID      string
	sourceIdentity string
	publisher      string
	transport      string
	marker         string
}, values ...interface{}) {
	t.Helper()
	encoded, err := json.Marshal(values)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range variants {
		if bytes.Contains(encoded, []byte(variant.sourceIdentity)) {
			t.Fatalf("source identity leaked into model-visible or run state for binding %s", variant.bindingID)
		}
	}
}

func dropLiveTestSchema(t *testing.T, dsn, schema string) {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open PostgreSQL cleanup connection: %v", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`); err != nil {
		t.Fatalf("drop live test schema: %v", err)
	}
}
