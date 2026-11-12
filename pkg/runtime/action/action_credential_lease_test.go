package action_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/action/actiontest"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type testCredentialLeaseRedeemer struct {
	mu        sync.Mutex
	seen      map[string]bool
	authorize func(models.ActionCredentialLeaseRedemptionRequest) error
}

func (g *testCredentialLeaseRedeemer) RedeemActionCredentialLease(_ context.Context, request models.ActionCredentialLeaseRedemptionRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authorize != nil {
		if err := g.authorize(request); err != nil {
			return err
		}
	}
	key := request.Lease.Issuer + "\x00" + request.Lease.Audience + "\x00" + request.Lease.Nonce
	if g.seen[key] {
		return models.ErrActionCredentialLeaseReplay
	}
	g.seen[key] = true
	return nil
}

func TestSignedActionCredentialLeaseBindsExactDurableAuthority(t *testing.T) {
	now, call, run, fields := actiontest.CredentialLeaseFixture()
	lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := actiontest.CredentialLeaseSigner{Key: []byte("lease-signing-key")}
	envelope, err := action.SignActionCredentialLease(t.Context(), *lease, signer)
	if err != nil {
		t.Fatal(err)
	}
	redeemer := &testCredentialLeaseRedeemer{seen: map[string]bool{}, authorize: func(request models.ActionCredentialLeaseRedemptionRequest) error {
		return models.MatchActionCredentialLease(request.Lease, call, run, request.Transport, request.CredentialFields)
	}}
	validator, err := action.NewCredentialLeaseValidator(signer, redeemer)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := validator.Validate(t.Context(), action.CredentialLeaseValidationRequest{
		Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(time.Second),
		Transport: actiontest.CredentialLeaseTransport, CredentialFields: fields,
	})
	if err != nil {
		t.Fatal(err)
	}
	if validated.ActionLease.ID == "" || validated.ActionLease.ActionCallRevision != call.Revision || validated.BindingOwnerID != "team-marketing" || validated.AssignedAgentID != "agent-researcher" || len(validated.Credentials) != 1 || validated.Credentials[0].Fields[0] != "api.token" || validated.Credentials[0].Reference.ID != "opaque://tenant-7/reddit" {
		t.Fatalf("validated lease = %+v", validated)
	}
	if _, err := validator.Validate(t.Context(), action.CredentialLeaseValidationRequest{
		Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(2 * time.Second),
		Transport: actiontest.CredentialLeaseTransport, CredentialFields: fields,
	}); err == nil {
		t.Fatal("one-time credential lease replay was accepted")
	}
}

func TestActionCredentialLeaseValidationFailsClosed(t *testing.T) {
	now, call, run, fields := actiontest.CredentialLeaseFixture()
	newEnvelope := func(t *testing.T) (*models.SignedActionCredentialLease, actiontest.CredentialLeaseSigner) {
		t.Helper()
		lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
			TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport,
			Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000002",
		})
		if err != nil {
			t.Fatal(err)
		}
		signer := actiontest.CredentialLeaseSigner{Key: []byte("lease-signing-key")}
		envelope, err := action.SignActionCredentialLease(t.Context(), *lease, signer)
		if err != nil {
			t.Fatal(err)
		}
		return envelope, signer
	}
	request := func(envelope *models.SignedActionCredentialLease) action.CredentialLeaseValidationRequest {
		return action.CredentialLeaseValidationRequest{Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(time.Second), Transport: actiontest.CredentialLeaseTransport, CredentialFields: fields}
	}
	for _, test := range []struct {
		name   string
		mutate func(*models.SignedActionCredentialLease, *action.CredentialLeaseValidationRequest)
	}{
		{"cross tenant", func(_ *models.SignedActionCredentialLease, req *action.CredentialLeaseValidationRequest) {
			req.TenantID = "tenant-8"
		}},
		{"wrong scope", func(_ *models.SignedActionCredentialLease, req *action.CredentialLeaseValidationRequest) {
			req.Scope.ID = "tenant-8"
		}},
		{"wrong issuer", func(_ *models.SignedActionCredentialLease, req *action.CredentialLeaseValidationRequest) {
			req.TrustedIssuer = "other-control-plane"
		}},
		{"wrong audience", func(_ *models.SignedActionCredentialLease, req *action.CredentialLeaseValidationRequest) {
			req.Audience = "other-host"
		}},
		{"expired", func(_ *models.SignedActionCredentialLease, req *action.CredentialLeaseValidationRequest) {
			req.Now = now.Add(2 * time.Minute)
		}},
		{"signature drift", func(envelope *models.SignedActionCredentialLease, _ *action.CredentialLeaseValidationRequest) {
			envelope.Lease.Action = "delete"
		}},
		{"lease identity drift", func(envelope *models.SignedActionCredentialLease, _ *action.CredentialLeaseValidationRequest) {
			envelope.Lease.ActionLease.Attempt++
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, signer := newEnvelope(t)
			req := request(envelope)
			test.mutate(envelope, &req)
			validator, err := action.NewCredentialLeaseValidator(signer, &testCredentialLeaseRedeemer{seen: map[string]bool{}, authorize: func(request models.ActionCredentialLeaseRedemptionRequest) error {
				return models.MatchActionCredentialLease(request.Lease, call, run, request.Transport, request.CredentialFields)
			}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := validator.Validate(t.Context(), req); err == nil {
				t.Fatal("invalid signed credential lease was accepted")
			}
		})
	}
}

func TestActionCredentialLeaseAuthorityRejectsSignedExtraFieldsAndBindingDrift(t *testing.T) {
	now, call, run, fields := actiontest.CredentialLeaseFixture()
	lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000003",
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := actiontest.CredentialLeaseSigner{Key: []byte("lease-signing-key")}
	for _, test := range []struct {
		name   string
		mutate func(*models.ActionCredentialLease, *models.ActionCall)
	}{
		{"extra credential field", func(candidate *models.ActionCredentialLease, _ *models.ActionCall) {
			candidate.Credentials[0].Fields = append(candidate.Credentials[0].Fields, "api.unexpected")
		}},
		{"binding revision drift", func(_ *models.ActionCredentialLease, durable *models.ActionCall) { durable.BindingRevision++ }},
		{"assigned agent drift", func(candidate *models.ActionCredentialLease, _ *models.ActionCall) {
			candidate.AssignedAgentID = "agent-other"
		}},
		{"transport drift", func(candidate *models.ActionCredentialLease, _ *models.ActionCall) {
			candidate.Transport = "other_tool"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, durable := *lease, models.CloneActionCall(call)
			candidate.Credentials = models.CloneActionCredentialFieldReferences(lease.Credentials)
			test.mutate(&candidate, durable)
			envelope, err := action.SignActionCredentialLease(t.Context(), candidate, signer)
			if err != nil {
				t.Fatal(err)
			}
			validator, _ := action.NewCredentialLeaseValidator(signer, &testCredentialLeaseRedeemer{seen: map[string]bool{}, authorize: func(request models.ActionCredentialLeaseRedemptionRequest) error {
				return models.MatchActionCredentialLease(request.Lease, durable, run, request.Transport, request.CredentialFields)
			}})
			if _, err := validator.Validate(t.Context(), action.CredentialLeaseValidationRequest{Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(time.Second), Transport: actiontest.CredentialLeaseTransport, CredentialFields: fields}); err == nil {
				t.Fatal("signed authority drift was accepted")
			}
		})
	}
}

func TestActionCredentialLeaseAuthorityAcceptsHeartbeatButRejectsReclaim(t *testing.T) {
	now, call, run, fields := actiontest.CredentialLeaseFixture()
	lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000006",
	})
	if err != nil {
		t.Fatal(err)
	}
	renewed := models.CloneActionCall(call)
	renewed.Revision += 2
	renewedExpiry := call.LeaseExpiresAt.Add(time.Minute)
	renewed.LeaseExpiresAt = &renewedExpiry
	if err := models.MatchActionCredentialLease(*lease, renewed, run, actiontest.CredentialLeaseTransport, fields); err != nil {
		t.Fatalf("same-owner heartbeat renewal was rejected: %v", err)
	}

	reclaimed := models.CloneActionCall(renewed)
	reclaimed.Attempt++
	reclaimed.LeaseOwner = "runtime-worker-2"
	if err := models.MatchActionCredentialLease(*lease, reclaimed, run, actiontest.CredentialLeaseTransport, fields); err == nil {
		t.Fatal("reclaimed ActionCall accepted an earlier credential lease")
	}
}

func TestNewActionCredentialLeaseRejectsIncompleteOrOverbroadRequests(t *testing.T) {
	now, call, run, fields := actiontest.CredentialLeaseFixture()
	valid := func() models.CreateActionCredentialLeaseRequest {
		return models.CreateActionCredentialLeaseRequest{TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport, Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000004"}
	}
	for _, test := range []struct {
		name   string
		mutate func(*models.CreateActionCredentialLeaseRequest)
	}{
		{"unclaimed action", func(req *models.CreateActionCredentialLeaseRequest) {
			req.Call = models.CloneActionCall(call)
			req.Call.LeaseOwner = ""
		}},
		{"wrong run", func(req *models.CreateActionCredentialLeaseRequest) {
			req.Run = models.CloneAgentRun(run)
			req.Run.ID = "run-other"
		}},
		{"missing fields", func(req *models.CreateActionCredentialLeaseRequest) { req.CredentialFields = map[string][]string{} }},
		{"missing transport", func(req *models.CreateActionCredentialLeaseRequest) { req.Transport = "" }},
		{"short nonce", func(req *models.CreateActionCredentialLeaseRequest) { req.Nonce = "short" }},
		{"extra credential", func(req *models.CreateActionCredentialLeaseRequest) {
			req.CredentialFields = map[string][]string{"reddit": {"api.token"}, "other": {"token"}}
		}},
		{"expiry beyond action lease", func(req *models.CreateActionCredentialLeaseRequest) { req.ExpiresAt = now.Add(3 * time.Minute) }},
		{"overlong ttl", func(req *models.CreateActionCredentialLeaseRequest) {
			expiry := now.Add(10 * time.Minute)
			req.Call = models.CloneActionCall(call)
			req.Call.LeaseExpiresAt = &expiry
			req.ExpiresAt = expiry
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := valid()
			test.mutate(&req)
			if _, err := models.NewActionCredentialLease(req); err == nil {
				t.Fatal("invalid credential lease request was accepted")
			}
		})
	}
}

func TestActionCredentialLeaseContainsOnlyOpaqueReferences(t *testing.T) {
	now, call, run, fields := actiontest.CredentialLeaseFixture()
	lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport, Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000005"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := action.CanonicalActionCredentialLeasePayload(*lease)
	if string(encoded) == "" || containsAny(string(encoded), "resolved-secret-value", "plaintext", "apiKeyValue") {
		t.Fatalf("credential lease contains a resolved value: %s", encoded)
	}
}

func TestActionCredentialLeaseBindingMayCarryCredentialsForOtherCapabilities(t *testing.T) {
	_, call, _, _ := actiontest.CredentialLeaseFixture()
	call.Risk = skill.RiskLevelRead
	binding := &skill.Binding{
		ID: call.BindingID, Scope: skill.ScopeReference{Kind: call.Scope.Kind, ID: call.Scope.ID},
		DeploymentID: call.DeploymentID, SkillID: call.SkillID, SkillVersion: call.SkillVersion,
		Revision: call.BindingRevision, AllowedActions: []string{call.Action}, MaximumRisk: skill.RiskLevelDestructive,
		Credentials: map[string]skill.CredentialReference{
			"reddit":         call.CredentialRefs["reddit"],
			"SIGNING_SECRET": {Kind: "slack_signing_secret", ID: "credential://007 Slack.signing_secret"},
		},
	}
	lease := models.ActionCredentialLease{
		Scope: call.Scope, SkillID: call.SkillID, SkillVersion: call.SkillVersion, Action: call.Action,
		BindingOwnerID: call.DeploymentID, BindingID: call.BindingID, BindingRevision: call.BindingRevision,
	}
	if err := models.CheckCurrentActionCredentialBinding(lease, call, binding); err != nil {
		t.Fatalf("binding with an unrelated adapter credential was rejected: %v", err)
	}
	binding.Credentials["reddit"] = skill.CredentialReference{Kind: "reddit-oauth", ID: "credential://other.oauth"}
	if err := models.CheckCurrentActionCredentialBinding(lease, call, binding); !errors.Is(err, models.ErrActionCredentialLeaseMismatch) {
		t.Fatalf("selected credential drift error = %v, want %v", err, models.ErrActionCredentialLeaseMismatch)
	}
}

func TestActionCredentialLeaseAcceptsOpaqueReferencesWithSpaces(t *testing.T) {
	now, call, run, fields := actiontest.CredentialLeaseFixture()
	call.CredentialRefs["reddit"] = skill.CredentialReference{Kind: "slack_bot_token", ID: "credential://007 Slack.bot_token"}
	lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000006",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.Credentials[0].Reference.ID; got != "credential://007 Slack.bot_token" {
		t.Fatalf("credential reference = %q", got)
	}
}

func TestActionCredentialLeaseRejectsNonCanonicalOpaqueReferences(t *testing.T) {
	for _, referenceID := range []string{" credential://credential.token", "credential://credential.token ", "credential://credential\ntoken"} {
		t.Run(referenceID, func(t *testing.T) {
			now, call, run, fields := actiontest.CredentialLeaseFixture()
			call.CredentialRefs["reddit"] = skill.CredentialReference{Kind: "token", ID: referenceID}
			if _, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
				TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: actiontest.CredentialLeaseTransport,
				Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000007",
			}); err == nil {
				t.Fatal("non-canonical opaque credential reference was accepted")
			}
		})
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
