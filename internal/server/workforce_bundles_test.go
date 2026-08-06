package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/agent"
	kernelbundle "github.com/axiom-studio/openseal/pkg/bundle"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestWorkforceBundleHTTPContractIsCapabilityTruthfulAndHostAuthorized(t *testing.T) {
	server := NewServer(runtime.NewMemoryStore(), zap.NewNop().Sugar())
	document := requestWorkforceBundleAPI(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", nil, "", http.StatusOK)
	var capabilities kernelapi.CapabilityDocument
	if err := json.Unmarshal(document, &capabilities); err != nil {
		t.Fatal(err)
	}
	contract, ok := capabilities.Find(kernelapi.WorkforceBundlesCapabilityID, kernelapi.WorkforceBundlesCapabilityVersion)
	if !ok || contract.Supports(kernelapi.OperationInstall) || !contract.Supports(kernelapi.OperationInspect) || !contract.Supports(kernelapi.OperationPreviewInstallation) {
		t.Fatalf("unconfigured capability = %#v", contract)
	}

	bundle, placement := serverWorkforceBundle(t)
	inspectionBody := requestWorkforceBundleAPI(t, server.Handler(), http.MethodPost, "/api/v1/workforce-bundles/inspect", bundle, "", http.StatusOK)
	var inspection kernelbundle.Inspection
	if err := json.Unmarshal(inspectionBody, &inspection); err != nil || inspection.Agents != 1 || inspection.Digest != bundle.Digest {
		t.Fatalf("inspection = %#v, %v", inspection, err)
	}
	previewBody := requestWorkforceBundleAPI(t, server.Handler(), http.MethodPost, "/api/v1/workforce-bundles/installation-preview", map[string]interface{}{"bundle": bundle, "placement": placement}, "", http.StatusOK)
	var preview kernelbundle.InstallationPreview
	if err := json.Unmarshal(previewBody, &preview); err != nil || !preview.Ready {
		t.Fatalf("preview = %#v, %v", preview, err)
	}
	requestWorkforceBundleAPI(t, server.Handler(), http.MethodPost, "/api/v1/workforce-bundles/install", map[string]interface{}{"bundle": bundle, "scope": capability.ScopeReference{Kind: "tenant", ID: "target"}, "placement": placement}, "import-one", http.StatusNotImplemented)

	store := &serverBundleInstallationStore{}
	server.SetWorkforceBundleInstallation(store, "trusted-operator")
	document = requestWorkforceBundleAPI(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", nil, "", http.StatusOK)
	if err := json.Unmarshal(document, &capabilities); err != nil {
		t.Fatal(err)
	}
	contract, ok = capabilities.Find(kernelapi.WorkforceBundlesCapabilityID, kernelapi.WorkforceBundlesCapabilityVersion)
	if !ok || !contract.Supports(kernelapi.OperationInstall) {
		t.Fatalf("configured capability = %#v", contract)
	}
	requestWorkforceBundleAPI(t, server.Handler(), http.MethodPost, "/api/v1/workforce-bundles/install", map[string]interface{}{"bundle": bundle, "scope": capability.ScopeReference{Kind: "tenant", ID: "target"}, "placement": placement, "reason": "Import reviewed bundle"}, "", http.StatusBadRequest)
	receiptBody := requestWorkforceBundleAPI(t, server.Handler(), http.MethodPost, "/api/v1/workforce-bundles/install", map[string]interface{}{"bundle": bundle, "scope": capability.ScopeReference{Kind: "tenant", ID: "target"}, "placement": placement, "reason": "Import reviewed bundle"}, "import-one", http.StatusCreated)
	var receipt kernelbundle.InstallationReceipt
	if err := json.Unmarshal(receiptBody, &receipt); err != nil || receipt.PlanDigest == "" || store.plan == nil || store.plan.ActorID != "trusted-operator" || store.plan.ActorType != "user" {
		t.Fatalf("receipt=%#v plan=%#v err=%v", receipt, store.plan, err)
	}
}

type serverBundleInstallationStore struct {
	plan *kernelbundle.InstallationPlan
}

func (s *serverBundleInstallationStore) ApplyWorkforceBundle(_ context.Context, plan *kernelbundle.InstallationPlan) (*kernelbundle.InstallationReceipt, error) {
	s.plan = plan
	return &kernelbundle.InstallationReceipt{BundleID: plan.BundleID, BundleVersion: plan.BundleVersion, BundleDigest: plan.BundleDigest, PlanDigest: plan.PlanDigest, IdempotencyKey: plan.IdempotencyKey, AppliedAt: time.Unix(3, 0).UTC()}, nil
}

func serverWorkforceBundle(t *testing.T) (*kernelbundle.Bundle, kernelbundle.Placement) {
	t.Helper()
	now := time.Unix(1, 0).UTC()
	definition := &agent.AgentDefinition{ID: "portable/researcher", Version: "1.0.0", DisplayName: "Researcher", Purpose: "Research questions", SystemPrompt: "Research carefully.", Authority: agent.AuthorityPolicy{MaximumRisk: capability.RiskLevelRead, MaxConcurrentRuns: 1}}
	deployment := &agent.AgentDeployment{ID: "agent:source", Scope: capability.ScopeReference{Kind: "tenant", ID: "source"}, DefinitionID: definition.ID, ActiveVersion: definition.Version, RolloutStatus: agent.RolloutActive, Environment: "default", Capacity: agent.DeploymentCapacity{MaxConcurrentRuns: 1}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	artifact, err := agent.ExportBundle(agent.BundleExportRequest{Definition: definition, Deployment: deployment, Metadata: agent.BundleMetadata{ID: "researcher", Version: "1.0.0", DisplayName: "Researcher"}, Manifest: agent.ManifestMetadata{ID: "researcher", Version: "1.0.0", DisplayName: "Researcher"}})
	if err != nil {
		t.Fatal(err)
	}
	bundle := kernelbundle.New(kernelbundle.Metadata{ID: "research-workforce", Version: "1.0.0", DisplayName: "Research Workforce"})
	bundle.Agents = []kernelbundle.Agent{{Key: "researcher", Artifact: artifact}}
	if err := bundle.Seal(); err != nil {
		t.Fatal(err)
	}
	agentPlacement := agent.BundlePlacement{DeploymentID: "agent:target", Environment: "default", Runtime: map[string]capability.SkillIdentity{}, Skills: map[string]agent.BundleSkillPlacement{}, Credentials: map[string]capability.CredentialReference{}, Endpoints: map[string]agent.BundleEndpointPlacement{}, Callbacks: map[string]agent.BundleCallbackPlacement{}}
	for _, requirement := range artifact.Runtime {
		agentPlacement.Runtime[requirement.RequirementID] = requirement.Identity
	}
	return bundle, kernelbundle.Placement{Agents: map[string]agent.BundlePlacement{"researcher": agentPlacement}}
}

func requestWorkforceBundleAPI(t *testing.T, handler http.Handler, method, path string, body interface{}, idempotencyKey string, wantStatus int) []byte {
	t.Helper()
	var encoded string
	if body != nil {
		value, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = string(value)
	}
	request := httptest.NewRequest(method, path, strings.NewReader(encoded))
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != wantStatus {
		t.Fatalf("%s %s status=%d body=%s", method, path, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}
