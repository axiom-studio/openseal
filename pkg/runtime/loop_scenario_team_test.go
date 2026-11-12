package runtime

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/control"
	runtimeconversation "github.com/axiom-studio/openseal/pkg/runtime/conversation"
	"github.com/axiom-studio/openseal/pkg/runtime/conversationrun"
	"github.com/axiom-studio/openseal/pkg/runtime/looptest"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
	"github.com/axiom-studio/openseal/pkg/runtime/runloop"
	"github.com/axiom-studio/openseal/pkg/skill"
	kernelteam "github.com/axiom-studio/openseal/pkg/team"
)

// convTeamCatalog is the Agent catalog seam a Team member's control authority
// reads its delegation roster from: an active Team with no other eligible
// member, so the member gets no delegate targets.
type convTeamCatalog struct {
	conversationrun.AgentTurnCatalog
}

func (convTeamCatalog) GetTeamDeployment(context.Context, skill.ScopeReference, string) (*kernelteam.Deployment, error) {
	return &kernelteam.Deployment{Status: kernelteam.DeploymentActive}, nil
}

func (convTeamCatalog) GetTeamDefinition(context.Context, string, string) (*kernelteam.Definition, error) {
	return nil, nil
}

// convMemberBinding is the fixture's chat binding for the Runs the fixture
// cannot bind: a Team member Run, whose control authority is resolved through
// the Team catalog seam (the fixture resolves it without a catalog, which a
// Team-owned Run cannot do), and a thread Run, whose lane session is opened
// with its thread root (the fixture opens the chat's unthreaded lane). The
// conversation turn runner adds the conversation loop seams on top of
// convMemberAgentBinding, as it does for the fixture's own binding.
func convMemberBinding(f *looptest.Fixture, run *models.AgentRun) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	return f.ConversationBinding(run, func(run *models.AgentRun) *agentrun.TurnRunnerBinding { return convMemberAgentBinding(f, run) })
}

// convMemberAgentBinding is the member or thread Run's own Agent binding:
// its lane session, its control authority, the model producer, the run-start
// preparer and the claim tool executor.
func convMemberAgentBinding(f *looptest.Fixture, run *models.AgentRun) *agentrun.TurnRunnerBinding {
	f.T.Helper()
	sessions := f.Store.(models.AgentSessionStore)
	conversationID, _ := run.Context[models.ConversationRunConversationIDContextKey].(string)
	root, _ := run.Context["threadRootMessageId"].(string)
	opened, err := sessions.OpenConversationAgentSession(f.Ctx, models.OpenConversationAgentSessionRequest{
		Scope: f.Scope, ConversationID: conversationID, ThreadRootID: root, AgentID: run.AssignedAgentID, RunID: run.ID,
		Lease: models.AgentRunLeaseGuard{WorkerID: f.WorkerID, Now: f.Clock.Now()}, Now: f.Clock.Now(),
	})
	if err != nil {
		f.T.Fatalf("open conversation session: %v", err)
	}
	session := opened.Session
	control, err := control.ResolveControlAuthority(f.Ctx, control.AuthorityInput{
		Run: run, DeploymentID: run.AssignedAgentID, Runs: f.Store, Conversations: f.Chats, Catalog: convTeamCatalog{},
	})
	if err != nil {
		f.T.Fatalf("resolve member authority: %v", err)
	}
	preparer := runloop.NewCatalogModelStepPreparer(runloop.CatalogModelStepPreparerConfig{
		AgentInstructions: []string{"You are " + run.AssignedAgentID + ", the fixture Agent."},
	})
	host := looptest.FakeStepHost{ScriptedModel: f.Model, FakeHostTools: f.Tools}
	producer, err := modelstep.NewProducer(host, preparer, f.Store)
	if err != nil {
		f.T.Fatal(err)
	}
	binding := &agentrun.TurnRunnerBinding{
		DeploymentID: run.AssignedAgentID, ActionDeploymentID: looptest.FixtureAgent,
		DefinitionID: looptest.FixtureDefinition, DefinitionVersion: "1",
		ModelProvider: string(looptest.ModelProvider), Model: looptest.ModelName,
		ModelActions: append([]capability.ModelAction(nil), f.Offered...),
		Producer:     producer, Session: session, Host: host, Authority: control,
		Preparers: []agentrun.StepPreparer{runloop.NewRunStartPreparer(sessions, f.Store)},
		Tools:     f.Dispatcher.ForClaim(runloop.LoopToolClaim{}),
	}
	return binding
}

// convDriveNextMember claims the next queued Run (a Team member or thread
// Run) and drives it to completion with convMemberBinding.
func convDriveNextMember(f *looptest.Fixture) *models.AgentRun {
	f.T.Helper()
	claimed, err := f.Store.ClaimNextAgentRun(f.Ctx, models.AgentRunClaim{
		Scope: f.Scope, WorkerID: f.WorkerID, Now: f.Clock.Now(), LeaseDuration: looptest.FixtureLease, AgingInterval: time.Minute,
	})
	if err != nil || claimed == nil {
		f.T.Fatalf("claim next member: %#v, %v", claimed, err)
	}
	for i := 0; ; i++ {
		binding := convMemberBinding(f, claimed)
		if binding.Session != nil && claimed.SessionID != binding.Session.ID {
			claimed = f.MustRun(claimed.ID)
		}
		exit := f.Loop.Drive(f.Ctx, runloop.Claim{Run: claimed, WorkerID: f.WorkerID, Binding: binding})
		if exit.Err != nil && exit.Reason != runloop.ExitRequeued {
			f.T.Fatalf("member %s exited %s: %v", claimed.ID, exit.Reason, exit.Err)
		}
		run := f.MustRun(claimed.ID)
		if run.Status == models.AgentRunStatusCompleted {
			return run
		}
		if run.Status != models.AgentRunStatusQueued || i == 5 {
			f.T.Fatalf("member %s = %s (%#v), log %s", run.ID, run.Status, run.FailureExplanation, steerShape(f, run.ID))
		}
		if run.AvailableAt.After(f.Clock.Now()) {
			f.Clock.Advance(run.AvailableAt.Sub(f.Clock.Now()) + time.Millisecond)
		}
		claimed = f.Claim(run.ID)
	}
}

// convRoute schedules a Team message, drives its routing Run (the conversation
// runner's deterministic routing binding) and returns the routing Run.
func convRoute(f *looptest.Fixture, conversationID string, trigger *models.ChannelMessage) *models.AgentRun {
	f.T.Helper()
	routing := convSchedule(f, conversationID, trigger)
	if routing.AssignedAgentID != "" || routing.SessionID != "" {
		f.T.Fatalf("routing Run = %#v", routing)
	}
	claimed := f.Claim(routing.ID)
	binding, err := f.Runner.ResolveTurnRunner(f.Ctx, claimed)
	if err != nil {
		f.T.Fatal(err)
	}
	if exit := f.Loop.Drive(f.Ctx, runloop.Claim{Run: claimed, WorkerID: f.WorkerID, Binding: binding}); exit.Reason != runloop.ExitFinal {
		f.T.Fatalf("routing exit = %#v", exit)
	}
	return f.MustRun(routing.ID)
}

// TestTeamMembersHaveTheirOwnSessionsAndTheRouterHasNone: a Team message
// addressed to two members is routed by a deterministic routing Run that has
// no session and makes no model call; each member answers in its own lane
// session, and the second member sees the first member's answer as a user
// entry attributed to that teammate.
func TestTeamMembersHaveTheirOwnSessionsAndTheRouterHasNone(t *testing.T) {
	looptest.EachStore(t, "conv-team", func(t *testing.T, store models.KernelStore) {
		f := looptest.NewFixture(t, store, looptest.WithTeam("finn", "scout"))
		f.Model.Add(&looptest.ModelRule{Name: "member", Reply: func(v looptest.StepView) models.ModelStepResult {
			return looptest.FinalAnswer(map[string]any{"answer": v.Request().AgentID + " checked the canary."})
		}})
		team := models.ObjectiveOwner{Type: models.OwnerTypeTeam, ID: "team"}
		conversation := convChat(f, team, nil)
		trigger := convPost(f, conversation.ID, "@finn @scout can you both check the canary?", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.Mentions = []models.ConversationParticipant{{Type: models.ConversationParticipantAgent, ID: "finn"}, {Type: models.ConversationParticipantAgent, ID: "scout"}}
			r.RequiresResponse = true
		})
		routed := convRoute(f, conversation.ID, trigger)
		routing := routed
		ids, _ := routed.Output["responderRunIds"].([]interface{})
		if routed.Status != models.AgentRunStatusCompleted || routed.SessionID != "" || len(ids) != 2 {
			t.Fatalf("routing Run = %s session %q output %#v", routed.Status, routed.SessionID, routed.Output)
		}
		for _, req := range f.Model.Calls("") {
			if req.RunID == routing.ID {
				t.Fatalf("the routing Run called the model: %s", looptest.StepView{Req: req}.Describe())
			}
		}

		members := map[string]*models.AgentRun{}
		for _, id := range ids {
			run := f.MustRun(fmt.Sprint(id))
			members[run.AssignedAgentID] = run
		}
		if members["finn"] == nil || members["scout"] == nil {
			t.Fatalf("member Runs = %#v", members)
		}
		// The members answer one after the other; whoever goes first ("finn"
		// below) is seen by the second ("scout").
		finn := convDriveNextMember(f)
		finnAgent := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: finn.AssignedAgentID}
		finnAnswer := convAgentAnswer(f, conversation.ID)
		if finnAnswer.Sender != finnAgent || finnAnswer.Content != finn.AssignedAgentID+" checked the canary." {
			t.Fatalf("first answer = %#v", finnAnswer)
		}
		scout := convDriveNextMember(f)
		if scout.AssignedAgentID == finn.AssignedAgentID {
			t.Fatalf("one member answered twice: %s", scout.AssignedAgentID)
		}

		// Each member has its own lane session; the router has none.
		for _, run := range []*models.AgentRun{finn, scout} {
			agent := run.AssignedAgentID
			lane := models.ConversationRunLane(conversation.ID, "", agent)
			session := f.Session(run.ID)
			if run.ConcurrencyKey != lane || session.ID != models.ConversationSessionID(f.Scope, lane) || session.AgentID != agent || session.ConversationID != conversation.ID {
				t.Fatalf("%s session = %#v (run lane %q)", agent, session, run.ConcurrencyKey)
			}
		}
		if finn.SessionID == scout.SessionID {
			t.Fatal("members share a session")
		}

		// The second member answered the trigger, which came before the first
		// member's answer: its step saw only the trigger. When it is asked
		// again, catch-up brings the teammate's answer in as a user entry
		// attributed to that teammate, and skips its own earlier answer.
		scoutAnswer := convAgentAnswer(f, conversation.ID)
		again := convPost(f, conversation.ID, "@"+scout.AssignedAgentID+" what did your teammate find?", func(r *runtimeconversation.PostChannelMessageRequest) {
			r.Mentions = []models.ConversationParticipant{{Type: models.ConversationParticipantAgent, ID: scout.AssignedAgentID}}
			r.RequiresResponse = true
		})
		if rerouted := convRoute(f, conversation.ID, again); len(rerouted.Output["responderRunIds"].([]interface{})) != 1 {
			t.Fatalf("second routing = %#v", rerouted.Output)
		}
		scoutAgain := convDriveNextMember(f)
		if scoutAgain.AssignedAgentID != scout.AssignedAgentID || scoutAgain.SessionID != scout.SessionID {
			t.Fatalf("second member Run = %#v", scoutAgain)
		}
		caught := convUserBeforeAssistant(f, scoutAgain.ID)
		if ids := convMessageIDs(caught); len(ids) != 2 || ids[0] != finnAnswer.ID || ids[1] != again.ID {
			t.Fatalf("second member caught up %v, want [%s %s] (its own answer %s skipped)", ids, finnAnswer.ID, again.ID, scoutAnswer.ID)
		}
		if seen := caught[0]; convAuthor(seen) != "agent:"+finn.AssignedAgentID ||
			!strings.Contains(looptest.PartsText(seen.User.Content), "(agent, teammate "+finn.AssignedAgentID+")") {
			t.Fatalf("view of the teammate's answer = %s %q", convAuthor(seen), looptest.PartsText(seen.User.Content))
		}
		var scoutRequest models.ModelStepRequest
		for _, req := range f.Model.Calls(models.ModelStepPurposeStep) {
			if req.RunID == scoutAgain.ID {
				scoutRequest = req
				break
			}
		}
		if !slices.ContainsFunc(looptest.StepView{Req: scoutRequest}.UserTexts(), func(text string) bool { return strings.Contains(text, finnAnswer.Content) }) {
			t.Fatalf("the teammate's answer is not rendered: %s", looptest.StepView{Req: scoutRequest}.Describe())
		}
		if text := convRunContext(f, convFirstStep(f, scout.ID)); !strings.Contains(text, "Team roster:") {
			t.Fatalf("member run_context has no team roster: %q", text)
		}

		// finn's session holds its own answer once: as the assistant entry,
		// never caught up again as a user entry.
		assistant := 0
		for _, entry := range f.Entries(finn.ID) {
			if entry.Type == models.SessionEntryUser && entry.User != nil && entry.User.MessageID == finnAnswer.ID {
				t.Fatalf("finn caught up its own answer: %#v", entry)
			}
			if entry.Type == models.SessionEntryAssistant && entry.RunID == finn.ID {
				assistant++
			}
		}
		if assistant != 1 {
			t.Fatalf("finn's assistant entries = %d", assistant)
		}
	})
}
