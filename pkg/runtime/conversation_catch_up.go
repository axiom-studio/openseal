package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Catch-up of channel messages (run-loop section 05 §4). A chat session holds
// the channel as user entries: catch-up turns the visible messages newer than
// the session's channel watermark into entries, skipping what the session's
// own Runs posted (those are already its assistant entries). It never
// appends: the conversation preparer returns the entries to the loop, which
// writes them in the step's one context append with the watermark CAS
// (SPEC §3.1 phase 3). On ErrChannelWatermarkMoved the loop reloads the
// session and the preparer collects again, so two interleaved Runs of one
// lane never ingest a message twice.

const (
	// catchUpPageSize is the page of one ListChannelMessages read.
	catchUpPageSize = 100
	// seedMessageCount is how many recent messages a new session starts with.
	seedMessageCount = 30
	// maximumCatchUpMessages bounds one catch-up. Section 05 DI-8 asks for
	// 200, but every entry goes into the step's single context append, which
	// the store caps at MaxSessionAppendEntries; the rest of the cap is kept
	// for the other entries of that append (the history_omitted and
	// run_context notes, steering, event-wait and setup notes).
	maximumCatchUpMessages = MaxSessionAppendEntries - catchUpReservedAppendEntries
	// catchUpReservedAppendEntries is the part of the append cap left to the
	// entries that share the append with the catch-up.
	catchUpReservedAppendEntries = 16
	// catchUpAppendBytes is the encoded-size budget of the catch-up entries,
	// leaving room under MaxSessionAppendBytes for the rest of the append.
	catchUpAppendBytes = MaxSessionAppendBytes - 2<<20
)

// catchUpHistoryOmittedText is the history_omitted note: earlier or dropped
// messages exist that the session does not hold.
const catchUpHistoryOmittedText = "Earlier messages of this chat are not shown here. Read them with read_conversation_history when the request depends on them."

// conversationCatchUp turns visible channel messages newer than the session
// watermark into user entries.
type conversationCatchUp struct {
	conversations *ConversationService
	runs          PortfolioStore
	attachments   *conversationAttachmentReader // nil: message text only
}

type catchUpRequest struct {
	Run          *AgentRun
	Session      *AgentSession
	Conversation *Conversation
	Viewer       ConversationViewer // {Participant: {Type: agent, ID: run.AssignedAgentID}}
	// Trigger is the message the Run answers. A thread session's backdrop is
	// read from it at seed time.
	Trigger *ChannelMessage
	Through int64 // inclusive upper bound (channel sequence)
	Seed    bool  // the session was just created or has no entries (§4.5)
	Step    int64 // the step the entries are appended in
}

type catchUpResult struct {
	Entries     []*SessionEntry // user entries in sequence order; maybe empty
	Watermark   int64           // new ChannelWatermark (= Through when Through > the old watermark)
	Omitted     int             // messages dropped by the caps
	SeedFrom    int64           // first sequence ingested on a seed, 0 otherwise
	OlderExists bool            // seed: visible messages exist before SeedFrom
	// HistoryOmitted is the history_omitted note, set when OlderExists or
	// Omitted > 0. It goes before Entries.
	HistoryOmitted *SessionEntry
	// Backdrop is the thread backdrop of a thread session, read once, at seed
	// time (the run_context note's thread_backdrop section).
	Backdrop []*ChannelMessage
}

// collect reads the messages in (session watermark, Through] that the viewer
// can see and builds their user entries.
func (c *conversationCatchUp) collect(ctx context.Context, req catchUpRequest) (*catchUpResult, error) {
	if c == nil || c.conversations == nil || c.runs == nil {
		return nil, errors.New("conversation catch-up is not configured")
	}
	if req.Run == nil || req.Session == nil || req.Conversation == nil {
		return nil, fmt.Errorf("%w: catch-up needs a run, a session and a conversation", ErrInvalidAgentRun)
	}
	if req.Conversation.Scope != req.Session.Scope || req.Conversation.ID != req.Session.ConversationID {
		return nil, fmt.Errorf("%w: session %s does not belong to conversation %s", ErrInvalidAgentRun, req.Session.ID, req.Conversation.ID)
	}
	from := req.Session.ChannelWatermark
	result := &catchUpResult{Watermark: from}
	if req.Through <= from {
		return result, nil
	}
	result.Watermark = req.Through
	if req.Seed {
		seedFrom, olderExists, err := c.seedStart(ctx, req, from)
		if err != nil {
			return nil, err
		}
		from, result.OlderExists = seedFrom, olderExists
		if req.Session.ThreadRootID != "" {
			backdrop, err := loadConversationThreadBackdrop(ctx, c.conversations, req.Conversation, req.Trigger, req.Viewer)
			if err != nil {
				return nil, err
			}
			result.Backdrop = backdrop
		}
	}
	messages, dropped, err := c.read(ctx, req, from)
	if err != nil {
		return nil, err
	}
	result.Omitted = dropped
	entries, dropped := c.entries(ctx, req, messages)
	result.Entries = entries
	result.Omitted += dropped
	if req.Seed && len(entries) > 0 {
		result.SeedFrom = entries[0].User.MessageSequence
	}
	if result.OlderExists || result.Omitted > 0 {
		result.HistoryOmitted = catchUpHistoryOmittedNote(req, result)
	}
	return result, nil
}

// filter is the visible-message read of the session's lane: the thread of a
// thread session, the whole channel (thread replies included) otherwise.
func (c *conversationCatchUp) filter(req catchUpRequest, after int64) ChannelMessageFilter {
	viewer := req.Viewer
	return ChannelMessageFilter{
		Scope: req.Conversation.Scope, ConversationID: req.Conversation.ID, ThreadRootID: req.Session.ThreadRootID,
		AfterSequence: after, BeforeSequence: req.Through + 1, Limit: catchUpPageSize, Viewer: &viewer,
	}
}

// seedStart is where a new session's catch-up starts: just before the oldest
// of the newest seedMessageCount visible messages (§4.5).
func (c *conversationCatchUp) seedStart(ctx context.Context, req catchUpRequest, from int64) (int64, bool, error) {
	filter := c.filter(req, from)
	filter.Descending, filter.Limit = true, seedMessageCount+1
	recent, err := c.conversations.ListChannelMessages(ctx, filter)
	if err != nil {
		return 0, false, err
	}
	olderExists := len(recent) > seedMessageCount
	if olderExists {
		recent = recent[:seedMessageCount]
	}
	if len(recent) == 0 {
		return req.Through, false, nil
	}
	return recent[len(recent)-1].Sequence - 1, olderExists, nil
}

// read pages the visible messages after from, drops the session's own and
// keeps the newest maximumCatchUpMessages. It returns them ascending and the
// number dropped by the cap.
func (c *conversationCatchUp) read(ctx context.Context, req catchUpRequest, from int64) ([]*ChannelMessage, int, error) {
	own := make(map[string]bool)
	kept := make([]*ChannelMessage, 0, catchUpPageSize)
	dropped := 0
	for after := from; after < req.Through; {
		page, err := c.conversations.ListChannelMessages(ctx, c.filter(req, after))
		if err != nil {
			return nil, 0, err
		}
		for _, message := range page {
			skip, err := c.ownSessionMessage(ctx, req.Session, message, own)
			if err != nil {
				return nil, 0, err
			}
			if !skip {
				kept = append(kept, message)
			}
		}
		if over := len(kept) - maximumCatchUpMessages; over > 0 {
			kept = append(kept[:0:0], kept[over:]...)
			dropped += over
		}
		if len(page) < catchUpPageSize {
			break
		}
		after = page[len(page)-1].Sequence
	}
	return kept, dropped, nil
}

// ownSessionMessage reports whether message was posted by a Run of this
// session (§4.3): an answer, an artifact receipt, a progress message or a
// kernel failure reply. People's messages are never skipped; neither is a
// message of a Run bound to another session (a task report from a child
// session, another thread's answer). cache maps run ids to the verdict.
func (c *conversationCatchUp) ownSessionMessage(ctx context.Context, session *AgentSession, message *ChannelMessage, cache map[string]bool) (bool, error) {
	if message == nil || message.Sender.Type == ConversationParticipantUser {
		return false, nil
	}
	for _, ref := range message.References {
		if ref.Kind != ConversationReferenceRun || ref.ID == "" {
			continue
		}
		own, seen := cache[ref.ID]
		if !seen {
			run, err := c.runs.GetAgentRun(ctx, session.Scope, ref.ID)
			switch {
			case err == nil:
				own = run != nil && run.SessionID == session.ID
			case errors.Is(err, ErrRunNotFound):
				own = false
			default:
				return false, err
			}
			cache[ref.ID] = own
		}
		if own {
			return true, nil
		}
	}
	return false, nil
}

// entries builds the user entries of messages, newest first within
// catchUpAppendBytes, and returns them ascending with the number that did not
// fit.
func (c *conversationCatchUp) entries(ctx context.Context, req catchUpRequest, messages []*ChannelMessage) ([]*SessionEntry, int) {
	teamOwned := req.Conversation.Owner.Type == OwnerTypeTeam
	out := make([]*SessionEntry, len(messages))
	budget, first := catchUpAppendBytes, len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		entry := readConversationMessageEntry(ctx, c.attachments, req.Conversation, req.Session, req.Run.ID, messages[i], teamOwned)
		// The entry id is the ingesting Run's, the SourceKey the message's.
		// A stale collect then never names an entry another Run of the lane
		// stored: the store treats a batch holding some stored ids as a
		// partial replay (a conflict) before it checks the watermark, while a
		// stored SourceKey only skips the duplicate and the CAS reports
		// ErrChannelWatermarkMoved, which the loop retries.
		entry.ID = SessionEntryID(req.Session.ID, catchUpEntryKey(req.Run.ID, messages[i].ID))
		entry.Step = req.Step
		encoded, err := json.Marshal(entry)
		if err != nil || len(encoded) > budget {
			break
		}
		budget -= len(encoded)
		out[i], first = entry, i
	}
	return out[first:], first
}

// catchUpEntryKey is the logical key of the user entry runID ingests for one
// channel message.
func catchUpEntryKey(runID, messageID string) string {
	return "catch-up:" + runID + ":" + ChannelMessageSourceKey(messageID)
}

// catchUpHistoryOmittedNote tells the model that messages of this chat exist
// that the session does not hold. Its key is the Run and the watermark it
// moves to, so a re-collect after a watermark conflict builds the same entry.
func catchUpHistoryOmittedNote(req catchUpRequest, result *catchUpResult) *SessionEntry {
	key := "history-omitted:" + req.Run.ID + ":" + strconv.FormatInt(result.Watermark, 10)
	data, _ := json.Marshal(map[string]interface{}{
		"omitted": result.Omitted, "olderExists": result.OlderExists, "seedFrom": result.SeedFrom, "throughSequence": result.Watermark,
	})
	entry := &SessionEntry{
		ID: SessionEntryID(req.Session.ID, key), Scope: req.Session.Scope, SessionID: req.Session.ID, RunID: req.Run.ID,
		Step: req.Step, Type: SessionEntryNote, SourceKey: key,
		Note: &NoteEntry{Kind: NoteHistoryOmitted, Text: catchUpHistoryOmittedText, Data: data},
	}
	entry.Tokens = estimateEntryTokens(entry)
	return entry
}

// stepContextAppend is the result as the conversation preparer's part of the
// step's context append: the history_omitted note, then the message entries,
// with the watermark CAS from session's watermark. It is nil when there is
// nothing to write.
func (r *catchUpResult) stepContextAppend(session *AgentSession) *StepContextAppend {
	if r == nil || session == nil || (len(r.Entries) == 0 && r.HistoryOmitted == nil && r.Watermark <= session.ChannelWatermark) {
		return nil
	}
	entries := make([]*SessionEntry, 0, len(r.Entries)+1)
	if r.HistoryOmitted != nil {
		entries = append(entries, r.HistoryOmitted)
	}
	entries = append(entries, r.Entries...)
	expected := session.ChannelWatermark
	return &StepContextAppend{Entries: entries, ExpectedChannelWatermark: &expected, ChannelWatermark: max(r.Watermark, expected)}
}
