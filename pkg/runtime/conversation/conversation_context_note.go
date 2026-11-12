package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/runtime/action"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// The run_context note (run-loop section 05 §5, SPEC §6). Each chat Run
// appends one note at its start, after the catch-up entries, with the
// per-Run facts the model needs and the system prompt must not carry (it
// would break the prompt cache): the message to answer, the channel, the
// Team roster, approval mode, objectives, runbooks, active work, conversation
// tasks, card state, reactions, places and the thread backdrop.
//
// Sections other than the always-sent ones are diffed against the newest
// run_context note after the session's last compaction: an unchanged section
// is left out, a section that became empty is sent with its "none" text, and
// after a compaction every non-empty section is sent again.

// Section names, in the fixed order of the note text.
const (
	ContextSectionRespondTo         = "respond_to"
	ContextSectionReplyParent       = "reply_parent"
	ContextSectionChannel           = "channel"
	ContextSectionExternalContext   = "external_context"
	ContextSectionTeam              = "team"
	ContextSectionObjectives        = "objectives"
	ContextSectionRunbooks          = "runbooks"
	ContextSectionActiveRuns        = "active_runs"
	ContextSectionConversationTasks = "conversation_tasks"
	ContextSectionCardState         = "card_state"
	ContextSectionReactions         = "reactions"
	ContextSectionPlaces            = "places"
	ContextSectionImageStyle        = "image_style"
	ContextSectionWorkMachine       = "work_machine"
	ContextSectionThreadBackdrop    = "thread_backdrop"
)

const (
	// maximumRunContextBytes bounds the whole note text.
	maximumRunContextBytes = 32 << 10
	// The bounds of single values in section lines.
	RunContextObjectiveGoalRunes   = 300
	runContextActiveRunGoalRunes   = 200
	RunContextTaskGoalRunes        = 512
	runContextAssignmentRunes      = 4000
	RunContextReplyParentBytes     = 4 << 10
	runContextBackdropMessageBytes = 1 << 10
	RunContextMaximumObjectives    = 50
	RunContextMaximumRunbooks      = 50
	RunContextMaximumTasks         = 10
	runContextMaximumReactions     = 20
	runContextRecentSessionEntries = 200
	runContextPreviousNotes        = 20
)

const (
	runContextOpen  = "<run_context>\n"
	runContextClose = "\n</run_context>"
)

// runContextShrinkOrder is the order in which list sections lose lines when
// the note is over maximumRunContextBytes: active work and objectives first
// (section 05 §5.2), then the other lists.
var runContextShrinkOrder = []string{
	ContextSectionActiveRuns, ContextSectionObjectives, ContextSectionRunbooks,
	ContextSectionReactions, ContextSectionConversationTasks, ContextSectionCardState, ContextSectionPlaces,
	ContextSectionTeam, ContextSectionThreadBackdrop,
}

type ConversationContextSection struct {
	Name   string
	Text   string
	Always bool // emitted every Run when non-empty, never diffed
	// None is the text sent when a diffed section became empty while the
	// session had last seen it non-empty. "" for sections that are never
	// sent empty.
	None string
}

type ConversationContextInput struct {
	Run          *models.AgentRun
	Session      *models.AgentSession
	Sessions     models.AgentSessionStore // reads the session's entries (path messages, earlier notes)
	Conversation *models.Conversation
	Trigger      *models.ChannelMessage
	Viewer       models.ConversationViewer
	Recent       []*models.ChannelMessage // last 50 visible messages ≤ Through, ascending (card state and reactions)
	CatchUp      *CatchUpResult           // the entries appended with this note; nil when none
	Backdrop     []*models.ChannelMessage // seed only; CatchUp.Backdrop when nil
	Step         int64                    // the step the note is appended in
}

// previousConversationContextDigests is what the session last saw of each
// section: the digests of the newest run_context notes after its last
// compaction, merged newest first. The Run's own note (a rebuilt start) is
// left out.
func PreviousConversationContextDigests(ctx context.Context, sessions models.AgentSessionStore, session *models.AgentSession, runID string) (map[string]string, error) {
	if sessions == nil {
		return map[string]string{}, nil
	}
	entries, err := sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: session.Scope, SessionID: session.ID, Types: []models.SessionEntryType{models.SessionEntryNote},
		NoteKinds: []models.NoteKind{models.NoteRunContext}, AfterCompaction: true, Descending: true, Limit: runContextPreviousNotes,
	})
	if errors.Is(err, models.ErrAgentSessionNotFound) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	others := make([]*models.SessionEntry, 0, len(entries))
	for _, entry := range entries {
		if entry != nil && entry.RunID != runID {
			others = append(others, entry)
		}
	}
	return latestContextDigests(others), nil
}

// runContextNoteData is the note's Data: the digest of each section the note
// carries. The texts are only in the note text, so the entry's token estimate
// counts them once.
type runContextNoteData struct {
	Sections map[string]runContextSectionDigest `json:"sections"`
}

type runContextSectionDigest struct {
	Digest string `json:"digest"`
}

func runContextDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// latestContextDigests merges the section digests of run_context notes given
// newest first: the newest note that carries a section wins.
func latestContextDigests(entries []*models.SessionEntry) map[string]string {
	digests := make(map[string]string)
	for _, entry := range entries {
		if entry == nil || entry.Type != models.SessionEntryNote || entry.Note == nil || entry.Note.Kind != models.NoteRunContext || len(entry.Note.Data) == 0 {
			continue
		}
		var data runContextNoteData
		if json.Unmarshal(entry.Note.Data, &data) != nil {
			continue
		}
		for name, section := range data.Sections {
			if _, seen := digests[name]; !seen && section.Digest != "" {
				digests[name] = section.Digest
			}
		}
	}
	return digests
}

// conversationContextNote selects the sections to send and builds the note
// entry of runID in session. It is nil when no section is to be sent.
func ConversationContextNote(session *models.AgentSession, runID string, sections []ConversationContextSection, previous map[string]string) *models.SessionEntry {
	if session == nil {
		return nil
	}
	type emitted struct {
		name, text, digest string
	}
	selected := make([]emitted, 0, len(sections))
	for _, section := range sections {
		text := strings.TrimSpace(section.Text)
		if section.Always {
			if text != "" {
				selected = append(selected, emitted{section.Name, text, runContextDigest(text)})
			}
			continue
		}
		last, seen := previous[section.Name]
		if text == "" {
			if !seen || section.None == "" {
				continue
			}
			text = section.None
		}
		digest := runContextDigest(text)
		if seen && last == digest {
			continue
		}
		selected = append(selected, emitted{section.Name, text, digest})
	}
	if len(selected) == 0 {
		return nil
	}
	texts := make(map[string]string, len(selected))
	order := make([]string, 0, len(selected))
	data := runContextNoteData{Sections: make(map[string]runContextSectionDigest, len(selected))}
	for _, section := range selected {
		texts[section.name] = section.text
		order = append(order, section.name)
		// The digest is of the full text, so a section truncated to fit the
		// bound is not sent again while its state is unchanged.
		data.Sections[section.name] = runContextSectionDigest{Digest: section.digest}
	}
	text := boundRunContextText(order, texts)
	encoded, _ := json.Marshal(data)
	key := models.RunContextSourceKey(runID)
	entry := &models.SessionEntry{
		ID: models.SessionEntryID(session.ID, key), Scope: session.Scope, SessionID: session.ID, RunID: runID,
		Type: models.SessionEntryNote, SourceKey: key,
		Note: &models.NoteEntry{Kind: models.NoteRunContext, Text: text, Data: encoded},
	}
	entry.Tokens = modelstep.EstimateEntryTokens(entry)
	return entry
}

func joinRunContext(order []string, texts map[string]string) string {
	parts := make([]string, 0, len(order))
	for _, name := range order {
		parts = append(parts, texts[name])
	}
	return runContextOpen + strings.Join(parts, "\n\n") + runContextClose
}

// boundRunContextText joins the sections and keeps the result within
// maximumRunContextBytes: list sections lose their last lines, in
// runContextShrinkOrder, for a "… (N more)" line; whatever is still over is
// cut from the longest sections.
func boundRunContextText(order []string, texts map[string]string) string {
	text := joinRunContext(order, texts)
	for _, name := range runContextShrinkOrder {
		over := len(text) - maximumRunContextBytes
		if over <= 0 {
			return text
		}
		section, ok := texts[name]
		if !ok {
			continue
		}
		texts[name] = shrinkRunContextList(section, over)
		text = joinRunContext(order, texts)
	}
	for len(text) > maximumRunContextBytes {
		longest := ""
		for _, name := range order {
			if longest == "" || len(texts[name]) > len(texts[longest]) {
				longest = name
			}
		}
		over := len(text) - maximumRunContextBytes
		keep := len(texts[longest]) - over - len("…")
		if keep <= 0 {
			texts[longest] = "…"
		} else {
			texts[longest] = TruncateUTF8Bytes(texts[longest], keep) + "…"
		}
		text = joinRunContext(order, texts)
	}
	return text
}

// shrinkRunContextList drops the last lines of a list section (its first
// line is the heading) until it is at least over bytes shorter, and says how
// many were left out.
func shrinkRunContextList(section string, over int) string {
	lines := strings.Split(section, "\n")
	if len(lines) < 2 {
		return section
	}
	kept, items := lines, lines[1:]
	for dropped := 1; dropped <= len(items); dropped++ {
		kept = append(append([]string(nil), lines[:len(lines)-dropped]...), fmt.Sprintf("… (%d more)", dropped))
		if len(section)-len(strings.Join(kept, "\n")) >= over {
			break
		}
	}
	return strings.Join(kept, "\n")
}

// truncateUTF8Bytes cuts value to at most limit bytes on a rune boundary.
func TruncateUTF8Bytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	if limit <= 0 {
		return ""
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

// boundedRunContextValue is value on one line, at most limit runes.
func BoundedRunContextValue(value string, limit int) string {
	return models.TruncateRunes(strings.Join(strings.Fields(value), " "), limit)
}

// ---------------------------------------------------------------------------
// Sections

// runContextPathMessages is the set of channel message ids that are user
// entries of the session: the recent ones already stored and the ones being
// appended with the note.
func RunContextPathMessages(ctx context.Context, in ConversationContextInput) (map[string]bool, error) {
	path := make(map[string]bool)
	if in.CatchUp != nil {
		for _, entry := range in.CatchUp.Entries {
			if entry != nil && entry.User != nil && entry.User.MessageID != "" {
				path[entry.User.MessageID] = true
			}
		}
	}
	if in.Sessions == nil {
		return path, nil
	}
	entries, err := in.Sessions.ListAgentSessionEntries(ctx, models.SessionEntryFilter{
		Scope: in.Session.Scope, SessionID: in.Session.ID, Types: []models.SessionEntryType{models.SessionEntryUser},
		Descending: true, Limit: runContextRecentSessionEntries,
	})
	if errors.Is(err, models.ErrAgentSessionNotFound) {
		return path, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry != nil && entry.User != nil && entry.User.MessageID != "" {
			path[entry.User.MessageID] = true
		}
	}
	return path, nil
}

// runContextRespondTo names the message to answer and how: spoken, a voice
// call greeting, why a Team router picked this member, work handed over.
func RunContextRespondTo(in ConversationContextInput) string {
	trigger := in.Trigger
	name := strings.TrimSpace(trigger.SenderDisplayName)
	if name == "" {
		name = trigger.Sender.ID
	}
	attribution := conversationMessageAttribution(trigger, in.Session, in.Conversation.Owner.Type == models.OwnerTypeTeam)
	lines := []string{fmt.Sprintf("Reply to message %s from %s (%s %s).", trigger.ID, name, attribution, trigger.Sender.ID)}
	if trigger.ResponseMode == "spoken" {
		lines = append(lines, "Spoken reply: answer in words to be spoken.")
	}
	if models.VoiceCallStartedMessage(trigger) {
		lines = append(lines, "Voice call just started. "+models.VoiceCallGreetingBehaviorInstruction)
	}
	if reason, _ := in.Run.Context[models.RoutingReasonContextKey].(string); strings.TrimSpace(reason) != "" {
		lines = append(lines, "You were picked to answer because: "+BoundedRunContextValue(reason, models.MaximumRoutingReasonBytes)+".")
	}
	if assignment := conversationRunAssignment(in.Run); assignment != nil {
		from := strings.TrimSpace(assignment.From)
		if from == "" {
			from = "a teammate"
		}
		lines = append(lines, fmt.Sprintf("Work handed to you by %s (%s): %s", from, assignment.Kind, models.TruncateRunes(strings.TrimSpace(assignment.Work), runContextAssignmentRunes)))
	}
	return strings.Join(lines, "\n")
}

// runContextMessage is a message's header line and content, the content cut
// to limit bytes.
func RunContextMessage(in ConversationContextInput, message *models.ChannelMessage, limit int) string {
	attribution := conversationMessageAttribution(message, in.Session, in.Conversation.Owner.Type == models.OwnerTypeTeam)
	content := strings.TrimSpace(message.Content)
	if content == "" {
		content = conversationMessageNoText
	} else if len(content) > limit {
		content = TruncateUTF8Bytes(content, limit) + "…"
	}
	return renderConversationMessageHeader(message, attribution) + "\n" + content
}

func RunContextChannel(conversation *models.Conversation, trigger *models.ChannelMessage) string {
	text := fmt.Sprintf("Channel %q (%s)", BoundedRunContextValue(conversation.Title, 200), conversation.ID)
	if origin := conversation.Origin; origin != nil {
		text += fmt.Sprintf("; origin %s %s", origin.Kind, origin.ID)
	}
	if source := trigger.ExternalSource; source != nil {
		channel := source.ChannelName
		if channel == "" {
			channel = source.ChannelID
		}
		text += fmt.Sprintf("; %s channel %s", source.Provider, channel)
		if source.ChannelType != "" {
			text += " (" + source.ChannelType + ")"
		}
	}
	return text + "."
}

func RunContextExternalContext(conversation *models.Conversation, trigger *models.ChannelMessage) string {
	if !models.IsExternalChannelConversation(conversation) || conversation.Origin == nil || conversation.Origin.Kind != models.ConversationReferenceExternalSource {
		return ""
	}
	state := ExternalConversationContextAvailability(trigger, conversation.Origin.ID)
	if state == nil {
		return ""
	}
	text := fmt.Sprintf("Provider thread history: %s, %d imported", state.Status, state.ImportedMessages)
	if state.ErrorCode != "" {
		text += ", error " + state.ErrorCode
	}
	return text + "."
}

func RunContextTeam(team *AgentConversationTeam) string {
	if team == nil || len(team.Roster) == 0 && team.Notes == "" {
		return ""
	}
	lines := []string{"Team roster:"}
	for _, member := range team.Roster {
		line := fmt.Sprintf("- %s (%s)", BoundedRunContextValue(member.Name, 200), member.AgentID)
		if member.Lead {
			line += ", lead"
		}
		if member.You {
			line += ", you"
		}
		if role := BoundedRunContextValue(member.Role, 200); role != "" {
			line += ": " + role
		}
		if len(member.Owns) > 0 {
			line += "; owns " + BoundedRunContextValue(strings.Join(member.Owns, ", "), 300)
		}
		lines = append(lines, line)
	}
	if notes := BoundedRunContextValue(team.Notes, 2000); notes != "" {
		lines = append(lines, "Routing notes: "+notes)
	}
	return strings.Join(lines, "\n")
}

// approvalModeText is the conversation's approval mode. It is a line of the
// per-step capabilities note, not a system hint: every Run with a
// conversation origin (chat, task and delegated Runs) sees it, and a mode
// changed mid-Run reaches the next step as a one-line update instead of
// rewriting the prompt.
func ApprovalModeText(mode models.ConversationApprovalMode) string {
	switch action.ConversationApprovalModeOrDefault(mode) {
	case models.ConversationApprovalManual:
		return "Approval mode: manual. Every action that changes something waits for the user's approval in chat."
	case models.ConversationApprovalSkip:
		return "Approval mode: skip. The user chose to skip approvals in this conversation: complete tasks end to end yourself, including signing in, checkout, placing orders or bookings, accepting terms and sending. Actions whose Skill always asks, and payments, still wait for approval."
	}
	return "Approval mode: auto. Reads and ordinary writes run without approval; external, production and destructive actions wait for the user's approval in chat."
}

// runContextActiveRuns is never empty: "none" is a fact the model relies on.
func RunContextActiveRuns(active []AgentConversationActiveRun) string {
	if len(active) == 0 {
		return "Active work: none."
	}
	lines := []string{"Active work:"}
	for _, run := range active {
		line := fmt.Sprintf("- run %s (%s, %s", run.ID, run.Status, run.Source)
		if run.Entrypoint != "" {
			line += ", entrypoint " + run.Entrypoint
		}
		line += fmt.Sprintf(") %q", BoundedRunContextValue(run.Goal, runContextActiveRunGoalRunes))
		if len(run.AvailableControls) > 0 {
			line += " controls: " + strings.Join(run.AvailableControls, ",")
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// conversationTaskLister reads the conversation task ledger.
type ConversationTaskLister interface {
	ListConversationTasks(context.Context, models.ConversationTaskFilter) ([]*models.ConversationTaskResult, error)
}

func RunContextLines(lines []string) string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, "- "+line)
		}
	}
	return strings.Join(out, "\n")
}

// runContextReactions lists reactions on the recent messages the session
// holds (its user entries) and on this Agent's own answers, newest first.
func RunContextReactions(in ConversationContextInput, path map[string]bool) string {
	reactions := reactionPromptLines(in.Recent, in.Trigger)
	if len(reactions) == 0 {
		return ""
	}
	messages := append(append([]*models.ChannelMessage(nil), in.Recent...), in.Trigger)
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i] != nil && messages[j] != nil && messages[i].Sequence > messages[j].Sequence
	})
	self := models.ConversationParticipant{Type: models.ConversationParticipantAgent, ID: in.Run.AssignedAgentID}
	seen := make(map[string]bool)
	lines := []string{"Reactions:"}
	for _, message := range messages {
		if message == nil || seen[message.ID] || reactions[message.ID] == "" {
			continue
		}
		if !path[message.ID] && message.ID != in.Trigger.ID && message.Sender != self {
			continue
		}
		seen[message.ID] = true
		lines = append(lines, fmt.Sprintf("- %s: %s", message.ID, reactions[message.ID]))
		if len(lines) > runContextMaximumReactions {
			break
		}
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

func RunContextPlaces(places []ConversationPlace) string {
	if len(places) == 0 {
		return ""
	}
	if len(places) > MaximumConversationPlaces {
		places = places[:MaximumConversationPlaces]
	}
	lines := []string{"Places already found in this chat (reuse their ids):"}
	for _, place := range places {
		lines = append(lines, fmt.Sprintf("- %s (place %s, %s,%s)", BoundedRunContextValue(place.Name, 200), place.Place,
			strconv.FormatFloat(place.Lat, 'f', -1, 64), strconv.FormatFloat(place.Lon, 'f', -1, 64)))
	}
	return strings.Join(lines, "\n")
}

func RunContextBackdrop(in ConversationContextInput, backdrop []*models.ChannelMessage) string {
	parts := make([]string, 0, len(backdrop)+1)
	for _, message := range backdrop {
		if message != nil {
			parts = append(parts, RunContextMessage(in, message, runContextBackdropMessageBytes))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "Before this thread started (background excerpts, not requests):\n" + strings.Join(parts, "\n")
}
