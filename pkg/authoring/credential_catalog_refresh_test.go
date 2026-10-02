package authoring

import (
	"encoding/json"
	"github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"testing"
	"time"
)

func TestPlacementRefreshesHostCredentialFactsWithoutRegeneratingProposal(t *testing.T) {
	requirement := &capability.OAuth2Requirement{Provider: "example", Subject: "user", Scopes: []string{"mail.read"}}
	catalog := CapabilityCatalog{Skills: map[string]SkillCapability{"mail": {ID: "mail", Version: "1.0.0", Actions: []string{"read"}, Readiness: SkillReadinessReady, Credentials: []SkillCredential{{Name: "mail_account", Kind: "oauth-account", OAuth2: requirement, Actions: []string{"read"}}}}}}
	candidate := WorkforceCandidate{Activation: WorkforceActivationActive, Agents: []*agent.AgentDefinition{{ID: "mail-agent", Version: "1", SkillRequirements: []agent.SkillRequirement{{SkillID: "mail", RequiredActions: []string{"read"}}}}}}
	scope := capability.ScopeReference{Kind: "tenant", ID: "test"}
	store := NewMemoryChangeSetStore()
	initial := &ChangeSet{ID: "proposal", Scope: scope, Revision: 1, Status: ChangeSetBlocked, CandidateDigest: "reviewed-digest", Catalog: catalog, Result: CompileResult{Candidate: candidate, MissingRequirements: missingRequirements(&candidate, catalog)}}
	if _, _, err := store.CreateChangeSet(t.Context(), initial, "create", "digest"); err != nil {
		t.Fatal(err)
	}
	service := &ChangeSetService{store: store, now: time.Now}
	placement := ChangeSetPlacement{CredentialReferences: map[string]map[string]capability.CredentialReference{"mail-agent": {"mail_account": {Kind: "oauth-account", ID: "vault://account"}}}}
	update := UpdateChangeSetPlacementRequest{Scope: scope, ChangeSetID: initial.ID, ExpectedRevision: 1, Placement: placement, Reason: "Save authorized account", Actor: ChangeSetActor{Type: "user", ID: "7"}, IdempotencyKey: "connected"}
	// A reference alone cannot prove a grant; the frozen pre-sign-in facts stay blocked.
	unchanged, _, err := service.UpdatePlacement(t.Context(), update)
	if err != nil || unchanged.Status != ChangeSetBlocked {
		t.Fatalf("reference alone: %+v, %v", unchanged, err)
	}
	update.ExpectedRevision = unchanged.Revision
	update.IdempotencyKey = "refresh"
	update.CredentialCatalog = &CapabilityCatalog{AvailableCredentials: map[string]bool{"mail_account": true}, AvailableCredentialGrants: map[string][]capability.OAuth2GrantSummary{"mail_account": {{Provider: "example", Subject: "user", Scopes: []string{"mail.read"}}}}, Skills: map[string]SkillCapability{"unreviewed": {ID: "unreviewed"}}}
	updated, replayed, err := service.UpdatePlacement(t.Context(), update)
	if err != nil || replayed || updated.Status != ChangeSetReview || !updated.Result.Valid || len(updated.Result.MissingRequirements) != 0 {
		t.Fatalf("refresh: %+v, %v", updated, err)
	}
	if updated.ID != initial.ID || updated.CandidateDigest != initial.CandidateDigest || updated.Result.Candidate.Agents[0].ID != "mail-agent" || updated.Catalog.Skills["mail"].Version != "1.0.0" || len(updated.Catalog.Skills) != 1 {
		t.Fatal("refresh changed the proposal or reviewed skills")
	}
	if again, replay, err := service.UpdatePlacement(t.Context(), update); err != nil || !replay || again.Revision != updated.Revision {
		t.Fatalf("replay: %+v %v", again, err)
	}
	update.ExpectedRevision = updated.Revision
	update.IdempotencyKey = "revoked"
	update.CredentialCatalog = &CapabilityCatalog{}
	revoked, _, err := service.UpdatePlacement(t.Context(), update)
	if err != nil || revoked.Status != ChangeSetBlocked {
		t.Fatalf("revocation not blocked: %+v %v", revoked, err)
	}
}

func TestPlacementCredentialCatalogCannotBeSuppliedByJSON(t *testing.T) {
	var req UpdateChangeSetPlacementRequest
	if err := json.Unmarshal([]byte(`{"credentialCatalog":{"availableCredentials":{"mail_account":true}}}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.CredentialCatalog != nil {
		t.Fatal("client supplied host-owned credential facts")
	}
}

func TestProposalContinuationRetainsTargetAcrossGenerationFailures(t *testing.T) {
	if got := proposalContinuationCandidate(&ChangeSet{Status: ChangeSetFailed}); got != nil {
		t.Fatal("failed initial generation incorrectly treated as a reviewed candidate")
	}
	target := WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "original", Version: "1"}}}
	failed := &ChangeSet{Status: ChangeSetFailed, Generation: &ChangeSetGeneration{Request: GenerateRequest{Existing: &target}}}
	got := proposalContinuationCandidate(failed)
	if got == nil || len(got.Agents) != 1 || got.Agents[0].ID != "original" {
		t.Fatal("failed amendment lost its target")
	}
	got.Agents[0].ID = "changed"
	if target.Agents[0].ID != "original" {
		t.Fatal("continuation mutated the original generation intent")
	}
	reviewed := &ChangeSet{Result: CompileResult{Candidate: target}}
	if proposalContinuationCandidate(reviewed).Agents[0].ID != "original" {
		t.Fatal("reviewed target not retained")
	}
}

func TestFailedCreationRecoveryKeepsLineageAndGenerationMode(t *testing.T) {
	for _, amend := range []bool{false, true} {
		t.Run(map[bool]string{false: "initial", true: "amendment"}[amend], func(t *testing.T) {
			store := NewMemoryChangeSetStore()
			scope := capability.ScopeReference{Kind: "tenant", ID: "test"}
			actor := ChangeSetActor{Type: "user", ID: "7"}
			parent := &ChangeSet{ID: "failed-request", Scope: scope, Actor: actor, Status: ChangeSetFailed, Revision: 1, Generation: &ChangeSetGeneration{Request: GenerateRequest{}}}
			if amend {
				parent.Generation.Request.Existing = &WorkforceCandidate{Agents: []*agent.AgentDefinition{{ID: "same-agent", Version: "1"}}}
			}
			if _, _, err := store.CreateChangeSet(t.Context(), parent, "parent", "digest"); err != nil {
				t.Fatal(err)
			}
			service := &ChangeSetService{store: store, now: time.Now}
			next, _, err := service.Prepare(t.Context(), CreateChangeSetRequest{Scope: scope, ParentID: parent.ID, Prompt: "Revise my saved request", Catalog: CapabilityCatalog{Skills: map[string]SkillCapability{}}, Actor: actor, IdempotencyKey: "recover"})
			if err != nil {
				t.Fatal(err)
			}
			if next.ParentID != parent.ID {
				t.Fatal("recovery created an unrelated setup")
			}
			if amend && (next.Mode != ModeAmend || next.Generation.Request.Existing == nil || next.Generation.Request.Existing.Agents[0].ID != "same-agent") {
				t.Fatal("failed amendment lost its reviewed target")
			}
			if !amend && (next.Mode != ModeCreate || next.Generation.Request.Existing != nil) {
				t.Fatal("failed initial request was treated as an empty amendment")
			}
		})
	}
}
