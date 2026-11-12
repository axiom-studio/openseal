package actiontest

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// CredentialLeaseFixture is a running loop call of a Team's Reddit search, its
// Run, the time the lease is issued and the credential fields it needs.
func CredentialLeaseFixture() (time.Time, *models.ActionCall, *models.AgentRun, map[string][]string) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	expires := now.Add(2 * time.Minute)
	scope := models.Scope{Kind: "tenant", ID: "tenant-7"}
	call := &models.ActionCall{
		ID: "action-1", Scope: scope, RunID: "run-1", TurnID: "step-1", DeploymentID: "team-marketing", BindingID: "reddit-binding", BindingRevision: 3,
		Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop,
		SkillID: "reddit-research", SkillVersion: "1.2.0", Action: "search", Status: models.ActionCallStatusRunning,
		CredentialRefs: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "opaque://tenant-7/reddit"}},
		Attempt:        1, MaxAttempts: 3, LeaseOwner: "runtime-worker-1", LeaseExpiresAt: &expires, Revision: 4,
	}
	run := &models.AgentRun{ID: "run-1", Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team-marketing"}, AssignedAgentID: "agent-researcher"}
	return now, call, run, map[string][]string{"reddit": {"api.token"}}
}

// CredentialLeaseTransport is the tool transport the fixture's lease names.
const CredentialLeaseTransport = "reddit_search"

// CredentialLeaseSigner signs and verifies leases with HMAC-SHA256 over Key.
type CredentialLeaseSigner struct{ Key []byte }

func (s CredentialLeaseSigner) SignActionCredentialLease(_ context.Context, payload []byte) (models.ActionCredentialLeaseSignature, error) {
	mac := hmac.New(sha256.New, s.Key)
	_, _ = mac.Write(payload)
	return models.ActionCredentialLeaseSignature{Algorithm: "hmac-sha256", KeyID: "test-key", Value: mac.Sum(nil)}, nil
}

func (s CredentialLeaseSigner) VerifyActionCredentialLease(_ context.Context, payload []byte, signature models.ActionCredentialLeaseSignature) error {
	expected, _ := s.SignActionCredentialLease(context.Background(), payload)
	if signature.Algorithm != expected.Algorithm || signature.KeyID != expected.KeyID || !hmac.Equal(signature.Value, expected.Value) {
		return errors.New("invalid signature")
	}
	return nil
}
