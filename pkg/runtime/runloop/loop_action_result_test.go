package runloop

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
	"github.com/axiom-studio/openseal/pkg/toolfile"
)

func governedResultCall(status models.ActionCallStatus) *models.ActionCall {
	return &models.ActionCall{
		ID: "call-1", RunID: "run-1", TurnID: "step-1", SkillID: "repo", SkillVersion: "1.0.0", Action: "push",
		Origin: models.ActionOriginLoop, Execution: models.ActionExecutionLoop, Status: status,
		Risk: skill.RiskLevelExternal, SideEffect: skill.SideEffectExternal,
	}
}

func governedResultApproval(status models.ApprovalStatus, reason string) *models.ApprovalCheckpoint {
	return &models.ApprovalCheckpoint{ID: "approval-1", ActionCallID: "call-1", Status: status, DecisionReason: reason}
}

// requireGovernedResult checks state, status and the exact json part, and
// that the result is a valid terminal result for its state.
func requireGovernedResult(t *testing.T, state models.ToolEntryState, result *models.ToolResult, wantState models.ToolEntryState, wantStatus models.ToolResultStatus, wantJSON string) {
	t.Helper()
	if state != wantState || result == nil || result.Status != wantStatus {
		t.Fatalf("render = %q %#v; want %q %q", state, result, wantState, wantStatus)
	}
	if len(result.Content) == 0 || result.Content[0].Type != models.ContentPartJSON || string(result.Content[0].Value) != wantJSON {
		t.Fatalf("content = %#v\nwant json %s", result.Content, wantJSON)
	}
	entry := &models.SessionEntry{ID: "governed", Type: models.SessionEntryTool, State: state, StepID: "step-1", ToolCallID: "call-1",
		Tool: &models.ToolEntry{Name: "governed", SideEffect: models.ToolSideEffectWrite, Result: result}}
	if err := entry.Validate(); err != nil {
		t.Fatalf("result invalid for %s: %v", state, err)
	}
}

func TestRenderGovernedResultTable(t *testing.T) {
	type row struct {
		name     string
		call     func() *models.ActionCall
		approval *models.ApprovalCheckpoint
		settled  string
		state    models.ToolEntryState
		status   models.ToolResultStatus
		json     string
	}
	rows := []row{
		{name: "succeeded", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusSucceeded)
			c.Output = map[string]interface{}{"sha": "abc", "pushed": true}
			return c
		}, state: models.ToolEntryCompleted, status: models.ToolResultSucceeded,
			json: `{"evidenceRef":"action-call:call-1","output":{"pushed":true,"sha":"abc"}}`},
		{name: "succeeded after card", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusSucceeded)
			c.Output = map[string]interface{}{"requiresHuman": true}
			return c
		}, settled: "The person solved the challenge.", state: models.ToolEntryCompleted, status: models.ToolResultSucceeded,
			json: `{"evidenceRef":"action-call:call-1","outcome":"The person solved the challenge.","output":{"requiresHuman":true}}`},
		{name: "succeeded receipt", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusSucceeded)
			c.DuplicateOfActionCallID = "call-0"
			return c
		}, state: models.ToolEntryError, status: models.ToolResultDuplicateSuppressed,
			json: `{"evidenceRef":"action-call:call-1","note":"This exact operation was already performed earlier, so it was not sent again.","priorEvidenceRef":"action-call:call-0","status":"duplicate_suppressed"}`},
		{name: "recovered running write", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusFailed)
			c.RecoveredRunning, c.ErrorCode, c.Error, c.FailurePhase = true, models.ActionErrorCodeInterrupted, "lost", models.ActionFailureAfterDispatch
			return c
		}, state: models.ToolEntryInterrupted, status: models.ToolResultInterrupted,
			json: `{"error":"The previous operation may already have taken effect. It was not sent again. Check before retrying.","evidenceRef":"action-call:call-1","status":"interrupted"}`},
		{name: "recovered running read is a plain failure", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusFailed)
			c.SideEffect, c.RecoveredRunning, c.Error = skill.SideEffectRead, true, "timeout"
			return c
		}, state: models.ToolEntryError, status: models.ToolResultFailed,
			json: `{"error":"timeout","evidenceRef":"action-call:call-1","status":"failed"}`},
		{name: "failed", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusFailed)
			c.Error, c.ErrorCode, c.FailurePhase = "rate limited", "rate_limited", models.ActionFailureAfterDispatch
			c.ErrorDetails = map[string]string{"retryAfter": "30"}
			return c
		}, state: models.ToolEntryError, status: models.ToolResultFailed,
			json: `{"error":"rate limited","errorCode":"rate_limited","errorDetails":{"retryAfter":"30"},"evidenceRef":"action-call:call-1","failurePhase":"after_dispatch","status":"failed"}`},
		{name: "failed connect-once parked settled", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusFailed)
			c.SkillID, c.FailurePhase, c.Error = "mail", models.ActionFailureBeforeDispatch, "needs sign-in"
			c.Output = map[string]interface{}{"setupRequest": map[string]interface{}{"kind": models.SkillSetupKindConnectAccount, "id": skillbinding.ConnectAccountRequestPrefix + "call-1"}}
			return c
		}, settled: "The person you act for connected Google. Retry the step that needed it.", state: models.ToolEntryError, status: models.ToolResultFailed,
			json: `{"error":"An account sign-in was needed.","evidenceRef":"action-call:call-1","outcome":"The person you act for connected Google. Retry the step that needed it.","status":"failed"}`},
		{name: "denied by policy", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusDenied)
			c.Error = "policy forbids pushes"
			return c
		}, state: models.ToolEntryError, status: models.ToolResultDenied,
			json: `{"evidenceRef":"action-call:call-1","reason":"policy forbids pushes","status":"denied"}`},
		{name: "approval rejected", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusDenied)
			c.ApprovalID, c.Error = "approval-1", "rejected: not now"
			return c
		}, approval: governedResultApproval(models.ApprovalStatusRejected, "not now"), state: models.ToolEntryError, status: models.ToolResultDenied,
			json: `{"evidenceRef":"action-call:call-1","reason":"not now","status":"rejected"}`},
		{name: "approval changes requested", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusDenied)
			c.ApprovalID, c.Error = "approval-1", "changes_requested: use the main branch"
			return c
		}, approval: governedResultApproval(models.ApprovalStatusChangesRequested, "use the main branch"), state: models.ToolEntryError, status: models.ToolResultChangesRequested,
			json: `{"evidenceRef":"action-call:call-1","guidance":"use the main branch","status":"changes_requested"}`},
		{name: "approval superseded", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusDenied)
			c.ApprovalID = "approval-1"
			return c
		}, approval: governedResultApproval(models.ApprovalStatusSuperseded, action.ApprovalSupersededReason), state: models.ToolEntryError, status: models.ToolResultSuperseded,
			json: `{"evidenceRef":"action-call:call-1","reason":"` + action.ApprovalSupersededReason + `","status":"superseded"}`},
		{name: "approval expired", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusDenied)
			c.ApprovalID = "approval-1"
			return c
		}, approval: governedResultApproval(models.ApprovalStatusExpired, ""), state: models.ToolEntryError, status: models.ToolResultExpired,
			json: `{"evidenceRef":"action-call:call-1","reason":"Nobody answered the approval request in time; the action was not carried out.","status":"expired"}`},
		{name: "approval canceled", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusDenied)
			c.ApprovalID, c.Error = "approval-1", "the run was canceled"
			return c
		}, approval: governedResultApproval(models.ApprovalStatusCanceled, ""), state: models.ToolEntryError, status: models.ToolResultCanceled,
			json: `{"evidenceRef":"action-call:call-1","reason":"the run was canceled","status":"canceled"}`},
		{name: "canceled", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusCanceled)
			c.Error = "The run that made this call ended before it ran."
			return c
		}, state: models.ToolEntryError, status: models.ToolResultCanceled,
			json: `{"evidenceRef":"action-call:call-1","reason":"The run that made this call ended before it ran.","status":"canceled"}`},
		{name: "refused row", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusRefused)
			c.ErrorCode, c.Error = action.RefusedLoopCallRepeat, "already done"
			return c
		}, state: models.ToolEntryError, status: models.ToolResultRefused,
			json: `{"code":"` + action.RefusedLoopCallRepeat + `","error":"already done","evidenceRef":"action-call:call-1","status":"refused"}`},
		{name: "compensated", call: func() *models.ActionCall { return governedResultCall(models.ActionCallStatusCompensated) },
			state: models.ToolEntryError, status: models.ToolResultFailed,
			json: `{"error":"The action was reverted.","evidenceRef":"action-call:call-1","status":"failed"}`},
		{name: "compensating", call: func() *models.ActionCall { return governedResultCall(models.ActionCallStatusCompensating) },
			state: models.ToolEntryError, status: models.ToolResultFailed,
			json: `{"error":"The action was reverted.","evidenceRef":"action-call:call-1","status":"failed"}`},
		{name: "budget exhausted", call: func() *models.ActionCall {
			c := governedResultCall(models.ActionCallStatusDenied)
			c.Error = action.BudgetExhaustedReason
			return c
		}, state: models.ToolEntryError, status: models.ToolResultDenied,
			json: `{"evidenceRef":"action-call:call-1","reason":"This run has used its action budget.","status":"denied"}`},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			state, result := renderGovernedResult(r.call(), r.approval, r.settled)
			requireGovernedResult(t, state, result, r.state, r.status, r.json)
		})
	}

	t.Run("proposal rejection", func(t *testing.T) {
		state, result := renderGovernedRejection(&action.ProposalRejection{Code: action.ProposalRejectionToolArgumentsInvalid, Message: "path is required"})
		requireGovernedResult(t, state, result, models.ToolEntryError, models.ToolResultRefused,
			`{"code":"`+action.ProposalRejectionToolArgumentsInvalid+`","error":"path is required","status":"refused"}`)
	})
	t.Run("maintenance at proposal", func(t *testing.T) {
		state, result := renderGovernedMaintenanceRefusal()
		requireGovernedResult(t, state, result, models.ToolEntryError, models.ToolResultRefused,
			`{"code":"skill_maintenance","error":"This tool is temporarily unavailable while it is being updated. Try again later or use another way.","status":"refused"}`)
	})
	t.Run("in-flight external operation", func(t *testing.T) {
		state, result := renderGovernedInFlight("call-0")
		requireGovernedResult(t, state, result, models.ToolEntryError, models.ToolResultDuplicateSuppressed,
			`{"error":"The same operation is already in progress, so it was not sent again.","priorEvidenceRef":"action-call:call-0","status":"duplicate_suppressed"}`)
	})
	t.Run("budget exhausted without row", func(t *testing.T) {
		state, result := renderGovernedBudgetExhausted(nil)
		requireGovernedResult(t, state, result, models.ToolEntryError, models.ToolResultDenied,
			`{"reason":"This run has used its action budget.","status":"denied"}`)
	})
	t.Run("non-terminal calls do not render", func(t *testing.T) {
		for _, status := range []models.ActionCallStatus{models.ActionCallStatusReady, models.ActionCallStatusWaitingApproval, models.ActionCallStatusRunning} {
			if state, result := renderGovernedResult(governedResultCall(status), nil, ""); state != "" || result != nil {
				t.Fatalf("%s rendered %q %#v", status, state, result)
			}
		}
	})
}

func TestGovernedScreenshotBecomesImagePart(t *testing.T) {
	pixels := []byte("\x89PNG\r\n\x1a\nscreenshot")
	encoded := base64.StdEncoding.EncodeToString(pixels)
	call := governedResultCall(models.ActionCallStatusSucceeded)
	call.SideEffect = skill.SideEffectRead
	call.Output = map[string]interface{}{
		"url":        "https://example.com",
		"modelMedia": map[string]interface{}{"mediaType": "image/png", "contentBase64": encoded, "detail": "high"},
	}
	state, result := renderGovernedResult(call, nil, "")
	requireGovernedResult(t, state, result, models.ToolEntryCompleted, models.ToolResultSucceeded,
		`{"evidenceRef":"action-call:call-1","output":{"url":"https://example.com"}}`)
	if len(result.Content) != 2 {
		t.Fatalf("content = %#v; want json + image", result.Content)
	}
	digest := sha256.Sum256(pixels)
	image := result.Content[1]
	if image.Type != models.ContentPartImage || image.MediaType != "image/png" || image.Detail != "high" || image.ActionCallID != "call-1" ||
		image.SHA256 != hex.EncodeToString(digest[:]) || image.DataBase64 != "" {
		t.Fatalf("image part = %#v", image)
	}
	if strings.Contains(string(result.Content[0].Value), encoded) {
		t.Fatal("the screenshot bytes leaked into the json part")
	}
	if _, ok := call.Output["modelMedia"]; !ok {
		t.Fatal("rendering mutated the stored call output")
	}

	for name, media := range map[string]interface{}{
		"unsupported type": map[string]interface{}{"mediaType": "image/gif", "contentBase64": encoded},
		"bad detail":       map[string]interface{}{"mediaType": "image/png", "contentBase64": encoded, "detail": "ultra"},
		"not base64":       map[string]interface{}{"mediaType": "image/png", "contentBase64": "%%%"},
		"empty":            map[string]interface{}{"mediaType": "image/png", "contentBase64": ""},
		"too large":        map[string]interface{}{"mediaType": "image/png", "contentBase64": base64.StdEncoding.EncodeToString(make([]byte, models.MaxModelImageSourceBytes+1))},
		"not an object":    "attached",
	} {
		t.Run(name, func(t *testing.T) {
			bad := governedResultCall(models.ActionCallStatusSucceeded)
			bad.Output = map[string]interface{}{"url": "https://example.com", "modelMedia": media}
			state, result := renderGovernedResult(bad, nil, "")
			requireGovernedResult(t, state, result, models.ToolEntryCompleted, models.ToolResultSucceeded,
				`{"evidenceRef":"action-call:call-1","output":{"url":"https://example.com"}}`)
			if len(result.Content) != 1 {
				t.Fatalf("invalid media produced parts %#v", result.Content)
			}
		})
	}
}

// A file the tool returned that the host kept in the chat (a toolfile Ref)
// becomes an image part naming that chat file when it is an image the call
// itself returned; other Refs stay JSON only.
func TestGovernedFileRefImagesBecomeImageParts(t *testing.T) {
	ref := func(path, mimeType string, size int64) map[string]interface{} {
		return toolfile.Ref{Type: toolfile.RefType, Path: path, MimeType: mimeType, SizeBytes: size}.Value()
	}
	call := governedResultCall(models.ActionCallStatusSucceeded)
	call.SideEffect = skill.SideEffectRead
	call.Output = map[string]interface{}{
		"file": ref("tool-output/call-1/screenshot.png", "image/png", 1200),
		"attachments": []interface{}{
			map[string]interface{}{"preview": ref("tool-output/call-1/a.webp", "image/webp", 300)},
			ref("tool-output/call-1/report.pdf", "application/pdf", 900),
			map[string]interface{}{"type": toolfile.RefType, "mimeType": "image/png", "sizeBytes": float64(10), "note": "too large to keep"},
			ref("tool-output/call-2/other.png", "image/png", 50),
			ref("uploads/secret.png", "image/png", 50),
		},
	}
	state, result := renderGovernedResult(call, nil, "")
	if state != models.ToolEntryCompleted || result == nil {
		t.Fatalf("render = %q %#v", state, result)
	}
	want := []models.ContentPart{
		{Type: models.ContentPartImage, MediaType: "image/webp", Path: "tool-output/call-1/a.webp", SizeBytes: 300},
		{Type: models.ContentPartImage, MediaType: "image/png", Path: "tool-output/call-1/screenshot.png", SizeBytes: 1200},
	}
	if len(result.Content) != 1+len(want) || result.Content[0].Type != models.ContentPartJSON {
		t.Fatalf("content = %#v", result.Content)
	}
	for i, part := range want {
		if got := result.Content[1+i]; got.Type != part.Type || got.MediaType != part.MediaType || got.Path != part.Path ||
			got.SizeBytes != part.SizeBytes || got.ActionCallID != "" || got.DataBase64 != "" {
			t.Fatalf("image %d = %#v, want %#v", i, got, part)
		}
	}
	entry := &models.SessionEntry{ID: "governed", Type: models.SessionEntryTool, State: state, StepID: "step-1", ToolCallID: "call-1",
		Tool: &models.ToolEntry{Name: "governed", SideEffect: models.ToolSideEffectRead, Result: result}}
	if err := entry.Validate(); err != nil {
		t.Fatalf("result invalid: %v", err)
	}
}

func TestGovernedFileRefImagesAreCapped(t *testing.T) {
	call := governedResultCall(models.ActionCallStatusSucceeded)
	files := make([]interface{}, 0, models.MaxSessionEntryImageParts+3)
	for i := 0; i < models.MaxSessionEntryImageParts+3; i++ {
		files = append(files, toolfile.Ref{Type: toolfile.RefType, Path: fmt.Sprintf("tool-output/call-1/%02d.png", i), MimeType: "image/png", SizeBytes: 10}.Value())
	}
	call.Output = map[string]interface{}{
		"files":      files,
		"modelMedia": map[string]interface{}{"mediaType": "image/png", "contentBase64": base64.StdEncoding.EncodeToString([]byte("png"))},
	}
	_, result := renderGovernedResult(call, nil, "")
	images := 0
	for _, part := range result.Content {
		if part.Type == models.ContentPartImage {
			images++
		}
	}
	if images != models.MaxSessionEntryImageParts || result.Content[1].ActionCallID != "call-1" || result.Content[2].Path != "tool-output/call-1/00.png" {
		t.Fatalf("images = %d; content = %#v", images, result.Content)
	}
}
