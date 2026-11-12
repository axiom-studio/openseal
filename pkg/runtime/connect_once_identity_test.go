package runtime

import (
	"errors"
	"maps"
	"testing"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
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
		_, err := validator.ValidateActionProposal(t.Context(), agentrun.ActionProposalValidationInput{Run: test.run, Bound: &skill.BoundAction{Binding: test.binding}})
		if (err == nil) != test.ok {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestOnlyTheActingPersonSteersARun(t *testing.T) {
	store := newTestKernelStore(t)
	scope := Scope{Kind: "tenant", ID: "steering"}
	run, err := NewPortfolioService(store).CreateAgentRun(t.Context(), CreateAgentRunRequest{
		Scope: scope, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"}, AssignedAgentID: "agent", ActingUser: "7",
		Goal: "Work for a person", Source: RunSourceManual,
	})
	if err != nil {
		t.Fatal(err)
	}
	commands := agentrun.NewRunCommandService(store)
	command := func(kind agentrun.AgentRunCommandKind, actor ActivityActor) error {
		current, err := store.GetAgentRun(t.Context(), scope, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		instruction := ""
		if kind == agentrun.AgentRunCommandIntervene {
			instruction = "Do it differently"
		}
		_, err = commands.CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: scope, RunID: run.ID, ExpectedRevision: current.Revision, Kind: kind, Actor: actor, Instruction: instruction, Summary: "steer"})
		return err
	}
	other := ActivityActor{Type: "user", ID: "8"}
	for _, kind := range []agentrun.AgentRunCommandKind{agentrun.AgentRunCommandIntervene, agentrun.AgentRunCommandResolveHumanIntervention, agentrun.AgentRunCommandResume, agentrun.AgentRunCommandRetry} {
		if err := command(kind, other); !errors.Is(err, agentrun.ErrRunCommandNotAuthorized) {
			t.Errorf("%s by another user: err = %v", kind, err)
		}
		for _, allowed := range []ActivityActor{{Type: "user", ID: "7"}, {Type: "system", ID: "kernel"}, {Type: "agent", ID: "agent"}} {
			if err := command(kind, allowed); errors.Is(err, agentrun.ErrRunCommandNotAuthorized) {
				t.Errorf("%s by %#v refused", kind, allowed)
			}
		}
	}
	if err := command(agentrun.AgentRunCommandIntervene, ActivityActor{Type: "user", ID: "7"}); err != nil {
		t.Fatalf("the acting person's guidance: %v", err)
	}
	if err := command(agentrun.AgentRunCommandPause, other); err != nil {
		t.Fatalf("another user's pause: %v", err)
	}
	if err := command(agentrun.AgentRunCommandCancel, other); err != nil {
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
	if _, err := commands.CommandAgentRun(t.Context(), agentrun.AgentRunCommandRequest{Scope: scope, RunID: nobody.ID, ExpectedRevision: nobody.Revision, Kind: agentrun.AgentRunCommandIntervene, Actor: other, Instruction: "Do it", Summary: "steer"}); err != nil {
		t.Fatalf("guidance on a run without a person: %v", err)
	}
}

func TestOnlyTheRoutineCreatorChangesItsScriptOrResumesIt(t *testing.T) {
	store := newTestKernelStore(t)
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
		return dispatcher.DispatchAction(ctx, agentrun.ActionDispatchInput{Run: as, Bound: b, Call: &ActionCall{ID: callID, Scope: as.Scope, RunID: as.ID, DeploymentID: as.AssignedAgentID, IdempotencyKey: callID}, Arguments: arguments})
	}
	proposed := maps.Clone(args)
	proposed["script"] = map[string]interface{}{"path": "scripts/weekly.py", "command": "python3 scripts/weekly.py"}
	resolved, _, err := validator.ResolveActionProposalArguments(ctx, agentrun.ActionProposalValidationInput{Run: run, Bound: bound, Arguments: proposed})
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
		ActingUser: "intruder", Goal: "Handle another request", Source: RunSourceChat, Context: maps.Clone(run.Context), IdempotencyKey: "intruder"})
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
	repin, _, err := validator.ResolveActionProposalArguments(ctx, agentrun.ActionProposalValidationInput{Run: run, Bound: repinBound, Arguments: map[string]interface{}{"activationId": activation.ID, "expectedRevision": float64(current.Revision)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validator.ValidateActionProposal(ctx, agentrun.ActionProposalValidationInput{Run: intruder, Bound: repinBound, Arguments: repin}); !errors.Is(err, ErrRoutineNotCreator) {
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
	store := newTestKernelStore(t)
	run, _, bound, args := scheduledTaskFixture(t, store)
	activation := dispatchTaskForTest(t, store, run, bound, args, "create")["activation"].(*RunbookActivation)
	started, err := StartRunbookActivation(t.Context(), store, run.Scope, activation.ID, StartRunbookActivationRequest{IdempotencyKey: "start-now", Actor: ActivityActor{Type: "user", ID: "user"}})
	if err != nil || started.Run.ActingUser != "user" {
		t.Fatalf("started routine run = %#v, %v", started, err)
	}
}
