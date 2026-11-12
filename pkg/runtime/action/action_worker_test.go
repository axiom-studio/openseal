package action_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/portfolio"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestActionWorkerDispatchesOpaqueCredentialLeaseWithDurableAuthority(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	baseCatalog, proposal := createRunnableAction(t, store, now)
	catalog := &toolTransportActionCatalog{ActionExecutionCatalog: baseCatalog, endpoint: "release_deploy"}
	signer := testCredentialLeaseSigner{key: []byte("lease-signing-key")}
	issuerCalls := 0
	worker := action.NewActionWorkerWithCredentialLeaseIssuer(store, catalog, action.ActionCredentialLeaseIssuerFunc(func(ctx context.Context, request action.ActionCredentialLeaseIssueRequest) (*models.SignedActionCredentialLease, error) {
		issuerCalls++
		if request.Call == nil || request.Run == nil || request.Call.ID != proposal.Call.ID || request.Run.ID != proposal.Call.RunID || request.Run.AssignedAgentID == "" || request.References["token"] != proposal.Call.CredentialRefs["token"] || request.Transport != "release_deploy" {
			t.Fatalf("credential lease issue request = %#v", request)
		}
		lease, err := models.NewActionCredentialLease(models.CreateActionCredentialLeaseRequest{
			TenantID: request.Call.Scope.ID, Call: request.Call, Run: request.Run, CredentialFields: map[string][]string{"token": {"value"}}, Transport: request.Transport,
			Issuer: "control-plane", Audience: "execution-host", IssuedAt: now.Add(2 * time.Second), ExpiresAt: now.Add(30 * time.Second), Nonce: "nonce-worker-0000000001",
		})
		if err != nil {
			return nil, err
		}
		return action.SignActionCredentialLease(ctx, *lease, signer)
	}), action.ActionDispatcherFunc(func(_ context.Context, input agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		if len(input.Credentials) != 0 || input.CredentialLease == nil || input.CredentialLease.Lease.ActionCallID != proposal.Call.ID || input.CredentialReferences["token"] != proposal.Call.CredentialRefs["token"] {
			t.Fatalf("opaque dispatch input = %#v", input)
		}
		if input.Run == nil || input.Run.ID != proposal.Call.RunID || input.Call == nil || input.Call.LeaseOwner != "agent-worker" {
			t.Fatalf("durable dispatch authority = call %#v run %#v", input.Call, input.Run)
		}
		return map[string]interface{}{"ok": true}, nil
	}), models.WithClock(func() time.Time { return now.Add(2 * time.Second) }))
	worker.SetNewID(func() string { return "opaque-lease-execution" })
	result, err := worker.ExecuteOwnLoopCall(t.Context(), proposal.Call.Scope, proposal.Call.ID, "agent-worker")
	if err != nil {
		t.Fatal(err)
	}
	if issuerCalls != 1 || result.Call.Status != models.ActionCallStatusSucceeded {
		t.Fatalf("opaque credential execution = calls %d result %#v", issuerCalls, result)
	}
}

type toolTransportActionCatalog struct {
	agentrun.ActionExecutionCatalog
	endpoint string
}

func (c *toolTransportActionCatalog) Resolve(ctx context.Context, scope skill.ScopeReference, deploymentID, skillID, version, action string, bindings ...skill.BindingReference) (*skill.BoundAction, error) {
	bound, err := c.ActionExecutionCatalog.Resolve(ctx, scope, deploymentID, skillID, version, action, bindings...)
	if err != nil {
		return nil, err
	}
	definition := *bound.Definition
	definition.Transport = skill.TransportReference{Kind: "tool", Endpoint: c.endpoint}
	bound.Definition = &definition
	return bound, nil
}

func TestActionWorkerReturnsFailureWithoutLeakingCredentials(t *testing.T) {
	store := newTestKernelStore(t)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	catalog, proposal := createRunnableAction(t, store, now)
	attempts := 0
	current := now.Add(2 * time.Second)
	worker := action.NewActionWorker(store, catalog, action.CredentialResolverFunc(func(context.Context, action.CredentialResolutionRequest) (map[string]string, error) {
		return map[string]string{"token": "raw-secret-value"}, nil
	}), action.ActionDispatcherFunc(func(context.Context, agentrun.ActionDispatchInput) (map[string]interface{}, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("provider rejected raw-secret-value")
		}
		return map[string]interface{}{"ok": true}, nil
	}), models.WithClock(func() time.Time { return current }))
	first, err := worker.ExecuteOwnLoopCall(t.Context(), proposal.Call.Scope, proposal.Call.ID, "agent-worker")
	if err != nil {
		t.Fatal(err)
	}
	if first.Call.Status != models.ActionCallStatusFailed || first.Run != nil || first.Call.FailurePhase != models.ActionFailureAfterDispatch ||
		!strings.Contains(first.Call.Error, "[REDACTED]") || strings.Contains(first.Call.Error, "raw-secret-value") {
		t.Fatalf("failure result mismatch: %#v", first)
	}
	current = current.Add(30 * time.Second)
	if second, err := worker.ExecuteOwnLoopCall(t.Context(), proposal.Call.Scope, proposal.Call.ID, "agent-worker"); !errors.Is(err, action.ErrLoopActionNotClaimable) || second != nil || attempts != 1 {
		t.Fatalf("failed action was retried: %#v attempts=%d err=%v", second, attempts, err)
	}
	if pooled, err := worker.RunOnce(t.Context(), proposal.Call.Scope, "pool-worker", time.Minute); err != nil || pooled != nil || attempts != 1 {
		t.Fatalf("pool retried a failed loop call: %#v attempts=%d err=%v", pooled, attempts, err)
	}
}

// createRunnableAction proposes release.deploy as a ready loop call of
// step-1 for a Run that agent-worker holds for a minute.
func createRunnableAction(t *testing.T, store models.KernelStore, now time.Time) (*skill.Catalog, *models.ActionProposalResult) {
	t.Helper()
	catalog, scope := governedActionCatalog(t)
	portfolio := portfolio.NewPortfolioService(store, models.WithClock(func() time.Time { return now }))
	run, err := portfolio.CreateAgentRun(context.Background(), models.CreateAgentRunRequest{
		Scope: scope, Owner: models.ObjectiveOwner{Type: models.OwnerTypeAgent, ID: "release-agent"}, AssignedAgentID: "release-agent", Goal: "deploy", Source: models.RunSourceObjective,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimNextAgentRun(context.Background(), models.AgentRunClaim{Scope: scope, WorkerID: "agent-worker", Now: now, LeaseDuration: time.Minute, AgingInterval: time.Minute})
	if err != nil || claimed == nil || claimed.ID != run.ID {
		t.Fatalf("claim = %#v, %v", claimed, err)
	}
	return catalog, proposeLoopAction(t, store, catalog, claimed, "agent-worker", now.Add(time.Second), loopActionSpec{
		id: "call", skillID: "release", version: "1.0.0", action: "deploy", arguments: map[string]interface{}{"environment": "production"},
	})
}
