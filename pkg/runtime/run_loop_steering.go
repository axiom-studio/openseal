package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
	"github.com/axiom-studio/openseal/pkg/runtime/modelstep"
)

// Steering (SPEC §3.4, D8, A10). consumeSteering is the only consumer of the
// AgentRun.ConsumedInterventions cursor. The cursor is written only inside the
// step's one context append (ConsumeInterventionsThrough), so the steering
// entries and the cursor commit together or not at all.

// stepContextAttempts bounds the phase-3 append: the first try plus three
// re-runs of the preparers after ErrChannelWatermarkMoved (SPEC §3.1 phase 3).
const stepContextAttempts = 4

// stepSteering is what consumeSteering decided for one step.
type stepSteering struct {
	// Plan goes to every StepPreparer.
	Plan agentrun.SteeringPlan
	// Entries are the user entries for the unconsumed prefix, in order.
	Entries []*SessionEntry
	// Through is the cursor value the append writes: len(PendingInterventions)
	// when anything is unconsumed, nil otherwise.
	Through *int
}

// unconsumedInterventions is PendingInterventions[ConsumedInterventions:].
func unconsumedInterventions(run *AgentRun) []AgentRunIntervention {
	if run == nil {
		return nil
	}
	start := run.ConsumedInterventions
	if start < 0 {
		start = 0
	}
	if start >= len(run.PendingInterventions) {
		return nil
	}
	return run.PendingInterventions[start:]
}

// steeringEntryID is the logical key of the user entry one intervention
// becomes. Interventions own the "intervention:<id>" key outright. A steering
// message shares only its SourceKey ("message:<id>") with the conversation
// catch-up, not its entry id: an id the catch-up already stored would turn
// the whole step append into a replay conflict, while a stored SourceKey just
// skips the duplicate.
func steeringEntryID(intervention AgentRunIntervention) string {
	if m := intervention.ConversationMessage; m != nil && strings.TrimSpace(m.MessageID) != "" {
		return "steering:" + ChannelMessageSourceKey(m.MessageID)
	}
	return InterventionSourceKey(intervention.ID)
}

// consumeSteering classifies the unconsumed prefix of run's interventions:
//   - ActionCallID set: consumed by the governed/card tool result, no entry;
//   - ConversationAnswer set: consumed by the ask_user result, no entry;
//   - ConversationMessage set: a channel_message user entry with SourceKey
//     message:<id>; the conversation catch-up covers it through
//     Plan.ThroughMessageSequence, and stepContextAppendRequest drops this
//     entry when a preparer appends the same message;
//   - otherwise: a user entry, Origin intervention, SourceKey intervention:<id>.
//
// The cursor moves to len(PendingInterventions) whatever the kinds were.
func consumeSteering(run *AgentRun, step *AgentTurn, session *AgentSession) (stepSteering, error) {
	prefix := unconsumedInterventions(run)
	if len(prefix) == 0 {
		return stepSteering{}, nil
	}
	if session == nil {
		return stepSteering{}, fmt.Errorf("steering run %s: no session", run.ID)
	}
	through := len(run.PendingInterventions)
	out := stepSteering{
		Plan:    agentrun.SteeringPlan{Prefix: append([]AgentRunIntervention(nil), prefix...)},
		Through: &through,
	}
	var stepSequence int64
	if step != nil {
		stepSequence = step.Sequence
	}
	for _, intervention := range prefix {
		var entry *SessionEntry
		switch {
		case intervention.ActionCallID != "":
			continue
		case intervention.ConversationAnswer != nil:
			continue
		case intervention.ConversationMessage != nil && strings.TrimSpace(intervention.ConversationMessage.MessageID) != "":
			message := intervention.ConversationMessage
			if message.Sequence > out.Plan.ThroughMessageSequence {
				out.Plan.ThroughMessageSequence = message.Sequence
			}
			entry = &SessionEntry{
				Type:      SessionEntryUser,
				SourceKey: ChannelMessageSourceKey(message.MessageID),
				User: &UserEntry{
					Origin:          UserEntryChannelMessage,
					Author:          steeringParticipantAuthor(message.Sender),
					MessageID:       message.MessageID,
					ThreadRootID:    message.ThreadRootMessageID,
					MessageSequence: max(message.Sequence, 0),
					InterventionID:  intervention.ID,
				},
			}
		default:
			if strings.TrimSpace(intervention.ID) == "" {
				return stepSteering{}, fmt.Errorf("steering run %s: intervention without id", run.ID)
			}
			entry = &SessionEntry{
				Type:      SessionEntryUser,
				SourceKey: InterventionSourceKey(intervention.ID),
				User: &UserEntry{
					Origin:         UserEntryIntervention,
					Author:         steeringActorAuthor(intervention.Actor),
					InterventionID: intervention.ID,
				},
			}
		}
		content, err := steeringContent(intervention)
		if err != nil {
			return stepSteering{}, fmt.Errorf("steering run %s intervention %s: %w", run.ID, intervention.ID, err)
		}
		entry.ID = SessionEntryID(session.ID, steeringEntryID(intervention))
		entry.Step = stepSequence
		entry.User.Content = content
		entry.Tokens = modelstep.EstimateEntryTokens(entry)
		out.Entries = append(out.Entries, entry)
	}
	return out, nil
}

// steeringContent is the intervention text, then its setup receipt verbatim.
func steeringContent(intervention AgentRunIntervention) ([]ContentPart, error) {
	parts := []ContentPart{{Type: ContentPartText, Text: cutUTF8(intervention.Instruction, MaxUserEntryContentBytes/2)}}
	if receipt := intervention.SkillSetupResolution; receipt != nil {
		value, err := json.Marshal(map[string]interface{}{"skillSetupResolution": receipt})
		if err != nil {
			return nil, err
		}
		parts = append(parts, ContentPart{Type: ContentPartJSON, Value: value})
	}
	return parts, nil
}

func steeringParticipantAuthor(p ConversationParticipant) SessionAuthor {
	return SessionAuthor{Type: steeringAuthorType(string(p.Type)), ID: p.ID}
}

func steeringActorAuthor(a ActivityActor) SessionAuthor {
	return SessionAuthor{Type: steeringAuthorType(a.Type), ID: a.ID}
}

func steeringAuthorType(kind string) string {
	switch kind {
	case "user", "agent":
		return kind
	}
	return "service"
}

// stepContextAppendRequest builds the ONE phase-3 append: the preparer
// entries in preparer order, then the steering entries no preparer already
// carried, the cursor and the (single) watermark CAS. It returns ok=false
// when there is nothing to write. EarlyFinal is the first preparer's.
func stepContextAppendRequest(run *AgentRun, session *AgentSession, workerID string, now time.Time, steering stepSteering, prepared []*agentrun.StepContextAppend) (req AppendAgentSessionEntriesRequest, early *agentrun.LoopFinal, ok bool, err error) {
	req = AppendAgentSessionEntriesRequest{
		Scope:                       run.Scope,
		SessionID:                   session.ID,
		RunID:                       run.ID,
		Lease:                       &AgentRunLeaseGuard{WorkerID: workerID, Now: now},
		ConsumeInterventionsThrough: steering.Through,
		Now:                         now,
	}
	sourceKeys := make(map[string]bool)
	for _, part := range prepared {
		if part == nil {
			continue
		}
		if part.ExpectedChannelWatermark != nil {
			if req.ExpectedChannelWatermark != nil {
				return AppendAgentSessionEntriesRequest{}, nil, false, errors.New("step context: two preparers moved the channel watermark")
			}
			expected := *part.ExpectedChannelWatermark
			req.ExpectedChannelWatermark = &expected
			req.ChannelWatermark = part.ChannelWatermark
		}
		if early == nil && part.EarlyFinal != nil {
			early = part.EarlyFinal
		}
		for _, entry := range part.Entries {
			if entry == nil {
				continue
			}
			if entry.SourceKey != "" {
				sourceKeys[entry.SourceKey] = true
			}
			req.Entries = append(req.Entries, entry)
		}
	}
	for _, entry := range steering.Entries {
		if entry.SourceKey != "" && sourceKeys[entry.SourceKey] {
			continue
		}
		req.Entries = append(req.Entries, entry)
	}
	ok = len(req.Entries) > 0 || req.ConsumeInterventionsThrough != nil || req.ExpectedChannelWatermark != nil
	return req, early, ok, nil
}

// stepContextResult is the outcome of phase 3.
type stepContextResult struct {
	Steering agentrun.SteeringPlan
	// Append is nil when nothing was written.
	Append *AppendAgentSessionEntriesResult
	// Session is the session after the append (or as loaded).
	Session    *AgentSession
	EarlyFinal *agentrun.LoopFinal
	// CursorMoved means the append moved the Run's cursor and so bumped its
	// revision: the caller's Run copy is stale and must be reloaded before the
	// next revision-fenced Run write.
	CursorMoved bool
}

// appendStepContext runs phase 3: consumeSteering, every preparer, then one
// append. ErrChannelWatermarkMoved re-reads the session and re-runs the
// preparers; after stepContextAttempts it is returned (the loop requeues the
// Run as host-unavailable). in.Run must be the Run as read for this step.
func appendStepContext(ctx context.Context, store AgentSessionStore, in agentrun.StepInput, preparers []agentrun.StepPreparer, now time.Time) (*stepContextResult, error) {
	if in.Run == nil || in.Session == nil {
		return nil, errors.New("step context: run and session are required")
	}
	steering, err := consumeSteering(in.Run, in.Step, in.Session)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		prepared := make([]*agentrun.StepContextAppend, 0, len(preparers))
		for _, preparer := range preparers {
			part, err := preparer.BeforeStep(ctx, in, steering.Plan)
			if err != nil {
				return nil, err
			}
			prepared = append(prepared, part)
		}
		req, early, ok, err := stepContextAppendRequest(in.Run, in.Session, in.WorkerID, now, steering, prepared)
		if err != nil {
			return nil, err
		}
		result := &stepContextResult{Steering: steering.Plan, Session: in.Session, EarlyFinal: early}
		if !ok {
			return result, nil
		}
		appended, err := store.AppendAgentSessionEntries(ctx, req)
		if errors.Is(err, ErrChannelWatermarkMoved) && attempt < stepContextAttempts {
			session, getErr := store.GetAgentSession(ctx, in.Run.Scope, in.Session.ID)
			if getErr != nil {
				return nil, getErr
			}
			in.Session = session
			continue
		}
		if err != nil {
			return nil, err
		}
		result.Append = appended
		if appended.Session != nil {
			result.Session = appended.Session
		}
		result.CursorMoved = steering.Through != nil && *steering.Through != in.Run.ConsumedInterventions
		return result, nil
	}
}
