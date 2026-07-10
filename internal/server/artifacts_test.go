package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	artifactstore "github.com/axiom-studio/openseal/pkg/artifact"
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
			"provenance":{"producer":{"type":"agent","id":"analyst"},"owner":{"type":"team","id":"research"},"runId":"run-1"},
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
		"/api/v1/artifacts?scopeKind=tenant&scopeId=one&ownerType=team&ownerId=research&type=report&classification=confidential&evidenceTarget=https%3A%2F%2Fforum.example%2Fthread%2F1", "", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"report"`) {
		t.Fatalf("list status = %d, body = %s", listed.Code, listed.Body.String())
	}
	otherOwner := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/artifacts?scopeKind=tenant&scopeId=one&ownerType=agent&ownerId=analyst", "", "")
	if otherOwner.Code != http.StatusOK || otherOwner.Body.String() != "[]\n" {
		t.Fatalf("other owner list status = %d, body = %s", otherOwner.Code, otherOwner.Body.String())
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

func TestArtifactContentRoutesStreamOnlyConfiguredOperations(t *testing.T) {
	server := NewServer(nil, nil, runtime.NewMemoryStore(100), zap.NewNop().Sugar())
	contentStore, err := artifactstore.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server.SetArtifactContentStore(contentStore)
	content := []byte("durable game log")
	uploadRequest := httptest.NewRequest(http.MethodPost, "/api/v1/artifact-content?scopeKind=local&scopeId=game", bytes.NewReader(content))
	uploadRequest.Header.Set("Content-Type", "text/plain")
	upload := httptest.NewRecorder()
	server.Handler().ServeHTTP(upload, uploadRequest)
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload status = %d, body = %s", upload.Code, upload.Body.String())
	}
	if !strings.Contains(upload.Body.String(), `"contentRef"`) || strings.Contains(upload.Body.String(), `"ContentRef"`) {
		t.Fatalf("upload response is not camelCase: %s", upload.Body.String())
	}
	var stored runtime.ArtifactStoredContent
	if err := json.Unmarshal(upload.Body.Bytes(), &stored); err != nil {
		t.Fatal(err)
	}
	registerBody := fmt.Sprintf(`{
		"artifact":{"id":"game-log","version":1,"scope":{"kind":"local","id":"game"},
		"name":"game.txt","type":"game-log","mediaType":"text/plain","contentRef":%q,
		"digest":%q,"sizeBytes":%d,"classification":"internal",
		"provenance":{"producer":{"type":"agent","id":"player"},"runId":"run-game"}},
		"expectedLatestVersion":0}`, stored.ContentRef, stored.Digest, stored.SizeBytes)
	registered := performAgentRunRequest(t, server.Handler(), http.MethodPost, "/api/v1/artifacts", registerBody, "")
	if registered.Code != http.StatusCreated {
		t.Fatalf("register status = %d, body = %s", registered.Code, registered.Body.String())
	}
	download := performAgentRunRequest(t, server.Handler(), http.MethodGet,
		"/api/v1/artifacts/game-log/content?scopeKind=local&scopeId=game", "", "")
	if download.Code != http.StatusOK || !bytes.Equal(download.Body.Bytes(), content) || download.Header().Get("X-Content-SHA256") != stored.Digest {
		t.Fatalf("download status/body = %d / %q", download.Code, download.Body.Bytes())
	}
	capabilities := performAgentRunRequest(t, server.Handler(), http.MethodGet, "/api/v1/capabilities", "", "")
	var document kernelapi.CapabilityDocument
	if err := json.Unmarshal(capabilities.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	capability, _ := document.Find(kernelapi.ArtifactsCapabilityID, kernelapi.ArtifactsCapabilityVersion)
	if !capability.Supports(kernelapi.OperationUpload) || !capability.Supports(kernelapi.OperationDownload) || capability.Supports(kernelapi.OperationResolve) {
		t.Fatalf("content capability = %#v", capability)
	}

	resolver := &testArtifactResolver{now: time.Now()}
	server.SetArtifactContentResolver(resolver)
	resolved := performAgentRunRequest(t, server.Handler(), http.MethodPost,
		"/api/v1/artifacts/game-log/resolve?scopeKind=local&scopeId=game",
		`{"actor":{"type":"user","id":"operator"},"purpose":"download","ttlSeconds":60}`, "")
	if resolved.Code != http.StatusOK || !strings.Contains(resolved.Body.String(), "https://delivery.example/") {
		t.Fatalf("resolve status = %d, body = %s", resolved.Code, resolved.Body.String())
	}
	loaded, err := runtime.NewArtifactCatalog(server.store.(runtime.ArtifactStore)).Get(context.Background(), runtime.Scope{Kind: "local", ID: "game"}, "game-log", 1)
	if err != nil || loaded.ContentRef != stored.ContentRef || strings.Contains(loaded.ContentRef, "https://") {
		t.Fatalf("resolution leaked into catalog: %#v, %v", loaded, err)
	}
}

type testArtifactResolver struct{ now time.Time }

func (r *testArtifactResolver) Resolve(_ context.Context, request runtime.ArtifactContentResolutionRequest) (runtime.ArtifactContentResolution, error) {
	return runtime.ArtifactContentResolution{
		URL: "https://delivery.example/ephemeral", ExpiresAt: r.now.Add(request.TTL),
	}, nil
}
