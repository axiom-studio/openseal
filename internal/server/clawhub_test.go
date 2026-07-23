package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill/clawhub"
	"go.uber.org/zap"
)

type serverClawHubRegistry struct {
	archive []byte
	version string
}

func (r *serverClawHubRegistry) SearchSkills(context.Context, clawhub.SearchRequest) (*clawhub.SkillPage, error) {
	return &clawhub.SkillPage{}, nil
}
func (r *serverClawHubRegistry) ExploreSkills(context.Context, clawhub.ExploreRequest) (*clawhub.SkillPage, error) {
	return &clawhub.SkillPage{}, nil
}
func (r *serverClawHubRegistry) InspectSkill(_ context.Context, ref clawhub.SkillReference) (*clawhub.SkillDetail, error) {
	return &clawhub.SkillDetail{SkillSummary: clawhub.SkillSummary{Slug: ref.Slug}, Owner: ref.Owner, Version: r.version}, nil
}
func (r *serverClawHubRegistry) ListVersions(context.Context, clawhub.SkillReference, int, string) (*clawhub.VersionPage, error) {
	return &clawhub.VersionPage{Items: []clawhub.VersionSummary{{Version: r.version}}}, nil
}
func (r *serverClawHubRegistry) GetVersion(context.Context, clawhub.SkillReference, string) (*clawhub.VersionDetail, error) {
	return &clawhub.VersionDetail{Version: r.version}, nil
}
func (r *serverClawHubRegistry) GetFile(context.Context, clawhub.SkillReference, string, string, string) ([]byte, error) {
	return []byte("safe"), nil
}
func (r *serverClawHubRegistry) VerifySkill(_ context.Context, ref clawhub.SkillReference, version, _ string) (*clawhub.Verification, error) {
	if version == "" {
		version = r.version
	}
	return &clawhub.Verification{Schema: "clawhub.skill.verify.v1", OK: true, Decision: "pass", Slug: ref.Slug, PublisherHandle: ref.Owner, Version: version}, nil
}
func (r *serverClawHubRegistry) DownloadArchive(context.Context, clawhub.SkillReference, string, string) (*clawhub.DownloadedArchive, error) {
	sum := sha256.Sum256(r.archive)
	return &clawhub.DownloadedArchive{Bytes: r.archive, SHA256: hex.EncodeToString(sum[:])}, nil
}

func TestStandaloneClawHubLifecycleCapabilityAndMutations(t *testing.T) {
	registry := &serverClawHubRegistry{archive: serverClawHubArchive(t), version: "1.0.0"}
	engine, err := opensealkernel.New(opensealkernel.WithClawHubRegistry(clawhub.RegistryURL, registry, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	store := runtime.NewMemoryStore()
	api := NewServer(store, zap.NewNop().Sugar())
	api.SetClawHubLifecycle(engine, false)
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	response := serverRequest(t, http.MethodGet, server.URL+"/api/v1/capabilities", "")
	var document map[string]interface{}
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	encoded, _ := json.Marshal(document)
	if !bytes.Contains(encoded, []byte(`"clawhub-lifecycle"`)) || bytes.Contains(encoded, []byte(`"install"`)) {
		t.Fatalf("read-only capability=%s", encoded)
	}
	response = serverRequest(t, http.MethodPost, server.URL+"/api/v1/clawhub/catalog/%40acme%2Fresearch/install", `{}`)
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted install=%d", response.StatusCode)
	}
	response.Body.Close()
	server.Close()
	api = NewServer(store, zap.NewNop().Sugar())
	api.SetClawHubLifecycle(engine, true)
	server = httptest.NewServer(api.Handler())
	defer server.Close()
	response = serverRequest(t, http.MethodPost, server.URL+"/api/v1/clawhub/catalog/%40acme%2Fresearch/install", `{}`)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("install=%d", response.StatusCode)
	}
	response.Body.Close()
	response = serverRequest(t, http.MethodPost, server.URL+"/api/v1/clawhub/installed/%40acme%2Fresearch/pin", `{"reason":"reviewed"}`)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("pin=%d", response.StatusCode)
	}
	response.Body.Close()
	response = serverRequest(t, http.MethodGet, server.URL+"/api/v1/clawhub/installed", "")
	body := new(bytes.Buffer)
	body.ReadFrom(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || !strings.Contains(body.String(), `"pinned":true`) {
		t.Fatalf("installed=%d %s", response.StatusCode, body.String())
	}
}

func serverRequest(t *testing.T, method, url, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
func serverClawHubArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write([]byte("---\nname: research\ndescription: safe\n---\nSafe.")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
