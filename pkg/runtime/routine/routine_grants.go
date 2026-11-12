package routine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// Brief is what the user approves before a routine is created.
type Brief struct {
	Summary   string                `json:"summary"`
	DeliverTo string                `json:"deliverTo,omitempty"`
	Grants    []models.RoutineGrant `json:"grants"`
}

// GrantCatalog resolves the Agent's bound actions when a brief is
// proposed. skill.Catalog implements it.
type GrantCatalog interface {
	ListBindings(context.Context, skill.ScopeReference, string) ([]*skill.Binding, error)
	GetDefinition(ctx context.Context, id, version string) (*skill.Definition, error)
	GetDefinitionVariant(ctx context.Context, id, version, sourceIdentity string) (*skill.Definition, error)
}

// routineHostBindingCatalog also supplies the host's bindings for a run.
type routineHostBindingCatalog interface {
	HostBindings(context.Context, skill.ScopeReference, string, skill.RunActor) (*skill.HostBindingSet, error)
}

const (
	maximumRoutineBriefSummary = 300
	maximumRoutineDeliverTo    = 160
)

// normalizeRoutineBrief checks a brief's shape. Labels may be empty only
// when allowEmptyLabels (before the catalog fills them on proposal).
func normalizeRoutineBrief(brief *Brief, allowEmptyLabels bool) (*Brief, error) {
	if brief == nil {
		return nil, errors.New("a routine needs a brief: a summary, where results go and what it may do without asking")
	}
	summary, err := models.RoutineBriefText(brief.Summary, maximumRoutineBriefSummary)
	if err != nil {
		return nil, err
	}
	if summary == "" {
		return nil, errors.New("routine brief summary is required")
	}
	deliverTo, err := models.RoutineBriefText(brief.DeliverTo, maximumRoutineDeliverTo)
	if err != nil {
		return nil, err
	}
	grants, err := models.NormalizeRoutineGrants(brief.Grants, allowEmptyLabels)
	if err != nil {
		return nil, err
	}
	return &Brief{Summary: summary, DeliverTo: deliverTo, Grants: grants}, nil
}

// resolveRoutineBriefGrants proves every grant names an enabled action bound
// to the routine's Agent that a brief may waive, and fills labels from the
// catalog when the model gave none (or one naming a tool identifier). A
// binding tool is granted by its own model action name; the base actions it
// routes to are never grantable, since one grant would cover every tool of
// every binding. Each connected tool grant is pinned to that tool's fixed
// arguments, and each grant of an action that posts to a channel is pinned to
// the destinations the user approved, which its label names. An earlier
// contract that cannot carry a pin cannot grant those actions at all.
func resolveRoutineBriefGrants(ctx context.Context, catalog GrantCatalog, owners models.RoutineOwnerAddressResolver, run *models.AgentRun, grants []models.RoutineGrant, eventRoutine bool) ([]models.RoutineGrant, error) {
	if len(grants) == 0 {
		return grants, nil
	}
	if catalog == nil || run == nil || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, errors.New("routine grants cannot be checked on this host")
	}
	scope := skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}
	actor := models.RunSkillActor(run)
	bindings, err := catalog.ListBindings(ctx, scope, run.AssignedAgentID)
	if err != nil {
		return nil, err
	}
	// A Team run uses its Team's connections and only the member's bindings
	// that hold none, as activation does. A run delegated to another Agent
	// uses no connection at all.
	if !actor.Valid() || actor.Umbrella.ID != run.AssignedAgentID {
		own := bindings[:0]
		for _, binding := range bindings {
			if !skill.BindingHoldsConnection(binding) {
				own = append(own, binding)
			}
		}
		bindings = own
		if actor.Valid() {
			umbrellaBindings, listErr := catalog.ListBindings(ctx, scope, actor.Umbrella.ID)
			if listErr != nil {
				return nil, listErr
			}
			bindings = append(bindings, umbrellaBindings...)
		}
	}
	// The host's bindings for this run (connected apps) are granted like
	// stored ones, by their own tool names; the routine's runs resolve them
	// for its umbrella again.
	if hosted, ok := catalog.(routineHostBindingCatalog); ok && actor.Valid() {
		set, hostErr := hosted.HostBindings(ctx, scope, actor.Umbrella.ID, actor)
		if hostErr != nil {
			return nil, hostErr
		}
		bindings = append(bindings, set.Bindings...)
	}
	type boundAction struct {
		skillID string
		name    string
		action  skill.Action
		pin     map[string]interface{}
	}
	available := map[string]boundAction{}
	// routed maps a binding-tools base action to an example tool to grant.
	routed := map[string]string{}
	for _, binding := range bindings {
		if binding == nil || binding.Disabled || models.RoutineUngrantableSkill(binding.SkillID) {
			continue
		}
		var definition *skill.Definition
		if strings.TrimSpace(binding.SourceIdentity) == "" {
			definition, err = catalog.GetDefinition(ctx, binding.SkillID, binding.SkillVersion)
		} else {
			definition, err = catalog.GetDefinitionVariant(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
		}
		if err != nil {
			return nil, err
		}
		if definition == nil {
			continue
		}
		spec := definition.BindingTools
		for _, name := range binding.AllowedActions {
			identity := models.RoutineGrantIdentity(binding.SkillID, name)
			if spec.RoutesAction(name) {
				if _, exists := routed[identity]; !exists {
					routed[identity] = ""
				}
				continue
			}
			if _, exists := available[identity]; exists {
				continue
			}
			if action, ok := definition.Actions[name]; ok {
				available[identity] = boundAction{skillID: binding.SkillID, name: name, action: action}
			}
		}
		for _, tool := range binding.Tools {
			base := spec.Action(tool.Effect)
			action, ok := definition.Actions[base]
			if !ok || !models.MatchesStringFilter(binding.AllowedActions, base) {
				continue
			}
			identity := skill.BindingToolModelName(definition, binding, tool.Name)
			if _, exists := available[identity]; exists {
				continue
			}
			if example := models.RoutineGrantIdentity(binding.SkillID, base); routed[example] == "" {
				routed[example] = identity
			}
			action.Description = tool.Description
			action.InputSchema = tool.InputSchema
			available[identity] = boundAction{skillID: binding.SkillID, name: tool.Name, action: action, pin: tool.Arguments}
		}
	}
	resolved := make([]models.RoutineGrant, 0, len(grants))
	for _, grant := range grants {
		bound, ok := available[grant.Action]
		if !ok {
			if example, routes := routed[grant.Action]; routes {
				if example == "" {
					return nil, fmt.Errorf("routine grant %s runs connected tools and cannot be granted; grant the specific tool by its own action name", grant.Action)
				}
				return nil, fmt.Errorf("routine grant %s runs connected tools and cannot be granted; grant the specific tool, e.g. %s", grant.Action, example)
			}
			return nil, fmt.Errorf("routine grant %s is not an action this Agent can use", grant.Action)
		}
		// The pin always comes from the catalog, never from the model.
		grant.ToolArguments = maps.Clone(bound.pin)
		if bound.action.AlwaysReview() {
			return nil, fmt.Errorf("routine grant %s always asks first and cannot run without asking", grant.Action)
		}
		if grant.ToOwnerOnly {
			if owners == nil {
				return nil, errors.New("this host cannot limit a routine's messages to its owner")
			}
			if !routineActionHasRecipients(bound.action.InputSchema) {
				return nil, fmt.Errorf("routine grant %s does not send messages, so it cannot be limited to the owner", grant.Action)
			}
		}
		posts := models.RoutineActionPostsToDestination(bound.action)
		if posts && len(grant.ToDestinations) == 0 {
			return nil, fmt.Errorf("routine grant %s posts to a channel: set toDestinations to each channel its runs post to; results reach this chat without it", grant.Action)
		}
		if len(grant.ToDestinations) > 0 && len(models.RoutineActionDestinationKeys(bound.action.InputSchema)) == 0 {
			return nil, fmt.Errorf("routine grant %s takes no channel, so it cannot be limited to destinations", grant.Action)
		}
		if eventRoutine && !routineSendGrantPinned(bound.action, grant) {
			return nil, ErrRoutineEventSendGrantNotPinned
		}
		if grant.Label == "" || action.ApprovalNamesIdentifier(grant.Label, bound.skillID, bound.name, grant.Action) {
			grant.Label = routineGrantCatalogLabel(bound.name, bound.action)
		}
		if grant.Label, err = routineGrantDestinationLabel(grant.Label, grant.ToDestinations); err != nil {
			return nil, fmt.Errorf("routine grant %s: %w", grant.Action, err)
		}
		resolved = append(resolved, grant)
	}
	return resolved, nil
}

var routineSentenceEnd = regexp.MustCompile(`[.!?](\s|$)`)

// routineGrantCatalogLabel is the action's first descriptive sentence, else
// its humanized name.
func routineGrantCatalogLabel(name string, action skill.Action) string {
	description := strings.Join(strings.Fields(action.Description), " ")
	if location := routineSentenceEnd.FindStringIndex(description); location != nil {
		description = description[:location[0]]
	}
	if description != "" && utf8.RuneCountInString(description) <= models.MaximumRoutineGrantLabel && !strings.ContainsAny(description, "<>`") {
		return description
	}
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(name))
	label := strings.Join(words, " ")
	if label == "" {
		return "Use this action"
	}
	if utf8.RuneCountInString(label) > models.MaximumRoutineGrantLabel {
		label = string([]rune(label)[:models.MaximumRoutineGrantLabel])
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

func routineActionHasRecipients(schema map[string]interface{}) bool {
	properties, _ := schema["properties"].(map[string]interface{})
	for name := range properties {
		if models.IsRoutineRecipientKey(name) {
			return true
		}
	}
	return false
}

// ErrRoutineEventSendGrantNotPinned refuses a free-recipient send in an event
// routine's brief (E15): outside events must not choose who it writes to.
var ErrRoutineEventSendGrantNotPinned = errors.New("An event routine can only send to you or to the channels you name")

// routineSendGrantPinned reports whether an event routine's grant can only
// reach the owner or named destinations. It fails closed: recipients must be
// limited to the owner, destinations must be named, and an action with an
// external or destructive effect whose target the kernel cannot read from a
// known recipient or destination argument (a raw message body, an unknown
// key) is never pinned. Reads, and writes with no target argument, are.
func routineSendGrantPinned(action skill.Action, grant models.RoutineGrant) bool {
	if action.SideEffect == skill.SideEffectNone || action.SideEffect == skill.SideEffectRead {
		return true
	}
	recipients := routineActionHasRecipients(action.InputSchema)
	destinations := len(models.RoutineActionDestinationKeys(action.InputSchema)) > 0
	if recipients && !grant.ToOwnerOnly || destinations && len(grant.ToDestinations) == 0 {
		return false
	}
	if !recipients && !destinations {
		return action.SideEffect != skill.SideEffectExternal && action.SideEffect != skill.SideEffectDestructive
	}
	return true
}

// routineGrantDestinationLabel makes the approved label name every pinned
// destination, appending those it does not mention.
func routineGrantDestinationLabel(label string, destinations []string) (string, error) {
	missing := []string{}
	lower := strings.ToLower(label)
	for _, destination := range destinations {
		if !strings.Contains(lower, strings.ToLower(destination)) {
			missing = append(missing, destination)
		}
	}
	if len(missing) == 0 {
		return label, nil
	}
	label = strings.TrimSpace(strings.TrimRight(label, ".")) + " (" + strings.Join(missing, ", ") + ")"
	if utf8.RuneCountInString(label) > models.MaximumRoutineGrantLabel {
		return "", fmt.Errorf("label naming its destinations must be at most %d characters", models.MaximumRoutineGrantLabel)
	}
	return label, nil
}

// RevokeRoutineGrantsRequest clears the actions a routine may perform
// without asking. Only the user who asked for the routine may revoke.
type RevokeRoutineGrantsRequest struct {
	Requester models.ConversationParticipant `json:"-"`
}

// RevokeGrants clears a scheduled task's approved grants. It is idempotent:
// a routine without grants is returned unchanged.
func (s *RunbookActivationService) RevokeGrants(ctx context.Context, scope models.Scope, id string, request RevokeRoutineGrantsRequest) (*models.RunbookActivation, error) {
	if s == nil || s.store == nil {
		return nil, errors.New("Runbook activation store is not configured")
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	current, err := s.store.GetRunbookActivation(ctx, scope, strings.TrimSpace(id))
	if err != nil {
		return nil, err
	}
	if current == nil {
		return nil, models.ErrRunbookActivationNotFound
	}
	if current.Task == nil {
		return nil, errors.New("only a routine created from a chat has approved actions")
	}
	owner, err := models.RoutineOwner(current)
	if err != nil {
		return nil, err
	}
	if request.Requester != owner {
		return nil, ErrRoutineGrantsNotOwner
	}
	if len(current.Task.Grants) == 0 {
		return models.CloneRunbookActivation(current), nil
	}
	next := models.CloneRunbookActivation(current)
	next.Task.Grants = nil
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := s.store.UpdateRunbookActivation(ctx, next, current.Revision); err != nil {
		return nil, err
	}
	return models.CloneRunbookActivation(next), nil
}

// ErrRoutineGrantsNotOwner is returned when someone other than the user who
// asked for a routine tries to change what it may do without asking.
var ErrRoutineGrantsNotOwner = errors.New("only the person who asked for this routine can change what it does without asking")

// routineBriefPreview is the brief as the approval card shows it.
func routineBriefPreview(brief *Brief) map[string]interface{} {
	if brief == nil {
		return nil
	}
	grants := make([]interface{}, 0, len(brief.Grants))
	for _, grant := range brief.Grants {
		item := map[string]interface{}{"action": grant.Action, "label": grant.Label}
		if grant.ToOwnerOnly {
			item["toOwnerOnly"] = true
		}
		if len(grant.ToDestinations) > 0 {
			destinations := make([]interface{}, 0, len(grant.ToDestinations))
			for _, destination := range grant.ToDestinations {
				destinations = append(destinations, destination)
			}
			item["toDestinations"] = destinations
		}
		if grant.ToolArguments != nil {
			item["toolArguments"] = maps.Clone(grant.ToolArguments)
		}
		grants = append(grants, item)
	}
	preview := map[string]interface{}{"summary": brief.Summary, "grants": grants}
	if brief.DeliverTo != "" {
		preview["deliverTo"] = brief.DeliverTo
	}
	return preview
}

// resolveRoutineBriefArguments normalizes a proposed create_task brief and
// fills grant labels from the catalog before the arguments are digested, so
// the approved arguments carry exactly the brief the user sees.
func resolveRoutineBriefArguments(ctx context.Context, catalog GrantCatalog, owners models.RoutineOwnerAddressResolver, run *models.AgentRun, arguments map[string]interface{}, eventRoutine bool) error {
	var parsed struct {
		Brief *Brief `json:"brief"`
	}
	raw, ok := arguments["brief"].(map[string]interface{})
	if !ok {
		return errors.New("a routine needs a brief: a summary, where results go and what it may do without asking")
	}
	if err := decodeScheduledTaskArguments(map[string]interface{}{"brief": raw}, &parsed); err != nil {
		return fmt.Errorf("routine brief is invalid: %w", err)
	}
	brief, err := normalizeRoutineBrief(parsed.Brief, true)
	if err != nil {
		return err
	}
	if brief.Grants, err = resolveRoutineBriefGrants(ctx, catalog, owners, run, brief.Grants, eventRoutine); err != nil {
		return err
	}
	arguments["brief"] = routineBriefPreview(brief)
	return nil
}
