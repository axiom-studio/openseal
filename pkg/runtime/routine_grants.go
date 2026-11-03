package runtime

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/authoring"
	"github.com/axiom-studio/openseal/pkg/skill"
)

// RoutineGrant is one action a routine's runs may perform without asking.
// It exists only because the user approved a routine brief listing it: the
// grant is copied from the approved create_task arguments (bound by the
// approval's digest) and never added later.
type RoutineGrant struct {
	// Action is the model-visible action identity, skillId + "." + action.
	Action string `json:"action"`
	// Label is the plain-language line the user approved.
	Label string `json:"label"`
	// ToOwnerOnly limits a message-sending grant to sends whose recipients
	// are all the routine owner's own addresses.
	ToOwnerOnly bool `json:"toOwnerOnly,omitempty"`
}

// RoutineBrief is what the user approves before a routine is created.
type RoutineBrief struct {
	Summary   string         `json:"summary"`
	DeliverTo string         `json:"deliverTo,omitempty"`
	Grants    []RoutineGrant `json:"grants"`
}

// RoutineOwnerAddressResolver returns the verified email addresses of a
// routine's owner: the user who asked for it in the chat. Hosts that cannot
// prove a user's addresses leave it unset, and owner-only grants are then
// neither offered nor applied.
type RoutineOwnerAddressResolver interface {
	RoutineOwnerAddresses(ctx context.Context, scope Scope, owner ConversationParticipant) ([]string, error)
}

// RoutineGrantCatalog resolves the Agent's bound actions when a brief is
// proposed. skill.Catalog implements it.
type RoutineGrantCatalog interface {
	ListBindings(context.Context, skill.ScopeReference, string) ([]*skill.Binding, error)
	GetDefinition(ctx context.Context, id, version string) (*skill.Definition, error)
	GetDefinitionVariant(ctx context.Context, id, version, sourceIdentity string) (*skill.Definition, error)
}

const (
	maximumRoutineGrants       = 12
	maximumRoutineBriefSummary = 300
	maximumRoutineDeliverTo    = 160
	maximumRoutineGrantLabel   = 120
)

var routineGrantActionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,254}\.[A-Za-z0-9][A-Za-z0-9_:-]{0,127}$`)

// Kernel management Skills govern the Agent's own authority, work and
// routines; a routine brief never waives their review.
var routineUngrantableSkills = map[string]bool{
	SkillManagementSkillID: true, AgentManagementSkillID: true, TeamManagementSkillID: true,
	RunbookManagementSkillID: true, RunManagementSkillID: true, ObjectiveManagementSkillID: true, ProjectManagementSkillID: true,
}

func routineGrantIdentity(skillID, action string) string {
	return skillID + "." + action
}

// routineBriefText collapses whitespace and bounds plain approval prose.
func routineBriefText(value string, limit int) (string, error) {
	text := strings.Join(strings.Fields(value), " ")
	if utf8.RuneCountInString(text) > limit || strings.Contains(strings.ToUpper(text), "[REDACTED]") || strings.ContainsAny(text, "<>`") {
		return "", fmt.Errorf("routine brief text must be plain prose of at most %d characters", limit)
	}
	if text != "" {
		if err := authoring.ValidateAuthoringPrompt(text); err != nil {
			return "", err
		}
	}
	return text, nil
}

// normalizeRoutineBrief checks a brief's shape. Labels may be empty only
// when allowEmptyLabels (before the catalog fills them on proposal).
func normalizeRoutineBrief(brief *RoutineBrief, allowEmptyLabels bool) (*RoutineBrief, error) {
	if brief == nil {
		return nil, errors.New("a routine needs a brief: a summary, where results go and what it may do without asking")
	}
	summary, err := routineBriefText(brief.Summary, maximumRoutineBriefSummary)
	if err != nil {
		return nil, err
	}
	if summary == "" {
		return nil, errors.New("routine brief summary is required")
	}
	deliverTo, err := routineBriefText(brief.DeliverTo, maximumRoutineDeliverTo)
	if err != nil {
		return nil, err
	}
	grants, err := normalizeRoutineGrants(brief.Grants, allowEmptyLabels)
	if err != nil {
		return nil, err
	}
	return &RoutineBrief{Summary: summary, DeliverTo: deliverTo, Grants: grants}, nil
}

func normalizeRoutineGrants(values []RoutineGrant, allowEmptyLabels bool) ([]RoutineGrant, error) {
	if len(values) > maximumRoutineGrants {
		return nil, fmt.Errorf("a routine brief lists at most %d actions", maximumRoutineGrants)
	}
	grants := make([]RoutineGrant, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		action := strings.TrimSpace(value.Action)
		if !routineGrantActionPattern.MatchString(action) {
			return nil, fmt.Errorf("routine grant action %q must be an exact skillId.action", action)
		}
		if seen[action] {
			return nil, fmt.Errorf("routine grant action %s is listed twice", action)
		}
		seen[action] = true
		label, err := routineBriefText(value.Label, maximumRoutineGrantLabel)
		if err != nil {
			return nil, err
		}
		if label == "" && !allowEmptyLabels {
			return nil, errors.New("routine grant label is required")
		}
		grants = append(grants, RoutineGrant{Action: action, Label: label, ToOwnerOnly: value.ToOwnerOnly})
	}
	return grants, nil
}

func cloneRoutineGrants(values []RoutineGrant) []RoutineGrant {
	if values == nil {
		return nil
	}
	return append([]RoutineGrant(nil), values...)
}

// resolveRoutineBriefGrants proves every grant names an enabled action bound
// to the routine's Agent that a brief may waive, and fills labels from the
// catalog when the model gave none (or one naming a tool identifier).
func resolveRoutineBriefGrants(ctx context.Context, catalog RoutineGrantCatalog, owners RoutineOwnerAddressResolver, run *AgentRun, grants []RoutineGrant) ([]RoutineGrant, error) {
	if len(grants) == 0 {
		return grants, nil
	}
	if catalog == nil || run == nil || strings.TrimSpace(run.AssignedAgentID) == "" {
		return nil, errors.New("routine grants cannot be checked on this host")
	}
	bindings, err := catalog.ListBindings(ctx, skill.ScopeReference{Kind: run.Scope.Kind, ID: run.Scope.ID}, run.AssignedAgentID)
	if err != nil {
		return nil, err
	}
	type boundAction struct {
		skillID string
		name    string
		action  skill.Action
	}
	available := map[string]boundAction{}
	for _, binding := range bindings {
		if binding == nil || binding.Disabled || routineUngrantableSkills[binding.SkillID] {
			continue
		}
		var definition *skill.Definition
		for _, name := range binding.AllowedActions {
			identity := routineGrantIdentity(binding.SkillID, name)
			if _, exists := available[identity]; exists {
				continue
			}
			if definition == nil {
				if strings.TrimSpace(binding.SourceIdentity) == "" {
					definition, err = catalog.GetDefinition(ctx, binding.SkillID, binding.SkillVersion)
				} else {
					definition, err = catalog.GetDefinitionVariant(ctx, binding.SkillID, binding.SkillVersion, binding.SourceIdentity)
				}
				if err != nil {
					return nil, err
				}
				if definition == nil {
					break
				}
			}
			if action, ok := definition.Actions[name]; ok {
				available[identity] = boundAction{skillID: binding.SkillID, name: name, action: action}
			}
		}
	}
	resolved := make([]RoutineGrant, 0, len(grants))
	for _, grant := range grants {
		bound, ok := available[grant.Action]
		if !ok {
			return nil, fmt.Errorf("routine grant %s is not an action this Agent can use", grant.Action)
		}
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
		if grant.Label == "" || approvalNamesIdentifier(grant.Label, bound.skillID, bound.name, grant.Action) {
			grant.Label = routineGrantCatalogLabel(bound.name, bound.action)
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
	if description != "" && utf8.RuneCountInString(description) <= maximumRoutineGrantLabel && !strings.ContainsAny(description, "<>`") {
		return description
	}
	words := strings.Fields(strings.NewReplacer("-", " ", "_", " ", ".", " ").Replace(name))
	label := strings.Join(words, " ")
	if label == "" {
		return "Use this action"
	}
	if utf8.RuneCountInString(label) > maximumRoutineGrantLabel {
		label = string([]rune(label)[:maximumRoutineGrantLabel])
	}
	return strings.ToUpper(label[:1]) + label[1:]
}

// routineRecipientKey reports whether an argument names message recipients.
func routineRecipientKey(name string) bool {
	key := strings.ToLower(strings.TrimSpace(name))
	switch key {
	case "to", "cc", "bcc", "recipient", "recipients", "replyto", "reply_to":
		return true
	}
	return strings.Contains(key, "recipient") || strings.Contains(key, "email") || strings.Contains(key, "address")
}

func routineActionHasRecipients(schema map[string]interface{}) bool {
	properties, _ := schema["properties"].(map[string]interface{})
	for name := range properties {
		if routineRecipientKey(name) {
			return true
		}
	}
	return false
}

// routineRecipientsOwned reports whether every recipient in arguments, at any
// depth, is one of owned, and there is at least one. Anything it cannot read
// as an address under a recipient key fails closed.
func routineRecipientsOwned(arguments map[string]interface{}, owned []string) bool {
	allowed := make(map[string]bool, len(owned))
	for _, address := range owned {
		if address = strings.ToLower(strings.TrimSpace(address)); address != "" {
			allowed[address] = true
		}
	}
	if len(allowed) == 0 {
		return false
	}
	count := 0
	var visit func(value interface{}, recipient bool) bool
	visit = func(value interface{}, recipient bool) bool {
		switch typed := value.(type) {
		case nil:
			return true
		case map[string]interface{}:
			for key, nested := range typed {
				lower := strings.ToLower(key)
				if recipient && !routineRecipientKey(key) {
					if lower == "name" || lower == "displayname" || lower == "display_name" {
						continue
					}
					return false
				}
				if !visit(nested, recipient || routineRecipientKey(key)) {
					return false
				}
			}
			return true
		case []interface{}:
			for _, nested := range typed {
				if !visit(nested, recipient) {
					return false
				}
			}
			return true
		case []string:
			for _, nested := range typed {
				if !visit(nested, recipient) {
					return false
				}
			}
			return true
		case string:
			if !recipient || strings.TrimSpace(typed) == "" {
				return true
			}
			addresses, err := mail.ParseAddressList(typed)
			if err != nil {
				return false
			}
			for _, address := range addresses {
				if !allowed[strings.ToLower(address.Address)] {
					return false
				}
				count++
			}
			return true
		default:
			return !recipient
		}
	}
	return visit(arguments, false) && count > 0
}

type routineGrantStore interface {
	GetRunbookActivation(context.Context, Scope, string) (*RunbookActivation, error)
	GetChannelMessage(context.Context, Scope, string, string) (*ChannelMessage, error)
}

// routineActivationForRun returns the routine activation a Run executes for:
// a kernel-scheduled or started occurrence of a scheduled task, or a child of
// one assigned to the same Agent.
func routineActivationForRun(ctx context.Context, runs agentRunReader, store routineGrantStore, run *AgentRun) (*RunbookActivation, error) {
	if run == nil || store == nil {
		return nil, nil
	}
	origin := run
	if origin.Context[scheduledTaskContextKey] != true {
		rootID := strings.TrimSpace(run.RootRunID)
		if rootID == "" || rootID == run.ID || runs == nil {
			return nil, nil
		}
		root, err := runs.GetAgentRun(ctx, run.Scope, rootID)
		if errors.Is(err, ErrRunNotFound) || err == nil && root == nil {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		origin = root
	}
	activationID, _ := origin.Context["runbookActivationId"].(string)
	if origin.Context[scheduledTaskContextKey] != true || strings.TrimSpace(activationID) == "" {
		return nil, nil
	}
	activation, err := store.GetRunbookActivation(ctx, run.Scope, strings.TrimSpace(activationID))
	if errors.Is(err, ErrRunbookActivationNotFound) {
		return nil, nil
	}
	if err != nil || activation == nil {
		return nil, err
	}
	if activation.Task == nil || activation.Scope != run.Scope || activation.Owner != run.Owner || origin.Owner != run.Owner ||
		activation.AssignedAgentID == "" || activation.AssignedAgentID != run.AssignedAgentID || origin.AssignedAgentID != run.AssignedAgentID ||
		origin.ObjectiveID != activation.ObjectiveID {
		return nil, nil
	}
	return activation, nil
}

// RoutineOwner is the user who asked for the routine in its chat.
func routineOwner(ctx context.Context, store routineGrantStore, activation *RunbookActivation) (ConversationParticipant, error) {
	message, err := store.GetChannelMessage(ctx, activation.Scope, activation.Task.ConversationID, activation.Task.SourceMessageID)
	if err != nil {
		return ConversationParticipant{}, err
	}
	if message == nil || message.Scope != activation.Scope || message.ConversationID != activation.Task.ConversationID || message.Sender.Type != ConversationParticipantUser {
		return ConversationParticipant{}, errors.New("routine owner is unavailable")
	}
	return message.Sender, nil
}

// resolveRoutineGrant returns the approved routine grant that covers this
// action, or nil. A review: always action is never covered, and an owner-only
// grant covers a send only when every recipient is the owner's address.
func resolveRoutineGrant(ctx context.Context, runs agentRunReader, store routineGrantStore, owners RoutineOwnerAddressResolver, run *AgentRun, bound *skill.BoundAction, arguments map[string]interface{}) (*RoutineGrant, error) {
	if bound == nil || bound.Definition == nil || bound.Action.AlwaysReview() || routineUngrantableSkills[bound.Definition.ID] {
		return nil, nil
	}
	activation, err := routineActivationForRun(ctx, runs, store, run)
	if err != nil || activation == nil || len(activation.Task.Grants) == 0 {
		return nil, err
	}
	identity := routineGrantIdentity(bound.Definition.ID, bound.Action.Name)
	for _, grant := range activation.Task.Grants {
		if grant.Action != identity {
			continue
		}
		if grant.ToOwnerOnly {
			if owners == nil {
				return nil, nil
			}
			owner, err := routineOwner(ctx, store, activation)
			if err != nil {
				return nil, nil
			}
			addresses, err := owners.RoutineOwnerAddresses(ctx, activation.Scope, owner)
			if err != nil || !routineRecipientsOwned(arguments, addresses) {
				return nil, nil
			}
		}
		matched := grant
		return &matched, nil
	}
	return nil, nil
}

// RevokeRoutineGrantsRequest clears the actions a routine may perform
// without asking. Only the user who asked for the routine may revoke.
type RevokeRoutineGrantsRequest struct {
	Requester ConversationParticipant `json:"-"`
}

// RevokeGrants clears a scheduled task's approved grants. It is idempotent:
// a routine without grants is returned unchanged.
func (s *RunbookActivationService) RevokeGrants(ctx context.Context, scope Scope, id string, request RevokeRoutineGrantsRequest) (*RunbookActivation, error) {
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
		return nil, ErrRunbookActivationNotFound
	}
	if current.Task == nil {
		return nil, errors.New("only a routine created from a chat has approved actions")
	}
	messages, ok := s.store.(routineGrantStore)
	if !ok {
		return nil, errors.New("routine owner cannot be checked on this host")
	}
	owner, err := routineOwner(ctx, messages, current)
	if err != nil {
		return nil, err
	}
	if request.Requester != owner {
		return nil, ErrRoutineGrantsNotOwner
	}
	if len(current.Task.Grants) == 0 {
		return cloneRunbookActivation(current), nil
	}
	next := cloneRunbookActivation(current)
	next.Task.Grants = nil
	next.Revision++
	next.UpdatedAt = s.now().UTC()
	if err := s.store.UpdateRunbookActivation(ctx, next, current.Revision); err != nil {
		return nil, err
	}
	return cloneRunbookActivation(next), nil
}

// ErrRoutineGrantsNotOwner is returned when someone other than the user who
// asked for a routine tries to change what it may do without asking.
var ErrRoutineGrantsNotOwner = errors.New("only the person who asked for this routine can change what it does without asking")

// routineBriefPreview is the brief as the approval card shows it.
func routineBriefPreview(brief *RoutineBrief) map[string]interface{} {
	if brief == nil {
		return nil
	}
	grants := make([]interface{}, 0, len(brief.Grants))
	for _, grant := range brief.Grants {
		item := map[string]interface{}{"action": grant.Action, "label": grant.Label}
		if grant.ToOwnerOnly {
			item["toOwnerOnly"] = true
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
func resolveRoutineBriefArguments(ctx context.Context, catalog RoutineGrantCatalog, owners RoutineOwnerAddressResolver, run *AgentRun, arguments map[string]interface{}) error {
	var parsed struct {
		Brief *RoutineBrief `json:"brief"`
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
	if brief.Grants, err = resolveRoutineBriefGrants(ctx, catalog, owners, run, brief.Grants); err != nil {
		return err
	}
	arguments["brief"] = routineBriefPreview(brief)
	return nil
}

// routineBriefContract reports whether a create_task binding uses the
// contract that carries a routine brief.
func routineBriefContract(bound *skill.BoundAction) bool {
	return bound != nil && bound.Definition != nil && bound.Definition.ID == RunbookManagementSkillID && bound.Definition.Version == RunbookManagementSkillVersion
}
