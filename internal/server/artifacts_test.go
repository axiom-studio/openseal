package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/kernelapi"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"go.uber.org/zap"
)

func TestArtifactRoutesAreStrictScopedVersionedAndCapabilityAdvertised(t *testing.T) {
	server := NewServer(nil, nil, runtime.NewMemoryStore(100), zap.NewNop().Sugar())
	body := `{
		"artifact": {
			"id":"report","version":1,"scope":{"kind":"tenant","id":"one"},
			"name":"report.pdf","type":"report","mediaType":"application/pdf",
			"contentRef":"object-store:report-v1",
			"digest":"sha256:0000000000000000000000000000000000000000000000000000000000000000",
			"sizeBytes":1200,"classification":"confidential",
			"metadata":{"pages":4},
			"provenance":{"producer":{"type":"agent","id":"analyst"},"runId":"run-1"},
			"evidence":[{"relation":"cites","targetKind":"external_source","targetRef":"https://forum.example/thread/1"}]
		},
		"expectedLatestVersion":0
	}`
	created := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/artifacts", body, "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", created.Code, created.Body.String())
	}
	var createResult runtime.ArtifactRegistrationResult
	if err := json.Unmarshal(created.Body.Bytes(), &createResult); err != nil {
		t.Fatal(err)
	}
	if createResult.Artifact == nil || createResult.Artifact.Fingerprint == "" || createResult.Replayed {
		t.Fatalf("create result = %#v", createResult)
	}

	replayed := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/artifacts", body, "")
	if replayed.Code != http.StatusOK || !strings.Contains(replayed.Body.String(), `"replayed":true`) {
		t.Fatalf("replay status = %d, body = %s", replayed.Code, replayed.Body.String())
	}
	listed := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/artifacts?scopeKind=tenant&scopeId=one&type=report&classification=confidential&evidenceTarget=https%3A%2F%2Fforum.example%2Fthread%2F1", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"report"`) {
		t.Fatalf("list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	loaded := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/artifacts/report?scopeKind=tenant&scopeId=one&version=1", "", "")
	if loaded.Code != http.StatusOK || !strings.Contains(loaded.Body.String(), `"contentRef":"object-store:report-v1"`) {
		t.Fatalf("get status = %d, body = %s", loaded.Code, loaded.Body.String())
	}
	crossScope := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/artifacts/report?scopeKind=tenant&scopeId=two", "", "")
	if crossScope.Code != http.StatusNotFound {
		t.Fatalf("cross-scope status = %d, body = %s", crossScope.Code, crossScope.Body.String())
	}
	invalid := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/artifacts", strings.TrimSuffix(body, "}")+`,"unknown":true}`, "")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("strict status = %d, body = %s", invalid.Code, invalid.Body.String())
	}

	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.Unmarshal(capabilities.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	capability, ok := document.Find(kernelapi.ArtifactsCapabilityID, kernelapi.ArtifactsCapabilityVersion)
	if !ok || !capability.Supports(kernelapi.OperationRegister) || capability.Supports("resolve") {
		t.Fatalf("artifact capability = %#v", capability)
	}
}
