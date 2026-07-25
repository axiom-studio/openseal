package runtime

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/capability"
)

func TestSensitiveAuthoringPayloadMigrationRedactsProviderFacingText(t *testing.T) {
	now := time.Now().UTC()
	value := &authoring.ChangeSet{
		ID: "sensitive", Scope: capability.ScopeReference{Kind: "tenant", ID: "1"},
		Prompt: "Create an Agent with password: never-store-this", PromptDigest: "old-digest",
		Status: authoring.ChangeSetEvaluating, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Generation: &authoring.ChangeSetGeneration{Request: authoring.GenerateRequest{
			Mode: authoring.ModeCreate, Prompt: "Create an Agent with password: never-store-this",
		}},
		Refinement: authoring.ChangeSetRefinement{Answers: []authoring.RefinementAnswerEvent{{
			Value: authoring.RefinementAnswerValue{Text: "API key: sk-1234567890abcdefghijklmnop", Items: []string{"token=top-secret-token"}},
		}}},
		Result: authoring.CompileResult{Candidate: authoring.WorkforceCandidate{Agents: []*agent.AgentDefinition{{
			ID: "sensitive-agent", Version: "1", DisplayName: "Agent", Purpose: "Work safely",
			SystemPrompt: "Authenticate with password: copied-provider-secret",
			Authority:    agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1},
		}}}}, CandidateDigest: "old-candidate-digest",
		Evaluations:  []authoring.ChangeSetEvaluation{{CandidateDigest: "old-candidate-digest"}},
		ApplyReceipt: &authoring.ChangeSetApplyReceipt{CandidateDigest: "old-candidate-digest"},
	}
	payload, _ := json.Marshal(value)
	redacted, changed, err := sanitizeAuthoringChangeSetPayload(string(payload))
	if err != nil || !changed || strings.Contains(redacted, "never-store-this") || strings.Contains(redacted, "sk-123") || strings.Contains(redacted, "top-secret-token") || strings.Contains(redacted, "copied-provider-secret") {
		t.Fatalf("redacted payload changed=%v err=%v payload=%s", changed, err, redacted)
	}
	var decoded authoring.ChangeSet
	if err := json.Unmarshal([]byte(redacted), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.PromptDigest == "old-digest" || !strings.Contains(decoded.Prompt, "password: [REDACTED]") ||
		!strings.Contains(decoded.Generation.Request.Prompt, "password: [REDACTED]") || decoded.CandidateDigest == "old-candidate-digest" ||
		decoded.Evaluations[0].CandidateDigest != decoded.CandidateDigest || decoded.ApplyReceipt.CandidateDigest != decoded.CandidateDigest ||
		decoded.Result.Candidate.Agents[0].Digest == "" {
		t.Fatalf("decoded migration = %#v", decoded)
	}
}

func TestSQLiteStartupRedactsLegacySensitiveAuthoringPrompt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
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
	if _, err := store.db.Exec(`INSERT INTO agent_definitions(id,version,digest,created_at,payload) VALUES(?,?,?,?,?)`, definition.ID, definition.Version, definition.Digest, now, string(definitionPayload)); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(value)
	if _, err := store.db.Exec(`INSERT INTO workforce_change_sets
		(scope_kind,scope_id,id,parent_id,status,revision,idempotency_key,request_digest,candidate_digest,created_at,updated_at,payload)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, "tenant", "1", value.ID, "", value.Status, 1, "legacy-key", "request", "candidate", now, now, string(payload)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restored, err := restarted.GetChangeSet(t.Context(), value.Scope, value.ID)
	if err != nil || strings.Contains(restored.Prompt, "erase-me-now") || !strings.Contains(restored.Prompt, "[REDACTED]") {
		t.Fatalf("restored=%#v err=%v", restored, err)
	}
	if got := restored.Result.Candidate.Agents[0].SystemPrompt; strings.Contains(got, "erase-definition-secret") || got == definition.SystemPrompt {
		t.Fatalf("ChangeSet candidate Agent was not hardened: %q", got)
	}
	var stored string
	if err := restarted.db.QueryRow(`SELECT payload FROM workforce_change_sets WHERE scope_kind=? AND scope_id=? AND id=?`, "tenant", "1", value.ID).Scan(&stored); err != nil || strings.Contains(stored, "erase-me-now") {
		t.Fatalf("durable payload still sensitive: err=%v payload=%s", err, stored)
	}
	restoredDefinition, err := restarted.GetDefinition(t.Context(), definition.ID, definition.Version)
	if err != nil || strings.Contains(restoredDefinition.SystemPrompt, "erase-definition-secret") || restoredDefinition.SystemPrompt == definition.SystemPrompt || restoredDefinition.Digest == definition.Digest {
		t.Fatalf("durable Agent definition was not safely migrated: definition=%#v err=%v", restoredDefinition, err)
	}
}
