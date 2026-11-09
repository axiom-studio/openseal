package runtime

import (
	"errors"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/runbook"
	"github.com/axiom-studio/openseal/pkg/skill"
)

func TestChatRunsActForThePersonWhoAsked(t *testing.T) {
	scope := Scope{Kind: "tenant", ID: "acting"}
	conversation := &Conversation{ID: "chat", Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}}
	initiator := ConversationParticipant{Type: ConversationParticipantUser, ID: "9"}
	for name, test := range map[string]struct {
		message *ChannelMessage
		want    string
	}{
		"user":              {&ChannelMessage{ID: "m1", Scope: scope, ConversationID: "chat", Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "7"}}, "7"},
		"service initiator": {&ChannelMessage{ID: "m2", Scope: scope, ConversationID: "chat", Sender: ConversationParticipant{Type: ConversationParticipantService, ID: "coordinator"}, Initiator: &initiator}, "9"},
		"agent":             {&ChannelMessage{ID: "m3", Scope: scope, ConversationID: "chat", Sender: ConversationParticipant{Type: ConversationParticipantAgent, ID: "helper"}}, ""},
	} {
		if got := conversationAgentRunRequest(conversation, test.message).ActingUser; got != test.want {
			t.Errorf("%s: ActingUser = %q, want %q", name, got, test.want)
		}
	}

	// A Team member's answer acts for the person the routing run acts for.
	routing := &AgentRun{ID: "routing", Scope: scope, Owner: conversation.Owner, ActingUser: "7", Context: map[string]interface{}{}}
	trigger := &ChannelMessage{ID: "m1", Scope: scope, ConversationID: "chat"}
	member := teamMemberRunRequest(routing, conversation, trigger, ConversationResponder{Participant: ConversationParticipant{Type: ConversationParticipantAgent, ID: "member"}}, "key")
	if member.ActingUser != "7" {
		t.Fatalf("Team member run ActingUser = %q", member.ActingUser)
	}
}

func TestCollaborationChildrenActForTheSourcePersonAndKeepTheirOwner(t *testing.T) {
	agent := ObjectiveOwner{Type: OwnerTypeAgent, ID: "a"}
	team := ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}
	recipient := CollaborationParty{Type: OwnerTypeAgent, ID: "b"}
	for name, test := range map[string]struct {
		source *AgentRun
		kind   AgentRequestKind
		want   ObjectiveOwner
	}{
		"1:1 request":          {&AgentRun{Kind: RunKindConversation, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindRequest, agent},
		"non-chat hand-off":    {&AgentRun{Kind: RunKindAgentWork, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindHandoff, ObjectiveOwner{Type: OwnerTypeAgent, ID: "b"}},
		"1:1 chat hand-off":    {&AgentRun{Kind: RunKindConversation, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindHandoff, ObjectiveOwner{Type: OwnerTypeAgent, ID: "b"}},
		"Team-chat hand-off":   {&AgentRun{Kind: RunKindConversation, Owner: team, AssignedAgentID: "a"}, AgentRequestKindHandoff, team},
		"Team-chat request":    {&AgentRun{Kind: RunKindConversation, Owner: team, AssignedAgentID: "a"}, AgentRequestKindRequest, team},
		"escalation from work": {&AgentRun{Kind: RunKindAgentWork, Owner: agent, AssignedAgentID: "a"}, AgentRequestKindEscalation, agent},
	} {
		test.source.ID, test.source.ActingUser = "source", "7"
		request := &AgentRequest{ID: "request", Kind: test.kind, Recipient: recipient, AssignedAgentID: "b", Goal: "Help"}
		if owner := collaborationChildOwner(test.source, request); owner != test.want {
			t.Errorf("%s: owner = %#v, want %#v", name, owner, test.want)
		}
		child := buildCollaborationChildRun(test.source, request, time.Now(), "child")
		if child.Owner != test.want || child.ActingUser != "7" {
			t.Errorf("%s: child owner %#v acting %q", name, child.Owner, child.ActingUser)
		}
	}
}

func TestRunSkillActorUmbrellaIsTheRunOwner(t *testing.T) {
	for name, test := range map[string]struct {
		run   *AgentRun
		valid bool
	}{
		"1:1":       {&AgentRun{ID: "r", Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "a"}, AssignedAgentID: "a", ActingUser: "7"}, true},
		"Team":      {&AgentRun{ID: "r", Owner: ObjectiveOwner{Type: OwnerTypeTeam, ID: "crew"}, AssignedAgentID: "a", ActingUser: "7"}, true},
		"delegated": {&AgentRun{ID: "r", Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "a"}, AssignedAgentID: "b", ActingUser: "7"}, false},
	} {
		actor := RunSkillActor(test.run)
		if actor.Umbrella != SkillUmbrella(test.run.Owner) || actor.AgentID != test.run.AssignedAgentID || actor.ActingUser != "7" || actor.Valid() != test.valid {
			t.Errorf("%s: actor = %#v valid=%v", name, actor, actor.Valid())
		}
	}
}

func TestDelegatedAgentRunWorksWithoutAnyConnection(t *testing.T) {
	catalog, scope := newUmbrellaFixture(t)
	// Agent "other" asked "member" for help: member's Gmail, member's Drive
	// and other's connections all stay out.
	delegated := &AgentRun{ID: "delegated", Scope: scope, Kind: RunKindAgentWork, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "other"}, AssignedAgentID: "member", Context: map[string]interface{}{}}
	got := umbrellaRunActions(t, catalog, delegated)
	if len(got) != 1 || got[0] != "member:notes" {
		t.Fatalf("delegated run actions = %v, want only member:notes", got)
	}

	validator, err := NewTeamSkillActionValidator(catalog)
	if err != nil {
		t.Fatal(err)
	}
	skillScope := skill.ScopeReference{Kind: scope.Kind, ID: scope.ID}
	for name, test := range map[string]struct {
		run     *AgentRun
		binding *skill.Binding
		ok      bool
	}{
		"1:1 own connection":        {&AgentRun{Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "member"}, AssignedAgentID: "member"}, &skill.Binding{Scope: skillScope, ID: "gmail", DeploymentID: "member", Credentials: map[string]skill.CredentialReference{"access": {Kind: "oauth2", ID: "member-grant"}}}, true},
		"delegated own connection":  {delegated, &skill.Binding{Scope: skillScope, ID: "gmail", DeploymentID: "member", Credentials: map[string]skill.CredentialReference{"access": {Kind: "oauth2", ID: "member-grant"}}}, false},
		"delegated plain binding":   {delegated, &skill.Binding{Scope: skillScope, ID: "notes", DeploymentID: "member"}, true},
		"delegated owner's binding": {delegated, &skill.Binding{Scope: skillScope, ID: "notes", DeploymentID: "other"}, false},
	} {
		_, err := validator.ValidateActionProposal(t.Context(), ActionProposalValidationInput{Run: test.run, Bound: &skill.BoundAction{Binding: test.binding}})
		if (err == nil) != test.ok {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestOnlyTheActingPersonSteersARun(t *testing.T) {
	store := NewMemoryStore()
	scope := Scope{Kind: "tenant", ID: "steering"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", ActingUser: "7",
		Goal: "Work for a person", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	commands := NewRunCommandService(store)
	command := func(kind AgentRunCommandKind, actor ActivityActor) error {
		current, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		instruction := ""
		if kind == AgentRunCommandIntervene {
			instruction = "Do it differently"
		}
		_, err = commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: run.ID, ExpectedRevision: current.Revision, Kind: kind, Actor: actor, Instruction: instruction, Summary: "steer"})
		return err
	}
	other := ActivityActor{Type: "user", ID: "8"}
	for _, kind := range []AgentRunCommandKind{AgentRunCommandIntervene, AgentRunCommandResolveHumanIntervention, AgentRunCommandResume, AgentRunCommandRetry} {
		if err := command(kind, other); !errors.Is(err, ErrRunCommandNotAuthorized) {
			t.Errorf("%s by another user: err = %v", kind, err)
		}
		for _, allowed := range []ActivityActor{{Type: "user", ID: "7"}, {Type: "system", ID: "kernel"}, {Type: "agent", ID: "agent"}} {
			if err := command(kind, allowed); errors.Is(err, ErrRunCommandNotAuthorized) {
				t.Errorf("%s by %#v refused", kind, allowed)
			}
		}
	}
	if err := command(AgentRunCommandIntervene, ActivityActor{Type: "user", ID: "7"}); err != nil {
		t.Fatalf("the acting person's guidance: %v", err)
	}
	if err := command(AgentRunCommandPause, other); err != nil {
		t.Fatalf("another user's pause: %v", err)
	}
	if err := command(AgentRunCommandCancel, other); err != nil {
		t.Fatalf("another user's cancel: %v", err)
	}

	// A run that acts for no person keeps today's rules.
	nobody, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent",
		Goal: "Work for no one", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commands.CommandAgentRun(t.Context(), AgentRunCommandRequest{Scope: scope, RunID: nobody.ID, ExpectedRevision: nobody.Revision, Kind: AgentRunCommandIntervene, Actor: other, Instruction: "Do it", Summary: "steer"}); err != nil {
		t.Fatalf("guidance on a run without a person: %v", err)
	}
}

func TestOnlyTheRoutineCreatorChangesItsScriptOrResumesIt(t *testing.T) {
	store := NewMemoryStore()
	ctx := t.Context()
	run, catalog, bound, args := scheduledTaskFixture(t, store)
	conversationID := run.Context[conversationRunContextConversationID].(string)
	files := &routineScriptFiles{}
	files.write(conversationID, "scripts/weekly.py", routineScriptDigestOf("a"))
	validator, _ := NewRunbookActionValidator(store)
	validator.SetChatFiles(files)
	dispatcher, _ := NewRunbookActionDispatcher(store, nil)
	dispatcher.SetChatFiles(files)
	dispatch := func(as *AgentRun, b *skill.BoundAction, arguments map[string]interface{}, callID string) (map[string]interface{}, error) {
		return dispatcher.DispatchAction(ctx, ActionDispatchInput{Run: as, Bound: b, Call: &ActionCall{ID: callID, Scope: as.Scope, RunID: as.ID, DeploymentID: as.AssignedAgentID, IdempotencyKey: callID}, Arguments: arguments})
	}
	proposed := cloneMap(args)
	proposed["script"] = map[string]interface{}{"path": "scripts/weekly.py", "command": "python3 scripts/weekly.py"}
	resolved, _, err := validator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{Run: run, Bound: bound, Arguments: proposed})
	if err != nil {
		t.Fatal(err)
	}
	created, err := dispatch(run, bound, resolved, "create")
	if err != nil {
		t.Fatal(err)
	}
	activation := created["activation"].(*RunbookActivation)
	if activation.Task.CreatedBy != "user" {
		t.Fatalf("routine creator = %q", activation.Task.CreatedBy)
	}

	// Someone else in the same chat.
	intruder, err := NewPortfolioService(store).CreateAgentRun(ctx, CreateAgentRunRequest{Scope: run.Scope, Kind: RunKindConversation, Owner: run.Owner, AssignedAgentID: run.AssignedAgentID,
		ActingUser: "intruder", Goal: "Handle another request", Source: RunSourceChat, Context: cloneMap(run.Context), IdempotencyKey: "intruder"})
	if err != nil {
		t.Fatal(err)
	}
	statusBound, err := catalog.Resolve(ctx, bound.Binding.Scope, run.AssignedAgentID, bound.Definition.ID, bound.Definition.Version, RunbookActionSetStatus)
	if err != nil {
		t.Fatal(err)
	}
	setStatus := func(as *AgentRun, status RunbookActivationStatus, callID string) (*RunbookActivation, error) {
		current, err := store.GetRunbookActivation(ctx, run.Scope, activation.ID)
		if err != nil {
			t.Fatal(err)
		}
		result, err := dispatch(as, statusBound, map[string]interface{}{"activationId": activation.ID, "expectedRevision": current.Revision, "status": string(status), "reason": "change"}, callID)
		if err != nil {
			return nil, err
		}
		return result["activation"].(*RunbookActivation), nil
	}
	if _, err := setStatus(intruder, RunbookActivationPaused, "intruder-pause"); err != nil {
		t.Fatalf("another person's pause: %v", err)
	}
	if _, err := setStatus(intruder, RunbookActivationActive, "intruder-resume"); !errors.Is(err, ErrRoutineNotCreator) {
		t.Fatalf("another person's resume: %v", err)
	}
	if _, err := setStatus(run, RunbookActivationActive, "creator-resume"); err != nil {
		t.Fatalf("the creator's resume: %v", err)
	}

	files.write(conversationID, "scripts/weekly.py", routineScriptDigestOf("b"))
	current, _ := store.GetRunbookActivation(ctx, run.Scope, activation.ID)
	repinBound := &skill.BoundAction{Definition: bound.Definition, Binding: bound.Binding, Action: RunbookManagementSkill().Actions[RunbookActionSetScript]}
	repin, _, err := validator.ResolveActionProposalArguments(ctx, ActionProposalValidationInput{Run: run, Bound: repinBound, Arguments: map[string]interface{}{"activationId": activation.ID, "expectedRevision": float64(current.Revision)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateActionProposal(ctx, ActionProposalValidationInput{Run: intruder, Bound: repinBound, Arguments: repin}); !errors.Is(err, ErrRoutineNotCreator) {
		t.Fatalf("another person's script proposal: %v", err)
	}
	if _, err := dispatch(intruder, repinBound, repin, "intruder-repin"); !errors.Is(err, ErrRoutineNotCreator) {
		t.Fatalf("another person's script change: %v", err)
	}
	if _, err := dispatch(run, repinBound, repin, "creator-repin"); err != nil {
		t.Fatalf("the creator's script change: %v", err)
	}
	if _, err := dispatch(intruder, repinBound, repin, "creator-repin"); !errors.Is(err, ErrRoutineNotCreator) {
		t.Fatalf("another person replayed the creator's script change: %v", err)
	}

	// A routine with no recorded creator becomes the routine of whoever
	// changes it next.
	current, _ = store.GetRunbookActivation(ctx, run.Scope, activation.ID)
	orphan := cloneRunbookActivation(current)
	orphan.Task.CreatedBy, orphan.Status = "", RunbookActivationPaused
	orphan.Revision++
	if err := store.UpdateRunbookActivation(ctx, orphan, current.Revision); err != nil {
		t.Fatal(err)
	}
	stamped, err := setStatus(intruder, RunbookActivationActive, "adopt")
	if err != nil || stamped.Task.CreatedBy != "intruder" {
		t.Fatalf("adopted routine = %#v, %v", stamped, err)
	}
	if _, err := setStatus(run, RunbookActivationPaused, "creator-pause"); err != nil {
		t.Fatal(err)
	}
	if _, err := setStatus(run, RunbookActivationActive, "former-creator-resume"); !errors.Is(err, ErrRoutineNotCreator) {
		t.Fatalf("former creator resumed an adopted routine: %v", err)
	}
}

func TestRoutineRunsActForTheirCreatorOnlyWithATask(t *testing.T) {
	// A started routine with a task acts for its creator. (A task routine is
	// always scheduled; its scheduled occurrences are checked in
	// TestScheduledTaskPersistsExecutesReportsAndRetiresAcrossStores.)
	store := NewMemoryStore()
	run, _, bound, args := scheduledTaskFixture(t, store)
	activation := dispatchTaskForTest(t, store, run, bound, args, "create")["activation"].(*RunbookActivation)
	started, err := StartRunbookActivation(t.Context(), store, run.Scope, activation.ID, StartRunbookActivationRequest{IdempotencyKey: "start-now", Actor: ActivityActor{Type: "user", ID: "user"}})
	if err != nil || started.Run.ActingUser != "user" {
		t.Fatalf("started routine run = %#v, %v", started, err)
	}

	// An event routine has no task and acts for no one.
	acceptedCreationStores(t, func(t *testing.T, store KernelStore) {
		activation, preparer := acceptedCreationActivation(t, store, runbook.Trigger{Kind: runbook.TriggerEvent, EventType: "message.received", Entrypoint: "review"})
		router := NewRunbookEventRouter(store)
		router.SetAcceptedRunExecutionPreparer(preparer)
		routed, err := router.Route(t.Context(), EventEnvelope{ID: "message", Scope: activation.Scope, Type: "message.received", Source: "verified", Subject: "thread", OccurredAt: time.Now().UTC()})
		if err != nil || len(routed.Routes) != 1 || routed.Routes[0].Run.ActingUser != "" {
			t.Fatalf("event routine run = %#v, %v", routed, err)
		}
	})
}
