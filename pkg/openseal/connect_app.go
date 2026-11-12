package openseal

import (
	"context"
	"errors"
	"time"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/skillbinding"
	"github.com/axiom-studio/openseal/pkg/skill"
)

type (
	AppCatalog       = skillbinding.AppCatalog
	AppCatalogEntry  = skillbinding.AppCatalogEntry
	ConnectAppTarget = models.ConnectAppTarget
)

const (
	SkillActionFindApps          = skillbinding.SkillActionFindApps
	SkillActionRequestConnection = agentrun.SkillActionRequestConnection
	SkillSetupKindConnectApp     = models.SkillSetupKindConnectApp
	ConnectAppWakeType           = skillbinding.ConnectAppWakeType
	AppSignInOAuth               = models.AppSignInOAuth
	AppSignInAPIKey              = models.AppSignInAPIKey
	AppSignInNone                = models.AppSignInNone
	AppSignInUnknown             = models.AppSignInUnknown
)

// ErrConnectAppNotConnected means the app a connect card asks for is not yet
// connected for the requesting Run.
var ErrConnectAppNotConnected = errors.New("the requested app is not connected yet")

// NormalizeAppAddress returns an MCP server address as connections store it.
func NormalizeAppAddress(value string) (string, error) { return models.NormalizeAppAddress(value) }

// SetAppCatalog installs the host's catalog of connectable apps, searched by
// every Agent's find_apps and request_connection. Hosts set it once at startup.
func (e *Engine) SetAppCatalog(catalog AppCatalog) {
	if e == nil {
		return
	}
	if e.appCatalog == nil {
		e.appCatalog = &skillbinding.AppCatalogRef{}
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
// requested app to be connected for the requesting Run and its person: the
// host binding provider, asked for that Run, must supply its connection, so
// the user and workspace rules are exactly those of connected apps.
// Dismissing needs no evidence. Either way the waiting Run (and every
// sibling card's run) resumes. A per_person card is resolved by the person it
// is for; any other card by a manager of its umbrella.
func (e *Engine) ResolveConnectAppRequest(ctx context.Context, scope models.Scope, owner ObjectiveOwner, id string, expected int64, resolver InteractionResolver, dismiss bool) (*ConnectAppResolution, error) {
	actor, err := resolver.actor()
	if err != nil {
		return nil, err
	}
	r, err := e.GetSkillSetupRequest(ctx, scope, owner, id)
	if err != nil {
		return nil, err
	}
	if r.Kind != models.SkillSetupKindConnectApp || r.App == nil {
		return nil, models.ErrInvalidSkillSetup
	}
	if err := authorizeSkillSetupInteraction(resolver, r, dismiss); err != nil {
		return nil, err
	}
	want := "resolved"
	if dismiss {
		want = "dismissed"
	}
	var tools []string
	if r.Status == "pending" {
		if r.Revision != expected {
			return nil, models.ErrSkillSetupConflict
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
		return nil, models.ErrSkillSetupConflict
	} else if !dismiss {
		// A retry after a failed resume still names the tools.
		if _, toolNames, lookupErr := e.connectedAppFor(ctx, r); lookupErr == nil {
			tools = toolNames
		}
	}
	resume := func(ctx context.Context, request *models.SkillSetupRequest) (bool, error) {
		return e.continueInteractionRun(ctx, interactionContinuation{
			scope: request.Scope, owner: request.Owner, agentID: request.AgentID, runID: request.RunID, requestID: request.ID, actionCallID: request.ActionCallID,
			status: request.Status, revision: request.Revision, updatedAt: request.UpdatedAt, wakeType: skillbinding.ConnectAppWakeType, coordinator: "connect-app-coordinator",
			eventType: "run.connection_request_settled", summary: "Received the connection request result; continuing",
			interventionPrefix: "connect-app-result:", instruction: skillbinding.ConnectAppResolutionInstruction(request, tools),
			requestedFor: request.RequestedFor,
		})
	}
	e.settleSkillSetupSiblings(ctx, r, resume)
	continued, err := resume(ctx, r)
	if err != nil {
		return nil, err
	}
	return &ConnectAppResolution{Request: r, Continued: continued}, nil
}

// connectedAppFor returns the connection of the request's umbrella for its
// app, as the person the request is for uses it, with the connection's model
// tool names.
func (e *Engine) connectedAppFor(ctx context.Context, r *SkillSetupRequest) (*skill.Binding, []string, error) {
	// A builder's request for another Agent reads that Agent's connections.
	agentID := r.AgentID
	if r.Targeted() {
		agentID = r.Owner.ID
	}
	actor := skill.RunActor{RunID: r.RunID, AgentID: agentID, Umbrella: models.SkillUmbrella(r.Owner), ActingUser: r.RequestedFor}
	set, err := e.skills.HostBindings(ctx, skill.ScopeReference{Kind: r.Scope.Kind, ID: r.Scope.ID}, r.Owner.ID, actor)
	if err != nil {
		return nil, nil, err
	}
	binding := skillbinding.ConnectedAppBinding(set, *r.App)
	if binding == nil {
		return nil, nil, nil
	}
	tools, err := skillbinding.ConnectedAppToolNames(ctx, e.skills, binding)
	return binding, tools, err
}

// ReconcileConnectAppRequests resolves the workspace's pending connect cards
// whose app is now connected for their Run, for example right after the user
// finished connecting it in Settings or from the card. It returns the
// resolved ones; a failure leaves a card for the next reconciliation.
func (e *Engine) ReconcileConnectAppRequests(ctx context.Context, scope models.Scope) ([]*ConnectAppResolution, error) {
	matches, err := e.pendingSkillSetupRequests(ctx, scope, func(request *models.SkillSetupRequest) bool {
		return request.Kind == models.SkillSetupKindConnectApp && request.App != nil
	})
	if err != nil {
		return nil, err
	}
	var resolved []*ConnectAppResolution
	var failures []error
	for _, request := range matches {
		result, err := e.ResolveConnectAppRequest(ctx, scope, request.Owner, request.ID, request.Revision, InteractionResolver{System: connectAppReconciliationActor}, false)
		switch {
		case err == nil:
			resolved = append(resolved, result)
		case errors.Is(err, ErrConnectAppNotConnected), errors.Is(err, models.ErrSkillSetupConflict):
		default:
			failures = append(failures, err)
		}
	}
	return resolved, errors.Join(failures...)
}

const connectAppReconciliationActor = "system:connection-reconciliation"
