//go:build integration

package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/internal/daemon"
	kernelagent "github.com/axiom-studio/openseal/pkg/agent"
	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/kernelapi"
	opensealkernel "github.com/axiom-studio/openseal/pkg/openseal"
	"github.com/axiom-studio/openseal/pkg/outreach"
	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/source"
)

const liveOutreachTargetEnvironment = "OPENSEAL_LIVE_OUTREACH_URL"

func TestDaemonProcessHelper(t *testing.T) {
	if os.Getenv("OPENSEAL_DAEMON_HELPER") != "1" {
		return
	}
	daemonCmd([]string{"--config", os.Getenv("OPENSEAL_DAEMON_CONFIG"), "--scope", "local:research", "--standalone-operator"})
}

func TestLiveStandaloneOutreachSurvivesApprovalRestart(t *testing.T) {
	target := strings.TrimSpace(os.Getenv(liveOutreachTargetEnvironment))
	if target == "" {
		t.Skip("set " + liveOutreachTargetEnvironment + " to a public credential-free HTTPS webhook test URL")
	}
	configDir := t.TempDir()
	apiPort := freeTCPPort(t)
	apiBase := fmt.Sprintf("http://127.0.0.1:%d", apiPort)
	configPath := filepath.Join(configDir, "daemon.yaml")
	databasePath := filepath.Join(configDir, "kernel.db")
	targetHost, targetPrefix := liveTargetPolicy(t, target)
	config := DefaultLiveDaemonConfig(databasePath, filepath.Join(configDir, "artifacts"), apiPort, targetHost, targetPrefix)
	if err := daemon.WriteDaemonConfig(configPath, config); err != nil {
		t.Fatal(err)
	}
	fixture := seedLiveOutreach(t, databasePath, target)

	process := startLiveDaemon(t, configPath, apiBase)
	thread := createLiveOutreachDraft(t, apiBase, fixture)
	run := deliverLiveOutreach(t, apiBase, thread, fixture)
	approval := waitForApproval(t, apiBase, run)
	process.stop(t)

	process = startLiveDaemon(t, configPath, apiBase)
	approval = getApproval(t, apiBase, approval.ID, fixture.scope)
	resolveApproval(t, apiBase, approval, fixture.scope)
	waitForLiveDelivery(t, apiBase, thread, run, fixture)
	process.stop(t)
}

func DefaultLiveDaemonConfig(databasePath, artifactsPath string, apiPort int, host, pathPrefix string) *daemon.DaemonConfig {
	return &daemon.DaemonConfig{
		LogLevel: "info",
		Storage:  daemon.StorageConfig{Driver: "sqlite", Path: databasePath, ArtifactsPath: artifactsPath},
		API:      daemon.APIConfig{ListenAddr: fmt.Sprintf("127.0.0.1:%d", apiPort)},
		SourcePolicies: []daemon.ScopedSourcePolicy{{Scope: runtime.Scope{Kind: "local", ID: "research"}, Policy: source.Policy{
			ID: "live-community", Version: "1", Enabled: true, MaximumItems: 10,
			Sources:  []source.PolicySource{{Host: host, PathPrefixes: []string{pathPrefix}}},
			Outreach: &source.OutreachPolicy{Enabled: true, ApprovalPolicy: "human-review", MaximumBytes: 2000},
		}}},
	}
}

type liveOutreachFixture struct {
	scope       runtime.Scope
	project  string
	observation string
}

func seedLiveOutreach(t *testing.T, path, target string) liveOutreachFixture {
	t.Helper()
	ctx := t.Context()
	store, err := runtime.NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := opensealkernel.New(opensealkernel.WithPersistentStore(store))
	if err != nil {
		t.Fatal(err)
	}
	scope := runtime.Scope{Kind: "local", ID: "research"}
	owner := runtime.ObjectiveOwner{Type: runtime.OwnerTypeAgent, ID: "research-agent"}
	definition, err := engine.RegisterAgentDefinition(ctx, &kernelagent.AgentDefinition{
		ID: "research-agent", Version: "1", DisplayName: "Research Agent", Purpose: "Ask reviewed evidence-linked research questions.",
		SystemPrompt: "Use only reviewed outreach drafts and preserve truthful disclosure.", Authority: kernelagent.AuthorityPolicy{MaximumRisk: capability.RiskLevelExternal, MaxConcurrentRuns: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.CreateAgentDeployment(ctx, &kernelagent.AgentDeployment{ID: "research-agent", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DefinitionID: definition.ID,
		ActiveVersion: definition.Version, RolloutStatus: kernelagent.RolloutActive, Environment: "live-test", Capacity: kernelagent.DeploymentCapacity{MaxConcurrentRuns: 2}}, "user", "local", "live integration fixture"); err != nil {
		t.Fatal(err)
	}
	if err := engine.RegisterSkill(ctx, outreach.SkillDefinition()); err != nil {
		t.Fatal(err)
	}
	if err := engine.BindSkill(ctx, &skill.Binding{ID: "live-outreach", Scope: skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}, DeploymentID: "research-agent",
		SkillID: outreach.SkillID, SkillVersion: outreach.SkillVersion, AllowedActions: []string{outreach.PostReply}, EnablePrompt: true, MaximumRisk: skill.RiskLevelExternal, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	objective, err := engine.CreateObjective(ctx, runtime.CreateObjectiveRequest{Scope: scope, Owner: owner, Title: "Collect live feedback", Goal: "Ask one evidence-linked question", Status: runtime.ObjectiveStatusActive,
		Cadence: &runtime.ObjectiveCadence{Type: runtime.ObjectiveCadenceInterval, IntervalSeconds: 3600, AssignedAgentID: "research-agent", RunTemplate: &runtime.ObjectiveRunTemplate{
			Entrypoint: "monitor", Context: map[string]interface{}{"projectId": "live-research", "sourceMonitorId": "live-monitor"}, Policy: map[string]interface{}{"sourcePolicyRef": "live-community@1"},
			Capability: &runtime.ObjectiveCapabilityInvocation{SkillID: "evidence-source", SkillVersion: "1", Action: "observe", Inputs: map[string]interface{}{"url": target, "maxItems": 1}},
		}}})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := engine.CreateProject(ctx, runtime.CreateProjectRequest{Project: &runtime.Project{
		ID: "live-research", Scope: scope, Owner: owner, Title: "Live research", Purpose: "Prove governed outreach delivery", Status: runtime.ProjectStatusActive,
		ObjectiveRefs: []string{objective.ID}, SourceMonitors: []runtime.SourceMonitorReference{{ID: "live-monitor", ObjectiveID: objective.ID, AssignedAgentID: "research-agent",
			SkillID: "evidence-source", SkillVersion: "1", Action: "observe", SourcePolicyRef: "live-community@1", Deduplication: runtime.SourceMonitorDeduplicateStableSourceAndContent}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	sourceRun, err := engine.CreateAgentRun(ctx, runtime.CreateAgentRunRequest{Scope: scope, Kind: runtime.RunKindAgentWork, ObjectiveID: objective.ID, Owner: owner,
		AssignedAgentID: "research-agent", Goal: "Retain live source evidence", Source: runtime.RunSourceManual,
		Context: map[string]interface{}{"projectId": project.ID, "sourceMonitorId": "live-monitor"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CommandAgentRun(ctx, runtime.AgentRunCommandRequest{Scope: scope, RunID: sourceRun.ID, ExpectedRevision: sourceRun.Revision, Kind: runtime.AgentRunCommandCancel,
		Actor: runtime.ActivityActor{Type: "system", ID: "live-fixture"}, Summary: "Source evidence retained before daemon execution"}); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte("A user asked for a simpler autonomous-agent setup."))
	ingested, err := engine.IngestSourceObservation(ctx, runtime.IngestSourceObservationRequest{Scope: scope, ProjectID: project.ID, MonitorID: "live-monitor", Cursor: "live-1",
		StableSourceID: "live-thread", SourceURI: target, ContentDigest: "sha256:" + hex.EncodeToString(digest[:]), Summary: "A user asked for a simpler autonomous-agent setup.",
		ObservedAt: time.Now().UTC(), RunID: sourceRun.ID, AgentID: "research-agent", SkillID: "evidence-source", SkillVersion: "1", Action: "observe", ActionCallID: "live-source-call"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return liveOutreachFixture{scope: scope, project: project.ID, observation: ingested.Observation.ID}
}

type liveDaemonProcess struct {
	command *exec.Cmd
	logs    *bytes.Buffer
}

func startLiveDaemon(t *testing.T, configPath, apiBase string) *liveDaemonProcess {
	t.Helper()
	logs := &bytes.Buffer{}
	command := exec.Command(os.Args[0], "-test.run=^TestDaemonProcessHelper$")
	command.Env = append(withoutLLMEnvironment(os.Environ()), "OPENSEAL_DAEMON_HELPER=1", "OPENSEAL_DAEMON_CONFIG="+configPath, "OPENSEAL_SKILLS_DIR="+filepath.Join(filepath.Dir(configPath), "skills"))
	command.Stdout, command.Stderr = logs, logs
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := &liveDaemonProcess{command: command, logs: logs}
	t.Cleanup(func() {
		if process.command != nil && process.command.Process != nil {
			_ = process.command.Process.Signal(syscall.SIGTERM)
			_, _ = process.command.Process.Wait()
		}
	})
	waitUntil(t, 15*time.Second, func() bool {
		response, err := http.Get(apiBase + "/api/v1/capabilities")
		if err != nil {
			return false
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response.StatusCode == http.StatusOK && bytes.Contains(body, []byte(`"deliver"`))
	}, func() string { return "daemon did not advertise delivery:\n" + logs.String() })
	return process
}

func (p *liveDaemonProcess) stop(t *testing.T) {
	t.Helper()
	if p == nil || p.command == nil || p.command.Process == nil {
		return
	}
	if err := p.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := p.command.Wait(); err != nil {
		t.Fatalf("daemon shutdown: %v\n%s", err, p.logs.String())
	}
	p.command = nil
}

func createLiveOutreachDraft(t *testing.T, apiBase string, fixture liveOutreachFixture) *runtime.OutreachThread {
	t.Helper()
	disclosure := "Disclosure: I am an automated OpenSeal research agent operated for a live integration test."
	payload := kernelapi.CreateOutreachThreadRequest{Scope: fixture.scope, ProjectID: fixture.project, SourceObservationID: fixture.observation, ApprovalPolicyRef: "human-review",
		Identity: runtime.OutreachIdentity{ProfileRef: "profile:live-test", DisplayName: "OpenSeal Research", Affiliation: "OpenSeal", Disclosure: disclosure},
		Message: kernelapi.CreateOutreachMessageRequest{ID: "live-message", Intent: runtime.OutreachIntentRequestFeedback,
			Body:       "Which part of autonomous-agent setup should be simpler? " + disclosure,
			Capability: kernelapi.OutreachCapabilitySelection{BindingID: "live-outreach", BindingRevision: 1, SkillID: outreach.SkillID, SkillVersion: outreach.SkillVersion, Action: outreach.PostReply}}}
	var thread runtime.OutreachThread
	requestJSON(t, http.MethodPost, apiBase+"/api/v1/projects/"+fixture.project+"/outreach", payload, map[string]string{"Idempotency-Key": "live-draft-1"}, http.StatusCreated, &thread)
	return &thread
}

func deliverLiveOutreach(t *testing.T, apiBase string, thread *runtime.OutreachThread, fixture liveOutreachFixture) *runtime.AgentRun {
	t.Helper()
	var run runtime.AgentRun
	requestJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/projects/%s/outreach/%s/messages/%s/deliveries", apiBase, fixture.project, thread.ID, thread.Messages[0].ID),
		kernelapi.DeliverOutreachMessageRequest{Scope: fixture.scope}, map[string]string{"Idempotency-Key": "live-delivery-1"}, http.StatusCreated, &run)
	return &run
}

func waitForApproval(t *testing.T, apiBase string, run *runtime.AgentRun) *runtime.ApprovalCheckpoint {
	t.Helper()
	var approval *runtime.ApprovalCheckpoint
	waitUntil(t, 15*time.Second, func() bool {
		var approvals []*runtime.ApprovalCheckpoint
		requestJSON(t, http.MethodGet, fmt.Sprintf("%s/api/v1/action-approvals?scopeKind=%s&scopeId=%s&runId=%s", apiBase, run.Scope.Kind, run.Scope.ID, run.ID), nil, nil, http.StatusOK, &approvals)
		if len(approvals) == 1 && approvals[0].Status == runtime.ApprovalStatusPending {
			approval = approvals[0]
			return true
		}
		return false
	}, func() string { return "delivery Run did not reach pending approval" })
	return approval
}

func getApproval(t *testing.T, apiBase, approvalID string, scope runtime.Scope) *runtime.ApprovalCheckpoint {
	t.Helper()
	var approval runtime.ApprovalCheckpoint
	requestJSON(t, http.MethodGet, fmt.Sprintf("%s/api/v1/action-approvals/%s?scopeKind=%s&scopeId=%s", apiBase, approvalID, scope.Kind, scope.ID), nil, nil, http.StatusOK, &approval)
	return &approval
}

func resolveApproval(t *testing.T, apiBase string, approval *runtime.ApprovalCheckpoint, scope runtime.Scope) {
	t.Helper()
	requestJSON(t, http.MethodPost, fmt.Sprintf("%s/api/v1/action-approvals/%s/decisions?scopeKind=%s&scopeId=%s", apiBase, approval.ID, scope.Kind, scope.ID),
		kernelapi.ResolveActionApprovalRequest{ExpectedRevision: approval.Revision, DecisionID: "live-approval-1", Approve: true, Principal: runtime.ApprovalPrincipal{Type: "user", ID: "local"}, Reason: "Approve the reviewed live integration message"},
		map[string]string{"Idempotency-Key": "live-approval-1"}, http.StatusOK, nil)
}

func waitForLiveDelivery(t *testing.T, apiBase string, original *runtime.OutreachThread, run *runtime.AgentRun, fixture liveOutreachFixture) {
	t.Helper()
	waitUntil(t, 30*time.Second, func() bool {
		var thread runtime.OutreachThread
		requestJSON(t, http.MethodGet, fmt.Sprintf("%s/api/v1/projects/%s/outreach/%s?scopeKind=%s&scopeId=%s", apiBase, fixture.project, original.ID, fixture.scope.Kind, fixture.scope.ID), nil, nil, http.StatusOK, &thread)
		var current runtime.AgentRun
		requestJSON(t, http.MethodGet, fmt.Sprintf("%s/api/v1/agent-runs/%s?scopeKind=%s&scopeId=%s", apiBase, run.ID, fixture.scope.Kind, fixture.scope.ID), nil, nil, http.StatusOK, &current)
		message := thread.Messages[0]
		return message.Status == runtime.OutreachMessageDelivered && message.Receipt != nil && message.Receipt.Provider == "http-webhook" &&
			message.Receipt.ExternalID == "http:"+message.ActionCallID && message.Receipt.Digest != "" && current.Status == runtime.AgentRunStatusCompleted
	}, func() string {
		return "restarted daemon did not persist a provider receipt and complete the delivery Run"
	})
}

func requestJSON(t *testing.T, method, endpoint string, input interface{}, headers map[string]string, expected int, output interface{}) {
	t.Helper()
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(t.Context(), method, endpoint, body)
	if err != nil {
		t.Fatal(err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != expected {
		t.Fatalf("%s %s = %d, want %d: %s", method, endpoint, response.StatusCode, expected, responseBody)
	}
	if output != nil && len(responseBody) > 0 {
		if err := json.Unmarshal(responseBody, output); err != nil {
			t.Fatalf("decode %s %s: %v: %s", method, endpoint, err, responseBody)
		}
	}
}

func waitUntil(t *testing.T, timeout time.Duration, condition func() bool, failure func() string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		select {
		case <-t.Context().Done():
			t.Fatal(t.Context().Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal(failure())
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func liveTargetPolicy(t *testing.T, raw string) (string, string) {
	t.Helper()
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, raw, nil)
	if err != nil || request.URL.Scheme != "https" || request.URL.Hostname() == "" || request.URL.User != nil || request.URL.Fragment != "" || (request.URL.Port() != "" && request.URL.Port() != "443") {
		t.Fatalf("%s must be a credential-free public HTTPS URL on port 443", liveOutreachTargetEnvironment)
	}
	prefix := request.URL.EscapedPath()
	if prefix == "" {
		prefix = "/"
	}
	return request.URL.Hostname(), prefix
}

func withoutLLMEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		if strings.HasPrefix(value, "OPENSEAL_LLM_BASE_URL=") || strings.HasPrefix(value, "OPENSEAL_LLM_MODEL=") || strings.HasPrefix(value, "OPENAI_API_KEY=") {
			continue
		}
		result = append(result, value)
	}
	return result
}
