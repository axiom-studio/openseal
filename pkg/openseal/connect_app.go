package openseal

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type (
	AppCatalog       = runtime.AppCatalog
	AppCatalogEntry  = runtime.AppCatalogEntry
	ConnectAppTarget = runtime.ConnectAppTarget
)

const (
	SkillActionFindApps          = runtime.SkillActionFindApps
	SkillActionRequestConnection = runtime.SkillActionRequestConnection
	SkillSetupKindConnectApp     = runtime.SkillSetupKindConnectApp
	ConnectAppWakeType           = runtime.ConnectAppWakeType
	AppSignInOAuth               = runtime.AppSignInOAuth
	AppSignInAPIKey              = runtime.AppSignInAPIKey
	AppSignInNone                = runtime.AppSignInNone
	AppSignInUnknown             = runtime.AppSignInUnknown
)

// ErrConnectAppNotConnected means the app a connect card asks for is not yet
// connected for the requesting Run.
var ErrConnectAppNotConnected = errors.New("the requested app is not connected yet")

// NormalizeAppAddress returns an MCP server address as connections store it.
func NormalizeAppAddress(value string) (string, error) { return runtime.NormalizeAppAddress(value) }

// SetAppCatalog installs the host's catalog of connectable apps, searched by
// every Agent's find_apps and request_connection. Hosts set it once at startup.
func (e *Engine) SetAppCatalog(catalog AppCatalog) {
	if e == nil {
		return
	}
	if e.appCatalog == nil {
		e.appCatalog = &runtime.AppCatalogRef{}
	}
	e.appCatalog.Set(catalog)
}

// ConnectAppResolution is a settled connect card and what happened to the
// Run that asked for it.
type ConnectAppResolution struct {
	Request *SkillSetupRequest `json:"request"`
	// Continued is true when the requesting Run resumed (or is still active
	// and will see the new tools). False means the Run already ended; the
	// host should start a new reply for the conversation.
	Continued bool `json:"continued"`
}

// ResolveConnectAppRequest settles a connect card. Resolving needs the
// requested app to be connected for the requesting Run: the host binding
// provider, asked for that Run, must supply its connection, so the user and
// workspace rules are exactly those of connected apps. Dismissing needs no
// evidence. Either way the waiting Run resumes. Hosts authorize the actor.
func (e *Engine) ResolveConnectAppRequest(ctx context.Context, scope runtime.Scope, deploymentID, id string, expected int64, actor string, dismiss bool) (*ConnectAppResolution, error) {
	if strings.TrimSpace(actor) == "" {
		return nil, runtime.ErrInvalidSkillSetup
	}
	r, err := e.GetSkillSetupRequest(ctx, scope, deploymentID, id)
	if err != nil {
		return nil, err
	}
	if r.Kind != runtime.SkillSetupKindConnectApp || r.App == nil {
		return nil, runtime.ErrInvalidSkillSetup
	}
	want := "resolved"
	if dismiss {
		want = "dismissed"
	}
	var tools []string
	if r.Status == "pending" {
		if r.Revision != expected {
			return nil, runtime.ErrSkillSetupConflict
		}
		if !dismiss {
			binding, toolNames, lookupErr := e.connectedAppFor(ctx, r)
			if lookupErr != nil {
				return nil, lookupErr
			}
			if binding == nil {
				return nil, ErrConnectAppNotConnected
			}
			r.ResolvedBindingID, r.ResolvedBindingRevision, tools = binding.ID, binding.Revision, toolNames
		}
		r.Status, r.ResolvedBy = want, actor
		r.Revision++
		r.UpdatedAt = time.Now().UTC()
		if err := e.store.SaveSkillSetupRequest(ctx, r, expected); err != nil {
			return nil, err
		}
	} else if r.Status != want {
		return nil, runtime.ErrSkillSetupConflict
	} else if !dismiss {
		// A retry after a failed resume still names the tools.
		if _, toolNames, lookupErr := e.connectedAppFor(ctx, r); lookupErr == nil {
			tools = toolNames
		}
	}
	continued, err := e.continueInteractionRun(ctx, interactionContinuation{
		scope: r.Scope, runID: r.RunID, requestID: r.ID, actionCallID: r.ActionCallID, status: r.Status, revision: r.Revision,
		updatedAt: r.UpdatedAt, wakeType: runtime.ConnectAppWakeType, coordinator: "connect-app-coordinator",
		eventType: "run.connection_request_settled", summary: "Received the connection request result; continuing",
		interventionPrefix: "connect-app-result:", instruction: runtime.ConnectAppResolutionInstruction(r, tools),
	})
	if err != nil {
		return nil, err
	}
	return &ConnectAppResolution{Request: r, Continued: continued}, nil
}

// connectedAppFor returns the connection the request's Run can use for its
// app, with the connection's model tool names.
func (e *Engine) connectedAppFor(ctx context.Context, r *SkillSetupRequest) (*skill.Binding, []string, error) {
	set, err := e.skills.HostBindings(ctx, skill.ScopeReference{Kind: r.Scope.Kind, ID: r.Scope.ID}, r.DeploymentID, skill.RunActor{RunID: r.RunID, AgentID: r.DeploymentID})
	if err != nil {
		return nil, nil, err
	}
	binding := runtime.ConnectedAppBinding(set, *r.App)
	if binding == nil {
		return nil, nil, nil
	}
	tools, err := runtime.ConnectedAppToolNames(ctx, e.skills, binding)
	return binding, tools, err
}

// ReconcileConnectAppRequests resolves the workspace's pending connect cards
// whose app is now connected for their Run, for example right after the user
// finished connecting it in Settings or from the card. It returns the
// resolved ones; a failure leaves a card for the next reconciliation.
func (e *Engine) ReconcileConnectAppRequests(ctx context.Context, scope runtime.Scope) ([]*ConnectAppResolution, error) {
	pending, ok := e.store.(interface {
		ListPendingSkillSetupRequests(context.Context, runtime.Scope, int, int) ([]*runtime.SkillSetupRequest, error)
	})
	if !ok || scope.Validate() != nil {
		return nil, nil
	}
	const pageSize, maximumPages = 100, 20
	var matches []*runtime.SkillSetupRequest
	for page := 0; page < maximumPages; page++ {
		requests, err := pending.ListPendingSkillSetupRequests(ctx, scope, pageSize, page*pageSize)
		if err != nil {
			return nil, err
		}
		for _, request := range requests {
			if request.Kind == runtime.SkillSetupKindConnectApp && request.App != nil {
				matches = append(matches, request)
			}
		}
		if len(requests) < pageSize {
			break
		}
	}
	var resolved []*ConnectAppResolution
	var failures []error
	for _, request := range matches {
		result, err := e.ResolveConnectAppRequest(ctx, scope, request.DeploymentID, request.ID, request.Revision, connectAppReconciliationActor, false)
		switch {
		case err == nil:
			resolved = append(resolved, result)
		case errors.Is(err, ErrConnectAppNotConnected), errors.Is(err, runtime.ErrSkillSetupConflict):
		default:
			failures = append(failures, err)
		}
	}
	return resolved, errors.Join(failures...)
}

const connectAppReconciliationActor = "system:connection-reconciliation"
