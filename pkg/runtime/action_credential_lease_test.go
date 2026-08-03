package runtime

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/skill"
)

type testCredentialLeaseSigner struct{ key []byte }

const testActionCredentialLeaseTransport = "reddit_search"

func (s testCredentialLeaseSigner) SignActionCredentialLease(_ context.Context, payload []byte) (ActionCredentialLeaseSignature, error) {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write(payload)
	return ActionCredentialLeaseSignature{Algorithm: "hmac-sha256", KeyID: "test-key", Value: mac.Sum(nil)}, nil
}

func (s testCredentialLeaseSigner) VerifyActionCredentialLease(_ context.Context, payload []byte, signature ActionCredentialLeaseSignature) error {
	expected, _ := s.SignActionCredentialLease(context.Background(), payload)
	if signature.Algorithm != expected.Algorithm || signature.KeyID != expected.KeyID || !hmac.Equal(signature.Value, expected.Value) {
		return errors.New("invalid signature")
	}
	return nil
}

type testCredentialLeaseRedeemer struct {
	mu        sync.Mutex
	seen      map[string]bool
	authorize func(ActionCredentialLeaseRedemptionRequest) error
}

func (g *testCredentialLeaseRedeemer) RedeemActionCredentialLease(_ context.Context, request ActionCredentialLeaseRedemptionRequest) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.authorize != nil {
		if err := g.authorize(request); err != nil {
			return err
		}
	}
	key := request.Lease.Issuer + "\x00" + request.Lease.Audience + "\x00" + request.Lease.Nonce
	if g.seen[key] {
		return ErrActionCredentialLeaseReplay
	}
	g.seen[key] = true
	return nil
}

func TestSignedActionCredentialLeaseBindsExactDurableAuthority(t *testing.T) {
	now, call, run, fields := actionCredentialLeaseFixture()
	lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := testCredentialLeaseSigner{key: []byte("lease-signing-key")}
	envelope, err := SignActionCredentialLease(t.Context(), *lease, signer)
	if err != nil {
		t.Fatal(err)
	}
	redeemer := &testCredentialLeaseRedeemer{seen: map[string]bool{}, authorize: func(request ActionCredentialLeaseRedemptionRequest) error {
		return MatchActionCredentialLease(request.Lease, call, run, request.Transport, request.CredentialFields)
	}}
	validator, err := NewActionCredentialLeaseValidator(signer, redeemer)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := validator.Validate(t.Context(), ActionCredentialLeaseValidationRequest{
		Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(time.Second),
		Transport: testActionCredentialLeaseTransport, CredentialFields: fields,
	})
	if err != nil {
		t.Fatal(err)
	}
	if validated.ActionLease.ID == "" || validated.ActionLease.ActionCallRevision != call.Revision || validated.BindingOwnerID != "team-marketing" || validated.AssignedAgentID != "agent-researcher" || len(validated.Credentials) != 1 || validated.Credentials[0].Fields[0] != "api.token" || validated.Credentials[0].Reference.ID != "opaque://tenant-7/reddit" {
		t.Fatalf("validated lease = %+v", validated)
	}
	if _, err := validator.Validate(t.Context(), ActionCredentialLeaseValidationRequest{
		Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(2 * time.Second),
		Transport: testActionCredentialLeaseTransport, CredentialFields: fields,
	}); err == nil {
		t.Fatal("one-time credential lease replay was accepted")
	}
}

func TestActionCredentialLeaseValidationFailsClosed(t *testing.T) {
	now, call, run, fields := actionCredentialLeaseFixture()
	newEnvelope := func(t *testing.T) (*SignedActionCredentialLease, testCredentialLeaseSigner) {
		t.Helper()
		lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
			TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport,
			Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000002",
		})
		if err != nil {
			t.Fatal(err)
		}
		signer := testCredentialLeaseSigner{key: []byte("lease-signing-key")}
		envelope, err := SignActionCredentialLease(t.Context(), *lease, signer)
		if err != nil {
			t.Fatal(err)
		}
		return envelope, signer
	}
	request := func(envelope *SignedActionCredentialLease) ActionCredentialLeaseValidationRequest {
		return ActionCredentialLeaseValidationRequest{Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(time.Second), Transport: testActionCredentialLeaseTransport, CredentialFields: fields}
	}
	for _, test := range []struct {
		name   string
		mutate func(*SignedActionCredentialLease, *ActionCredentialLeaseValidationRequest)
	}{
		{"cross tenant", func(_ *SignedActionCredentialLease, req *ActionCredentialLeaseValidationRequest) {
			req.TenantID = "tenant-8"
		}},
		{"wrong scope", func(_ *SignedActionCredentialLease, req *ActionCredentialLeaseValidationRequest) {
			req.Scope.ID = "tenant-8"
		}},
		{"wrong issuer", func(_ *SignedActionCredentialLease, req *ActionCredentialLeaseValidationRequest) {
			req.TrustedIssuer = "other-control-plane"
		}},
		{"wrong audience", func(_ *SignedActionCredentialLease, req *ActionCredentialLeaseValidationRequest) {
			req.Audience = "other-host"
		}},
		{"expired", func(_ *SignedActionCredentialLease, req *ActionCredentialLeaseValidationRequest) {
			req.Now = now.Add(2 * time.Minute)
		}},
		{"signature drift", func(envelope *SignedActionCredentialLease, _ *ActionCredentialLeaseValidationRequest) {
			envelope.Lease.Action = "delete"
		}},
		{"lease identity drift", func(envelope *SignedActionCredentialLease, _ *ActionCredentialLeaseValidationRequest) {
			envelope.Lease.ActionLease.Attempt++
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, signer := newEnvelope(t)
			req := request(envelope)
			test.mutate(envelope, &req)
			validator, err := NewActionCredentialLeaseValidator(signer, &testCredentialLeaseRedeemer{seen: map[string]bool{}, authorize: func(request ActionCredentialLeaseRedemptionRequest) error {
				return MatchActionCredentialLease(request.Lease, call, run, request.Transport, request.CredentialFields)
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
	now, call, run, fields := actionCredentialLeaseFixture()
	lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000003",
	})
	if err != nil {
		t.Fatal(err)
	}
	signer := testCredentialLeaseSigner{key: []byte("lease-signing-key")}
	for _, test := range []struct {
		name   string
		mutate func(*ActionCredentialLease, *ActionCall)
	}{
		{"extra credential field", func(candidate *ActionCredentialLease, _ *ActionCall) {
			candidate.Credentials[0].Fields = append(candidate.Credentials[0].Fields, "api.unexpected")
		}},
		{"binding revision drift", func(_ *ActionCredentialLease, durable *ActionCall) { durable.BindingRevision++ }},
		{"assigned agent drift", func(candidate *ActionCredentialLease, _ *ActionCall) { candidate.AssignedAgentID = "agent-other" }},
		{"transport drift", func(candidate *ActionCredentialLease, _ *ActionCall) { candidate.Transport = "other_tool" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, durable := *lease, cloneActionCall(call)
			candidate.Credentials = cloneActionCredentialFieldReferences(lease.Credentials)
			test.mutate(&candidate, durable)
			envelope, err := SignActionCredentialLease(t.Context(), candidate, signer)
			if err != nil {
				t.Fatal(err)
			}
			validator, _ := NewActionCredentialLeaseValidator(signer, &testCredentialLeaseRedeemer{seen: map[string]bool{}, authorize: func(request ActionCredentialLeaseRedemptionRequest) error {
				return MatchActionCredentialLease(request.Lease, durable, run, request.Transport, request.CredentialFields)
			}})
			if _, err := validator.Validate(t.Context(), ActionCredentialLeaseValidationRequest{Envelope: envelope, TenantID: "tenant-7", Scope: call.Scope, TrustedIssuer: "control-plane", Audience: "execution-host", Now: now.Add(time.Second), Transport: testActionCredentialLeaseTransport, CredentialFields: fields}); err == nil {
				t.Fatal("signed authority drift was accepted")
			}
		})
	}
}

func TestActionCredentialLeaseAuthorityAcceptsHeartbeatButRejectsReclaim(t *testing.T) {
	now, call, run, fields := actionCredentialLeaseFixture()
	lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000006",
	})
	if err != nil {
		t.Fatal(err)
	}
	renewed := cloneActionCall(call)
	renewed.Revision += 2
	renewedExpiry := call.LeaseExpiresAt.Add(time.Minute)
	renewed.LeaseExpiresAt = &renewedExpiry
	if err := MatchActionCredentialLease(*lease, renewed, run, testActionCredentialLeaseTransport, fields); err != nil {
		t.Fatalf("same-owner heartbeat renewal was rejected: %v", err)
	}

	reclaimed := cloneActionCall(renewed)
	reclaimed.Attempt++
	reclaimed.LeaseOwner = "runtime-worker-2"
	if err := MatchActionCredentialLease(*lease, reclaimed, run, testActionCredentialLeaseTransport, fields); err == nil {
		t.Fatal("reclaimed ActionCall accepted an earlier credential lease")
	}
}

func TestNewActionCredentialLeaseRejectsIncompleteOrOverbroadRequests(t *testing.T) {
	now, call, run, fields := actionCredentialLeaseFixture()
	valid := func() CreateActionCredentialLeaseRequest {
		return CreateActionCredentialLeaseRequest{TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport, Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000004"}
	}
	for _, test := range []struct {
		name   string
		mutate func(*CreateActionCredentialLeaseRequest)
	}{
		{"unclaimed action", func(req *CreateActionCredentialLeaseRequest) {
			req.Call = cloneActionCall(call)
			req.Call.LeaseOwner = ""
		}},
		{"wrong run", func(req *CreateActionCredentialLeaseRequest) { req.Run = cloneAgentRun(run); req.Run.ID = "run-other" }},
		{"missing fields", func(req *CreateActionCredentialLeaseRequest) { req.CredentialFields = map[string][]string{} }},
		{"missing transport", func(req *CreateActionCredentialLeaseRequest) { req.Transport = "" }},
		{"short nonce", func(req *CreateActionCredentialLeaseRequest) { req.Nonce = "short" }},
		{"extra credential", func(req *CreateActionCredentialLeaseRequest) {
			req.CredentialFields = map[string][]string{"reddit": {"api.token"}, "other": {"token"}}
		}},
		{"expiry beyond action lease", func(req *CreateActionCredentialLeaseRequest) { req.ExpiresAt = now.Add(3 * time.Minute) }},
		{"overlong ttl", func(req *CreateActionCredentialLeaseRequest) {
			expiry := now.Add(10 * time.Minute)
			req.Call = cloneActionCall(call)
			req.Call.LeaseExpiresAt = &expiry
			req.ExpiresAt = expiry
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := valid()
			test.mutate(&req)
			if _, err := NewActionCredentialLease(req); err == nil {
				t.Fatal("invalid credential lease request was accepted")
			}
		})
	}
}

func TestActionCredentialLeaseContainsOnlyOpaqueReferences(t *testing.T) {
	now, call, run, fields := actionCredentialLeaseFixture()
	lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport, Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000005"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := canonicalActionCredentialLeasePayload(*lease)
	if string(encoded) == "" || containsAny(string(encoded), "resolved-secret-value", "plaintext", "apiKeyValue") {
		t.Fatalf("credential lease contains a resolved value: %s", encoded)
	}
}

func TestActionCredentialLeaseAcceptsOpaqueReferencesWithSpaces(t *testing.T) {
	now, call, run, fields := actionCredentialLeaseFixture()
	call.CredentialRefs["reddit"] = skill.CredentialReference{Kind: "slack_bot_token", ID: "vault://007 Slack.bot_token"}
	lease, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
		TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport,
		Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000006",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := lease.Credentials[0].Reference.ID; got != "vault://007 Slack.bot_token" {
		t.Fatalf("credential reference = %q", got)
	}
}

func TestActionCredentialLeaseRejectsNonCanonicalOpaqueReferences(t *testing.T) {
	for _, referenceID := range []string{" vault://credential.token", "vault://credential.token ", "vault://credential\ntoken"} {
		t.Run(referenceID, func(t *testing.T) {
			now, call, run, fields := actionCredentialLeaseFixture()
			call.CredentialRefs["reddit"] = skill.CredentialReference{Kind: "token", ID: referenceID}
			if _, err := NewActionCredentialLease(CreateActionCredentialLeaseRequest{
				TenantID: "tenant-7", Call: call, Run: run, CredentialFields: fields, Transport: testActionCredentialLeaseTransport,
				Issuer: "control-plane", Audience: "execution-host", IssuedAt: now, ExpiresAt: now.Add(time.Minute), Nonce: "nonce-0000000000000007",
			}); err == nil {
				t.Fatal("non-canonical opaque credential reference was accepted")
			}
		})
	}
}

func actionCredentialLeaseFixture() (time.Time, *ActionCall, *AgentRun, map[string][]string) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	expires := now.Add(2 * time.Minute)
	scope := Scope{Kind: "tenant", ID: "tenant-7"}
	call := &ActionCall{
		ID: "action-1", Scope: scope, RunID: "run-1", DeploymentID: "team-marketing", BindingID: "reddit-binding", BindingRevision: 3,
		SkillID: "reddit-research", SkillVersion: "1.2.0", Action: "search", Status: ActionCallStatusRunning,
		CredentialRefs: map[string]skill.CredentialReference{"reddit": {Kind: "reddit-oauth", ID: "opaque://tenant-7/reddit"}},
		Attempt:        1, MaxAttempts: 3, LeaseOwner: "runtime-worker-1", LeaseExpiresAt: &expires, Revision: 4,
	}
	run := &AgentRun{ID: "run-1", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "team-marketing"}, AssignedAgentID: "agent-researcher"}
	return now, call, run, map[string][]string{"reddit": {"api.token"}}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
